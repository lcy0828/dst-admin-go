package gameupdate

import (
	"bytes"
	"context"
	"dont/internal/installationlock"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	backupapi "dont/internal/backups"
	dstinstall "dont/internal/dstserver"
	"dont/internal/jobs"
	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/runtimeaudit"
	"dont/internal/shards"
	"dont/shared"
)

var (
	ErrConfirmationNeeded  = errors.New("game update confirmation does not match")
	ErrUpdateInProgress    = errors.New("a game update is already active")
	ErrSteamCMDUnavailable = errors.New("steamcmd is unavailable")
	ErrSteamClientManaged  = errors.New("game installation is managed by the Steam client")
	ErrUpdateDisabled      = errors.New("local game update is disabled for this deployment")
	ErrUnsafeCachePath     = errors.New("steam cache path is unsafe")
	ErrRoomStateChanged    = errors.New("room runtime state changed while the game update was queued")
)

const (
	defaultVersionSteamCheckTimeout    = 30 * time.Second
	defaultVersionOfficialCheckTimeout = 6 * time.Second
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

type LifecycleNotifier interface {
	BeforeOperations(context.Context, []string, string, string, string) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, arguments []string, output io.Writer) error {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Stdout = output
	command.Stderr = output
	return command.Run()
}

type Config struct {
	ServerPath             string
	SteamCMDPath           string
	AppID                  string
	UpdateMethod           string
	DisableUpdate          bool
	OfficialReleaseChecker OfficialReleaseChecker
}

type plannedWorld struct {
	runtimeMode shared.RuntimePerformanceMode
	roomID      string
	roomName    string
	worldID     string
	worldName   string
	isMaster    bool
}

type Service struct {
	config               Config
	rooms                RoomCatalog
	control              shards.Control
	backups              BackupCreator
	store                *Store
	runner               CommandRunner
	latest               LatestChecker
	official             OfficialReleaseChecker
	now                  func() time.Time
	pollInterval         time.Duration
	stopTimeout          time.Duration
	startTimeout         time.Duration
	steamCheckTimeout    time.Duration
	officialCheckTimeout time.Duration
	mu                   sync.Mutex
	active               bool
	audit                interface {
		RecordAction(runtimeaudit.ActionRequest) error
	}
	notifier LifecycleNotifier
}

func (s *Service) ConfigureNotifier(notifier LifecycleNotifier) {
	s.notifier = notifier
}

func NewService(config Config, roomCatalog RoomCatalog, control shards.Control, backups BackupCreator, store *Store, runner CommandRunner, latest LatestChecker, audits ...interface {
	RecordAction(runtimeaudit.ActionRequest) error
}) (*Service, error) {
	if roomCatalog == nil || control == nil || backups == nil || store == nil || runner == nil || latest == nil {
		return nil, errors.New("rooms, control, backups, store, runner, and latest checker are required")
	}
	config.ServerPath = filepath.Clean(strings.TrimSpace(config.ServerPath))
	config.SteamCMDPath = filepath.Clean(strings.TrimSpace(config.SteamCMDPath))
	config.AppID = strings.TrimSpace(config.AppID)
	config.UpdateMethod = strings.TrimSpace(config.UpdateMethod)
	if layout, ok := dstinstall.Resolve(config.ServerPath, "64"); ok {
		if config.AppID == "" {
			config.AppID = layout.AppID
		}
		if config.UpdateMethod == "" {
			config.UpdateMethod = layout.UpdateMethod
		}
	}
	if config.AppID == "" {
		config.AppID = dstinstall.AppIDDedicatedServer
	}
	if config.UpdateMethod == "" {
		config.UpdateMethod = dstinstall.UpdateMethodSteamCMD
	}
	if config.UpdateMethod != dstinstall.UpdateMethodSteamCMD && config.UpdateMethod != dstinstall.UpdateMethodSteamClient {
		return nil, fmt.Errorf("unsupported game update method %q", config.UpdateMethod)
	}
	service := &Service{
		config: config, rooms: roomCatalog, control: control, backups: backups, store: store, runner: runner, latest: latest,
		official: config.OfficialReleaseChecker,
		now:      time.Now, pollInterval: 500 * time.Millisecond, stopTimeout: 60 * time.Second, startTimeout: 20 * time.Second,
		steamCheckTimeout: defaultVersionSteamCheckTimeout, officialCheckTimeout: defaultVersionOfficialCheckTimeout,
	}
	if len(audits) > 0 {
		service.audit = audits[0]
	}
	return service, nil
}

func (s *Service) localVersionReport() VersionReport {
	local, installed := readLocalVersion(s.config.ServerPath, s.config.AppID)
	executable := findSteamCMD(s.config.SteamCMDPath)
	report := VersionReport{
		Installed: installed, AppID: s.config.AppID, LocalVersion: local, InstallPath: installRoot(s.config.ServerPath),
		UpdateMethod: s.config.UpdateMethod, UpdateSupported: !s.config.DisableUpdate && s.config.UpdateMethod == dstinstall.UpdateMethodSteamCMD && executable != "",
		SteamCMDAvailable: executable != "", SteamCMDPath: executable, CheckedAt: s.now().UTC(),
	}
	if s.config.DisableUpdate {
		report.UpdateBlockedReason = "local_runtime_not_managed"
	}
	report.GameVersion, _ = readLocalGameVersion(s.config.ServerPath)
	report.Branch = dstinstall.SteamBranch(steamManifestCandidates(installRoot(s.config.ServerPath), s.config.AppID)...)
	return report
}

func (s *Service) VersionSummary(ctx context.Context, refresh bool) VersionReport {
	report := s.localVersionReport()
	release, err := s.OfficialVersion(ctx, refresh)
	if release.Version != "" {
		report.OfficialRelease = &release
	}
	if err != nil {
		report.OfficialCheckError = err.Error()
	}
	if err == nil && !release.Stale && report.Installed && report.Branch == "public" && releaseVersionPattern.MatchString(report.GameVersion) && releaseVersionPattern.MatchString(release.Version) && !newerGameVersion(report.GameVersion, release.Version) {
		current := report.GameVersion == release.Version
		report.UpToDate = &current
		if current {
			report.LatestVersion = report.LocalVersion
		}
	}
	return report
}

func (s *Service) Version(ctx context.Context) VersionReport {
	report := s.localVersionReport()
	local := report.LocalVersion
	queryVersion := local
	if queryVersion == "" {
		queryVersion = "0"
	}
	type steamResult struct {
		latest   string
		upToDate bool
		err      error
	}
	steamResults := make(chan steamResult, 1)
	steamTimeout := s.steamCheckTimeout
	if steamTimeout <= 0 {
		steamTimeout = defaultVersionSteamCheckTimeout
	}
	steamContext, cancelSteam := context.WithTimeout(ctx, steamTimeout)
	defer cancelSteam()
	go func() {
		latest, upToDate, err := s.latest.Check(steamContext, s.config.AppID, queryVersion)
		steamResults <- steamResult{latest: latest, upToDate: upToDate, err: err}
	}()

	type officialResult struct {
		release OfficialRelease
		err     error
	}
	var officialResults chan officialResult
	var officialContext context.Context
	var cancelOfficial context.CancelFunc
	if s.official != nil {
		officialTimeout := s.officialCheckTimeout
		if officialTimeout <= 0 {
			officialTimeout = defaultVersionOfficialCheckTimeout
		}
		officialContext, cancelOfficial = context.WithTimeout(ctx, officialTimeout)
		defer cancelOfficial()
		officialResults = make(chan officialResult, 1)
		go func() {
			release, err := s.official.Check(officialContext)
			officialResults <- officialResult{release: release, err: err}
		}()
	}

	var steam steamResult
	select {
	case steam = <-steamResults:
	case <-steamContext.Done():
		steam.err = steamContext.Err()
	}
	if steam.err != nil {
		report.CheckError = steam.err.Error()
	} else {
		report.LatestVersion = steam.latest
		if report.LatestVersion == "" && steam.upToDate {
			report.LatestVersion = local
		}
		report.UpToDate = &steam.upToDate
	}
	if officialResults != nil {
		var official officialResult
		select {
		case official = <-officialResults:
		case <-officialContext.Done():
			official.err = officialContext.Err()
		}
		if official.release.Version != "" {
			release := official.release
			report.OfficialRelease = &release
		}
		if official.err != nil {
			report.OfficialCheckError = official.err.Error()
		}
	}
	return report
}

func (s *Service) Run(jobID string) (Run, error) { return s.store.Get(jobID) }

func (s *Service) Prepare(ctx context.Context, request UpdateRequest) ([]jobs.TargetSpec, func(jobs.Job) jobs.Runner, func(), error) {
	if s.config.DisableUpdate {
		return nil, nil, nil, ErrUpdateDisabled
	}
	if s.config.UpdateMethod == dstinstall.UpdateMethodSteamClient {
		return nil, nil, nil, ErrSteamClientManaged
	}
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
	roomIDs := make([]string, 0, len(managed))
	for _, room := range managed {
		roomIDs = append(roomIDs, room.ID)
	}
	ctx, releaseRooms, err := roomops.AcquireMany(ctx, roomIDs)
	if err != nil {
		return err
	}
	defer releaseRooms()
	if s.notifier != nil && len(running) > 0 {
		action := string(shards.ActionStop)
		if request.RestartRunning {
			action = string(shards.ActionRestart)
		}
		roomIDs := make([]string, 0, len(running))
		seenRooms := make(map[string]bool, len(running))
		for _, world := range running {
			if !seenRooms[world.roomID] {
				seenRooms[world.roomID] = true
				roomIDs = append(roomIDs, world.roomID)
			}
		}
		if err := s.notifier.BeforeOperations(ctx, roomIDs, action, string(runtimeaudit.SourceGameUpdate), jobID); err != nil {
			return err
		}
	}
	currentManaged, currentRunning, err := s.captureState(ctx)
	if err != nil {
		return err
	}
	if !sameUpdatePlan(managed, running, currentManaged, currentRunning) {
		s.reportPlanChanged(request, managed, running, report)
		return ErrRoomStateChanged
	}
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
		if s.audit != nil {
			if err := s.audit.RecordAction(runtimeaudit.ActionRequest{
				RoomID: world.roomID, WorldIDs: []string{world.worldID}, Action: string(shards.ActionStop),
				Source: runtimeaudit.SourceGameUpdate, JobID: jobID,
			}); err != nil {
				log.Printf("[RuntimeAudit] record game-update stop room=%s world=%s: %v", world.roomID, world.worldID, err)
			}
		}
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
			if s.audit != nil {
				if err := s.audit.RecordAction(runtimeaudit.ActionRequest{
					RoomID: world.roomID, WorldIDs: []string{world.worldID}, Action: string(shards.ActionStart),
					Source: runtimeaudit.SourceGameUpdate, JobID: jobID,
				}); err != nil {
					log.Printf("[RuntimeAudit] record game-update start room=%s world=%s: %v", world.roomID, world.worldID, err)
				}
			}
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

func sameUpdatePlan(plannedRooms []rooms.Room, plannedWorlds []plannedWorld, currentRooms []rooms.Room, currentWorlds []plannedWorld) bool {
	roomKeys := func(items []rooms.Room) []string {
		keys := make([]string, 0, len(items))
		for _, room := range items {
			keys = append(keys, room.ID+"\x00"+room.DirectoryName)
		}
		sort.Strings(keys)
		return keys
	}
	worldKeys := func(items []plannedWorld) []string {
		keys := make([]string, 0, len(items))
		for _, world := range items {
			mode, valid := shared.NormalizeRuntimePerformanceMode(world.runtimeMode)
			if !valid {
				mode = world.runtimeMode
			}
			keys = append(keys, world.roomID+"\x00"+world.worldID+"\x00"+world.roomName+"\x00"+world.worldName+"\x00"+string(mode))
		}
		sort.Strings(keys)
		return keys
	}
	return equalStrings(roomKeys(plannedRooms), roomKeys(currentRooms)) && equalStrings(worldKeys(plannedWorlds), worldKeys(currentWorlds))
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *Service) reportPlanChanged(request UpdateRequest, managed []rooms.Room, running []plannedWorld, report func(jobs.TargetResult)) {
	failure := func(targetID string) {
		report(jobs.TargetResult{TargetID: targetID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "ROOM_CHANGED", Message: "等待执行期间房间或分片运行状态已变化，请重新提交游戏更新"}})
	}
	for _, room := range managed {
		failure("protect:" + room.ID)
	}
	for _, world := range running {
		failure(stopTargetID(world))
	}
	failure("steamcmd")
	if request.RestartRunning {
		for _, world := range running {
			failure(startTargetID(world))
		}
	}
}

func (s *Service) runSteamCMD(ctx context.Context, jobID string, cleanCache bool, executable string) error {
	unlock, lockErr := installationlock.Acquire(installRoot(s.config.ServerPath))
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	before, _ := readLocalVersion(s.config.ServerPath, s.config.AppID)
	if _, err := s.store.Begin(jobID, before, cleanCache); err != nil {
		return err
	}
	logBuffer := &boundedBuffer{limit: 2 * 1024 * 1024}
	if cleanCache {
		if err := cleanSteamCache(executable, installRoot(s.config.ServerPath), s.config.AppID); err != nil {
			_, _ = s.store.Complete(jobID, before, logBuffer.String(), err)
			return err
		}
	}
	arguments := []string{"+force_install_dir", installRoot(s.config.ServerPath), "+login", "anonymous", "+app_update", s.config.AppID, "validate", "+quit"}
	runErr := s.runner.Run(ctx, executable, arguments, logBuffer)
	after, _ := readLocalVersion(s.config.ServerPath, s.config.AppID)
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
				mode := shared.RuntimePerformanceModeGame
				if control, ok := s.control.(interface {
					Status(context.Context, string, string) (shards.RuntimeStatus, error)
				}); ok {
					status, err := control.Status(ctx, room.DirectoryName, world.DirectoryName)
					if err != nil {
						return nil, nil, err
					}
					var valid bool
					mode, valid = shared.NormalizeRuntimePerformanceMode(status.RuntimeMode)
					if !valid {
						return nil, nil, shards.ErrInvalidRuntimeMode
					}
				}
				running = append(running, plannedWorld{roomID: room.ID, roomName: room.DirectoryName, worldID: world.ID, worldName: world.DirectoryName, isMaster: world.IsMaster, runtimeMode: mode})
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
		if control, ok := s.control.(interface {
			StartWithRuntimeMode(context.Context, string, string, shared.RuntimePerformanceMode) error
		}); ok {
			err = control.StartWithRuntimeMode(ctx, world.roomName, world.worldName, world.runtimeMode)
		} else if mode, valid := shared.NormalizeRuntimePerformanceMode(world.runtimeMode); !valid || mode != shared.RuntimePerformanceModeGame {
			return shards.ErrRuntimeModeUnavailable
		} else {
			err = s.control.Start(ctx, world.roomName, world.worldName)
		}
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

func cleanSteamCache(executable, serverRoot, appID string) error {
	roots := []string{filepath.Join(filepath.Dir(executable), "steamapps"), filepath.Join(serverRoot, "steamapps")}
	seen := make(map[string]bool)
	for _, root := range roots {
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		for _, relative := range []string{filepath.Join("downloading", appID), filepath.Join("temp", appID), "appmanifest_" + appID + ".acf"} {
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
