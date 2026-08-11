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

	"github.com/google/uuid"
)

type Config struct {
	SaveRoot      string
	ImportRoot    string
	WorkshopRoot  string
	MaxUploadSize int64
}

type Service struct {
	config  Config
	store   *Store
	scanner *Scanner
	rooms   RoomManager
	runtime Runtime
	backups BackupCreator
	mods    ModDownloader
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
	now     func() time.Time
}

type RoomManager interface {
	List() ([]rooms.Room, error)
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
	Adopt(string) (rooms.Room, error)
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

func NewService(config Config, store *Store, roomManager RoomManager, runtime Runtime, backupCreator BackupCreator, modDownloader ModDownloader) (*Service, error) {
	if store == nil {
		return nil, errors.New("save import store is required")
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
	return &Service{
		config: config, store: store, scanner: NewScanner(config.WorkshopRoot), rooms: roomManager,
		runtime: runtime, backups: backupCreator, mods: modDownloader, locks: make(map[string]*sync.Mutex), now: time.Now,
	}, nil
}

func (s *Service) List() ([]Session, error) { return s.store.List() }

func (s *Service) Get(id string) (Session, error) { return s.store.Get(id) }

func (s *Service) Upload(ctx context.Context, name, sourceName string, input io.Reader) (Session, error) {
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

func (s *Service) Analyze(ctx context.Context, id string) (Session, error) {
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
	directory := filepath.Join(s.config.ImportRoot, id)
	artifactPath := filepath.Join(directory, "source.archive")
	if !contained(s.config.ImportRoot, artifactPath) {
		return Session{}, ErrUnsafeArchive
	}
	sha, err := fileSHA256(artifactPath)
	if err != nil {
		_ = s.store.MarkInvalid(id, AnalysisErrorCode(err), err.Error())
		return Session{}, err
	}
	staging, err := os.MkdirTemp(directory, ".analyze-")
	if err != nil {
		return Session{}, err
	}
	defer os.RemoveAll(staging)
	manifest, err := s.scanner.Scan(ctx, artifactPath, value.SourceName, staging, value.Size, sha)
	if err != nil {
		_ = s.store.MarkInvalid(id, AnalysisErrorCode(err), err.Error())
		return Session{}, err
	}
	contentRoot := filepath.Join(directory, "content")
	if err := os.RemoveAll(contentRoot); err != nil {
		return Session{}, err
	}
	if err := os.Rename(staging, contentRoot); err != nil {
		return Session{}, fmt.Errorf("publish analyzed save content: %w", err)
	}
	return s.store.SaveManifest(id, sha, manifest)
}

func (s *Service) Delete(id string) error {
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
