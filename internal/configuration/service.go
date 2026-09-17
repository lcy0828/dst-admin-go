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
	"time"

	"dont/internal/backups"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/shared"
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
	saveRoot  string
	rooms     RoomCatalog
	backups   BackupCreator
	publisher Publisher
	reader    ConfigurationSnapshotReader
	tokens    ClusterTokenSnapshotReader
	states    ConfigurationStateRepository
	now       func() time.Time
}

type ConfigurationSnapshotReader interface {
	ReadConfiguration(context.Context, string, string, string) (runtimedriver.ConfigurationSnapshot, error)
}

type ClusterTokenSnapshotReader interface {
	RevealClusterToken(context.Context, string, string) (shared.RuntimeClusterTokenReveal, error)
}

type roomWorldCatalog interface {
	Worlds(string) ([]rooms.World, error)
}

func (s *Service) ConfigurePublisher(publisher Publisher) error {
	if publisher == nil {
		return errors.New("configuration publisher is required")
	}
	s.publisher = publisher
	return nil
}

func (s *Service) ConfigureReader(reader ConfigurationSnapshotReader) error {
	if reader == nil {
		return errors.New("configuration reader is required")
	}
	if _, ok := s.rooms.(roomWorldCatalog); !ok {
		return errors.New("configuration room catalog cannot enumerate worlds")
	}
	s.reader = reader
	if tokens, ok := reader.(ClusterTokenSnapshotReader); ok {
		s.tokens = tokens
	}
	return nil
}

func (s *Service) ConfigureStateRepository(repository ConfigurationStateRepository) error {
	if repository == nil {
		return errors.New("configuration state repository is required")
	}
	s.states = repository
	return nil
}

func (s *Service) observedRuntimeConfiguration(roomID, worldID, scope string, readErr error) (runtimedriver.ConfigurationSnapshot, SyncState, error) {
	if s.states == nil {
		return runtimedriver.ConfigurationSnapshot{}, SyncState{}, readErr
	}
	state, err := s.states.Get(roomID, worldID, scope)
	if err != nil {
		return runtimedriver.ConfigurationSnapshot{}, SyncState{}, readErr
	}
	snapshot, ok := state.ObservedSnapshot()
	if !ok {
		return runtimedriver.ConfigurationSnapshot{}, SyncState{}, readErr
	}
	message := ""
	if readErr != nil {
		message = readErr.Error()
	}
	sync := syncStateFromStored(state, true, message)
	return snapshot, sync, nil
}

func (s *Service) observeRuntimeConfiguration(roomID, worldID, scope string, snapshot runtimedriver.ConfigurationSnapshot, revision string, modified time.Time) SyncState {
	observedAt := s.now().UTC()
	if s.states == nil {
		return SyncState{Status: "synced", Source: "runtime-disk", ObservedRevision: revision, ObservedAt: &observedAt}
	}
	state, err := s.states.Observe(roomID, worldID, scope, snapshot, revision, observedAt)
	if err != nil {
		return SyncState{Status: "untracked", Source: "runtime-disk", ObservedRevision: revision, LastError: err.Error()}
	}
	return syncStateFromStored(state, false, "")
}

func syncStateFromStored(state StoredConfigurationState, stale bool, lastError string) SyncState {
	status := "synced"
	if stale {
		status = "stale"
	}
	return SyncState{
		Status: status, Source: "runtime-disk", ObservedRevision: state.ObservedRevision,
		TargetID: state.Observed.Target.TargetID, InstallationID: state.Observed.Target.InstallationID,
		ObservedAt: state.ObservedAt, ReadOnly: stale, Stale: stale, LastError: lastError,
	}
}

func (s *Service) readRuntimeConfiguration(ctx context.Context, roomID, worldID, scope string) (runtimedriver.ConfigurationSnapshot, error) {
	if s.reader == nil {
		return runtimedriver.ConfigurationSnapshot{}, errors.New("configuration reader is unavailable")
	}
	worldID, err := s.configurationTargetWorld(roomID, worldID)
	if err != nil {
		return runtimedriver.ConfigurationSnapshot{}, err
	}
	return s.reader.ReadConfiguration(ctx, roomID, worldID, scope)
}

// SharedConfigurationPayload reads cluster.ini from the Master Runtime. It is
// used by topology repair so a start operation can render target-specific
// Shard fields without ever falling back to Controller provisioning files.
func (s *Service) SharedConfigurationPayload(ctx context.Context, roomID string) ([]rooms.ProvisionFile, error) {
	snapshot, err := s.readRuntimeConfiguration(ctx, roomID, "", string(PublicationShared))
	if err != nil {
		return nil, err
	}
	file, _, err := configurationSnapshotFile(snapshot.Result.Files, "cluster.ini", false)
	if err != nil {
		return nil, err
	}
	return roomPublicationFiles(file.data, file.mode), nil
}

func (s *Service) configurationTargetWorld(roomID, worldID string) (string, error) {
	if worldID != "" {
		return worldID, nil
	}
	worlds, err := s.rooms.(roomWorldCatalog).Worlds(roomID)
	if err != nil {
		return "", err
	}
	for _, world := range worlds {
		if world.IsMaster {
			return world.ID, nil
		}
	}
	if len(worlds) == 0 {
		return "", rooms.ErrWorldNotFound
	}
	return worlds[0].ID, nil
}

func (s *Service) managedRoom(roomID string) (rooms.Room, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, err
	}
	if !room.Managed {
		return rooms.Room{}, ErrRoomNotManaged
	}
	return room, nil
}

func (s *Service) publish(ctx context.Context, request PublicationRequest) (int, error) {
	if s.publisher == nil {
		return 0, nil
	}
	result, err := s.publisher.Publish(ctx, request)
	return result.PublishedCount, err
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
		now: time.Now,
	}, nil
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

func configurationSnapshotFile(files []shared.RuntimeConfigurationFile, name string, optional bool) (fileSnapshot, time.Time, error) {
	for _, file := range files {
		if file.Name != name {
			continue
		}
		if !file.Exists {
			if optional {
				return fileSnapshot{mode: 0o640}, time.Time{}, nil
			}
			return fileSnapshot{}, time.Time{}, os.ErrNotExist
		}
		mode := os.FileMode(file.Mode).Perm()
		if mode == 0 {
			return fileSnapshot{}, time.Time{}, ErrUnsafePath
		}
		return fileSnapshot{data: append([]byte(nil), file.Data...), mode: mode, exists: true}, file.UpdatedAt.UTC(), nil
	}
	if optional {
		return fileSnapshot{mode: 0o640}, time.Time{}, nil
	}
	return fileSnapshot{}, time.Time{}, os.ErrNotExist
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

func configurationFileDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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
