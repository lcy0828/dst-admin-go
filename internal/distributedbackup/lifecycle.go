package distributedbackup

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/tempfiles"
	"github.com/google/uuid"
)

// ConfigureDeletionGuard adds reference checks owned by other coordinators.
// Configure it during application initialization, before accepting requests.
func (c *Coordinator) ConfigureDeletionGuard(guard func(Set) error) { c.deletionGuard = guard }

// Delete removes only this backup set's controller-side artifacts. It never
// addresses a Runtime save directory or deletes protection needed for recovery.
func (c *Coordinator) Delete(ctx context.Context, id, confirmation string) (Set, error) {
	value, err := c.store.GetSet(id)
	if err != nil {
		return Set{}, err
	}
	ctx, release, err := roomops.Acquire(ctx, value.RoomID)
	if err != nil {
		return Set{}, err
	}
	defer release()
	lease, err := c.leases.Acquire(ctx, value.RoomID, "backup-set.delete:"+uuid.NewString(), leaseTTL)
	if err != nil {
		return Set{}, err
	}
	defer c.leases.Release(lease)
	value, err = c.store.GetSet(id)
	if err != nil {
		return Set{}, err
	}
	if confirmation != value.Name {
		return Set{}, ErrConfirmationRequired
	}
	if err := c.deleteSet(value); err != nil {
		return Set{}, err
	}
	return value, nil
}

func (c *Coordinator) setDirectory(id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", ErrInvalidInput
	}
	directory := filepath.Join(c.root, "sets", id)
	for _, candidate := range []string{filepath.Join(c.root, "sets"), directory} {
		info, err := os.Lstat(candidate)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return "", ErrIntegrity
		}
	}
	return directory, nil
}

func (c *Coordinator) deleteSet(value Set) error {
	if value.Status == StatusCreating {
		return ErrInUse
	}
	operations, err := c.store.ActiveOperations()
	if err != nil {
		return err
	}
	for _, op := range operations {
		if op.RoomID == value.RoomID || op.SetID == value.ID || op.ProtectionSetID == value.ID {
			return ErrInUse
		}
	}
	if c.deletionGuard != nil {
		if err := c.deletionGuard(value); err != nil {
			return err
		}
	}
	directory, err := c.setDirectory(value.ID)
	if err != nil {
		return err
	}
	tombstone := filepath.Join(c.root, "sets", ".deleting-"+value.ID)
	if _, err := os.Lstat(tombstone); err == nil {
		return ErrInUse
	} else if !os.IsNotExist(err) {
		return err
	}
	moved := false
	if err := os.Rename(directory, tombstone); err == nil {
		moved = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := c.store.DeleteSet(value.ID); err != nil {
		if moved {
			err = errors.Join(err, os.Rename(tombstone, directory))
		}
		return err
	}
	if moved {
		return os.RemoveAll(tombstone)
	}
	return nil
}

// Reconcile interrupted deletion before operation recovery. If the database
// transaction did not commit, put the backup back instead of discarding it.
func (c *Coordinator) recoverDeletions(ctx context.Context) error {
	entries, err := filepath.Glob(filepath.Join(c.root, "sets", ".deleting-*"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.recoverDeletion(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) recoverDeletion(ctx context.Context, entry string) error {
	id := strings.TrimPrefix(filepath.Base(entry), ".deleting-")
	value, err := c.store.GetSet(id)
	if err == nil {
		// Startup recovery may overlap the first request. Serialize with a live
		// deletion before deciding whether its database transaction committed.
		_, release, lockErr := roomops.Acquire(ctx, value.RoomID)
		if lockErr != nil {
			return lockErr
		}
		defer release()
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	directory, err := c.setDirectory(id)
	if err != nil {
		return err
	}
	if _, err := c.store.GetSet(id); errors.Is(err, ErrNotFound) {
		return os.RemoveAll(entry)
	} else if err != nil {
		return err
	}
	if _, err := os.Lstat(entry); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		return ErrIntegrity
	}
	return os.Rename(entry, directory)
}

// PruneSnapshots keeps the newest verified automatic sets. Manual, protection,
// failed, and in-use sets are never removed by retention.
func (c *Coordinator) PruneSnapshots(ctx context.Context, roomID string, keep int) (int64, int, error) {
	if keep < 1 || keep > 100 {
		return 0, 0, ErrInvalidInput
	}
	ctx, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return 0, 0, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	lease, err := c.leases.Acquire(ctx, roomID, "backup-set.prune:"+uuid.NewString(), leaseTTL)
	if err != nil {
		return 0, 0, err
	}
	defer c.leases.Release(lease)
	values, err := c.store.ListSets(roomID)
	if err != nil {
		return 0, 0, err
	}
	snapshots := make([]Set, 0)
	for _, value := range values {
		if value.Kind == "snapshot" && value.Status == StatusVerified {
			snapshots = append(snapshots, value)
		}
	}
	sort.SliceStable(snapshots, func(i, j int) bool { return snapshots[i].CreatedAt.After(snapshots[j].CreatedAt) })
	var size int64
	count := 0
	for index, value := range snapshots {
		if err := ctx.Err(); err != nil {
			return size, count, err
		}
		if index < keep {
			continue
		}
		if err := c.renewLease(ctx, &lease); err != nil {
			return size, count, err
		}
		if err := ctx.Err(); err != nil {
			return size, count, err
		}
		if err := c.deleteSet(value); err != nil {
			return size, count, err
		}
		size += value.Size
		count++
	}
	return size, count, ctx.Err()
}

// Export returns a portable room ZIP: shared files at its root and one
// directory per world. The caller closes and removes the temporary export.
func (c *Coordinator) Export(ctx context.Context, id string) (*tempfiles.File, Set, error) {
	value, err := c.store.GetSet(id)
	if err != nil {
		return nil, Set{}, err
	}
	_, release, err := roomops.Acquire(ctx, value.RoomID)
	if err != nil {
		return nil, Set{}, err
	}
	defer release()
	value, err = c.store.GetSet(id)
	if err != nil {
		return nil, Set{}, err
	}
	if value.Status != StatusVerified || len(value.Parts) == 0 {
		return nil, Set{}, ErrIncomplete
	}
	if _, err := c.setDirectory(id); err != nil {
		return nil, Set{}, err
	}
	parts := append([]Part(nil), value.Parts...)
	sort.SliceStable(parts, func(i, j int) bool {
		return parts[i].WorldRole == string(rooms.WorldRoleMaster) && parts[j].WorldRole != string(rooms.WorldRoleMaster)
	})
	// Verify every source before producing a downloadable artifact.
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, Set{}, err
		}
		source, err := c.partPath(part)
		if err != nil {
			return nil, Set{}, err
		}
		if part.Status != PartVerified {
			return nil, Set{}, ErrIncomplete
		}
		if err := verifyRegularFile(ctx, source, part.Size, part.SHA256); err != nil {
			return nil, Set{}, err
		}
	}
	file, err := tempfiles.Create(ctx, c.root, tempfiles.Exports)
	if err != nil {
		return nil, Set{}, err
	}
	complete := false
	defer func() {
		if !complete {
			file.Close()
		}
	}()
	archive := zip.NewWriter(file)
	seen := map[string]bool{}
	for index, part := range parts {
		if part.Shard == "" || part.Shard == "." || part.Shard == ".." || strings.ContainsAny(part.Shard, "/\\\x00") {
			return nil, Set{}, ErrIntegrity
		}
		source, _ := c.partPath(part)
		if err := appendExportPart(ctx, archive, source, part.Shard, index == 0, seen); err != nil {
			return nil, Set{}, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, Set{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, Set{}, err
	}
	complete = true
	return file, value, nil
}

func appendExportPart(ctx context.Context, archive *zip.Writer, source, shard string, shared bool, seen map[string]bool) error {
	input, err := zip.OpenReader(source)
	if err != nil {
		return err
	}
	defer input.Close()
	for _, entry := range input.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name
		if entry.FileInfo().IsDir() {
			continue
		}
		if !entry.Mode().IsRegular() || path.Clean(name) != name || strings.ContainsAny(name, "\\\x00") {
			return ErrIntegrity
		}
		switch {
		case strings.HasPrefix(name, "shared/"):
			if !shared {
				continue
			}
			name = strings.TrimPrefix(name, "shared/")
		case strings.HasPrefix(name, "shard/"):
			name = shard + "/" + strings.TrimPrefix(name, "shard/")
		default:
			return ErrIntegrity
		}
		if name == "" || name == "." || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || seen[name] {
			return ErrIntegrity
		}
		seen[name] = true
		header := entry.FileHeader
		header.Name = name
		output, err := archive.CreateRaw(&header)
		if err != nil {
			return err
		}
		raw, err := entry.OpenRaw()
		if err != nil {
			return err
		}
		if _, err := io.Copy(output, exportReader{ctx, raw}); err != nil {
			return err
		}
	}
	return nil
}

type exportReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r exportReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
