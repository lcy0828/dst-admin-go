package softwareupdate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dont/internal/buildinfo"
	"github.com/shirou/gopsutil/v3/disk"
)

type Config struct {
	Kind               string
	Root               string
	Current            buildinfo.Info
	BaseVersion        string
	Platform           string
	Managed            bool
	Client             ReleaseClient
	Wake               func() error
	Ready              func() bool
	FreeSpace          func(context.Context, string) (uint64, error)
	ValidateExecutable func(context.Context, string, string, string) error
}

type Service struct {
	config    Config
	mu        sync.Mutex
	checkMu   sync.Mutex
	check     Check
	operation *Operation
	cancel    context.CancelFunc
	done      chan struct{}
	guard     func(context.Context) (func(), error)
	closed    bool
	paused    bool
}

func New(config Config) *Service {
	if config.Client == nil {
		if config.Kind == "agent" {
			config.Client = NewAgentGitHubClient(nil)
		} else {
			config.Client = NewGitHubClient(nil)
		}
	}
	if config.Wake == nil {
		config.Wake = wakeSupervisor
	}
	if config.Ready == nil {
		config.Ready = func() bool { return supervisorReady(config.Root) }
	}
	if config.FreeSpace == nil {
		config.FreeSpace = func(ctx context.Context, root string) (uint64, error) {
			usage, err := disk.UsageWithContext(ctx, root)
			if err != nil {
				return 0, err
			}
			return usage.Free, nil
		}
	}
	if config.ValidateExecutable == nil {
		config.ValidateExecutable = validateExecutable
		if config.Kind == "agent" {
			config.ValidateExecutable = validateAgentExecutable
		}
	}
	return &Service{config: config}
}

func (s *Service) SetRestartGuard(guard func(context.Context) (func(), error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.guard = guard
}

func (s *Service) supported() (bool, string) {
	if _, valid := NormalizeVersion(s.config.Current.Version); !valid {
		return false, "development_build"
	}
	if !s.config.Managed {
		return false, "launcher_unavailable"
	}
	if s.config.Kind == "agent" && AgentPlatform(s.config.Platform) {
		return true, ""
	}
	if s.config.Platform != "linux-amd64" && s.config.Platform != "darwin-arm64" {
		return false, "platform_unavailable"
	}
	return true, ""
}

func (s *Service) Snapshot() (Snapshot, error) {
	supported, reason := s.supported()
	result := Snapshot{Current: s.config.Current, Platform: s.config.Platform, Supported: supported, Ready: supported && s.config.Ready(), UnsupportedReason: reason}
	state, err := readState(s.config.Root)
	if err != nil {
		return result, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result.Check = s.check
	if s.check.Latest != nil {
		copy := *s.check.Latest
		result.Check.Latest = &copy
	}
	if s.operation != nil && (s.operation.Phase == "downloading" || s.operation.Phase == "verifying") {
		copy := *s.operation
		result.Operation = &copy
	} else if state.Operation != nil {
		copy := *state.Operation
		result.Operation = &copy
	}
	return result, nil
}

// PauseIfIdle reserves the updater during a configuration replacement. A
// verified package survives replacement, while an active download must finish.
func (s *Service) PauseIfIdle() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.paused || s.cancel != nil {
		return nil, ErrBusy
	}
	state, err := readState(s.config.Root)
	if err != nil {
		return nil, err
	}
	if state.Pending != nil || state.Operation != nil && state.Operation.Busy() {
		return nil, ErrBusy
	}
	s.paused = true
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); s.paused = false; s.mu.Unlock() }) }, nil
}

func (s *Service) Check(ctx context.Context, force bool, source string) (Snapshot, error) {
	if !ValidSource(source) {
		return Snapshot{}, ErrInvalid
	}
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	s.mu.Lock()
	last := s.check.CheckedAt
	if !last.IsZero() && (time.Since(last) < 5*time.Second || !force && time.Since(last) < 20*time.Minute) {
		s.check.Cached = true
		s.mu.Unlock()
		return s.Snapshot()
	}
	s.mu.Unlock()
	checkCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	release, err := s.config.Client.Latest(checkCtx, s.config.Platform, source)
	if err == nil && release == nil {
		err = ErrInvalid
	}
	s.mu.Lock()
	s.check.CheckedAt = time.Now().UTC()
	s.check.Cached = false
	if err != nil {
		s.check.Warning = err.Error()
		s.check.Cached = s.check.Latest != nil
	} else {
		s.check.Latest = release
		s.check.Warning = ""
		_, known := NormalizeVersion(s.config.Current.Version)
		s.check.HasUpdate = known && CompareVersions(release.Version, s.config.Current.Version) > 0
	}
	s.mu.Unlock()
	return s.Snapshot()
}

func newID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}

func (s *Service) Start(version, source string) (Operation, error) {
	return s.StartWithClient(version, source, s.config.Client)
}

// StartWithClient permits authenticated controller transfer without changing
// the client's source for concurrent version checks or later updates.
func (s *Service) StartWithClient(version, source string, client ReleaseClient) (Operation, error) {
	version, valid := NormalizeVersion(version)
	if !valid || !ValidSource(source) {
		return Operation{}, ErrInvalid
	}
	if supported, _ := s.supported(); !supported {
		return Operation{}, ErrUnsupported
	}
	if !s.config.Ready() {
		return Operation{}, ErrBusy
	}
	if CompareVersions(version, s.config.Current.Version) <= 0 {
		return Operation{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Operation{}, ErrUnsupported
	}
	if s.paused {
		return Operation{}, ErrBusy
	}
	state, err := readState(s.config.Root)
	if err != nil {
		return Operation{}, err
	}
	if s.cancel != nil || state.Pending != nil || state.Operation != nil && state.Operation.Busy() {
		return Operation{}, ErrBusy
	}
	if state.Operation != nil && state.Operation.Phase == "prepared" && state.Operation.Version == version {
		return *state.Operation, nil
	}
	// No download or parent switch is active. Retain committed, previous and
	// prepared code, and clean temporary files left by interrupted downloads.
	(&supervisor{root: s.config.Root}).prune(state)
	now := time.Now().UTC()
	operation := Operation{ID: newID(), Version: version, Phase: "downloading", StartedAt: now, UpdatedAt: now}
	state.Operation = &operation
	if err := writeState(s.config.Root, state); err != nil {
		return Operation{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	s.cancel = cancel
	s.done = make(chan struct{})
	s.operation = &operation
	go s.download(ctx, operation, source, client)
	return operation, nil
}

type progressWriter struct {
	service *Service
	writer  io.Writer
	id      string
	start   time.Time
	last    time.Time
	count   int64
	total   int64
}

func (w *progressWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.count += int64(n)
	now := time.Now()
	if now.Sub(w.last) >= 250*time.Millisecond || w.count == w.total {
		w.last = now
		w.service.mu.Lock()
		if op := w.service.operation; op != nil && op.ID == w.id {
			op.DownloadedBytes = w.count
			op.TotalBytes = w.total
			op.UpdatedAt = now.UTC()
			if elapsed := now.Sub(w.start).Seconds(); elapsed > 0 {
				op.BytesPerSecond = int64(float64(w.count) / elapsed)
			}
			if w.total > 0 {
				op.Progress = int(w.count * 80 / w.total)
			}
		}
		w.service.mu.Unlock()
	}
	return n, err
}

func (s *Service) phase(id, phase string, progress int, failure error, releaseID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := readState(s.config.Root)
	if err != nil {
		return err
	}
	if state.Operation == nil || state.Operation.ID != id || s.operation == nil || s.operation.ID != id {
		return ErrInvalid
	}
	op := *s.operation
	op.Phase = phase
	op.Progress = progress
	op.BytesPerSecond = 0
	op.ReleaseID = releaseID
	op.UpdatedAt = time.Now().UTC()
	if failure != nil {
		op.Error = failure.Error()
	}
	state.Operation = &op
	if err := writeState(s.config.Root, state); err != nil {
		return err
	}
	s.operation = &op
	return nil
}

func (s *Service) download(ctx context.Context, operation Operation, source string, client ReleaseClient) {
	var finalErr error
	defer func() {
		if finalErr != nil {
			_ = s.phase(operation.ID, "failed", 0, finalErr, "")
		}
		s.mu.Lock()
		cancel, done := s.cancel, s.done
		s.cancel = nil
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		close(done)
	}()
	finalErr = s.prepareWithClient(ctx, operation, source, client)
}

func (s *Service) prepare(ctx context.Context, operation Operation, source string) error {
	return s.prepareWithClient(ctx, operation, source, s.config.Client)
}

func (s *Service) prepareWithClient(ctx context.Context, operation Operation, source string, client ReleaseClient) error {
	if client == nil {
		return ErrInvalid
	}
	release, err := client.Latest(ctx, s.config.Platform, source)
	if err != nil {
		return err
	}
	if release == nil || release.Version != operation.Version {
		return ErrReleaseChanged
	}
	if !release.OnlineUpdate {
		return ErrUnsupported
	}
	available, err := s.config.FreeSpace(ctx, s.config.Root)
	if err != nil {
		return fmt.Errorf("inspect software update disk space: %w", err)
	}
	required := uint64(release.Archive.Size + MaxExtractedBytes + (256 << 20))
	if available < required {
		return fmt.Errorf("software update needs %d MiB free including temporary files and data headroom; available %d MiB", required>>20, available>>20)
	}
	checksumData, err := client.Checksum(ctx, release.Checksum, source)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(checksumData))
	if len(fields) != 2 || !digestPattern.MatchString(fields[0]) || strings.TrimPrefix(fields[1], "*") != release.Archive.Name {
		return fmt.Errorf("%w: invalid archive checksum", ErrInvalid)
	}
	if release.Archive.Digest != "" && release.Archive.Digest != "sha256:"+fields[0] {
		return fmt.Errorf("%w: GitHub asset digest mismatch", ErrInvalid)
	}
	archive, err := os.CreateTemp(s.config.Root, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	hash := sha256.New()
	writer := &progressWriter{service: s, writer: io.MultiWriter(archive, hash), id: operation.ID, start: time.Now(), total: release.Archive.Size}
	err = client.Download(ctx, release.Archive, source, writer)
	if err == nil {
		err = archive.Sync()
	}
	closeErr := archive.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if writer.count != release.Archive.Size || hex.EncodeToString(hash.Sum(nil)) != fields[0] {
		return fmt.Errorf("%w: downloaded archive checksum mismatch", ErrInvalid)
	}
	if err := s.phase(operation.ID, "verifying", 85, nil, ""); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Join(s.config.Root, "releases"), ".stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := extractBundleFor(archive.Name(), stage, s.config.Kind, s.config.Platform); err != nil {
		return err
	}
	if _, err := validateBundleFor(stage, operation.Version, s.config.Platform, s.config.Kind); err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.config.ValidateExecutable(probeCtx, filepath.Join(stage, binaryFor(s.config.Kind, s.config.Platform)), operation.Version, s.config.Platform); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	releaseID := newID()
	directory := filepath.Join(s.config.Root, "releases", releaseID)
	if err := os.Rename(stage, directory); err != nil {
		return err
	}
	if err := s.phase(operation.ID, "prepared", 100, nil, releaseID); err != nil {
		_ = os.RemoveAll(directory)
		return err
	}
	return nil
}

// PrepareApply closes write admission and pauses idle job executors before a
// supervisor restart. The HTTP response is sent before NotifyApply is called.
func (s *Service) PrepareApply(ctx context.Context, id string) (Operation, func(), error) {
	if supported, _ := s.supported(); !supported {
		return Operation{}, nil, ErrUnsupported
	}
	if !s.config.Ready() {
		return Operation{}, nil, ErrBusy
	}
	s.mu.Lock()
	guard := s.guard
	downloading := s.cancel != nil || s.paused || s.closed
	s.mu.Unlock()
	if guard == nil {
		return Operation{}, nil, ErrUnsupported
	}
	if downloading {
		return Operation{}, nil, ErrBusy
	}
	resume, err := guard(ctx)
	if err != nil {
		return Operation{}, nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			resume()
		}
	}()
	state, err := readState(s.config.Root)
	if err != nil {
		return Operation{}, nil, err
	}
	if state.Pending != nil {
		return Operation{}, nil, ErrBusy
	}
	if state.Operation == nil || state.Operation.ID != id || state.Operation.Phase != "prepared" || CompareVersions(state.Operation.Version, s.config.Current.Version) < 0 {
		return Operation{}, nil, ErrNotPrepared
	}
	operation := *state.Operation
	target := &installedRelease{ID: operation.ReleaseID, Version: operation.Version}
	operation.Phase = "restarting"
	operation.UpdatedAt = time.Now().UTC()
	if target.ID != "" {
		directory, err := releaseDirectory(s.config.Root, target.ID)
		if err != nil {
			return Operation{}, nil, err
		}
		if _, err := validateBundleFor(directory, target.Version, s.config.Platform, s.config.Kind); err != nil {
			return Operation{}, nil, err
		}
	}
	state.Pending = target
	state.Operation = &operation
	if err := writeState(s.config.Root, state); err != nil {
		return Operation{}, nil, err
	}
	succeeded = true
	return operation, resume, nil
}

func (s *Service) NotifyApply(resume func()) {
	go func() {
		time.Sleep(400 * time.Millisecond)
		if err := s.config.Wake(); err != nil {
			state, readErr := readState(s.config.Root)
			if readErr == nil && state.Operation != nil {
				state.Pending = nil
				state.Operation.Phase = "failed"
				state.Operation.Error = err.Error()
				state.Operation.UpdatedAt = time.Now().UTC()
				_ = writeState(s.config.Root, state)
			}
			resume()
		}
	}()
}

func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
