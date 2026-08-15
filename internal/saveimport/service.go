package saveimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"dont/internal/backups"
	"dont/internal/mods"
	"dont/internal/rooms"
	"dont/internal/runtimeguard"
	"dont/internal/topology"

	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v3/disk"
)

type Config struct {
	SaveRoot      string
	ImportRoot    string
	WorkshopRoot  string
	MaxUploadSize int64
}

var inspectDiskUsage = disk.Usage

type Service struct {
	config        Config
	store         *Store
	scanner       *Scanner
	rooms         RoomManager
	runtime       Runtime
	backups       BackupCreator
	mods          ModDownloader
	guard         runtimeguard.MutationGuard
	ports         PortAllocator
	locksMu       sync.Mutex
	locks         map[string]*sync.Mutex
	activeMu      sync.Mutex
	active        map[string]string
	activeTargets map[string]string
	now           func() time.Time
}

type RoomManager interface {
	List() ([]rooms.Room, error)
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
	Adopt(string) (rooms.Room, error)
	Unadopt(string) error
}

type Runtime interface {
	IsRunning(context.Context, string, string) (bool, error)
}

type BackupCreator interface {
	Create(context.Context, string, string, backups.Kind, string) (backups.Backup, error)
}

type ModDownloader interface {
	Download(context.Context, mods.DownloadRequest, io.Writer) (mods.ActionResult, error)
	EnsureLibrarySetup([]string) error
}

type PortAllocator interface {
	ReservePorts(context.Context, topology.PortAllocationRequest) (topology.PortAllocation, error)
	ActivatePorts(context.Context, string) error
	ReleasePorts(context.Context, string) error
}

func (s *Service) ConfigurePortAllocator(allocator PortAllocator) error {
	if allocator == nil {
		return errors.New("save import port allocator is required")
	}
	s.ports = allocator
	return nil
}

func NewService(config Config, store *Store, roomManager RoomManager, runtime Runtime, backupCreator BackupCreator, modDownloader ModDownloader, guards ...runtimeguard.MutationGuard) (*Service, error) {
	if store == nil {
		return nil, errors.New("save import store is required")
	}
	if len(guards) > 1 {
		return nil, errors.New("save import accepts at most one runtime mutation guard")
	}
	var guard runtimeguard.MutationGuard
	if len(guards) == 1 {
		guard = guards[0]
	}
	var err error
	config.SaveRoot, err = absoluteDirectory(config.SaveRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve save root: %w", err)
	}
	config.ImportRoot, err = absoluteDirectory(config.ImportRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve save import root: %w", err)
	}
	if config.MaxUploadSize <= 0 {
		config.MaxUploadSize = MaxUploadBytes
	}
	service := &Service{
		config: config, store: store, scanner: NewScanner(config.WorkshopRoot), rooms: roomManager,
		runtime: runtime, backups: backupCreator, mods: modDownloader, guard: guard, locks: make(map[string]*sync.Mutex),
		active: make(map[string]string), activeTargets: make(map[string]string), now: time.Now,
	}
	if err := service.recoverInterrupted(); err != nil {
		return nil, fmt.Errorf("recover save imports: %w", err)
	}
	return service, nil
}

func (s *Service) List() ([]Session, error) { return s.store.List() }

func (s *Service) Get(id string) (Session, error) { return s.store.Get(id) }

func (s *Service) Upload(ctx context.Context, name, sourceName string, input io.Reader) (Session, error) {
	return s.UploadWithSize(ctx, name, sourceName, input, -1)
}

func (s *Service) CheckUploadSpace(size int64, includeMultipartCopy bool) error {
	limit := s.config.MaxUploadSize
	if includeMultipartCopy {
		limit += 1024 * 1024
	}
	if size > limit {
		return ErrArchiveTooLarge
	}
	required := size
	if required < 0 {
		required = 0
	}
	if includeMultipartCopy && required <= (1<<63-1)/2 {
		required *= 2
	}
	return requireImportSpace(s.config.ImportRoot, required)
}

func (s *Service) UploadWithSize(ctx context.Context, name, sourceName string, input io.Reader, sizeHint int64) (Session, error) {
	if err := s.CheckUploadSpace(sizeHint, false); err != nil {
		return Session{}, err
	}
	name, err := normalizeDisplayName(name, "导入 "+s.now().Format("2006-01-02 15:04:05"))
	if err != nil {
		return Session{}, err
	}
	sourceName = safeSourceName(sourceName)
	id := uuid.NewString()
	directory := filepath.Join(s.config.ImportRoot, id)
	if err := os.Mkdir(directory, 0750); err != nil {
		return Session{}, fmt.Errorf("create save import directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(directory)
		}
	}()
	artifactName := "source.archive"
	artifactPath := filepath.Join(directory, artifactName)
	file, err := os.OpenFile(artifactPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return Session{}, err
	}
	written, copyErr := copyContext(ctx, file, io.LimitReader(input, s.config.MaxUploadSize+1))
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return Session{}, copyErr
	}
	if written > s.config.MaxUploadSize {
		return Session{}, ErrArchiveTooLarge
	}
	if syncErr != nil {
		return Session{}, syncErr
	}
	if closeErr != nil {
		return Session{}, closeErr
	}
	now := s.now().UTC()
	value, err := s.store.Create(importRecord{
		ID: id, Name: name, SourceName: sourceName, ArtifactName: artifactName,
		Status: string(StatusUploaded), Size: written, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return Session{}, err
	}
	published = true
	return value, nil
}

func requireImportSpace(target string, contentBytes int64) error {
	if contentBytes < 0 {
		contentBytes = 0
	}
	required := uint64(contentBytes)
	if required > ^uint64(0)-minimumImportHeadroom {
		return ErrInsufficientSpace
	}
	usage, err := inspectDiskUsage(target)
	if err != nil {
		return err
	}
	if usage.Free < required+minimumImportHeadroom {
		return ErrInsufficientSpace
	}
	return nil
}

func (s *Service) Analyze(ctx context.Context, id string) (result Session, resultErr error) {
	lock := s.importLock(id)
	lock.Lock()
	defer lock.Unlock()
	value, err := s.store.Get(id)
	if err != nil {
		return Session{}, err
	}
	if err := s.store.MarkAnalyzing(id); err != nil {
		return Session{}, err
	}
	defer func() {
		if resultErr == nil {
			return
		}
		code := AnalysisErrorCode(resultErr)
		if errors.Is(resultErr, ErrUnsafeArchive) || errors.Is(resultErr, ErrInvalidArchive) || errors.Is(resultErr, ErrArchiveTooLarge) {
			_ = s.store.MarkInvalid(id, code, resultErr.Error())
			return
		}
		recovery := importRecord{ID: id}
		if value.Manifest != nil {
			recovery.ManifestJSON = "{}"
		}
		if recoveryErr := s.recoverAnalysis(recovery); recoveryErr != nil {
			resultErr = fmt.Errorf("%v; recover interrupted analysis: %w", resultErr, recoveryErr)
			return
		}
		_ = s.store.MarkAnalysisFailed(id, code, resultErr.Error())
	}()
	directory := filepath.Join(s.config.ImportRoot, id)
	artifactPath := filepath.Join(directory, "source.archive")
	if !contained(s.config.ImportRoot, artifactPath) {
		return Session{}, ErrUnsafeArchive
	}
	sha, err := fileSHA256(artifactPath)
	if err != nil {
		return Session{}, err
	}
	staging, err := os.MkdirTemp(directory, ".analyze-")
	if err != nil {
		return Session{}, err
	}
	defer os.RemoveAll(staging)
	manifest, err := s.scanner.Scan(ctx, artifactPath, value.SourceName, staging, value.Size, sha)
	if err != nil {
		return Session{}, err
	}
	contentRoot := filepath.Join(directory, "content")
	rollbackRoot := filepath.Join(directory, ".content-rollback")
	if err := os.RemoveAll(rollbackRoot); err != nil {
		return Session{}, err
	}
	hadContent := regularDirectory(contentRoot)
	if hadContent {
		if err := os.Rename(contentRoot, rollbackRoot); err != nil {
			return Session{}, fmt.Errorf("stage previous analyzed content: %w", err)
		}
	}
	if err := os.Rename(staging, contentRoot); err != nil {
		if hadContent {
			if rollbackErr := os.Rename(rollbackRoot, contentRoot); rollbackErr != nil {
				return Session{}, fmt.Errorf("publish analyzed save content: %v; restore previous content: %w", err, rollbackErr)
			}
		}
		return Session{}, fmt.Errorf("publish analyzed save content: %w", err)
	}
	result, err = s.store.SaveManifest(id, sha, manifest)
	if err != nil {
		rollbackErr := restoreAnalyzedContent(contentRoot, staging, rollbackRoot, hadContent)
		if rollbackErr != nil {
			return Session{}, fmt.Errorf("save import manifest: %v; restore analyzed content: %w", err, rollbackErr)
		}
		return Session{}, err
	}
	_ = os.RemoveAll(rollbackRoot)
	return result, nil
}

func (s *Service) Delete(id string) error {
	release, err := s.Reserve(id, "delete")
	if err != nil {
		return err
	}
	defer release()
	lock := s.importLock(id)
	lock.Lock()
	defer lock.Unlock()
	if _, err := s.store.Get(id); err != nil {
		return err
	}
	directory := filepath.Join(s.config.ImportRoot, id)
	if !contained(s.config.ImportRoot, directory) {
		return ErrUnsafeArchive
	}
	tombstone := directory + ".deleting-" + uuid.NewString()
	if err := os.Rename(directory, tombstone); err != nil {
		return err
	}
	if err := s.store.Delete(id); err != nil {
		_ = os.Rename(tombstone, directory)
		return err
	}
	return os.RemoveAll(tombstone)
}

func (s *Service) importLock(id string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	lock := s.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[id] = lock
	}
	return lock
}

func (s *Service) Reserve(id, operation string) (func(), error) {
	return s.reserve(id, operation, "")
}

func (s *Service) ReserveApply(id, targetRoomID string) (func(), error) {
	return s.reserve(id, "apply", strings.TrimSpace(targetRoomID))
}

func (s *Service) reserve(id, operation, targetRoomID string) (func(), error) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if _, exists := s.active[id]; exists {
		return nil, ErrImportBusy
	}
	if targetRoomID != "" {
		if _, exists := s.activeTargets[targetRoomID]; exists {
			return nil, ErrImportBusy
		}
	}
	s.active[id] = operation
	if targetRoomID != "" {
		s.activeTargets[targetRoomID] = id
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.activeMu.Lock()
			delete(s.active, id)
			if s.activeTargets[targetRoomID] == id {
				delete(s.activeTargets, targetRoomID)
			}
			s.activeMu.Unlock()
		})
	}, nil
}

func restoreAnalyzedContent(contentRoot, staging, rollbackRoot string, hadContent bool) error {
	if regularDirectory(contentRoot) {
		if err := os.Rename(contentRoot, staging); err != nil {
			return err
		}
	}
	if hadContent && regularDirectory(rollbackRoot) {
		if err := os.Rename(rollbackRoot, contentRoot); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recoverInterrupted() error {
	records, err := s.store.RecoveryRecords()
	if err != nil {
		return err
	}
	for _, record := range records {
		switch Status(record.Status) {
		case StatusAnalyzing:
			if err := s.recoverAnalysis(record); err != nil {
				return err
			}
			if err := s.store.MarkAnalysisFailed(record.ID, "SERVER_RESTARTED", "服务重启中断了存档分析，请重新分析"); err != nil {
				return err
			}
		case StatusApplying:
			if blocked, err := s.markRemoteRecoveryBlocked(record); err != nil {
				return err
			} else if blocked {
				continue
			}
			if err := s.recoverApply(record); err != nil {
				return err
			}
		case StatusApplied:
			if record.ApplyPhase != applyPhaseCommitted && record.ApplyPhase != applyPhaseApplied {
				return fmt.Errorf("recover applied save import %s: invalid phase %q", record.ID, record.ApplyPhase)
			}
			if blocked, err := s.markRemoteRecoveryBlocked(record); err != nil {
				return err
			} else if blocked {
				continue
			}
			if err := s.finalizeCommittedApply(record); err != nil {
				return err
			}
		}
	}
	return s.cleanupOrphanApplyStaging()
}

func (s *Service) markRemoteRecoveryBlocked(record importRecord) (bool, error) {
	if s.guard == nil || ApplyMode(record.ApplyMode) != ApplyModeReplace {
		return false, nil
	}
	roomID := strings.TrimSpace(record.ApplyRoomID)
	if roomID == "" && directoryNamePattern.MatchString(record.ApplyTarget) {
		roomID = rooms.EncodeID(record.ApplyTarget)
	}
	if roomID == "" {
		return false, nil
	}
	err := s.guard.RequireRoom(roomID)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, runtimeguard.ErrRemoteMutationUnavailable) {
		return false, err
	}
	message := "目标房间包含远程分片；已暂停旧存档替换的本机恢复，待房间恢复为全本机部署后重启服务重试"
	if err := s.store.MarkRecoveryBlocked(record.ID, runtimeguard.ErrorCode, message); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) cleanupOrphanApplyStaging() error {
	records, err := s.store.RecoveryRecords()
	if err != nil {
		return err
	}
	retained := make(map[string]bool, len(records))
	for _, record := range records {
		if strings.HasPrefix(record.ApplyStaging, ".dst-admin-import-") && filepath.Base(record.ApplyStaging) == record.ApplyStaging {
			retained[filepath.Join(s.config.SaveRoot, record.ApplyStaging)] = true
		}
	}
	matches, err := filepath.Glob(filepath.Join(s.config.SaveRoot, ".dst-admin-import-*"))
	if err != nil {
		return err
	}
	for _, match := range matches {
		name := filepath.Base(match)
		if strings.HasPrefix(name, ".dst-admin-import-rollback-") || retained[match] || !contained(s.config.SaveRoot, match) {
			continue
		}
		if err := os.RemoveAll(match); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recoverAnalysis(record importRecord) error {
	directory := filepath.Join(s.config.ImportRoot, record.ID)
	contentRoot := filepath.Join(directory, "content")
	rollbackRoot := filepath.Join(directory, ".content-rollback")
	if regularDirectory(rollbackRoot) {
		discard := filepath.Join(directory, ".content-interrupted-"+uuid.NewString())
		if regularDirectory(contentRoot) {
			if err := os.Rename(contentRoot, discard); err != nil {
				return err
			}
			defer os.RemoveAll(discard)
		}
		if err := os.Rename(rollbackRoot, contentRoot); err != nil {
			return err
		}
	} else if strings.TrimSpace(record.ManifestJSON) == "" {
		if err := os.RemoveAll(contentRoot); err != nil {
			return err
		}
	}
	matches, err := filepath.Glob(filepath.Join(directory, ".analyze-*"))
	if err != nil {
		return err
	}
	for _, match := range matches {
		if contained(directory, match) {
			if err := os.RemoveAll(match); err != nil {
				return err
			}
		}
	}
	return nil
}

func absoluteDirectory(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("directory is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if err := os.MkdirAll(absolute, 0750); err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", ErrUnsafeArchive
	}
	return absolute, nil
}

func normalizeDisplayName(value, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if value == "" || !utf8.ValidString(value) || len([]rune(value)) > 128 {
		return "", ErrInvalidRequest
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", ErrInvalidRequest
		}
	}
	return value, nil
}

func safeSourceName(value string) string {
	value = strings.TrimSpace(filepath.Base(strings.ReplaceAll(value, "\\", "/")))
	if value == "" || !utf8.ValidString(value) || len([]rune(value)) > 255 {
		return "uploaded.archive"
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "uploaded.archive"
		}
	}
	return value
}
