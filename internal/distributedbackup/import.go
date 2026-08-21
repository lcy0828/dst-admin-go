package distributedbackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dont/internal/roomops"
	"dont/internal/shardtransfer"
)

// ImportDirectory turns an analyzed controller-side Cluster directory into a
// verified BackupSet bound to the room's current Placement. Restore then uses
// the normal transactional transfer path for local, Agent, or mixed targets.
func (c *Coordinator) ImportDirectory(ctx context.Context, request DirectoryImportRequest) (Set, error) {
	request.RoomID = strings.TrimSpace(request.RoomID)
	if request.RoomID == "" {
		return Set{}, ErrInvalidInput
	}
	ctx, releaseRoom, err := roomops.Acquire(ctx, request.RoomID)
	if err != nil {
		return Set{}, err
	}
	defer releaseRoom()

	room, runtimeParts, revision, running, err := c.plan(ctx, request.RoomID)
	if err != nil {
		return Set{}, err
	}
	sourceRoot, sourceWorlds, err := validateDirectoryImport(request.SourceRoot, request.Worlds, runtimeParts)
	if err != nil {
		return Set{}, err
	}

	now := c.now().UTC()
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = "导入存档 " + now.Format("2006-01-02 15:04:05")
	}
	if len([]rune(name)) > 128 || strings.ContainsAny(name, "\x00\r\n") {
		return Set{}, ErrInvalidInput
	}
	kind := strings.TrimSpace(request.Kind)
	if kind == "" {
		kind = "import"
	}
	set := Set{
		ID: runtimeParts[0].part.SetID, RoomID: room.ID, RoomName: room.Name, Name: name,
		Kind: kind, Mode: ModeCold, ManifestVersion: manifestVersion, TopologyRevision: revision,
		Status: StatusCreating, OriginalRunningWorlds: append([]string(nil), running...),
		SourceJobID: strings.TrimSpace(request.SourceJobID), CreatedAt: now, UpdatedAt: now,
	}
	parts := make([]Part, 0, len(runtimeParts))
	for _, current := range runtimeParts {
		parts = append(parts, current.part)
	}
	set, err = c.store.CreateSet(set, parts)
	if err != nil {
		return Set{}, err
	}

	sharedSHA := ""
	for index := range runtimeParts {
		current := &runtimeParts[index]
		partPath, pathErr := c.partPath(current.part)
		if pathErr != nil {
			return c.failDirectoryImport(set, pathErr)
		}
		descriptor, createErr := shardtransfer.CreateBackupArchive(
			ctx, current.part.ID, current.target.Cluster, current.target.Shard,
			sourceRoot, filepath.Join(sourceRoot, sourceWorlds[current.part.WorldID]), partPath,
		)
		if createErr != nil {
			return c.failDirectoryImport(set, createErr)
		}
		inspection, inspectErr := shardtransfer.InspectBackupArchive(partPath)
		if inspectErr != nil {
			return c.failDirectoryImport(set, errors.Join(ErrIntegrity, inspectErr))
		}
		if descriptor.BackupID != current.part.ID || descriptor.Size < 1 || descriptor.ContentSize < 1 ||
			descriptor.FileCount < 2 || !inspection.Restorable {
			return c.failDirectoryImport(set, errors.Join(ErrNotRestorable, errors.New(inspection.ValidationError)))
		}
		if sharedSHA == "" {
			sharedSHA = descriptor.SharedSHA256
		} else if !strings.EqualFold(sharedSHA, descriptor.SharedSHA256) {
			return c.failDirectoryImport(set, ErrSharedFilesDiffer)
		}
		verifiedAt := c.now().UTC()
		part := current.part
		part.Status, part.Size, part.ContentSize, part.FileCount = PartVerified, descriptor.Size, descriptor.ContentSize, descriptor.FileCount
		part.SHA256, part.SharedSHA256, part.VerifiedAt = strings.ToLower(descriptor.SHA256), strings.ToLower(descriptor.SharedSHA256), &verifiedAt
		part.ContentKind, part.Restorable = inspection.ContentKind, inspection.Restorable
		part.SessionID, part.LatestSnapshot, part.HasShardIndex = inspection.SessionID, inspection.LatestSnapshot, inspection.HasShardIndex
		part.ValidationError, part.Failure = inspection.ValidationError, ""
		if _, err := c.store.SavePart(part); err != nil {
			return c.failDirectoryImport(set, err)
		}
	}
	result, err := c.finalizeSet(set.ID, sharedSHA)
	if err != nil {
		return c.failDirectoryImport(set, err)
	}
	return result, nil
}

func validateDirectoryImport(sourceRoot string, worlds []DirectoryImportWorld, runtimeParts []runtimePart) (string, map[string]string, error) {
	sourceRoot = strings.TrimSpace(sourceRoot)
	if sourceRoot == "" || len(worlds) != len(runtimeParts) || len(runtimeParts) == 0 {
		return "", nil, ErrImportWorldMismatch
	}
	absolute, err := filepath.Abs(sourceRoot)
	if err != nil {
		return "", nil, ErrInvalidInput
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", nil, errors.Join(err, ErrInvalidInput)
	}
	worldIDs := make(map[string]bool, len(runtimeParts))
	for _, part := range runtimeParts {
		worldIDs[part.part.WorldID] = true
	}
	result := make(map[string]string, len(worlds))
	usedDirectories := make(map[string]bool, len(worlds))
	for _, world := range worlds {
		world.WorldID = strings.TrimSpace(world.WorldID)
		world.DirectoryName = strings.TrimSpace(world.DirectoryName)
		if !worldIDs[world.WorldID] || result[world.WorldID] != "" || world.DirectoryName == "" ||
			filepath.Base(world.DirectoryName) != world.DirectoryName || strings.ContainsAny(world.DirectoryName, "\x00/\\\r\n") || usedDirectories[world.DirectoryName] {
			return "", nil, ErrImportWorldMismatch
		}
		path := filepath.Join(absolute, world.DirectoryName)
		if !containedPath(absolute, path) {
			return "", nil, ErrImportWorldMismatch
		}
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", nil, errors.Join(statErr, ErrImportWorldMismatch)
		}
		result[world.WorldID], usedDirectories[world.DirectoryName] = world.DirectoryName, true
	}
	for worldID := range worldIDs {
		if result[worldID] == "" {
			return "", nil, ErrImportWorldMismatch
		}
	}
	return filepath.Clean(absolute), result, nil
}

func (c *Coordinator) failDirectoryImport(value Set, cause error) (Set, error) {
	current, loadErr := c.store.GetSet(value.ID)
	if loadErr != nil {
		return value, errors.Join(cause, loadErr)
	}
	verified := 0
	for _, part := range current.Parts {
		if part.Status == PartVerified {
			verified++
		}
	}
	current.Status = StatusFailed
	if verified > 0 {
		current.Status = StatusPartial
	}
	current.Failure, current.UpdatedAt = cause.Error(), c.now().UTC()
	saved, saveErr := c.store.SaveSet(current)
	return saved, errors.Join(fmt.Errorf("import save directory: %w", cause), saveErr)
}
