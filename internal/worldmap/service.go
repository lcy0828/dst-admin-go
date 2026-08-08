package worldmap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"dont/internal/jobs"
	"dont/internal/rooms"

	"github.com/google/uuid"
)

const (
	maxSessions       = 500
	maxRendererLog    = 512 * 1024
	maxLayerImageSize = int64(64 * 1024 * 1024)
	maxMapDimension   = 16384
	failedRetention   = 20
)

var (
	ErrRoomNotManaged       = errors.New("room must be managed before maps can be used")
	ErrSessionNotFound      = errors.New("session snapshot not found")
	ErrUnsafeSessionPath    = errors.New("session snapshot path is unsafe")
	ErrInvalidLayers        = errors.New("map layers are invalid")
	ErrMapImageNotFound     = errors.New("map image not found")
	ErrGenerationInProgress = errors.New("map generation is already active for this world")
	ErrRendererOutput       = errors.New("map renderer output is invalid")
)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}

type Config struct {
	SaveRoot  string
	MapRoot   string
	Retention int
}

type sessionReference struct {
	RoomID  string `json:"r"`
	WorldID string `json:"w"`
	Session string `json:"s"`
	File    string `json:"f"`
}

type Service struct {
	config   Config
	rooms    RoomCatalog
	store    *Store
	renderer Renderer
	mu       sync.Mutex
	active   map[string]bool
}

func NewService(config Config, roomCatalog RoomCatalog, store *Store, renderer Renderer) (*Service, error) {
	if roomCatalog == nil || store == nil || renderer == nil {
		return nil, errors.New("room catalog, map store, and renderer are required")
	}
	var err error
	config.SaveRoot, err = absolutePath(config.SaveRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve save root: %w", err)
	}
	config.MapRoot, err = absolutePath(config.MapRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve map root: %w", err)
	}
	if config.Retention <= 0 {
		config.Retention = 3
	}
	if config.Retention > 20 {
		return nil, errors.New("map retention cannot exceed 20")
	}
	if err := cleanupStaging(config.MapRoot); err != nil {
		return nil, fmt.Errorf("clean map staging directories: %w", err)
	}
	return &Service{config: config, rooms: roomCatalog, store: store, renderer: renderer, active: make(map[string]bool)}, nil
}

func (s *Service) RendererStatus() (bool, string) { return s.renderer.Available() }

func (s *Service) List(roomID string) ([]Map, error) {
	if _, _, err := s.resolveRoom(roomID); err != nil {
		return nil, err
	}
	return s.store.List(roomID)
}

func (s *Service) Get(id string) (Map, error) { return s.store.Get(id) }

func (s *Service) Sessions(roomID, worldID string) ([]Session, error) {
	room, world, err := s.resolveWorld(roomID, worldID)
	if err != nil {
		return nil, err
	}
	root, err := s.sessionRoot(room, world)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return []Session{}, nil
		}
		return nil, err
	}
	directories, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read session directory: %w", err)
	}
	result := make([]Session, 0)
	for _, directory := range directories {
		if !directory.IsDir() || !safeComponent(directory.Name()) {
			continue
		}
		directoryPath := filepath.Join(root, directory.Name())
		info, err := os.Lstat(directoryPath)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		entries, err := os.ReadDir(directoryPath)
		if err != nil {
			continue
		}
		players := 0
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "KU_") {
				players++
			}
		}
		for _, entry := range entries {
			if entry.IsDir() || !safeComponent(entry.Name()) || strings.HasPrefix(entry.Name(), ".") || strings.HasSuffix(strings.ToLower(entry.Name()), ".meta") {
				continue
			}
			path := filepath.Join(directoryPath, entry.Name())
			fileInfo, err := os.Lstat(path)
			if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 {
				continue
			}
			reference := sessionReference{RoomID: roomID, WorldID: worldID, Session: directory.Name(), File: entry.Name()}
			id, err := encodeSessionReference(reference)
			if err != nil {
				return nil, err
			}
			result = append(result, Session{
				ID: id, RoomID: roomID, WorldID: worldID, SessionID: directory.Name(), FileName: entry.Name(),
				Size: fileInfo.Size(), PlayerCount: players, ModifiedAt: fileInfo.ModTime().UTC(),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ModifiedAt.Equal(result[j].ModifiedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].ModifiedAt.After(result[j].ModifiedAt)
	})
	if len(result) > maxSessions {
		result = result[:maxSessions]
	}
	if len(result) > 0 {
		result[0].Latest = true
	}
	return result, nil
}

func (s *Service) OpenSession(id string) (*os.File, os.FileInfo, Session, error) {
	path, session, err := s.resolveSession(id, "", "")
	if err != nil {
		return nil, nil, Session{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, Session{}, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, Session{}, err
	}
	session.Size = info.Size()
	session.ModifiedAt = info.ModTime().UTC()
	return file, info, session, nil
}

func (s *Service) Prepare(roomID string, request GenerateRequest) ([]jobs.TargetSpec, func(jobs.Job) jobs.Runner, func(), error) {
	layers, err := normalizeLayers(request.Layers)
	if err != nil {
		return nil, nil, nil, err
	}
	_, session, err := s.resolveSession(request.SessionID, roomID, request.WorldID)
	if err != nil {
		return nil, nil, nil, err
	}
	if available, _ := s.renderer.Available(); !available {
		return nil, nil, nil, ErrRendererUnavailable
	}
	key := roomID + "\x00" + request.WorldID
	s.mu.Lock()
	if s.active[key] {
		s.mu.Unlock()
		return nil, nil, nil, ErrGenerationInProgress
	}
	s.active[key] = true
	s.mu.Unlock()
	release := sync.OnceFunc(func() {
		s.mu.Lock()
		delete(s.active, key)
		s.mu.Unlock()
	})
	targets := []jobs.TargetSpec{{ID: request.WorldID, Name: "渲染 " + session.SessionID + " / " + session.FileName}}
	factory := func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			defer release()
			value, generateErr := s.Generate(ctx, job.ID, roomID, request.WorldID, request.SessionID, layers)
			if generateErr != nil {
				status := jobs.StatusFailed
				code := "MAP_GENERATION_FAILED"
				if errors.Is(generateErr, context.Canceled) {
					status = jobs.StatusCanceled
					code = "JOB_CANCELED"
				}
				report(jobs.TargetResult{TargetID: request.WorldID, Status: status, Error: &jobs.Error{Code: code, Message: generateErr.Error()}})
				return generateErr
			}
			report(jobs.TargetResult{TargetID: request.WorldID, Status: jobs.StatusSucceeded, Message: fmt.Sprintf("地图已生成：%dx%d，%d 个图层", value.Width, value.Height, len(value.Layers))})
			return nil
		}
	}
	return targets, factory, release, nil
}

func (s *Service) Generate(ctx context.Context, jobID, roomID, worldID, sessionID string, layers []Layer) (Map, error) {
	input, session, err := s.resolveSession(sessionID, roomID, worldID)
	if err != nil {
		return Map{}, err
	}
	id := uuid.NewString()
	value, err := s.store.Begin(Map{
		ID: id, RoomID: roomID, WorldID: worldID, SessionID: sessionID,
		SessionLabel: session.SessionID + " / " + session.FileName, Layers: layers, SourceJobID: jobID,
	})
	if err != nil {
		return Map{}, err
	}
	if err := os.MkdirAll(s.config.MapRoot, 0750); err != nil {
		return s.fail(value, "staging", "", err)
	}
	staging, err := os.MkdirTemp(s.config.MapRoot, ".map-render-")
	if err != nil {
		return s.fail(value, "staging", "", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	logBuffer := &boundedLog{limit: maxRendererLog}
	if err := s.renderer.Render(ctx, input, staging, layers, logBuffer); err != nil {
		return s.fail(value, "renderer", logBuffer.String(), err)
	}
	width, height, err := validateLayerImages(staging, layers)
	if err != nil {
		return s.fail(value, "validate", logBuffer.String(), err)
	}
	final := filepath.Join(s.config.MapRoot, id)
	if err := os.Rename(staging, final); err != nil {
		return s.fail(value, "publish", logBuffer.String(), err)
	}
	published = true
	completed, err := s.store.Complete(id, "succeeded", "complete", logBuffer.String(), "", width, height)
	if err != nil {
		_ = os.RemoveAll(final)
		return Map{}, err
	}
	_ = s.prune(roomID, worldID)
	return completed, nil
}

func (s *Service) OpenImage(id string, layer Layer) (*os.File, os.FileInfo, Map, error) {
	value, err := s.store.Get(id)
	if err != nil {
		return nil, nil, Map{}, err
	}
	if value.Status != "succeeded" || !containsLayer(value.Layers, layer) {
		return nil, nil, Map{}, ErrMapImageNotFound
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, nil, Map{}, ErrMapImageNotFound
	}
	path := filepath.Join(s.config.MapRoot, id, layerFileName(layer))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, Map{}, ErrMapImageNotFound
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, Map{}, err
	}
	return file, info, value, nil
}

func (s *Service) fail(value Map, stage, logText string, generationErr error) (Map, error) {
	_, storeErr := s.store.Complete(value.ID, "failed", stage, logText, generationErr.Error(), 0, 0)
	if storeErr != nil {
		return Map{}, errors.Join(generationErr, storeErr)
	}
	if pruneErr := s.pruneFailed(value.RoomID, value.WorldID); pruneErr != nil {
		return Map{}, errors.Join(generationErr, pruneErr)
	}
	return Map{}, generationErr
}

func (s *Service) prune(roomID, worldID string) error {
	items, err := s.store.Successful(roomID, worldID)
	if err != nil {
		return err
	}
	if len(items) <= s.config.Retention {
		return nil
	}
	for _, value := range items[s.config.Retention:] {
		if _, err := uuid.Parse(value.ID); err != nil {
			continue
		}
		path := filepath.Join(s.config.MapRoot, value.ID)
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		if err := s.store.Delete(value.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) pruneFailed(roomID, worldID string) error {
	items, err := s.store.ByStatus(roomID, worldID, "failed")
	if err != nil {
		return err
	}
	if len(items) <= failedRetention {
		return nil
	}
	for _, value := range items[failedRetention:] {
		if err := s.store.Delete(value.ID); err != nil {
			return err
		}
	}
	return nil
}

func cleanupStaging(root string) error {
	if err := os.MkdirAll(root, 0750); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".map-render-") || !safeComponent(entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) resolveRoom(roomID string) (rooms.Room, string, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, "", err
	}
	if !room.Managed {
		return rooms.Room{}, "", ErrRoomNotManaged
	}
	path, err := safeDirectory(s.config.SaveRoot, room.DirectoryName)
	return room, path, err
}

func (s *Service) resolveWorld(roomID, worldID string) (rooms.Room, rooms.World, error) {
	room, _, err := s.resolveRoom(roomID)
	if err != nil {
		return rooms.Room{}, rooms.World{}, err
	}
	world, err := s.rooms.World(roomID, worldID)
	if err != nil {
		return rooms.Room{}, rooms.World{}, err
	}
	return room, world, nil
}

func (s *Service) sessionRoot(room rooms.Room, world rooms.World) (string, error) {
	roomPath, err := safeDirectory(s.config.SaveRoot, room.DirectoryName)
	if err != nil {
		return "", err
	}
	worldPath, err := safeDirectory(roomPath, world.DirectoryName)
	if err != nil {
		return "", err
	}
	savePath, err := safeDirectory(worldPath, "save")
	if err != nil {
		return "", ErrSessionNotFound
	}
	path, err := safeDirectory(savePath, "session")
	if err != nil {
		return "", ErrSessionNotFound
	}
	return path, nil
}

func (s *Service) resolveSession(id, expectedRoomID, expectedWorldID string) (string, Session, error) {
	reference, err := decodeSessionReference(id)
	if err != nil || (expectedRoomID != "" && reference.RoomID != expectedRoomID) || (expectedWorldID != "" && reference.WorldID != expectedWorldID) {
		return "", Session{}, ErrSessionNotFound
	}
	room, world, err := s.resolveWorld(reference.RoomID, reference.WorldID)
	if err != nil {
		return "", Session{}, err
	}
	root, err := s.sessionRoot(room, world)
	if err != nil {
		return "", Session{}, err
	}
	directory, err := safeDirectory(root, reference.Session)
	if err != nil {
		return "", Session{}, ErrUnsafeSessionPath
	}
	if !safeComponent(reference.File) {
		return "", Session{}, ErrUnsafeSessionPath
	}
	path := filepath.Join(directory, reference.File)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", Session{}, ErrSessionNotFound
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", Session{}, ErrUnsafeSessionPath
	}
	return path, Session{
		ID: id, RoomID: reference.RoomID, WorldID: reference.WorldID, SessionID: reference.Session,
		FileName: reference.File, Size: info.Size(), ModifiedAt: info.ModTime().UTC(),
	}, nil
}

func validateLayerImages(root string, layers []Layer) (int, int, error) {
	width, height := 0, 0
	for _, layer := range layers {
		path := filepath.Join(root, layerFileName(layer))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxLayerImageSize {
			return 0, 0, fmt.Errorf("%w: missing or unsafe %s", ErrRendererOutput, layer)
		}
		file, err := os.Open(path)
		if err != nil {
			return 0, 0, err
		}
		config, decodeErr := png.DecodeConfig(io.LimitReader(file, maxLayerImageSize))
		_ = file.Close()
		if decodeErr != nil || config.Width <= 0 || config.Height <= 0 || config.Width > maxMapDimension || config.Height > maxMapDimension {
			return 0, 0, fmt.Errorf("%w: invalid PNG for %s", ErrRendererOutput, layer)
		}
		if width == 0 {
			width, height = config.Width, config.Height
		} else if config.Width != width || config.Height != height {
			return 0, 0, fmt.Errorf("%w: layer dimensions do not match", ErrRendererOutput)
		}
	}
	return width, height, nil
}

func normalizeLayers(input []Layer) ([]Layer, error) {
	allowed := map[Layer]bool{LayerTerrain: true, LayerWalrusCamps: true, LayerSpawnPoints: true, LayerPlayers: true, LayerWorldState: true}
	seen := make(map[Layer]bool)
	result := []Layer{LayerTerrain}
	seen[LayerTerrain] = true
	for _, layer := range input {
		if !allowed[layer] {
			return nil, ErrInvalidLayers
		}
		if !seen[layer] {
			seen[layer] = true
			if layer != LayerTerrain {
				result = append(result, layer)
			}
		}
	}
	return result, nil
}

func containsLayer(layers []Layer, wanted Layer) bool {
	for _, layer := range layers {
		if layer == wanted {
			return true
		}
	}
	return false
}

func encodeSessionReference(value sessionReference) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeSessionReference(id string) (sessionReference, error) {
	data, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || base64.RawURLEncoding.EncodeToString(data) != id || len(data) > 2048 {
		return sessionReference{}, ErrSessionNotFound
	}
	var value sessionReference
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || !safeComponent(value.Session) || !safeComponent(value.File) || value.RoomID == "" || value.WorldID == "" {
		return sessionReference{}, ErrSessionNotFound
	}
	return value, nil
}

func safeDirectory(parent, component string) (string, error) {
	if !safeComponent(component) {
		return "", ErrUnsafeSessionPath
	}
	path := filepath.Join(parent, component)
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return "", ErrUnsafeSessionPath
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrUnsafeSessionPath
	}
	return path, nil
}

func safeComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !strings.ContainsAny(value, "/\\\x00\r\n") && len(value) <= 255
}

func absolutePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("path is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

type boundedLog struct {
	buffer bytes.Buffer
	limit  int
	cut    bool
}

func (b *boundedLog) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
			b.cut = true
		}
		_, _ = b.buffer.Write(value)
	} else {
		b.cut = true
	}
	return original, nil
}

func (b *boundedLog) String() string {
	if b.cut {
		return b.buffer.String() + "\n[DST Admin] 渲染器输出已截断\n"
	}
	return b.buffer.String()
}
