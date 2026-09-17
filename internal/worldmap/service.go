package worldmap

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/jobs"
	"dont/internal/maprenderer"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/shared"

	"github.com/google/uuid"
)

const (
	maxSessions          = 500
	maxRendererLog       = 512 * 1024
	maxLayerImageSize    = int64(64 * 1024 * 1024)
	maxManifestSize      = int64(2 * 1024 * 1024)
	maxFeaturesSize      = int64(64 * 1024 * 1024)
	maxSnapshotSize      = int64(128 * 1024 * 1024)
	maxMapDimension      = 16384
	failedRetention      = 20
	defaultRenderTimeout = 90 * time.Second
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

var rendererSecretPattern = regexp.MustCompile(`(?i)\b(api[_ -]?key|token|password|secret)\b\s*[:=]\s*[^;\s]+`)
var rendererANSIPattern = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}

type RuntimeRouter interface {
	DriverTarget(context.Context, string, string) (runtimedriver.Driver, runtimedriver.Target, error)
}

type LeaseService interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Renew(context.Context, operationlease.Lease, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

type Config struct {
	SaveRoot      string
	MapRoot       string
	Retention     int
	RenderTimeout time.Duration
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
	runtimes RuntimeRouter
	leases   LeaseService
}

type remoteMapRuntime struct {
	driver runtimedriver.MapDriver
	target runtimedriver.Target
}

type sessionLocation struct {
	path    string
	session Session
	remote  *remoteMapRuntime
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
	if config.RenderTimeout <= 0 {
		config.RenderTimeout = defaultRenderTimeout
	}
	if config.RenderTimeout > 10*time.Minute {
		return nil, errors.New("map render timeout cannot exceed 10 minutes")
	}
	if err := cleanupStaging(config.MapRoot); err != nil {
		return nil, fmt.Errorf("clean map staging directories: %w", err)
	}
	return &Service{config: config, rooms: roomCatalog, store: store, renderer: renderer, active: make(map[string]bool)}, nil
}

func (s *Service) ConfigureRemote(runtimes RuntimeRouter, leases LeaseService) error {
	if runtimes == nil || leases == nil {
		return errors.New("map runtime router and lease service are required")
	}
	s.runtimes, s.leases = runtimes, leases
	return nil
}

func (s *Service) RendererStatus() RendererInfo {
	if renderer, ok := s.renderer.(interface{ Info() RendererInfo }); ok {
		return renderer.Info()
	}
	available, path := s.renderer.Available()
	return RendererInfo{Available: available, Path: path, Artifacts: []string{}}
}

func (s *Service) RendererStatusForWorld(ctx context.Context, roomID, worldID string) RendererInfo {
	remote, err := s.remoteRuntime(ctx, roomID, worldID)
	if err != nil {
		return RendererInfo{Artifacts: []string{}, Error: err.Error()}
	}
	if remote == nil {
		return s.RendererStatus()
	}
	status, err := remote.driver.MapRendererStatus(ctx, remote.target)
	if err != nil {
		return RendererInfo{Artifacts: []string{}, Error: err.Error()}
	}
	return RendererInfo{
		Available: status.Available, ProtocolVersion: status.ProtocolVersion, Version: status.Version,
		Artifacts: append([]string(nil), status.Artifacts...), Error: status.Error,
	}
}

func (s *Service) List(roomID string) ([]Map, error) {
	if _, _, err := s.resolveRoom(roomID); err != nil {
		return nil, err
	}
	return s.store.List(roomID)
}

func (s *Service) Get(id string) (Map, error) { return s.store.Get(id) }

func (s *Service) Sessions(roomID, worldID string) ([]Session, error) {
	return s.SessionsContext(context.Background(), roomID, worldID)
}

func (s *Service) SessionsContext(ctx context.Context, roomID, worldID string) ([]Session, error) {
	room, world, err := s.resolveWorld(roomID, worldID)
	if err != nil {
		return nil, err
	}
	remote, err := s.remoteRuntime(ctx, roomID, worldID)
	if err != nil {
		return nil, err
	}
	if remote != nil {
		return s.remoteSessions(ctx, roomID, worldID, remote)
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

func (s *Service) remoteSessions(ctx context.Context, roomID, worldID string, remote *remoteMapRuntime) ([]Session, error) {
	items, err := remote.driver.ListMapSessions(ctx, remote.target)
	if err != nil {
		return nil, err
	}
	result := make([]Session, 0, len(items))
	for _, item := range items {
		if !safeComponent(item.SessionID) || !safeComponent(item.FileName) || item.Size <= 0 || item.Size > maxSnapshotSize || item.PlayerCount < 0 || item.ModifiedAt.IsZero() {
			return nil, ErrUnsafeSessionPath
		}
		reference := sessionReference{RoomID: roomID, WorldID: worldID, Session: item.SessionID, File: item.FileName}
		id, err := encodeSessionReference(reference)
		if err != nil {
			return nil, err
		}
		result = append(result, Session{
			ID: id, RoomID: roomID, WorldID: worldID, SessionID: item.SessionID, FileName: item.FileName,
			Size: item.Size, PlayerCount: item.PlayerCount, ModifiedAt: item.ModifiedAt.UTC(),
		})
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

func (s *Service) OpenSessionContext(ctx context.Context, id string) (*os.File, os.FileInfo, Session, func(), error) {
	location, err := s.resolveSessionLocation(ctx, id, "", "")
	if err != nil {
		return nil, nil, Session{}, nil, err
	}
	if location.remote == nil {
		file, err := os.Open(location.path)
		if err != nil {
			return nil, nil, Session{}, nil, err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, nil, Session{}, nil, err
		}
		return file, info, location.session, func() {}, nil
	}
	return s.downloadRemoteSession(ctx, location)
}

func (s *Service) Prepare(roomID string, request GenerateRequest) ([]jobs.TargetSpec, func(jobs.Job) jobs.Runner, func(), error) {
	return s.PrepareContext(context.Background(), roomID, request)
}

func (s *Service) PrepareContext(ctx context.Context, roomID string, request GenerateRequest) ([]jobs.TargetSpec, func(jobs.Job) jobs.Runner, func(), error) {
	layers, err := normalizeLayers(request.Layers)
	if err != nil {
		return nil, nil, nil, err
	}
	location, err := s.resolveSessionLocation(ctx, request.SessionID, roomID, request.WorldID)
	if err != nil {
		return nil, nil, nil, err
	}
	if location.remote == nil {
		if available, _ := s.renderer.Available(); !available {
			return nil, nil, nil, ErrRendererUnavailable
		}
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
	targets := []jobs.TargetSpec{{ID: request.WorldID, Name: "渲染 " + location.session.SessionID + " / " + location.session.FileName}}
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
	layers, err := normalizeLayers(layers)
	if err != nil {
		return Map{}, err
	}
	location, err := s.resolveSessionLocation(ctx, sessionID, roomID, worldID)
	if err != nil {
		return Map{}, err
	}
	id := uuid.NewString()
	value, err := s.store.Begin(Map{
		ID: id, RoomID: roomID, WorldID: worldID, SessionID: sessionID,
		SessionLabel: location.session.SessionID + " / " + location.session.FileName, Layers: layers, SourceJobID: jobID,
	})
	if err != nil {
		return Map{}, err
	}
	if location.remote != nil {
		return s.generateRemote(ctx, value, location, layers)
	}
	input := location.path
	if err := os.MkdirAll(s.config.MapRoot, 0750); err != nil {
		return s.fail(value, "staging", "", err)
	}
	workDirectory, err := os.MkdirTemp(s.config.MapRoot, ".map-render-")
	if err != nil {
		return s.fail(value, "staging", "", err)
	}
	defer os.RemoveAll(workDirectory)
	snapshotPath := filepath.Join(workDirectory, "session.snapshot")
	sourceSHA256, err := copySessionSnapshot(ctx, input, snapshotPath)
	if err != nil {
		return s.fail(value, "snapshot", "", err)
	}
	artifactDirectory := filepath.Join(workDirectory, "artifacts")
	if err := os.Mkdir(artifactDirectory, 0750); err != nil {
		return s.fail(value, "staging", "", err)
	}
	if err := s.store.Stage(value.ID, "renderer"); err != nil {
		return Map{}, err
	}
	logBuffer := &boundedLog{limit: maxRendererLog}
	renderContext, cancel := context.WithTimeout(ctx, s.config.RenderTimeout)
	renderErr := s.renderer.Render(renderContext, snapshotPath, artifactDirectory, layers, logBuffer)
	cancel()
	sanitizedLog := sanitizeRendererText(logBuffer.String(), input, snapshotPath, workDirectory, s.config.SaveRoot, s.config.MapRoot)
	if renderErr != nil {
		return s.fail(value, "renderer", sanitizedLog, renderErr)
	}
	if err := s.store.Stage(value.ID, "validate"); err != nil {
		return Map{}, err
	}
	manifest, err := validateRendererArtifacts(artifactDirectory, sourceSHA256)
	if err != nil {
		return s.fail(value, "validate", sanitizedLog, err)
	}
	final := filepath.Join(s.config.MapRoot, id)
	if err := s.store.Stage(value.ID, "publish"); err != nil {
		return Map{}, err
	}
	if err := os.Rename(artifactDirectory, final); err != nil {
		return s.fail(value, "publish", sanitizedLog, err)
	}
	value.Width = manifest.Map.ImageWidth
	value.Height = manifest.Map.ImageHeight
	value.FeatureCount = manifest.Statistics.FeatureCount
	value.WarningCount = len(manifest.Warnings)
	value.SourceSHA256 = sourceSHA256
	value.RendererVersion = manifest.RendererVersion
	completed, err := s.store.Complete(id, "succeeded", "complete", sanitizedLog, "", value)
	if err != nil {
		_ = os.RemoveAll(final)
		return Map{}, err
	}
	_ = s.prune(roomID, worldID)
	return completed, nil
}

func (s *Service) remoteRuntime(ctx context.Context, roomID, worldID string) (*remoteMapRuntime, error) {
	if s.runtimes == nil {
		return nil, nil
	}
	driver, target, err := s.runtimes.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return nil, err
	}
	if target.TargetID == "local" {
		return nil, nil
	}
	remote, ok := driver.(runtimedriver.MapDriver)
	if !ok || !runtimedriver.HasTargetCapability(driver, target, runtimedriver.CapabilityMapRender) {
		return nil, ErrRendererUnavailable
	}
	return &remoteMapRuntime{driver: remote, target: target}, nil
}

func (s *Service) resolveSessionLocation(ctx context.Context, id, expectedRoomID, expectedWorldID string) (sessionLocation, error) {
	reference, err := decodeSessionReference(id)
	if err != nil || expectedRoomID != "" && reference.RoomID != expectedRoomID || expectedWorldID != "" && reference.WorldID != expectedWorldID {
		return sessionLocation{}, ErrSessionNotFound
	}
	if _, _, err := s.resolveWorld(reference.RoomID, reference.WorldID); err != nil {
		return sessionLocation{}, err
	}
	remote, err := s.remoteRuntime(ctx, reference.RoomID, reference.WorldID)
	if err != nil {
		return sessionLocation{}, err
	}
	if remote == nil {
		path, session, err := s.resolveSession(id, expectedRoomID, expectedWorldID)
		return sessionLocation{path: path, session: session}, err
	}
	sessions, err := s.remoteSessions(ctx, reference.RoomID, reference.WorldID, remote)
	if err != nil {
		return sessionLocation{}, err
	}
	for _, session := range sessions {
		if session.ID == id {
			return sessionLocation{session: session, remote: remote}, nil
		}
	}
	return sessionLocation{}, ErrSessionNotFound
}

func (s *Service) generateRemote(ctx context.Context, value Map, location sessionLocation, layers []Layer) (Map, error) {
	if s.leases == nil || location.remote == nil {
		return s.fail(value, "runtime", "", ErrRendererUnavailable)
	}
	if err := os.MkdirAll(s.config.MapRoot, 0o750); err != nil {
		return s.fail(value, "staging", "", err)
	}
	workDirectory, err := os.MkdirTemp(s.config.MapRoot, ".map-render-")
	if err != nil {
		return s.fail(value, "staging", "", err)
	}
	defer os.RemoveAll(workDirectory)
	artifactDirectory := filepath.Join(workDirectory, "artifacts")
	if err := os.Mkdir(artifactDirectory, 0o750); err != nil {
		return s.fail(value, "staging", "", err)
	}
	lease, err := s.leases.Acquire(ctx, value.RoomID, "map.render:"+value.ID, 5*time.Minute)
	if err != nil {
		return s.fail(value, "runtime", "", err)
	}
	defer func() { _ = s.leases.Release(lease) }()
	if err := s.store.Stage(value.ID, "renderer"); err != nil {
		return Map{}, err
	}
	requestedLayers := make([]string, len(layers))
	for index := range layers {
		requestedLayers[index] = string(layers[index])
	}
	renderContext, cancel := context.WithTimeout(ctx, s.config.RenderTimeout)
	descriptor, renderErr := location.remote.driver.RenderMap(
		renderContext, location.remote.target, mapOperation(lease, value.ID, "render"), value.ID,
		location.session.SessionID, location.session.FileName, requestedLayers,
	)
	cancel()
	sanitizedLog := sanitizeRendererText(descriptor.Log, s.config.SaveRoot, s.config.MapRoot)
	if renderErr != nil {
		return s.fail(value, "renderer", sanitizedLog, renderErr)
	}
	cleanupNeeded := true
	defer func() {
		if cleanupNeeded {
			_ = s.releaseRemoteTransfer(context.WithoutCancel(ctx), location.remote, &lease, value.ID)
		}
	}()
	if err := validateMapDescriptor(descriptor, value.ID); err != nil {
		return s.fail(value, "transfer", sanitizedLog, err)
	}
	if err := s.store.Stage(value.ID, "transfer"); err != nil {
		return Map{}, err
	}
	archivePath := filepath.Join(workDirectory, "artifacts.zip")
	if err := readRemoteTransfer(ctx, location.remote.driver, location.remote.target, descriptor, archivePath); err != nil {
		return s.fail(value, "transfer", sanitizedLog, err)
	}
	if err := extractArtifactArchive(archivePath, artifactDirectory); err != nil {
		return s.fail(value, "validate", sanitizedLog, err)
	}
	cleanupErr := s.releaseRemoteTransfer(context.WithoutCancel(ctx), location.remote, &lease, value.ID)
	cleanupNeeded = false
	if cleanupErr != nil {
		sanitizedLog = strings.TrimSpace(sanitizedLog + "\n[DST Admin] temporary map cleanup failed: " + sanitizeRendererText(cleanupErr.Error()))
	}
	if err := s.store.Stage(value.ID, "validate"); err != nil {
		return Map{}, err
	}
	manifest, err := validateRendererArtifacts(artifactDirectory, descriptor.SourceSHA256)
	if err != nil {
		return s.fail(value, "validate", sanitizedLog, err)
	}
	final := filepath.Join(s.config.MapRoot, value.ID)
	if err := s.store.Stage(value.ID, "publish"); err != nil {
		return Map{}, err
	}
	if err := os.Rename(artifactDirectory, final); err != nil {
		return s.fail(value, "publish", sanitizedLog, err)
	}
	value.Width, value.Height = manifest.Map.ImageWidth, manifest.Map.ImageHeight
	value.FeatureCount, value.WarningCount = manifest.Statistics.FeatureCount, len(manifest.Warnings)
	value.SourceSHA256, value.RendererVersion = descriptor.SourceSHA256, manifest.RendererVersion
	completed, err := s.store.Complete(value.ID, "succeeded", "complete", sanitizedLog, "", value)
	if err != nil {
		_ = os.RemoveAll(final)
		return Map{}, err
	}
	_ = s.prune(value.RoomID, value.WorldID)
	return completed, nil
}

func (s *Service) downloadRemoteSession(ctx context.Context, location sessionLocation) (*os.File, os.FileInfo, Session, func(), error) {
	if s.leases == nil || location.remote == nil {
		return nil, nil, Session{}, nil, ErrRendererUnavailable
	}
	transferID := uuid.NewString()
	lease, err := s.leases.Acquire(ctx, location.session.RoomID, "map.snapshot:"+transferID, 5*time.Minute)
	if err != nil {
		return nil, nil, Session{}, nil, err
	}
	defer func() { _ = s.leases.Release(lease) }()
	descriptor, err := location.remote.driver.PrepareMapSnapshot(
		ctx, location.remote.target, mapOperation(lease, transferID, "prepare"), transferID,
		location.session.SessionID, location.session.FileName,
	)
	if err != nil {
		return nil, nil, Session{}, nil, err
	}
	cleanupNeeded := true
	defer func() {
		if cleanupNeeded {
			_ = s.releaseRemoteTransfer(context.WithoutCancel(ctx), location.remote, &lease, transferID)
		}
	}()
	if err := validateMapDescriptor(descriptor, transferID); err != nil || descriptor.SHA256 != descriptor.SourceSHA256 {
		return nil, nil, Session{}, nil, errors.Join(err, ErrRendererOutput)
	}
	workDirectory, err := os.MkdirTemp(s.config.MapRoot, ".map-download-")
	if err != nil {
		return nil, nil, Session{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(workDirectory) }
	path := filepath.Join(workDirectory, "session.snapshot")
	if err := readRemoteTransfer(ctx, location.remote.driver, location.remote.target, descriptor, path); err != nil {
		cleanup()
		return nil, nil, Session{}, nil, err
	}
	if err := s.releaseRemoteTransfer(context.WithoutCancel(ctx), location.remote, &lease, transferID); err != nil {
		cleanupNeeded = false
		cleanup()
		return nil, nil, Session{}, nil, err
	}
	cleanupNeeded = false
	file, err := os.Open(path)
	if err != nil {
		cleanup()
		return nil, nil, Session{}, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, Session{}, nil, err
	}
	location.session.Size, location.session.ModifiedAt = info.Size(), info.ModTime().UTC()
	return file, info, location.session, cleanup, nil
}

func (s *Service) releaseRemoteTransfer(ctx context.Context, remote *remoteMapRuntime, lease *operationlease.Lease, transferID string) error {
	renewed, err := s.leases.Renew(ctx, *lease, 5*time.Minute)
	if err != nil {
		return err
	}
	*lease = renewed
	return remote.driver.ReleaseMapTransfer(ctx, remote.target, mapOperation(*lease, transferID, "release"), transferID)
}

func mapOperation(lease operationlease.Lease, transferID, phase string) runtimedriver.Operation {
	expiresAt := lease.ExpiresAt.UTC()
	return runtimedriver.Operation{
		ID: transferID, Key: transferID + "-" + phase, LeaseID: lease.LeaseID,
		FencingToken: lease.FencingToken, LeaseExpiresAt: &expiresAt,
	}
}

func validateMapDescriptor(value runtimedriver.MapDescriptor, transferID string) error {
	if value.TransferID != transferID || value.Size <= 0 || value.Size > 256*1024*1024 || len(value.SHA256) != 64 || len(value.SourceSHA256) != 64 {
		return ErrRendererOutput
	}
	if _, err := hex.DecodeString(value.SHA256); err != nil {
		return ErrRendererOutput
	}
	if _, err := hex.DecodeString(value.SourceSHA256); err != nil {
		return ErrRendererOutput
	}
	return nil
}

func readRemoteTransfer(ctx context.Context, driver runtimedriver.MapDriver, target runtimedriver.Target, descriptor runtimedriver.MapDescriptor, destination string) error {
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	offset := int64(0)
	for offset < descriptor.Size {
		chunk, readErr := driver.ReadMapTransfer(ctx, target, descriptor.TransferID, offset)
		if readErr != nil {
			_ = output.Close()
			return readErr
		}
		if chunk.TransferID != descriptor.TransferID || chunk.Size != descriptor.Size || chunk.SHA256 != descriptor.SHA256 || chunk.SourceSHA256 != descriptor.SourceSHA256 ||
			chunk.Offset != offset || chunk.NextOffset <= offset || chunk.NextOffset > descriptor.Size || int64(len(chunk.Data)) != chunk.NextOffset-offset || len(chunk.Data) > shared.MaxChunkBytes || chunk.Complete != (chunk.NextOffset == descriptor.Size) {
			_ = output.Close()
			return ErrRendererOutput
		}
		if _, err := io.MultiWriter(output, hash).Write(chunk.Data); err != nil {
			_ = output.Close()
			return err
		}
		offset = chunk.NextOffset
	}
	if err := output.Close(); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != descriptor.SHA256 {
		return ErrRendererOutput
	}
	return nil
}

func extractArtifactArchive(archivePath, destination string) error {
	archiveInfo, err := os.Stat(archivePath)
	if err != nil {
		return err
	}
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return errors.Join(ErrRendererOutput, err)
	}
	defer reader.Close()
	expected := map[string]int64{
		maprenderer.TerrainFileName: maxLayerImageSize, maprenderer.IconsFileName: maxLayerImageSize,
		maprenderer.ManifestFileName: maxManifestSize, maprenderer.FeaturesFileName: maxFeaturesSize,
	}
	if archiveInfo.Size() <= 0 || len(reader.File) != len(expected) {
		return ErrRendererOutput
	}
	seen := make(map[string]bool, len(expected))
	for _, entry := range reader.File {
		limit, ok := expected[entry.Name]
		if !ok || seen[entry.Name] || entry.FileInfo().IsDir() || entry.Mode()&os.ModeSymlink != 0 || int64(entry.UncompressedSize64) <= 0 || int64(entry.UncompressedSize64) > limit {
			return ErrRendererOutput
		}
		seen[entry.Name] = true
		input, err := entry.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(filepath.Join(destination, entry.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = input.Close()
			return err
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, limit+1))
		closeErr := errors.Join(input.Close(), output.Close())
		if copyErr != nil || closeErr != nil || written != int64(entry.UncompressedSize64) || written > limit {
			return errors.Join(ErrRendererOutput, copyErr, closeErr)
		}
	}
	return nil
}

func (s *Service) OpenImage(id string, layer Layer) (*os.File, os.FileInfo, Map, error) {
	value, err := s.store.Get(id)
	if err != nil {
		return nil, nil, Map{}, err
	}
	if value.Status != "succeeded" || (layer != LayerIcons && !containsLayer(value.Layers, layer)) {
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

func (s *Service) OpenArtifact(id string, artifact Artifact) (*os.File, os.FileInfo, Map, error) {
	value, err := s.store.Get(id)
	if err != nil {
		return nil, nil, Map{}, err
	}
	if value.Status != "succeeded" {
		return nil, nil, Map{}, ErrMapImageNotFound
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, nil, Map{}, ErrMapImageNotFound
	}
	name := map[Artifact]string{
		ArtifactManifest: maprenderer.ManifestFileName,
		ArtifactFeatures: maprenderer.FeaturesFileName,
	}[artifact]
	if name == "" {
		return nil, nil, Map{}, ErrMapImageNotFound
	}
	path := filepath.Join(s.config.MapRoot, id, name)
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
	sanitizedError := sanitizeRendererText(generationErr.Error(), s.config.SaveRoot, s.config.MapRoot)
	_, storeErr := s.store.Complete(value.ID, "failed", stage, logText, sanitizedError, value)
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

func validateRendererArtifacts(root, sourceSHA256 string) (maprenderer.Manifest, error) {
	expected := map[string]int64{
		maprenderer.TerrainFileName:  maxLayerImageSize,
		maprenderer.IconsFileName:    maxLayerImageSize,
		maprenderer.ManifestFileName: maxManifestSize,
		maprenderer.FeaturesFileName: maxFeaturesSize,
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return maprenderer.Manifest{}, err
	}
	if len(entries) != len(expected) {
		return maprenderer.Manifest{}, fmt.Errorf("%w: renderer must emit exactly four v1 artifacts", ErrRendererOutput)
	}
	for _, entry := range entries {
		limit, ok := expected[entry.Name()]
		if !ok || entry.IsDir() {
			return maprenderer.Manifest{}, fmt.Errorf("%w: unexpected renderer artifact %q", ErrRendererOutput, entry.Name())
		}
		info, statErr := os.Lstat(filepath.Join(root, entry.Name()))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > limit {
			return maprenderer.Manifest{}, fmt.Errorf("%w: artifact %q is missing, unsafe, or too large", ErrRendererOutput, entry.Name())
		}
	}
	var manifest maprenderer.Manifest
	if err := decodeArtifactJSON(filepath.Join(root, maprenderer.ManifestFileName), maxManifestSize, &manifest); err != nil {
		return maprenderer.Manifest{}, fmt.Errorf("%w: invalid manifest: %v", ErrRendererOutput, err)
	}
	if manifest.ProtocolVersion != maprenderer.ProtocolVersion || manifest.RendererVersion == "" || manifest.GeneratedAt.IsZero() {
		return maprenderer.Manifest{}, fmt.Errorf("%w: manifest protocol metadata is invalid", ErrRendererOutput)
	}
	if manifest.SourceSHA256 != sourceSHA256 {
		return maprenderer.Manifest{}, fmt.Errorf("%w: manifest source hash does not match the immutable snapshot", ErrRendererOutput)
	}
	if manifest.Map.ImageWidth <= 0 || manifest.Map.ImageHeight <= 0 || manifest.Map.ImageWidth > maxMapDimension || manifest.Map.ImageHeight > maxMapDimension || manifest.Map.TileWidth <= 0 || manifest.Map.TileHeight <= 0 || manifest.Map.PixelsPerTile <= 0 || manifest.Map.WorldUnitsPerTile <= 0 {
		return maprenderer.Manifest{}, fmt.Errorf("%w: manifest map dimensions are invalid", ErrRendererOutput)
	}
	if !manifestHasLayer(manifest, "terrain", "raster", maprenderer.TerrainFileName, "image/png") || !manifestHasLayer(manifest, "icons", "sprite", maprenderer.IconsFileName, "image/png") || !manifestHasLayer(manifest, "features", "vector", maprenderer.FeaturesFileName, "application/json") {
		return maprenderer.Manifest{}, fmt.Errorf("%w: manifest layer descriptors are incomplete", ErrRendererOutput)
	}
	terrain, err := os.Open(filepath.Join(root, maprenderer.TerrainFileName))
	if err != nil {
		return maprenderer.Manifest{}, err
	}
	imageConfig, decodeErr := png.DecodeConfig(io.LimitReader(terrain, maxLayerImageSize))
	_ = terrain.Close()
	if decodeErr != nil || imageConfig.Width != manifest.Map.ImageWidth || imageConfig.Height != manifest.Map.ImageHeight {
		return maprenderer.Manifest{}, fmt.Errorf("%w: terrain PNG does not match the manifest", ErrRendererOutput)
	}
	icons, err := os.Open(filepath.Join(root, maprenderer.IconsFileName))
	if err != nil {
		return maprenderer.Manifest{}, err
	}
	iconsConfig, iconsDecodeErr := png.DecodeConfig(io.LimitReader(icons, maxLayerImageSize))
	_ = icons.Close()
	if iconsDecodeErr != nil || iconsConfig.Width <= 0 || iconsConfig.Height <= 0 || iconsConfig.Width > maxMapDimension || iconsConfig.Height > maxMapDimension {
		return maprenderer.Manifest{}, fmt.Errorf("%w: icon sprite PNG is invalid", ErrRendererOutput)
	}
	var features maprenderer.FeatureCollection
	if err := decodeArtifactJSON(filepath.Join(root, maprenderer.FeaturesFileName), maxFeaturesSize, &features); err != nil {
		return maprenderer.Manifest{}, fmt.Errorf("%w: invalid features: %v", ErrRendererOutput, err)
	}
	if features.ProtocolVersion != maprenderer.ProtocolVersion || len(features.Features) != manifest.Statistics.FeatureCount {
		return maprenderer.Manifest{}, fmt.Errorf("%w: feature collection metadata does not match the manifest", ErrRendererOutput)
	}
	if manifest.Statistics.IconFeatureCount < 0 || manifest.Statistics.IconFeatureCount > manifest.Statistics.FeatureCount {
		return maprenderer.Manifest{}, fmt.Errorf("%w: icon statistics are invalid", ErrRendererOutput)
	}
	for _, feature := range features.Features {
		if feature.ID == "" || feature.Prefab == "" || feature.Category == "" || !finite(feature.X) || !finite(feature.Z) || !finite(feature.PixelX) || !finite(feature.PixelY) {
			return maprenderer.Manifest{}, fmt.Errorf("%w: feature collection contains an invalid feature", ErrRendererOutput)
		}
		if feature.Icon != nil && (feature.Icon.X < 0 || feature.Icon.Y < 0 || feature.Icon.Width <= 0 || feature.Icon.Height <= 0 || feature.Icon.X+feature.Icon.Width > iconsConfig.Width || feature.Icon.Y+feature.Icon.Height > iconsConfig.Height) {
			return maprenderer.Manifest{}, fmt.Errorf("%w: feature collection contains an invalid icon reference", ErrRendererOutput)
		}
	}
	return manifest, nil
}

func decodeArtifactJSON(path string, limit int64, target interface{}) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, limit+1))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON artifact contains trailing data")
	}
	return nil
}

func sanitizeRendererText(value string, paths ...string) string {
	value = rendererANSIPattern.ReplaceAllString(value, "")
	cleanedPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path != "" {
			cleanedPaths = append(cleanedPaths, filepath.Clean(path))
		}
	}
	sort.Slice(cleanedPaths, func(i, j int) bool { return len(cleanedPaths[i]) > len(cleanedPaths[j]) })
	for _, path := range cleanedPaths {
		value = strings.ReplaceAll(value, path, "[REDACTED_PATH]")
	}
	value = rendererSecretPattern.ReplaceAllString(value, "$1=[REDACTED]")
	value = strings.Map(func(character rune) rune {
		switch character {
		case '\n', '\r', '\t':
			return character
		}
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, value)
	return strings.TrimSpace(value)
}

func manifestHasLayer(manifest maprenderer.Manifest, id, kind, fileName, mimeType string) bool {
	for _, layer := range manifest.Layers {
		if layer.ID == id && layer.Kind == kind && layer.File == fileName && layer.MimeType == mimeType {
			return true
		}
	}
	return false
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func copySessionSnapshot(ctx context.Context, sourcePath, targetPath string) (string, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("open Session for snapshot: %w", err)
	}
	defer source.Close()
	before, err := source.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maxSnapshotSize {
		return "", errors.New("Session snapshot source is invalid or exceeds 128 MiB")
	}
	target, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", fmt.Errorf("create Session snapshot: %w", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(target, hash), &contextReader{ctx: ctx, reader: io.LimitReader(source, maxSnapshotSize+1)})
	closeErr := target.Close()
	if copyErr != nil {
		_ = os.Remove(targetPath)
		return "", fmt.Errorf("copy Session snapshot: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(targetPath)
		return "", fmt.Errorf("close Session snapshot: %w", closeErr)
	}
	after, err := source.Stat()
	if err != nil || written != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		_ = os.Remove(targetPath)
		return "", errors.New("Session changed while its snapshot was being copied; retry generation")
	}
	if err := os.Chmod(targetPath, 0440); err != nil {
		_ = os.Remove(targetPath)
		return "", fmt.Errorf("make Session snapshot read-only: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(value []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(value)
}

func normalizeLayers(input []Layer) ([]Layer, error) {
	allowed := map[Layer]bool{
		LayerTerrain: true, LayerFeatures: true, LayerWorldState: true,
	}
	for _, layer := range input {
		if !allowed[layer] {
			return nil, ErrInvalidLayers
		}
	}
	return []Layer{LayerTerrain, LayerFeatures, LayerWorldState}, nil
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
