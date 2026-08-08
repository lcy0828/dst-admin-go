package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dont/internal/backups"
	"dont/internal/rooms"
)

const maxConfigurationBytes = int64(4 * 1024 * 1024)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}

type BackupCreator interface {
	Create(context.Context, string, string, backups.Kind, string) (backups.Backup, error)
}

type Service struct {
	saveRoot string
	rooms    RoomCatalog
	backups  BackupCreator
	now      func() time.Time
	locksMu  sync.Mutex
	locks    map[string]*sync.Mutex
}

func NewService(saveRoot string, roomCatalog RoomCatalog, backupCreator BackupCreator) (*Service, error) {
	if roomCatalog == nil || backupCreator == nil {
		return nil, errors.New("room catalog and backup creator are required")
	}
	root, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil || strings.TrimSpace(saveRoot) == "" {
		return nil, errors.New("save root is required")
	}
	return &Service{
		saveRoot: filepath.Clean(root), rooms: roomCatalog, backups: backupCreator,
		now: time.Now, locks: make(map[string]*sync.Mutex),
	}, nil
}

func (s *Service) roomLock(roomID string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	if s.locks[roomID] == nil {
		s.locks[roomID] = &sync.Mutex{}
	}
	return s.locks[roomID]
}

func (s *Service) resolveRoom(roomID string) (rooms.Room, string, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, "", err
	}
	if !room.Managed {
		return rooms.Room{}, "", ErrRoomNotManaged
	}
	path, err := existingDirectory(s.saveRoot, room.DirectoryName)
	return room, path, err
}

func (s *Service) resolveWorld(roomID, worldID string) (rooms.Room, rooms.World, string, error) {
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return rooms.Room{}, rooms.World{}, "", err
	}
	world, err := s.rooms.World(roomID, worldID)
	if err != nil {
		return rooms.Room{}, rooms.World{}, "", err
	}
	worldPath, err := existingDirectory(roomPath, world.DirectoryName)
	return room, world, worldPath, err
}

func existingDirectory(parent, name string) (string, error) {
	if !safeComponent(name) {
		return "", ErrUnsafePath
	}
	path := filepath.Join(parent, name)
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(path))
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", ErrUnsafePath
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrUnsafePath
	}
	return path, nil
}

func safeComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value &&
		!strings.ContainsAny(value, "/\\\x00\r\n") && len(value) <= 255
}

func readConfiguration(path string, optional bool) ([]byte, os.FileMode, time.Time, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && optional {
		return nil, 0640, time.Time{}, false, nil
	}
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, time.Time{}, false, ErrUnsafePath
	}
	if info.Size() > maxConfigurationBytes {
		return nil, 0, time.Time{}, false, ErrFileTooLarge
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigurationBytes+1))
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	if int64(len(data)) > maxConfigurationBytes {
		return nil, 0, time.Time{}, false, ErrFileTooLarge
	}
	return data, info.Mode().Perm(), info.ModTime().UTC(), true, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if int64(len(data)) > maxConfigurationBytes {
		return ErrFileTooLarge
	}
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".dst-admin-config-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	published := false
	defer func() {
		_ = file.Close()
		if !published {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(mode.Perm()); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	published = true
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := directoryHandle.Sync()
	closeErr := directoryHandle.Close()
	return errors.Join(syncErr, closeErr)
}

func revision(parts ...revisionPart) string {
	hash := sha256.New()
	for _, part := range parts {
		if part.exists {
			_, _ = hash.Write([]byte{1})
		} else {
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte(part.name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(part.data)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

type revisionPart struct {
	name   string
	data   []byte
	exists bool
}

func checkRevision(expected, current string) error {
	if expected == "" || expected != current {
		return &RevisionConflictError{CurrentRevision: current}
	}
	return nil
}

func (s *Service) protectionBackup(ctx context.Context, room rooms.Room, scope, sourceJobID string) (backups.Backup, error) {
	name := scope + "修改前保护备份 " + s.now().Format("2006-01-02 15:04:05")
	return s.backups.Create(ctx, room.ID, name, backups.KindProtection, sourceJobID)
}

func operation(before, after interface{}) string {
	if before == nil {
		return "add"
	}
	if after == nil {
		return "remove"
	}
	return "replace"
}

func intPointer(value int) *int { return &value }

func fieldLabel(schema []FieldSchema, key string) string {
	for _, field := range schema {
		if field.Key == key {
			return field.Label
		}
	}
	return key
}

func wrapApplyError(scope string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("apply %s configuration: %w", scope, err)
}
