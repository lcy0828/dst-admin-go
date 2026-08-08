package gameupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	backupapi "dont/internal/backups"
	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/shards"
)

var (
	ErrConfirmationNeeded  = errors.New("game update confirmation does not match")
	ErrUpdateInProgress    = errors.New("a game update is already active")
	ErrSteamCMDUnavailable = errors.New("steamcmd is unavailable")
	ErrUnsafeCachePath     = errors.New("steam cache path is unsafe")
)

type RoomCatalog interface {
	List() ([]rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}

type BackupCreator interface {
	Create(context.Context, string, string, backupapi.Kind, string) (backupapi.Backup, error)
}

type CommandRunner interface {
	Run(context.Context, string, []string, io.Writer) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, arguments []string, output io.Writer) error {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Stdout = output
	command.Stderr = output
	return command.Run()
}

type Config struct {
	ServerPath   string
	SteamCMDPath string
}

type plannedWorld struct {
	roomID    string
	roomName  string
	worldID   string
	worldName string
	isMaster  bool
}

type Service struct {
	config       Config
	rooms        RoomCatalog
	control      shards.Control
	backups      BackupCreator
	store        *Store
	runner       CommandRunner
	latest       LatestChecker
	now          func() time.Time
	pollInterval time.Duration
	stopTimeout  time.Duration
	startTimeout time.Duration
	mu           sync.Mutex
	active       bool
}

func NewService(config Config, roomCatalog RoomCatalog, control shards.Control, backups BackupCreator, store *Store, runner CommandRunner, latest LatestChecker) (*Service, error) {
	if roomCatalog == nil || control == nil || backups == nil || store == nil || runner == nil || latest == nil {
		return nil, errors.New("rooms, control, backups, store, runner, and latest checker are required")
	}
	config.ServerPath = filepath.Clean(strings.TrimSpace(config.ServerPath))
	config.SteamCMDPath = filepath.Clean(strings.TrimSpace(config.SteamCMDPath))
	return &Service{
		config: config, rooms: roomCatalog, control: control, backups: backups, store: store, runner: runner, latest: latest,
		now: time.Now, pollInterval: 500 * time.Millisecond, stopTimeout: 60 * time.Second, startTimeout: 20 * time.Second,
	}, nil
}

func (s *Service) Version(ctx context.Context) VersionReport {
	local, installed := readLocalVersion(s.config.ServerPath)
	executable := findSteamCMD(s.config.SteamCMDPath)
	report := VersionReport{
		Installed: installed, LocalVersion: local, InstallPath: installRoot(s.config.ServerPath),
		SteamCMDAvailable: executable != "", SteamCMDPath: executable, CheckedAt: s.now().UTC(),
	}
	queryVersion := local
	if queryVersion == "" {
		queryVersion = "0"
	}
	latest, upToDate, err := s.latest.Check(ctx, queryVersion)
	if err != nil {
		report.CheckError = err.Error()
		return report
	}
	report.LatestVersion = latest
	if report.LatestVersion == "" && upToDate {
		report.LatestVersion = local
	}
	report.UpToDate = &upToDate
	return report
}

func (s *Service) Run(jobID string) (Run, error) { return s.store.Get(jobID) }

func (s *Service) Prepare(ctx context.Context, request UpdateRequest) ([]jobs.TargetSpec, func(jobs.Job) jobs.Runner, func(), error) {
	if request.Confirmation != "更新游戏" {
		return nil, nil, nil, ErrConfirmationNeeded
	}
	executable := findSteamCMD(s.config.SteamCMDPath)
	if executable == "" {
		return nil, nil, nil, ErrSteamCMDUnavailable
	}
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return nil, nil, nil, ErrUpdateInProgress
	}
	s.active = true
	s.mu.Unlock()
	release := sync.OnceFunc(func() {
		s.mu.Lock()
		s.active = false
		s.mu.Unlock()
	})

	managed, running, err := s.captureState(ctx)
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	targets := make([]jobs.TargetSpec, 0, len(managed)+len(running)*2+1)
	for _, room := range managed {
		targets = append(targets, jobs.TargetSpec{ID: "protect:" + room.ID, Name: "保护备份 · " + room.Name})
	}
	for _, world := range running {
		targets = append(targets, jobs.TargetSpec{ID: stopTargetID(world), Name: "停止 · " + world.roomName + " / " + world.worldName})
	}
	targets = append(targets, jobs.TargetSpec{ID: "steamcmd", Name: "更新 DST 服务端"})
	if request.RestartRunning {
		for _, world := range running {
			targets = append(targets, jobs.TargetSpec{ID: startTargetID(world), Name: "恢复运行 · " + world.roomName + " / " + world.worldName})
		}
	}
	factory := func(job jobs.Job) jobs.Runner {
		return func(runContext context.Context, report func(jobs.TargetResult)) error {
			defer release()
			return s.execute(runContext, job.ID, request, executable, managed, running, report)
		}
	}
	return targets, factory, release, nil
}

func (s *Service) execute(ctx context.Context, jobID string, request UpdateRequest, executable string, managed []rooms.Room, running []plannedWorld, report func(jobs.TargetResult)) error {
	for _, room := range managed {
		value, err := s.backups.Create(ctx, room.ID, "游戏更新前保护备份 "+s.now().Format("2006-01-02 15:04:05"), backupapi.KindProtection, jobID)
		if err != nil {
			report(jobs.TargetResult{TargetID: "protect:" + room.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "PROTECTION_BACKUP_FAILED", Message: err.Error()}})
			return fmt.Errorf("create protection backup for %s: %w", room.Name, err)
		}
		report(jobs.TargetResult{TargetID: "protect:" + room.ID, Status: jobs.StatusSucceeded, Message: value.Name})
	}

	stopped := make([]plannedWorld, 0, len(running))
	for _, world := range stopOrder(running) {
		if err := s.setRunning(ctx, world, false); err != nil {
			report(jobs.TargetResult{TargetID: stopTargetID(world), Status: jobs.StatusFailed, Error: &jobs.Error{Code: "STOP_FAILED", Message: err.Error()}})
			s.recoverStopped(ctx, stopped)
			return fmt.Errorf("stop %s/%s: %w", world.roomName, world.worldName, err)
		}
		stopped = append(stopped, world)
		report(jobs.TargetResult{TargetID: stopTargetID(world), Status: jobs.StatusSucceeded, Message: "分片已停止"})
	}

	updateErr := s.runSteamCMD(ctx, jobID, request.CleanCache, executable)
	if updateErr != nil {
		report(jobs.TargetResult{TargetID: "steamcmd", Status: jobs.StatusFailed, Error: &jobs.Error{Code: "GAME_UPDATE_FAILED", Message: updateErr.Error()}})
	} else {
		report(jobs.TargetResult{TargetID: "steamcmd", Status: jobs.StatusSucceeded, Message: "DST 服务端更新完成"})
	}

	var restartErrors []error
	if request.RestartRunning {
		for _, world := range startOrder(running) {
			if err := s.setRunning(ctx, world, true); err != nil {
				report(jobs.TargetResult{TargetID: startTargetID(world), Status: jobs.StatusFailed, Error: &jobs.Error{Code: "RESTART_FAILED", Message: err.Error()}})
				restartErrors = append(restartErrors, fmt.Errorf("restart %s/%s: %w", world.roomName, world.worldName, err))
				continue
			}
			report(jobs.TargetResult{TargetID: startTargetID(world), Status: jobs.StatusSucceeded, Message: "分片已恢复运行"})
		}
	}
	return errors.Join(append([]error{updateErr}, restartErrors...)...)
}

func (s *Service) runSteamCMD(ctx context.Context, jobID string, cleanCache bool, executable string) error {
	before, _ := readLocalVersion(s.config.ServerPath)
	if _, err := s.store.Begin(jobID, before, cleanCache); err != nil {
		return err
	}
	logBuffer := &boundedBuffer{limit: 2 * 1024 * 1024}
	if cleanCache {
		if err := cleanSteamCache(executable, installRoot(s.config.ServerPath)); err != nil {
			_, _ = s.store.Complete(jobID, before, logBuffer.String(), err)
			return err
		}
	}
	arguments := []string{"+force_install_dir", installRoot(s.config.ServerPath), "+login", "anonymous", "+app_update", "343050", "validate", "+quit"}
	runErr := s.runner.Run(ctx, executable, arguments, logBuffer)
	after, _ := readLocalVersion(s.config.ServerPath)
	if _, err := s.store.Complete(jobID, after, logBuffer.String(), runErr); err != nil {
		return err
	}
	return runErr
}

func (s *Service) captureState(ctx context.Context) ([]rooms.Room, []plannedWorld, error) {
	items, err := s.rooms.List()
	if err != nil {
		return nil, nil, err
	}
	managed := make([]rooms.Room, 0)
	running := make([]plannedWorld, 0)
	for _, room := range items {
		if !room.Managed {
			continue
		}
		managed = append(managed, room)
		worlds, err := s.rooms.Worlds(room.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, world := range worlds {
			active, statusErr := s.control.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
			if statusErr != nil {
				return nil, nil, statusErr
			}
			if active {
				running = append(running, plannedWorld{roomID: room.ID, roomName: room.DirectoryName, worldID: world.ID, worldName: world.DirectoryName, isMaster: world.IsMaster})
			}
		}
	}
	return managed, running, nil
}

func (s *Service) setRunning(ctx context.Context, world plannedWorld, expected bool) error {
	current, err := s.control.IsRunning(ctx, world.roomName, world.worldName)
	if err != nil {
		return err
	}
	if current == expected {
		return nil
	}
	if expected {
		err = s.control.Start(ctx, world.roomName, world.worldName)
	} else {
		err = s.control.Stop(ctx, world.roomName, world.worldName)
	}
	if err != nil {
		return err
	}
	timeout := s.stopTimeout
	if expected {
		timeout = s.startTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("等待分片状态变化超时")
		case <-ticker.C:
			current, err := s.control.IsRunning(ctx, world.roomName, world.worldName)
			if err != nil {
				return err
			}
			if current == expected {
				return nil
			}
		}
	}
}

func (s *Service) recoverStopped(ctx context.Context, stopped []plannedWorld) {
	for _, world := range startOrder(stopped) {
		_ = s.setRunning(ctx, world, true)
	}
}

func stopOrder(input []plannedWorld) []plannedWorld {
	result := append([]plannedWorld(nil), input...)
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].isMaster != result[j].isMaster {
			return !result[i].isMaster
		}
		return result[i].roomName+result[i].worldName < result[j].roomName+result[j].worldName
	})
	return result
}

func startOrder(input []plannedWorld) []plannedWorld {
	result := append([]plannedWorld(nil), input...)
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].isMaster != result[j].isMaster {
			return result[i].isMaster
		}
		return result[i].roomName+result[i].worldName < result[j].roomName+result[j].worldName
	})
	return result
}

func stopTargetID(world plannedWorld) string  { return "stop:" + world.roomID + ":" + world.worldID }
func startTargetID(world plannedWorld) string { return "start:" + world.roomID + ":" + world.worldID }

func cleanSteamCache(executable, serverRoot string) error {
	roots := []string{filepath.Join(filepath.Dir(executable), "steamapps"), filepath.Join(serverRoot, "steamapps")}
	seen := make(map[string]bool)
	for _, root := range roots {
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		for _, relative := range []string{filepath.Join("downloading", "343050"), filepath.Join("temp", "343050"), "appmanifest_343050.acf"} {
			target := filepath.Join(root, relative)
			if !contained(root, target) {
				return ErrUnsafeCachePath
			}
			info, err := os.Lstat(target)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return ErrUnsafeCachePath
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
	}
	return nil
}

func contained(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
	cut    bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
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

func (b *boundedBuffer) String() string {
	if b.cut {
		return b.buffer.String() + "\n[DST Admin] 输出超过 2 MiB，已截断\n"
	}
	return b.buffer.String()
}
