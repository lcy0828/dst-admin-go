package saveimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"dont/internal/backups"
	"dont/internal/mods"
	"dont/internal/roomops"
	"dont/internal/rooms"

	"github.com/go-ini/ini"
	"github.com/google/uuid"
)

var directoryNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

const minimumImportHeadroom = uint64(256 * 1024 * 1024)

func (s *Service) Apply(ctx context.Context, id, jobID string, request ApplyRequest) (result ApplyResult, resultErr error) {
	if s.rooms == nil || s.runtime == nil || s.backups == nil {
		return ApplyResult{}, errors.New("save import deployment dependencies are unavailable")
	}
	lock := s.importLock(id)
	lock.Lock()
	defer lock.Unlock()
	value, err := s.store.Get(id)
	if err != nil {
		return ApplyResult{}, err
	}
	if value.Status != StatusReady && value.Status != StatusApplied {
		return ApplyResult{}, ErrImportNotReady
	}
	if value.Manifest == nil {
		return ApplyResult{}, ErrImportNotReady
	}
	candidate, err := findCandidate(*value.Manifest, request.CandidateID)
	if err != nil {
		return ApplyResult{}, err
	}
	if candidate.Compatibility == "blocked" {
		return ApplyResult{}, ErrInvalidArchive
	}
	if hasDiagnostic(candidate.Diagnostics, "MASTER_MISSING") && !request.AllowPartial {
		return ApplyResult{}, ErrPartialImport
	}
	if err := validateApplyPolicies(request); err != nil {
		return ApplyResult{}, err
	}

	operationRoomID := strings.TrimSpace(request.TargetRoomID)
	if request.Mode != ApplyModeReplace {
		operationRoomID = rooms.EncodeID(request.DirectoryName)
	}
	ctx, releaseRoom, err := roomops.Acquire(ctx, operationRoomID)
	if err != nil {
		return ApplyResult{}, err
	}
	defer releaseRoom()
	target, targetRoom, err := s.resolveApplyTarget(ctx, request)
	if err != nil {
		return ApplyResult{}, err
	}
	contentRoot := filepath.Join(s.config.ImportRoot, id, "content")
	sourceRoot := filepath.Join(contentRoot, filepath.FromSlash(candidate.Root))
	if !contained(contentRoot, sourceRoot) || !regularDirectory(sourceRoot) {
		return ApplyResult{}, ErrUnsafeArchive
	}
	usage, err := inspectDiskUsage(s.config.SaveRoot)
	if err != nil {
		return ApplyResult{}, err
	}
	required := uint64(value.Manifest.ContentSize) + minimumImportHeadroom
	if usage.Free < required {
		return ApplyResult{}, backups.ErrInsufficientSpace
	}
	staging, err := os.MkdirTemp(s.config.SaveRoot, ".dst-admin-import-")
	if err != nil {
		return ApplyResult{}, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := copyDirectory(ctx, sourceRoot, staging); err != nil {
		return ApplyResult{}, err
	}
	if err := canonicalizeDSTNames(staging); err != nil {
		return ApplyResult{}, err
	}
	if strings.TrimSpace(request.RoomName) != "" {
		if err := setClusterName(staging, request.RoomName); err != nil {
			return ApplyResult{}, err
		}
	}
	if err := s.applyTokenPolicy(staging, target, request); err != nil {
		return ApplyResult{}, err
	}
	if err := s.applyNetworkPolicy(staging, target, request.NetworkPolicy); err != nil {
		return ApplyResult{}, err
	}
	installedMods, warnings, err := s.reconcileMods(ctx, candidate, request.ModPolicy)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := verifyStagedRoom(staging, request.AllowPartial); err != nil {
		return ApplyResult{}, err
	}

	protectionID := ""
	if request.Mode == ApplyModeReplace {
		protection, err := s.backups.Create(ctx, targetRoom.ID, "导入存档前保护备份 "+s.now().Format("2006-01-02 15:04:05"), backups.KindProtection, jobID)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("create import protection backup: %w", err)
		}
		protectionID = protection.ID
	}
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}
	rollback := ""
	if request.Mode == ApplyModeReplace {
		rollback = filepath.Join(s.config.SaveRoot, ".dst-admin-import-rollback-"+uuid.NewString())
	}
	recovery := importRecord{
		ID: id, Status: string(StatusApplying), ApplyPhase: applyPhasePublishing, ApplyMode: string(request.Mode),
		ApplyRoomID: operationRoomID, ApplyTarget: filepath.Base(target), ApplyStaging: filepath.Base(staging),
		ApplyRollback: pathBaseOrEmpty(rollback),
	}
	if err := s.store.BeginApply(id, request.Mode, operationRoomID, recovery.ApplyTarget, recovery.ApplyStaging, recovery.ApplyRollback); err != nil {
		return ApplyResult{}, err
	}
	committed := false
	defer func() {
		if resultErr == nil || committed {
			return
		}
		if rollbackErr := s.rollbackApply(recovery); rollbackErr != nil {
			resultErr = fmt.Errorf("%v; rollback imported room: %w", resultErr, rollbackErr)
			return
		}
		published = false
		_ = s.store.MarkApplyFailed(id, ErrorCode(resultErr), resultErr.Error())
	}()
	if rollback != "" {
		if err := os.Rename(target, rollback); err != nil {
			return ApplyResult{}, fmt.Errorf("stage current room before import: %w", err)
		}
	}
	if err := os.Rename(staging, target); err != nil {
		return ApplyResult{}, fmt.Errorf("publish imported room: %w", err)
	}
	published = true
	roomID := rooms.EncodeID(filepath.Base(target))
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("verify published room: %w", err)
	}
	if request.Mode != ApplyModeReplace {
		room, err = s.rooms.Adopt(roomID)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("adopt imported room: %w", err)
		}
	}
	if err := s.store.MarkApplyCommitted(id); err != nil {
		return ApplyResult{}, err
	}
	recovery.ApplyPhase = applyPhaseCommitted
	committed = true
	if err := s.finalizeCommittedApply(recovery); err != nil {
		return ApplyResult{}, err
	}
	now := s.now().UTC()
	return ApplyResult{
		ImportID: id, CandidateID: candidate.ID, Mode: request.Mode, RoomID: room.ID,
		DirectoryName: room.DirectoryName, RoomName: room.Name, ProtectionBackupID: protectionID,
		InstalledMods: installedMods, Warnings: warnings, AppliedAt: now,
	}, nil
}

func pathBaseOrEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return filepath.Base(value)
}

func (s *Service) recoverApply(record importRecord) error {
	if strings.TrimSpace(record.ApplyTarget) == "" {
		return s.store.MarkApplyFailed(record.ID, "SERVER_RESTARTED", "服务重启中断了旧版存档部署，请重新执行")
	}
	if record.ApplyPhase == applyPhaseCommitted {
		return s.finalizeCommittedApply(record)
	}
	if err := s.rollbackApply(record); err != nil {
		return err
	}
	_, staging, _, err := s.applyRecoveryPaths(record)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	return s.store.MarkApplyFailed(record.ID, "SERVER_RESTARTED", "服务重启中断了存档部署，已恢复部署前状态")
}

func (s *Service) rollbackApply(record importRecord) error {
	target, staging, rollback, err := s.applyRecoveryPaths(record)
	if err != nil {
		return err
	}
	mode := ApplyMode(record.ApplyMode)
	if mode != ApplyModeReplace {
		if strings.TrimSpace(record.ApplyRoomID) != "" && s.rooms != nil {
			if err := s.rooms.Unadopt(record.ApplyRoomID); err != nil {
				return fmt.Errorf("remove interrupted room adoption: %w", err)
			}
		}
		if regularDirectory(staging) {
			// Publishing never consumed staging, so any target that appeared is
			// not owned by this import and must be left untouched.
			return nil
		}
		if regularDirectory(target) {
			if err := os.Rename(target, staging); err != nil {
				return err
			}
		}
		return nil
	}

	if rollback == "" || !regularDirectory(rollback) {
		// The original room is still at the target when the first rename did
		// not happen. The staged import can be discarded by the caller.
		if regularDirectory(target) {
			return nil
		}
		return errors.New("interrupted replacement has neither target nor rollback directory")
	}
	if regularDirectory(target) {
		if regularDirectory(staging) {
			return errors.New("replacement target appeared before the staged import was published")
		} else if err := os.Rename(target, staging); err != nil {
			return err
		}
	}
	if err := os.Rename(rollback, target); err != nil {
		return fmt.Errorf("restore replaced room: %w", err)
	}
	return nil
}

func (s *Service) finalizeCommittedApply(record importRecord) error {
	target, staging, rollback, err := s.applyRecoveryPaths(record)
	if err != nil {
		return err
	}
	if !regularDirectory(target) {
		return errors.New("committed import target is missing")
	}
	if ApplyMode(record.ApplyMode) != ApplyModeReplace && s.rooms != nil {
		room, roomErr := s.rooms.Room(record.ApplyRoomID)
		if roomErr != nil {
			return roomErr
		}
		if !room.Managed {
			if _, roomErr = s.rooms.Adopt(record.ApplyRoomID); roomErr != nil {
				return roomErr
			}
		}
	}
	if Status(record.Status) == StatusApplying {
		if _, err := s.store.MarkApplied(record.ID); err != nil {
			return err
		}
	}
	if rollback != "" {
		if err := os.RemoveAll(rollback); err != nil {
			return fmt.Errorf("remove committed import rollback: %w", err)
		}
	}
	if regularDirectory(staging) {
		if err := os.RemoveAll(staging); err != nil {
			return fmt.Errorf("remove committed import staging: %w", err)
		}
	}
	return s.store.ClearApplyJournal(record.ID)
}

func (s *Service) applyRecoveryPaths(record importRecord) (string, string, string, error) {
	if !directoryNamePattern.MatchString(record.ApplyTarget) ||
		!strings.HasPrefix(record.ApplyStaging, ".dst-admin-import-") || filepath.Base(record.ApplyStaging) != record.ApplyStaging {
		return "", "", "", ErrUnsafeArchive
	}
	if record.ApplyRollback != "" && (!strings.HasPrefix(record.ApplyRollback, ".dst-admin-import-rollback-") || filepath.Base(record.ApplyRollback) != record.ApplyRollback) {
		return "", "", "", ErrUnsafeArchive
	}
	target := filepath.Join(s.config.SaveRoot, record.ApplyTarget)
	staging := filepath.Join(s.config.SaveRoot, record.ApplyStaging)
	rollback := ""
	if record.ApplyRollback != "" {
		rollback = filepath.Join(s.config.SaveRoot, record.ApplyRollback)
	}
	if !contained(s.config.SaveRoot, target) || !contained(s.config.SaveRoot, staging) || rollback != "" && !contained(s.config.SaveRoot, rollback) {
		return "", "", "", ErrUnsafeArchive
	}
	return target, staging, rollback, nil
}

func (s *Service) resolveApplyTarget(ctx context.Context, request ApplyRequest) (string, rooms.Room, error) {
	switch request.Mode {
	case ApplyModeNew, ApplyModeClone:
		if !directoryNamePattern.MatchString(request.DirectoryName) {
			return "", rooms.Room{}, ErrInvalidRequest
		}
		target := filepath.Join(s.config.SaveRoot, request.DirectoryName)
		if !contained(s.config.SaveRoot, target) {
			return "", rooms.Room{}, ErrInvalidRequest
		}
		if _, err := os.Lstat(target); err == nil {
			return "", rooms.Room{}, ErrTargetExists
		} else if !os.IsNotExist(err) {
			return "", rooms.Room{}, err
		}
		return target, rooms.Room{}, nil
	case ApplyModeReplace:
		room, err := s.rooms.Room(request.TargetRoomID)
		if err != nil {
			return "", rooms.Room{}, err
		}
		if !room.Managed {
			return "", rooms.Room{}, ErrTargetNotManaged
		}
		if request.Confirmation != room.Name {
			return "", rooms.Room{}, ErrConfirmation
		}
		worlds, err := s.rooms.Worlds(room.ID)
		if err != nil {
			return "", rooms.Room{}, err
		}
		for _, world := range worlds {
			running, err := s.runtime.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
			if err != nil {
				return "", rooms.Room{}, err
			}
			if running {
				return "", rooms.Room{}, ErrRoomRunning
			}
		}
		return filepath.Join(s.config.SaveRoot, room.DirectoryName), room, nil
	default:
		return "", rooms.Room{}, ErrInvalidRequest
	}
}

func validateApplyPolicies(request ApplyRequest) error {
	validToken := request.TokenPolicy == TokenSource || request.TokenPolicy == TokenPreserve || request.TokenPolicy == TokenProvided || request.TokenPolicy == TokenNone
	validNetwork := request.NetworkPolicy == NetworkSource || request.NetworkPolicy == NetworkPreserve || request.NetworkPolicy == NetworkAuto
	validMods := request.ModPolicy == ModsPreserve || request.ModPolicy == ModsRequireDownloaded || request.ModPolicy == ModsInstallMissing
	if !validToken || !validNetwork || !validMods {
		return ErrInvalidRequest
	}
	if request.Mode != ApplyModeReplace && (request.TokenPolicy == TokenPreserve || request.NetworkPolicy == NetworkPreserve) {
		return ErrInvalidRequest
	}
	return nil
}

func findCandidate(manifest Manifest, id string) (Candidate, error) {
	for _, candidate := range manifest.Candidates {
		if candidate.ID == id {
			return candidate, nil
		}
	}
	return Candidate{}, ErrCandidateMissing
}

func hasDiagnostic(items []Diagnostic, code string) bool {
	for _, item := range items {
		if item.Code == code {
			return true
		}
	}
	return false
}

func (s *Service) applyTokenPolicy(staging, target string, request ApplyRequest) error {
	tokenPath := filepath.Join(staging, "cluster_token.txt")
	switch request.TokenPolicy {
	case TokenSource:
		if !regularFile(tokenPath) && !request.AllowMissingToken {
			return ErrTokenRequired
		}
		if regularFile(tokenPath) {
			content, err := os.ReadFile(tokenPath)
			if err != nil {
				return err
			}
			if err := rooms.ValidateClusterToken(strings.TrimSpace(string(content)), false); err != nil {
				return err
			}
			if err := os.Chmod(tokenPath, 0600); err != nil {
				return err
			}
		}
	case TokenPreserve:
		source := filepath.Join(target, "cluster_token.txt")
		if regularFile(source) {
			content, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			if err := os.WriteFile(tokenPath, content, 0600); err != nil {
				return err
			}
		} else {
			_ = os.Remove(tokenPath)
			if !request.AllowMissingToken {
				return ErrTokenRequired
			}
		}
	case TokenProvided:
		if err := rooms.ValidateClusterToken(request.ClusterToken, false); err != nil {
			return err
		}
		if err := os.WriteFile(tokenPath, []byte(strings.TrimSpace(request.ClusterToken)+"\n"), 0600); err != nil {
			return err
		}
	case TokenNone:
		if !request.AllowMissingToken {
			return ErrTokenRequired
		}
		if err := os.Remove(tokenPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Service) applyNetworkPolicy(staging, target string, policy NetworkPolicy) error {
	switch policy {
	case NetworkSource:
		used, err := usedPorts(s.config.SaveRoot, target)
		if err != nil {
			return err
		}
		return validateNetworkConfiguration(staging, used)
	case NetworkPreserve:
		if err := preserveNetworkConfiguration(target, staging); err != nil {
			return err
		}
		used, err := usedPorts(s.config.SaveRoot, target)
		if err != nil {
			return err
		}
		return validateNetworkConfiguration(staging, used)
	case NetworkAuto:
		used, err := usedPorts(s.config.SaveRoot, target)
		if err != nil {
			return err
		}
		return allocateNetworkConfiguration(staging, used)
	default:
		return ErrInvalidRequest
	}
}

func (s *Service) reconcileMods(ctx context.Context, candidate Candidate, policy ModPolicy) ([]string, []Diagnostic, error) {
	missing := make([]string, 0)
	available := make([]string, 0)
	for _, mod := range candidate.Mods {
		if !regularDirectory(filepath.Join(s.config.WorkshopRoot, mod.ID)) {
			missing = append(missing, mod.ID)
		} else {
			available = append(available, mod.ID)
		}
	}
	if len(missing) == 0 {
		if s.mods != nil {
			if err := s.mods.EnsureLibrarySetup(available); err != nil {
				return nil, nil, err
			}
		}
		return []string{}, []Diagnostic{}, nil
	}
	switch policy {
	case ModsRequireDownloaded:
		return nil, nil, fmt.Errorf("%w: %s", ErrMissingMods, strings.Join(missing, ", "))
	case ModsInstallMissing:
		if s.mods == nil {
			return nil, nil, errors.New("mod download service is unavailable")
		}
		installed := make([]string, 0, len(missing))
		for _, id := range missing {
			result, err := s.mods.Download(ctx, mods.DownloadRequest{ModID: id, IncludeDependencies: true}, io.Discard)
			if err != nil {
				return installed, nil, fmt.Errorf("download workshop mod %s: %w", id, err)
			}
			installed = append(installed, result.ModIDs...)
		}
		sort.Strings(installed)
		if err := s.mods.EnsureLibrarySetup(append(available, installed...)); err != nil {
			return installed, nil, err
		}
		return uniqueStrings(installed), []Diagnostic{}, nil
	default:
		if s.mods != nil && len(available) > 0 {
			if err := s.mods.EnsureLibrarySetup(available); err != nil {
				return nil, nil, err
			}
		}
		return []string{}, []Diagnostic{{
			Code: "WORKSHOP_MODS_NOT_INSTALLED", Severity: SeverityWarning,
			Message: "模组配置已保留，但缺失的 Workshop 模组尚未下载", Details: map[string]interface{}{"ids": missing},
		}}, nil
	}
}

func copyDirectory(ctx context.Context, source, destination string) error {
	return filepath.WalkDir(source, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, filePath)
		if err != nil || !contained(source, filePath) {
			return ErrUnsafeArchive
		}
		if relative == "." || isSystemMetadata(relative, entry.Name()) || isInternalAdminDirectory(entry) {
			if relative != "." && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return ErrUnsafeArchive
		}
		target := filepath.Join(destination, relative)
		if !contained(destination, target) {
			return ErrUnsafeArchive
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0750)
		}
		input, err := os.Open(filePath)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := copyContext(ctx, output, input)
		closeErr := output.Close()
		closeInputErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return closeInputErr
	})
}

func canonicalizeDSTNames(root string) error {
	if err := canonicalizeFile(root, "cluster.ini"); err != nil {
		return err
	}
	if err := canonicalizeFile(root, "cluster_token.txt"); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := canonicalizeFile(filepath.Join(root, entry.Name()), "server.ini"); err != nil {
				return err
			}
			if err := canonicalizeFile(filepath.Join(root, entry.Name()), "modoverrides.lua"); err != nil {
				return err
			}
			if err := canonicalizeFile(filepath.Join(root, entry.Name()), "leveldataoverride.lua"); err != nil {
				return err
			}
		}
	}
	return nil
}

func canonicalizeFile(directory, expected string) error {
	actual := findFileFold(directory, expected)
	if actual == "" || filepath.Base(actual) == expected {
		return nil
	}
	target := filepath.Join(directory, expected)
	temporary := filepath.Join(directory, ".dst-admin-case-"+uuid.NewString())
	if err := os.Rename(actual, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		_ = os.Rename(temporary, actual)
		return err
	}
	return nil
}

func setClusterName(root, name string) error {
	name, err := normalizeDisplayName(name, "")
	if err != nil {
		return err
	}
	filePath := filepath.Join(root, "cluster.ini")
	config, err := ini.Load(filePath)
	if err != nil {
		return ErrInvalidArchive
	}
	config.Section("NETWORK").Key("cluster_name").SetValue(name)
	return config.SaveTo(filePath)
}

func preserveNetworkConfiguration(sourceRoot, destinationRoot string) error {
	sourceCluster, err := ini.Load(filepath.Join(sourceRoot, "cluster.ini"))
	if err != nil {
		return err
	}
	destinationCluster, err := ini.Load(filepath.Join(destinationRoot, "cluster.ini"))
	if err != nil {
		return err
	}
	for _, key := range []string{"bind_ip", "master_ip", "master_port", "cluster_key"} {
		value := sourceCluster.Section("SHARD").Key(key).String()
		if value != "" {
			destinationCluster.Section("SHARD").Key(key).SetValue(value)
		}
	}
	if err := destinationCluster.SaveTo(filepath.Join(destinationRoot, "cluster.ini")); err != nil {
		return err
	}
	sourceWorlds, err := readNetworkWorlds(sourceRoot)
	if err != nil {
		return err
	}
	destinationWorlds, err := readNetworkWorlds(destinationRoot)
	if err != nil {
		return err
	}
	usedSource := make(map[string]bool)
	for _, destinationWorld := range destinationWorlds {
		sourceWorld := matchNetworkWorld(sourceWorlds, destinationWorld, usedSource)
		if sourceWorld == nil {
			continue
		}
		usedSource[sourceWorld.directory] = true
		copyINIKey(sourceWorld.config, destinationWorld.config, "NETWORK", "server_port")
		copyINIKey(sourceWorld.config, destinationWorld.config, "STEAM", "authentication_port")
		copyINIKey(sourceWorld.config, destinationWorld.config, "STEAM", "master_server_port")
		if err := destinationWorld.config.SaveTo(destinationWorld.path); err != nil {
			return err
		}
	}
	return nil
}

type networkWorld struct {
	directory string
	path      string
	shardID   int
	master    bool
	config    *ini.File
}

func readNetworkWorlds(root string) ([]networkWorld, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	worlds := make([]networkWorld, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		filePath := filepath.Join(root, entry.Name(), "server.ini")
		if !regularFile(filePath) {
			continue
		}
		config, err := ini.Load(filePath)
		if err != nil {
			return nil, err
		}
		worlds = append(worlds, networkWorld{
			directory: entry.Name(), path: filePath,
			shardID: config.Section("SHARD").Key("id").MustInt(0),
			master:  config.Section("SHARD").Key("is_master").MustBool(false), config: config,
		})
	}
	return worlds, nil
}

func matchNetworkWorld(sources []networkWorld, destination networkWorld, used map[string]bool) *networkWorld {
	for index := range sources {
		if !used[sources[index].directory] && strings.EqualFold(sources[index].directory, destination.directory) {
			return &sources[index]
		}
	}
	if destination.shardID > 0 {
		for index := range sources {
			if !used[sources[index].directory] && sources[index].shardID == destination.shardID {
				return &sources[index]
			}
		}
	}
	if destination.master {
		for index := range sources {
			if !used[sources[index].directory] && sources[index].master {
				return &sources[index]
			}
		}
	}
	return nil
}

func validateNetworkConfiguration(root string, used map[int]bool) error {
	reserve := func(value int) error {
		if value == 0 {
			return nil
		}
		if value < 1 || value > 65535 || used[value] {
			return fmt.Errorf("%w: %d", ErrPortConflict, value)
		}
		used[value] = true
		return nil
	}
	cluster, err := ini.Load(filepath.Join(root, "cluster.ini"))
	if err != nil {
		return err
	}
	if err := reserve(cluster.Section("SHARD").Key("master_port").MustInt(0)); err != nil {
		return err
	}
	worlds, err := readNetworkWorlds(root)
	if err != nil {
		return err
	}
	for _, world := range worlds {
		for _, value := range []int{
			world.config.Section("NETWORK").Key("server_port").MustInt(0),
			world.config.Section("STEAM").Key("authentication_port").MustInt(0),
			world.config.Section("STEAM").Key("master_server_port").MustInt(0),
		} {
			if err := reserve(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyINIKey(source, destination *ini.File, section, key string) {
	value := source.Section(section).Key(key).String()
	if value != "" {
		destination.Section(section).Key(key).SetValue(value)
	}
}

func usedPorts(saveRoot, excludedRoot string) (map[int]bool, error) {
	used := make(map[int]bool)
	roomsEntries, err := os.ReadDir(saveRoot)
	if err != nil {
		return nil, err
	}
	for _, roomEntry := range roomsEntries {
		if !roomEntry.IsDir() || strings.HasPrefix(roomEntry.Name(), ".") {
			continue
		}
		roomRoot := filepath.Join(saveRoot, roomEntry.Name())
		if filepath.Clean(roomRoot) == filepath.Clean(excludedRoot) {
			continue
		}
		if cluster, err := ini.Load(filepath.Join(roomRoot, "cluster.ini")); err == nil {
			if value := cluster.Section("SHARD").Key("master_port").MustInt(0); value > 0 {
				used[value] = true
			}
		}
		worldEntries, _ := os.ReadDir(roomRoot)
		for _, worldEntry := range worldEntries {
			if !worldEntry.IsDir() {
				continue
			}
			config, err := ini.Load(filepath.Join(roomRoot, worldEntry.Name(), "server.ini"))
			if err != nil {
				continue
			}
			for _, value := range []int{
				config.Section("NETWORK").Key("server_port").MustInt(0),
				config.Section("STEAM").Key("authentication_port").MustInt(0),
				config.Section("STEAM").Key("master_server_port").MustInt(0),
			} {
				if value > 0 {
					used[value] = true
				}
			}
		}
	}
	return used, nil
}

func allocateNetworkConfiguration(root string, used map[int]bool) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	clusterPath := filepath.Join(root, "cluster.ini")
	cluster, err := ini.Load(clusterPath)
	if err != nil {
		return err
	}
	clusterPort := nextAvailablePort(10889, used)
	if clusterPort == 0 {
		return ErrPortConflict
	}
	used[clusterPort] = true
	cluster.Section("SHARD").Key("master_port").SetValue(fmt.Sprintf("%d", clusterPort))
	if err := cluster.SaveTo(clusterPath); err != nil {
		return err
	}
	if err := os.Chmod(clusterPath, 0640); err != nil {
		return err
	}
	serverPort, authPort, masterPort := 10999, 8767, 27017
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		filePath := filepath.Join(root, entry.Name(), "server.ini")
		if !regularFile(filePath) {
			continue
		}
		config, err := ini.Load(filePath)
		if err != nil {
			return err
		}
		serverPort = nextAvailablePort(serverPort, used)
		if serverPort == 0 {
			return ErrPortConflict
		}
		used[serverPort] = true
		authPort = nextAvailablePort(authPort, used)
		if authPort == 0 {
			return ErrPortConflict
		}
		used[authPort] = true
		masterPort = nextAvailablePort(masterPort, used)
		if masterPort == 0 {
			return ErrPortConflict
		}
		used[masterPort] = true
		config.Section("NETWORK").Key("server_port").SetValue(fmt.Sprintf("%d", serverPort))
		config.Section("STEAM").Key("authentication_port").SetValue(fmt.Sprintf("%d", authPort))
		config.Section("STEAM").Key("master_server_port").SetValue(fmt.Sprintf("%d", masterPort))
		if err := config.SaveTo(filePath); err != nil {
			return err
		}
		if err := os.Chmod(filePath, 0640); err != nil {
			return err
		}
		serverPort++
		authPort++
		masterPort++
	}
	return nil
}

func nextAvailablePort(candidate int, used map[int]bool) int {
	for candidate <= 65535 && used[candidate] {
		candidate++
	}
	if candidate > 65535 {
		return 0
	}
	return candidate
}

func verifyStagedRoom(root string, allowPartial bool) error {
	if !regularFile(filepath.Join(root, "cluster.ini")) {
		return ErrInvalidArchive
	}
	if _, err := ini.Load(filepath.Join(root, "cluster.ini")); err != nil {
		return ErrInvalidArchive
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	worlds, masters := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		filePath := filepath.Join(root, entry.Name(), "server.ini")
		if !regularFile(filePath) {
			continue
		}
		config, err := ini.Load(filePath)
		if err != nil {
			return ErrInvalidArchive
		}
		worlds++
		if config.Section("SHARD").Key("is_master").MustBool(false) {
			masters++
		}
	}
	if worlds == 0 || masters > 1 || !allowPartial && masters != 1 {
		return ErrInvalidArchive
	}
	return nil
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrUnsafeArchive):
		return "UNSAFE_ARCHIVE"
	case errors.Is(err, ErrArchiveTooLarge):
		return "ARCHIVE_TOO_LARGE"
	case errors.Is(err, ErrInvalidArchive):
		return "INVALID_ARCHIVE"
	case errors.Is(err, ErrImportNotFound):
		return "SAVE_IMPORT_NOT_FOUND"
	case errors.Is(err, ErrImportNotReady):
		return "SAVE_IMPORT_NOT_READY"
	case errors.Is(err, ErrCandidateMissing):
		return "SAVE_IMPORT_CANDIDATE_NOT_FOUND"
	case errors.Is(err, ErrRoomRunning):
		return "WORLD_RUNNING"
	case errors.Is(err, ErrTokenRequired), errors.Is(err, rooms.ErrClusterTokenRequired), errors.Is(err, rooms.ErrClusterTokenInvalid):
		return "CLUSTER_TOKEN_REQUIRED"
	case errors.Is(err, ErrMissingMods):
		return "WORKSHOP_MODS_MISSING"
	case errors.Is(err, ErrTargetExists):
		return "ROOM_EXISTS"
	case errors.Is(err, ErrConfirmation):
		return "CONFIRMATION_REQUIRED"
	case errors.Is(err, ErrTargetNotManaged):
		return "ROOM_NOT_MANAGED"
	case errors.Is(err, ErrPartialImport):
		return "PARTIAL_IMPORT_CONFIRMATION_REQUIRED"
	case errors.Is(err, backups.ErrInsufficientSpace):
		return "INSUFFICIENT_SPACE"
	case errors.Is(err, ErrInsufficientSpace):
		return "INSUFFICIENT_SPACE"
	case errors.Is(err, ErrImportBusy):
		return "SAVE_IMPORT_BUSY"
	case errors.Is(err, ErrPortConflict):
		return "PORT_CONFLICT"
	default:
		return "SAVE_IMPORT_APPLY_FAILED"
	}
}
