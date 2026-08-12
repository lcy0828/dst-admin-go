package backups

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"dont/internal/roomops"
	"dont/internal/rooms"

	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v3/disk"
)

const (
	maxArchiveEntries   = 100000
	maxArchiveBytes     = int64(64 * 1024 * 1024 * 1024)
	maxUploadBytes      = int64(16 * 1024 * 1024 * 1024)
	minimumFreeHeadroom = uint64(256 * 1024 * 1024)
)

const MaxUploadSize = maxUploadBytes

var (
	ErrRoomNotManaged        = errors.New("room must be managed before backups can be used")
	ErrInvalidName           = errors.New("backup name is invalid")
	ErrInvalidArchive        = errors.New("backup archive is invalid")
	ErrUnsafeArchive         = errors.New("backup archive contains an unsafe entry")
	ErrArchiveTooLarge       = errors.New("backup archive exceeds safety limits")
	ErrInsufficientSpace     = errors.New("insufficient disk space")
	ErrConfirmationNeeded    = errors.New("backup restore confirmation does not match")
	ErrWorldRunning          = errors.New("all worlds must be stopped before restore")
	ErrConsistentSaveMissing = errors.New("running room has no running master world for a consistent save")
	ErrBackupRoomMismatch    = errors.New("backup does not belong to room")
	ErrUnsafeBackupPath      = errors.New("backup path is unsafe")
	ErrSnapshotPolicyInvalid = errors.New("backup snapshot policy is invalid")
)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}

type Runtime interface {
	IsRunning(context.Context, string, string) (bool, error)
	Send(context.Context, string, string, string) error
}

type archiveSummary struct {
	Size        int64
	ContentSize int64
	FileCount   int
	SHA256      string
}

type Service struct {
	saveRoot   string
	backupRoot string
	rooms      RoomCatalog
	runtime    Runtime
	store      *Store
	now        func() time.Time
	saveSettle time.Duration
	locksMu    sync.Mutex
	roomLocks  map[string]*sync.Mutex
}

func NewService(saveRoot, backupRoot string, roomCatalog RoomCatalog, runtime Runtime, store *Store) (*Service, error) {
	if roomCatalog == nil || runtime == nil || store == nil {
		return nil, errors.New("room catalog, runtime, and backup store are required")
	}
	resolvedSave, err := absoluteDirectory(saveRoot, false)
	if err != nil {
		return nil, fmt.Errorf("resolve save root: %w", err)
	}
	resolvedBackup, err := absoluteDirectory(backupRoot, false)
	if err != nil {
		return nil, fmt.Errorf("resolve backup root: %w", err)
	}
	return &Service{
		saveRoot: resolvedSave, backupRoot: resolvedBackup, rooms: roomCatalog, runtime: runtime, store: store,
		now: time.Now, saveSettle: 2 * time.Second, roomLocks: make(map[string]*sync.Mutex),
	}, nil
}

func (s *Service) List(roomID string) ([]Backup, error) {
	room, err := s.resolveRoom(roomID)
	if err != nil {
		return nil, err
	}
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	if err := s.syncLegacyLocked(room); err != nil {
		return nil, err
	}
	return s.store.List(roomID)
}

func (s *Service) Get(id string) (Backup, error) { return s.store.Get(id) }

func (s *Service) Create(ctx context.Context, roomID, name string, kind Kind, sourceJobID string) (Backup, error) {
	ctx, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return Backup{}, err
	}
	defer release()
	room, err := s.resolveRoom(roomID)
	if err != nil {
		return Backup{}, err
	}
	if kind != KindManual && kind != KindSnapshot && kind != KindProtection {
		return Backup{}, ErrInvalidArchive
	}
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	if err := s.prepareConsistentSave(ctx, room); err != nil {
		return Backup{}, err
	}
	return s.createArchiveLocked(ctx, room, name, kind, sourceJobID)
}

func (s *Service) Import(ctx context.Context, roomID, name, sourceName string, input io.Reader) (Backup, error) {
	room, err := s.resolveRoom(roomID)
	if err != nil {
		return Backup{}, err
	}
	name, err = normalizeName(name, "上传备份 "+s.now().Format("2006-01-02 15:04:05"))
	if err != nil {
		return Backup{}, err
	}
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	directory, err := s.roomBackupDirectory(room)
	if err != nil {
		return Backup{}, err
	}
	if err := requireFreeSpace(directory, minimumFreeHeadroom); err != nil {
		return Backup{}, err
	}
	temporary, err := os.CreateTemp(directory, ".dst-admin-upload-*.tmp")
	if err != nil {
		return Backup{}, fmt.Errorf("create upload staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		_ = temporary.Close()
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	written, err := copyContext(ctx, temporary, io.LimitReader(input, maxUploadBytes+1))
	if err != nil {
		return Backup{}, fmt.Errorf("write uploaded backup: %w", err)
	}
	if written > maxUploadBytes {
		return Backup{}, ErrArchiveTooLarge
	}
	if err := temporary.Sync(); err != nil {
		return Backup{}, fmt.Errorf("sync uploaded backup: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Backup{}, fmt.Errorf("close uploaded backup: %w", err)
	}
	summary, err := validateArchive(temporaryPath)
	if err != nil {
		return Backup{}, err
	}
	id := uuid.NewString()
	fileName := id + ".zip"
	finalPath := filepath.Join(directory, fileName)
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return Backup{}, fmt.Errorf("publish uploaded backup: %w", err)
	}
	published = true
	now := s.now().UTC()
	verified := now
	record, err := s.store.Create(Backup{
		ID: id, RoomID: room.ID, Name: name, Kind: KindUpload, FileName: fileName,
		SourceName: safeSourceName(sourceName), Size: summary.Size, ContentSize: summary.ContentSize,
		FileCount: summary.FileCount, SHA256: summary.SHA256, Status: "verified", VerifiedAt: &verified, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		_ = os.Remove(finalPath)
		return Backup{}, err
	}
	return record, nil
}

func (s *Service) Rename(id, name string) (Backup, error) {
	value, err := s.store.Get(id)
	if err != nil {
		return Backup{}, err
	}
	name, err = normalizeName(name, "")
	if err != nil {
		return Backup{}, err
	}
	lock := s.roomLock(value.RoomID)
	lock.Lock()
	defer lock.Unlock()
	return s.store.Rename(id, name)
}

func (s *Service) Delete(id string) (Backup, error) {
	value, err := s.store.Get(id)
	if err != nil {
		return Backup{}, err
	}
	lock := s.roomLock(value.RoomID)
	lock.Lock()
	defer lock.Unlock()
	filePath, _, err := s.backupPath(value)
	if err != nil {
		return Backup{}, err
	}
	tombstone := filePath + ".deleting-" + uuid.NewString()
	if err := os.Rename(filePath, tombstone); err != nil {
		return Backup{}, fmt.Errorf("stage backup deletion: %w", err)
	}
	if err := s.store.Delete(id); err != nil {
		_ = os.Rename(tombstone, filePath)
		return Backup{}, err
	}
	if err := os.Remove(tombstone); err != nil {
		return Backup{}, fmt.Errorf("remove deleted backup: %w", err)
	}
	return value, nil
}

func (s *Service) Open(id string) (*os.File, os.FileInfo, Backup, error) {
	value, err := s.store.Get(id)
	if err != nil {
		return nil, nil, Backup{}, err
	}
	filePath, info, err := s.backupPath(value)
	if err != nil {
		return nil, nil, Backup{}, err
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, nil, Backup{}, fmt.Errorf("open backup: %w", err)
	}
	return file, info, value, nil
}

func (s *Service) Restore(ctx context.Context, roomID, backupID, confirmation, sourceJobID string) (Backup, error) {
	ctx, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return Backup{}, err
	}
	defer release()
	room, err := s.resolveRoom(roomID)
	if err != nil {
		return Backup{}, err
	}
	if confirmation != room.Name {
		return Backup{}, ErrConfirmationNeeded
	}
	value, err := s.store.Get(backupID)
	if err != nil {
		return Backup{}, err
	}
	if value.RoomID != room.ID {
		return Backup{}, ErrBackupRoomMismatch
	}
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	if err := s.requireStopped(ctx, room); err != nil {
		return Backup{}, err
	}
	archivePath, _, err := s.backupPath(value)
	if err != nil {
		return Backup{}, err
	}
	summary, err := validateArchive(archivePath)
	if err != nil {
		return Backup{}, err
	}
	roomPath := filepath.Join(s.saveRoot, room.DirectoryName)
	currentSize, err := directorySize(roomPath)
	if err != nil {
		return Backup{}, err
	}
	required := uint64(summary.ContentSize + currentSize)
	if required > ^uint64(0)-minimumFreeHeadroom {
		return Backup{}, ErrInsufficientSpace
	}
	if err := requireFreeSpace(s.saveRoot, required+minimumFreeHeadroom); err != nil {
		return Backup{}, err
	}
	protection, err := s.createArchiveLocked(ctx, room, "恢复前保护备份 "+s.now().Format("2006-01-02 15:04:05"), KindProtection, sourceJobID)
	if err != nil {
		return Backup{}, fmt.Errorf("create protection backup: %w", err)
	}
	staging, err := os.MkdirTemp(s.saveRoot, ".dst-admin-restore-")
	if err != nil {
		return protection, fmt.Errorf("create restore staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractArchive(ctx, archivePath, staging); err != nil {
		return protection, err
	}
	if err := verifyRestoredRoom(staging); err != nil {
		return protection, err
	}
	rollback := filepath.Join(s.saveRoot, ".dst-admin-rollback-"+uuid.NewString())
	if err := os.Rename(roomPath, rollback); err != nil {
		return protection, fmt.Errorf("stage current room for restore: %w", err)
	}
	if err := os.Rename(staging, roomPath); err != nil {
		rollbackErr := os.Rename(rollback, roomPath)
		if rollbackErr != nil {
			return protection, fmt.Errorf("publish restored room: %v; rollback current room: %w", err, rollbackErr)
		}
		return protection, fmt.Errorf("publish restored room: %w", err)
	}
	published = true
	_ = os.Chmod(roomPath, 0750)
	_ = os.RemoveAll(rollback)
	return protection, nil
}

func (s *Service) ValidateRestore(ctx context.Context, roomID, backupID, confirmation string) (rooms.Room, Backup, error) {
	room, err := s.resolveRoom(roomID)
	if err != nil {
		return rooms.Room{}, Backup{}, err
	}
	if confirmation != room.Name {
		return rooms.Room{}, Backup{}, ErrConfirmationNeeded
	}
	value, err := s.store.Get(backupID)
	if err != nil {
		return rooms.Room{}, Backup{}, err
	}
	if value.RoomID != room.ID {
		return rooms.Room{}, Backup{}, ErrBackupRoomMismatch
	}
	if err := s.requireStopped(ctx, room); err != nil {
		return rooms.Room{}, Backup{}, err
	}
	return room, value, nil
}

func (s *Service) Policy(roomID string) (Policy, error) {
	if _, err := s.resolveRoom(roomID); err != nil {
		return Policy{}, err
	}
	return s.store.Policy(roomID)
}

func (s *Service) SavePolicy(roomID string, request PolicyRequest) (Policy, error) {
	if _, err := s.resolveRoom(roomID); err != nil {
		return Policy{}, err
	}
	if request.IntervalMinute < 15 || request.IntervalMinute > 44640 || request.MaxSnapshots < 1 || request.MaxSnapshots > 100 {
		return Policy{}, ErrSnapshotPolicyInvalid
	}
	now := s.now().UTC()
	if request.Enabled && request.NextRunAt != nil {
		next := request.NextRunAt.UTC()
		latest := now.Add(time.Duration(request.IntervalMinute) * time.Minute)
		if !next.After(now) || next.After(latest) {
			return Policy{}, ErrSnapshotPolicyInvalid
		}
	}
	policy, err := s.store.Policy(roomID)
	if err != nil {
		return Policy{}, err
	}
	policy.Enabled = request.Enabled
	policy.IntervalMinute = request.IntervalMinute
	policy.MaxSnapshots = request.MaxSnapshots
	if request.Enabled {
		next := now.Add(time.Duration(request.IntervalMinute) * time.Minute)
		if request.NextRunAt != nil {
			next = request.NextRunAt.UTC()
		}
		policy.NextRunAt = &next
	} else {
		policy.NextRunAt = nil
	}
	return s.store.SavePolicy(policy)
}

func (s *Service) MarkPolicyRun(roomID, jobID string, runErr error) error {
	policy, err := s.store.Policy(roomID)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	policy.LastRunAt = &now
	policy.LastJobID = jobID
	policy.LastError = ""
	if runErr != nil {
		policy.LastError = runErr.Error()
	}
	if policy.Enabled {
		next := now.Add(time.Duration(policy.IntervalMinute) * time.Minute)
		policy.NextRunAt = &next
	} else {
		policy.NextRunAt = nil
	}
	_, err = s.store.SavePolicy(policy)
	return err
}

func (s *Service) DuePolicies() ([]Policy, error) { return s.store.DuePolicies(s.now().UTC()) }

func (s *Service) PruneSnapshots(roomID string, keep int) (int64, int, error) {
	if keep < 1 || keep > 100 {
		return 0, 0, ErrSnapshotPolicyInvalid
	}
	room, err := s.resolveRoom(roomID)
	if err != nil {
		return 0, 0, err
	}
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	items, err := s.store.List(roomID)
	if err != nil {
		return 0, 0, err
	}
	snapshots := make([]Backup, 0)
	for _, item := range items {
		if item.Kind == KindSnapshot {
			snapshots = append(snapshots, item)
		}
	}
	sort.SliceStable(snapshots, func(i, j int) bool { return snapshots[i].CreatedAt.After(snapshots[j].CreatedAt) })
	var bytes int64
	removed := 0
	if len(snapshots) <= keep {
		return 0, 0, nil
	}
	for _, item := range snapshots[keep:] {
		filePath, _, pathErr := s.backupPath(item)
		if pathErr != nil {
			return bytes, removed, pathErr
		}
		tombstone := filePath + ".pruning-" + uuid.NewString()
		if err := os.Rename(filePath, tombstone); err != nil {
			return bytes, removed, err
		}
		if err := s.store.Delete(item.ID); err != nil {
			_ = os.Rename(tombstone, filePath)
			return bytes, removed, err
		}
		if err := os.Remove(tombstone); err != nil {
			return bytes, removed, err
		}
		bytes += item.Size
		removed++
	}
	_ = room
	return bytes, removed, nil
}

func (s *Service) createArchiveLocked(ctx context.Context, room rooms.Room, name string, kind Kind, sourceJobID string) (Backup, error) {
	defaultName := "手动备份 " + s.now().Format("2006-01-02 15:04:05")
	if kind == KindSnapshot {
		defaultName = "自动快照 " + s.now().Format("2006-01-02 15:04:05")
	} else if kind == KindProtection {
		defaultName = "保护性备份 " + s.now().Format("2006-01-02 15:04:05")
	}
	name, err := normalizeName(name, defaultName)
	if err != nil {
		return Backup{}, err
	}
	directory, err := s.roomBackupDirectory(room)
	if err != nil {
		return Backup{}, err
	}
	id := uuid.NewString()
	fileName := id + ".zip"
	finalPath := filepath.Join(directory, fileName)
	temporary, err := os.CreateTemp(directory, ".dst-admin-backup-*.tmp")
	if err != nil {
		return Backup{}, fmt.Errorf("create backup staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		_ = temporary.Close()
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	roomPath := filepath.Join(s.saveRoot, room.DirectoryName)
	if err := writeArchive(ctx, roomPath, temporary); err != nil {
		return Backup{}, err
	}
	if err := temporary.Sync(); err != nil {
		return Backup{}, fmt.Errorf("sync backup archive: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Backup{}, fmt.Errorf("close backup archive: %w", err)
	}
	summary, err := validateArchive(temporaryPath)
	if err != nil {
		return Backup{}, err
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return Backup{}, fmt.Errorf("publish backup archive: %w", err)
	}
	published = true
	now := s.now().UTC()
	verified := now
	record, err := s.store.Create(Backup{
		ID: id, RoomID: room.ID, Name: name, Kind: kind, FileName: fileName, Size: summary.Size,
		ContentSize: summary.ContentSize, FileCount: summary.FileCount, SHA256: summary.SHA256,
		Status: "verified", VerifiedAt: &verified, SourceJobID: sourceJobID, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		_ = os.Remove(finalPath)
		return Backup{}, err
	}
	return record, nil
}

func (s *Service) syncLegacyLocked(room rooms.Room) error {
	directory := filepath.Join(s.backupRoot, room.DirectoryName)
	if !contained(s.backupRoot, directory) {
		return ErrUnsafeBackupPath
	}
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafeBackupPath
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !safeLegacyFileName(name) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if _, findErr := s.store.FindByFileName(room.ID, name); findErr == nil {
			continue
		} else if !errors.Is(findErr, ErrBackupNotFound) {
			return findErr
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !entryInfo.Mode().IsRegular() {
			continue
		}
		created := entryInfo.ModTime().UTC()
		value := Backup{
			ID: uuid.NewString(), RoomID: room.ID, Name: strings.TrimSuffix(safeSourceName(name), filepath.Ext(name)),
			Kind: KindImported, FileName: name, SourceName: name, Size: entryInfo.Size(), Status: "invalid",
			CreatedAt: created, UpdatedAt: created,
		}
		summary, validateErr := validateArchive(filepath.Join(directory, name))
		if validateErr == nil {
			verified := s.now().UTC()
			value.ContentSize = summary.ContentSize
			value.FileCount = summary.FileCount
			value.SHA256 = summary.SHA256
			value.Status = "verified"
			value.VerifiedAt = &verified
		} else {
			value.ValidationError = validateErr.Error()
		}
		if _, err := s.store.Create(value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) prepareConsistentSave(ctx context.Context, room rooms.Room) error {
	worlds, err := s.rooms.Worlds(room.ID)
	if err != nil {
		return err
	}
	anyRunning := false
	masterRunning := false
	masterName := ""
	for _, world := range worlds {
		running, statusErr := s.runtime.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
		if statusErr != nil {
			return fmt.Errorf("inspect world before backup: %w", statusErr)
		}
		anyRunning = anyRunning || running
		if world.IsMaster {
			masterName = world.DirectoryName
			masterRunning = running
		}
	}
	if !anyRunning {
		return nil
	}
	if !masterRunning || masterName == "" {
		return ErrConsistentSaveMissing
	}
	if err := s.runtime.Send(ctx, room.DirectoryName, masterName, "c_save()"); err != nil {
		return fmt.Errorf("request consistent save: %w", err)
	}
	timer := time.NewTimer(s.saveSettle)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) requireStopped(ctx context.Context, room rooms.Room) error {
	worlds, err := s.rooms.Worlds(room.ID)
	if err != nil {
		return err
	}
	for _, world := range worlds {
		running, statusErr := s.runtime.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
		if statusErr != nil {
			return statusErr
		}
		if running {
			return ErrWorldRunning
		}
	}
	return nil
}

func (s *Service) resolveRoom(roomID string) (rooms.Room, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, err
	}
	if !room.Managed {
		return rooms.Room{}, ErrRoomNotManaged
	}
	return room, nil
}

func (s *Service) roomBackupDirectory(room rooms.Room) (string, error) {
	directory := filepath.Join(s.backupRoot, room.DirectoryName)
	if !contained(s.backupRoot, directory) {
		return "", ErrUnsafeBackupPath
	}
	if err := os.MkdirAll(directory, 0750); err != nil {
		return "", fmt.Errorf("create room backup directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", ErrUnsafeBackupPath
	}
	return directory, nil
}

func (s *Service) backupPath(value Backup) (string, os.FileInfo, error) {
	room, err := s.rooms.Room(value.RoomID)
	if err != nil {
		return "", nil, err
	}
	directory := filepath.Join(s.backupRoot, room.DirectoryName)
	filePath := filepath.Join(directory, value.FileName)
	if !contained(s.backupRoot, directory) || !contained(directory, filePath) || filepath.Base(value.FileName) != value.FileName || !strings.HasSuffix(value.FileName, ".zip") {
		return "", nil, ErrUnsafeBackupPath
	}
	info, err := os.Lstat(filePath)
	if os.IsNotExist(err) {
		return "", nil, ErrBackupNotFound
	}
	if err != nil {
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, ErrUnsafeBackupPath
	}
	return filePath, info, nil
}

func (s *Service) roomLock(roomID string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	lock := s.roomLocks[roomID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.roomLocks[roomID] = lock
	}
	return lock
}

func writeArchive(ctx context.Context, root string, output io.Writer) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafeBackupPath
	}
	writer := zip.NewWriter(output)
	walkErr := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil || !contained(root, filePath) {
			return ErrUnsafeBackupPath
		}
		if relative == "." {
			return nil
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeBackupPath
		}
		if !entryInfo.IsDir() && !entryInfo.Mode().IsRegular() {
			return ErrUnsafeBackupPath
		}
		header, err := zip.FileInfoHeader(entryInfo)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		if entryInfo.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}
		destination, err := writer.CreateHeader(header)
		if err != nil || entryInfo.IsDir() {
			return err
		}
		source, err := os.Open(filePath)
		if err != nil {
			return err
		}
		_, copyErr := copyContext(ctx, destination, source)
		closeErr := source.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeErr := writer.Close()
	if walkErr != nil {
		return fmt.Errorf("create backup archive: %w", walkErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close backup archive: %w", closeErr)
	}
	return nil
}

func validateArchive(filePath string) (archiveSummary, error) {
	info, err := os.Lstat(filePath)
	if err != nil {
		return archiveSummary{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return archiveSummary{}, ErrUnsafeArchive
	}
	reader, err := zip.OpenReader(filePath)
	if err != nil {
		return archiveSummary{}, fmt.Errorf("%w: %v", ErrInvalidArchive, err)
	}
	defer reader.Close()
	if len(reader.File) == 0 || len(reader.File) > maxArchiveEntries {
		return archiveSummary{}, ErrArchiveTooLarge
	}
	var total int64
	files := 0
	hasCluster := false
	hasWorld := false
	for _, entry := range reader.File {
		clean, pathErr := safeArchiveName(entry.Name)
		if pathErr != nil {
			return archiveSummary{}, pathErr
		}
		mode := entry.Mode()
		if mode&os.ModeSymlink != 0 || (!mode.IsDir() && !mode.IsRegular()) {
			return archiveSummary{}, ErrUnsafeArchive
		}
		if mode.IsDir() {
			continue
		}
		if entry.UncompressedSize64 > uint64(maxArchiveBytes) || total > maxArchiveBytes-int64(entry.UncompressedSize64) {
			return archiveSummary{}, ErrArchiveTooLarge
		}
		total += int64(entry.UncompressedSize64)
		files++
		hasCluster = hasCluster || clean == "cluster.ini"
		hasWorld = hasWorld || strings.HasSuffix(clean, "/server.ini")
	}
	if !hasCluster || !hasWorld {
		return archiveSummary{}, fmt.Errorf("%w: cluster.ini or world server.ini is missing", ErrInvalidArchive)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return archiveSummary{}, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return archiveSummary{}, copyErr
	}
	if closeErr != nil {
		return archiveSummary{}, closeErr
	}
	return archiveSummary{Size: info.Size(), ContentSize: total, FileCount: files, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func extractArchive(ctx context.Context, archivePath, destination string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open restore archive: %w", err)
	}
	defer reader.Close()
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		clean, err := safeArchiveName(entry.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if !contained(destination, target) {
			return ErrUnsafeArchive
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return err
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
		if err != nil {
			_ = source.Close()
			return err
		}
		_, copyErr := copyContext(ctx, output, io.LimitReader(source, int64(entry.UncompressedSize64)+1))
		closeOutputErr := output.Close()
		closeSourceErr := source.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeOutputErr != nil {
			return closeOutputErr
		}
		if closeSourceErr != nil {
			return closeSourceErr
		}
	}
	return nil
}

func safeArchiveName(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\\x00\r\n") || strings.HasPrefix(name, "/") {
		return "", ErrUnsafeArchive
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrUnsafeArchive
	}
	return strings.TrimSuffix(clean, "/"), nil
}

func verifyRestoredRoom(root string) error {
	cluster, err := os.Lstat(filepath.Join(root, "cluster.ini"))
	if err != nil || cluster.Mode()&os.ModeSymlink != 0 || !cluster.Mode().IsRegular() {
		return fmt.Errorf("%w: restored cluster.ini is missing", ErrInvalidArchive)
	}
	foundWorld := false
	err = filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrUnsafeArchive
		}
		if entry.Name() == "server.ini" && filepath.Dir(filePath) != root {
			foundWorld = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !foundWorld {
		return fmt.Errorf("%w: restored worlds are missing", ErrInvalidArchive)
	}
	return nil
}

func normalizeName(value, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if value == "" || !utf8.ValidString(value) || len([]rune(value)) > 128 {
		return "", ErrInvalidName
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", ErrInvalidName
		}
	}
	return value, nil
}

func safeSourceName(value string) string {
	value = strings.TrimSpace(filepath.Base(strings.ReplaceAll(value, "\\", "/")))
	if !utf8.ValidString(value) || len([]rune(value)) > 255 {
		return "uploaded.zip"
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "uploaded.zip"
		}
	}
	return value
}

func safeLegacyFileName(value string) bool {
	if filepath.Base(value) != value || !strings.EqualFold(filepath.Ext(value), ".zip") || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func absoluteDirectory(value string, create bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("directory is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if create {
		if err := os.MkdirAll(absolute, 0750); err != nil {
			return "", err
		}
	}
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) && !create {
		return absolute, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", ErrUnsafeBackupPath
	}
	return absolute, nil
}

func contained(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != read {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeBackupPath
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func requireFreeSpace(target string, required uint64) error {
	usage, err := disk.Usage(target)
	if err != nil {
		return fmt.Errorf("inspect disk space: %w", err)
	}
	if usage.Free < required {
		return ErrInsufficientSpace
	}
	return nil
}
