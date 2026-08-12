package dstruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dont/internal/rooms"
)

const (
	maxRuntimeRequestBytes   = 4096
	maxRuntimeResultBytes    = int64(256 * 1024)
	defaultCommandTimeout    = 5 * time.Second
	defaultDiagnosticTimeout = 8 * time.Second
	defaultLifecycleTimeout  = 10 * time.Second
	defaultPollInterval      = 100 * time.Millisecond
	managedActivationScript  = `TheSim:GetPersistentString("../dst-admin/bootstrap.lua",function(ok,source) if not ok or type(source)~="string" then print("[DST-ADMIN-RUNTIME ERROR] code=BOOTSTRAP_UNAVAILABLE") return end local chunk,compile_error=loadstring(source) if chunk==nil then print("[DST-ADMIN-RUNTIME ERROR] code=BOOTSTRAP_COMPILE_FAILED message="..tostring(compile_error)) return end local executed,runtime_error=xpcall(chunk,debug.traceback) if not executed then print("[DST-ADMIN-RUNTIME ERROR] code=BOOTSTRAP_EXECUTE_FAILED message="..tostring(runtime_error)) end end)`
)

var (
	runtimeRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,80}$`)
	runtimeActionPattern    = regexp.MustCompile(`^[a-z][a-z0-9_.]{1,63}$`)
	runtimeCodePattern      = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
)

var allowedRuntimeCommands = map[string]bool{
	"telemetry.emit":          true,
	"player.kick":             true,
	"player.announce":         true,
	"player.kill":             true,
	"player.god_mode":         true,
	"player.creative_mode":    true,
	"player.resurrect":        true,
	"player.change_character": true,
	"player.ban":              true,
	"player.unban":            true,
}

var allowedRuntimeEvents = map[string]bool{
	"player.joined": true, "player.left": true, "world.save": true, "world.reset": true, "world.shutdown": true,
	"world.cycles": true, "world.phase": true, "world.season": true, "world.rain": true,
}

type RuntimeProcess interface {
	IsRunning(context.Context, string, string) (bool, error)
}

type CommandSender interface {
	Send(context.Context, string, string, string) error
}

type Bridge struct {
	manager      *Manager
	process      RuntimeProcess
	sender       CommandSender
	now          func() time.Time
	timeout      time.Duration
	pollInterval time.Duration
	locksMu      sync.Mutex
	locks        map[string]*sync.Mutex
}

func NewBridge(manager *Manager, process RuntimeProcess, sender CommandSender) (*Bridge, error) {
	if manager == nil || process == nil || sender == nil {
		return nil, errors.New("runtime manager, process, and sender are required")
	}
	return &Bridge{
		manager: manager, process: process, sender: sender, now: time.Now,
		timeout: defaultCommandTimeout, pollInterval: defaultPollInterval, locks: make(map[string]*sync.Mutex),
	}, nil
}

func (b *Bridge) Activate(ctx context.Context, roomID, worldID string) (LifecycleResult, error) {
	room, world, worldPath, err := b.resolveWorld(roomID, worldID)
	if err != nil {
		return LifecycleResult{}, err
	}
	lock := b.worldLock(worldPath)
	lock.Lock()
	defer lock.Unlock()
	if err := b.lifecycleReady(ctx, room, world, worldPath); err != nil {
		return LifecycleResult{}, err
	}
	if health, healthErr := b.manager.Health(room.ID, world.ID); healthErr == nil && b.validLifecycleHealth(worldPath, health, time.Time{}) {
		return LifecycleResult{
			RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, Mode: LifecycleModeCurrent,
			Health: health, Message: "DST Admin 运行时已在当前分片健康运行",
		}, nil
	}
	startedAt := b.now().UTC()
	if err := b.sender.Send(ctx, room.DirectoryName, world.DirectoryName, managedActivationScript); err != nil {
		return LifecycleResult{}, fmt.Errorf("%w: send managed bootstrap: %v", ErrRuntimeActivation, err)
	}
	health, err := b.waitForLifecycleHealth(ctx, room.ID, world.ID, worldPath, startedAt, "")
	if err != nil {
		return LifecycleResult{}, err
	}
	return LifecycleResult{
		RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, Mode: LifecycleModeActivate,
		Health: health, Message: "DST Admin 运行时已在不中断分片的情况下激活",
	}, nil
}

func (b *Bridge) Reload(ctx context.Context, roomID, worldID string) (LifecycleResult, error) {
	room, world, worldPath, err := b.resolveWorld(roomID, worldID)
	if err != nil {
		return LifecycleResult{}, err
	}
	lock := b.worldLock(worldPath)
	lock.Lock()
	defer lock.Unlock()
	if err := b.lifecycleReady(ctx, room, world, worldPath); err != nil {
		return LifecycleResult{}, err
	}
	previous, err := b.manager.Health(room.ID, world.ID)
	if err != nil || !b.validLifecycleHealth(worldPath, previous, time.Time{}) {
		return LifecycleResult{}, fmt.Errorf("%w: current runtime health is unavailable", ErrRuntimeUnavailable)
	}
	startedAt := b.now().UTC()
	const reloadScript = `local runtime=rawget(_G,"DSTAdmin"); if runtime~=nil and type(runtime.Reload)=="function" then runtime.Reload() else print("[DST-ADMIN-RUNTIME ERROR] code=RELOAD_UNAVAILABLE") end`
	if err := b.sender.Send(ctx, room.DirectoryName, world.DirectoryName, reloadScript); err != nil {
		return LifecycleResult{}, fmt.Errorf("%w: send managed reload: %v", ErrRuntimeActivation, err)
	}
	health, err := b.waitForLifecycleHealth(ctx, room.ID, world.ID, worldPath, startedAt, previous.ProducerInstanceID)
	if err != nil {
		return LifecycleResult{}, err
	}
	return LifecycleResult{
		RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, Mode: LifecycleModeReload,
		Health: health, Message: "DST Admin 运行时已热重载，分片未重启",
	}, nil
}

func (b *Bridge) lifecycleReady(ctx context.Context, room rooms.Room, world rooms.World, worldPath string) error {
	running, err := b.process.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
	if err != nil {
		return fmt.Errorf("inspect runtime process: %w", err)
	}
	if !running {
		return fmt.Errorf("%w: shard is not running", ErrRuntimeUnavailable)
	}
	status := b.manager.inspect(room, world)
	if status.State != InstallStateInstalled {
		return fmt.Errorf("%w: %s", ErrRuntimeNotInstalled, status.Message)
	}
	if err := ensureRuntimeOutputDirectory(worldPath); err != nil {
		return err
	}
	return nil
}

func (b *Bridge) waitForLifecycleHealth(ctx context.Context, roomID, worldID, worldPath string, startedAt time.Time, previousInstanceID string) (Health, error) {
	deadline := time.NewTimer(defaultLifecycleTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	for {
		health, err := b.manager.Health(roomID, worldID)
		if err == nil && b.validLifecycleHealth(worldPath, health, startedAt) && (previousInstanceID == "" || health.ProducerInstanceID != previousInstanceID) {
			return health, nil
		}
		select {
		case <-ctx.Done():
			return Health{}, ctx.Err()
		case <-deadline.C:
			return Health{}, fmt.Errorf("%w: managed runtime did not publish fresh healthy state", ErrRuntimeActivation)
		case <-ticker.C:
		}
	}
}

func (b *Bridge) validLifecycleHealth(worldPath string, health Health, startedAt time.Time) bool {
	if health.ProducerVersion != RuntimeVersion || !health.Running || !health.Ready || health.Writing || health.LastError != nil || health.ConsecutiveFailures != 0 {
		return false
	}
	if !startedAt.IsZero() && health.ReadAt.Before(startedAt) {
		return false
	}
	if b.now().UTC().Sub(health.ReadAt) > defaultFreshFor {
		return false
	}
	if shardID, err := configuredShardID(worldPath); err != nil || shardID != "" && health.ShardID != shardID {
		return false
	}
	for _, name := range []string{"worldstate", "commands", "events", "diagnostics"} {
		module, exists := health.Modules[name]
		if !exists || !module.Running || !module.Ready || module.Busy || module.LastError != nil {
			return false
		}
	}
	return true
}

func (b *Bridge) ExecuteCommand(ctx context.Context, roomID, worldID string, request CommandRequest) (CommandReceipt, error) {
	if err := validateCommandRequest(request); err != nil {
		return CommandReceipt{}, err
	}
	room, world, worldPath, err := b.resolveWorld(roomID, worldID)
	if err != nil {
		return CommandReceipt{}, err
	}
	lock := b.worldLock(worldPath)
	lock.Lock()
	defer lock.Unlock()

	if err := b.commandReady(ctx, room, world); err != nil {
		return CommandReceipt{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil || len(payload) > maxRuntimeRequestBytes {
		return CommandReceipt{}, fmt.Errorf("runtime command request is invalid: %w", err)
	}
	script := `DSTAdmin.Commands.ExecuteJSON(` + quoteRuntimeLua(string(payload)) + `)`
	if err := b.sender.Send(ctx, room.DirectoryName, world.DirectoryName, script); err != nil {
		return CommandReceipt{}, fmt.Errorf("send runtime command: %w", err)
	}

	expectedSession := currentSessionID(worldPath)
	deadline := time.NewTimer(b.timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	for {
		receipt, readErr := readCommandReceipt(worldPath, expectedSession, request, b.now().UTC())
		if readErr == nil {
			return receipt, nil
		}
		if !errors.Is(readErr, ErrRuntimeResultAbsent) {
			return CommandReceipt{}, readErr
		}
		select {
		case <-ctx.Done():
			return CommandReceipt{}, ctx.Err()
		case <-deadline.C:
			return CommandReceipt{}, fmt.Errorf("%w: command %s may have executed; it will not be retried automatically", ErrRuntimeResultAbsent, request.RequestID)
		case <-ticker.C:
		}
	}
}

func (b *Bridge) CaptureDiagnostic(ctx context.Context, roomID, worldID string, request DiagnosticRequest) (DiagnosticReport, error) {
	if err := validateDiagnosticRequest(request); err != nil {
		return DiagnosticReport{}, err
	}
	room, world, worldPath, err := b.resolveWorld(roomID, worldID)
	if err != nil {
		return DiagnosticReport{}, err
	}
	lock := b.worldLock(worldPath)
	lock.Lock()
	defer lock.Unlock()
	if err := b.moduleReady(ctx, room, world, "diagnostics"); err != nil {
		return DiagnosticReport{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil || len(payload) > maxRuntimeRequestBytes {
		return DiagnosticReport{}, errors.New("runtime diagnostic request is invalid")
	}
	if err := b.sender.Send(ctx, room.DirectoryName, world.DirectoryName, `DSTAdmin.Diagnostics.CaptureJSON(`+quoteRuntimeLua(string(payload))+`)`); err != nil {
		return DiagnosticReport{}, fmt.Errorf("send runtime diagnostic: %w", err)
	}
	expectedSession := currentSessionID(worldPath)
	deadline := time.NewTimer(defaultDiagnosticTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	for {
		report, readErr := readDiagnosticReport(worldPath, expectedSession, request, b.now().UTC())
		if readErr == nil {
			return report, nil
		}
		if !errors.Is(readErr, ErrRuntimeResultAbsent) {
			return DiagnosticReport{}, readErr
		}
		select {
		case <-ctx.Done():
			return DiagnosticReport{}, ctx.Err()
		case <-deadline.C:
			return DiagnosticReport{}, fmt.Errorf("%w: diagnostic %s did not produce a report", ErrRuntimeResultAbsent, request.RequestID)
		case <-ticker.C:
		}
	}
}

func (b *Bridge) ReadEvents(ctx context.Context, roomID, worldID string) (EventBatch, error) {
	if err := ctx.Err(); err != nil {
		return EventBatch{}, err
	}
	_, _, worldPath, err := b.resolveWorld(roomID, worldID)
	if err != nil {
		return EventBatch{}, err
	}
	return readEventBatch(worldPath, currentSessionID(worldPath), b.now().UTC())
}

func (b *Bridge) LatestDiagnostic(ctx context.Context, roomID, worldID string) (DiagnosticReport, error) {
	if err := ctx.Err(); err != nil {
		return DiagnosticReport{}, err
	}
	_, _, worldPath, err := b.resolveWorld(roomID, worldID)
	if err != nil {
		return DiagnosticReport{}, err
	}
	return readLatestDiagnosticReport(worldPath, currentSessionID(worldPath), b.now().UTC())
}

func (b *Bridge) commandReady(ctx context.Context, room rooms.Room, world rooms.World) error {
	return b.moduleReady(ctx, room, world, "commands")
}

func (b *Bridge) moduleReady(ctx context.Context, room rooms.Room, world rooms.World, moduleName string) error {
	running, err := b.process.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
	if err != nil {
		return fmt.Errorf("inspect runtime process: %w", err)
	}
	if !running {
		return fmt.Errorf("%w: shard is not running", ErrRuntimeUnavailable)
	}
	status := b.manager.inspect(room, world)
	if status.State != InstallStateInstalled {
		return fmt.Errorf("%w: %s", ErrRuntimeNotInstalled, status.Message)
	}
	health, err := b.manager.Health(room.ID, world.ID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	module, exists := health.Modules[moduleName]
	if !health.Running || !health.Ready || !exists || !module.Running || !module.Ready || module.Busy {
		return fmt.Errorf("%w: %s module is not ready", ErrRuntimeUnavailable, moduleName)
	}
	if b.now().UTC().Sub(health.ReadAt) > defaultFreshFor {
		return fmt.Errorf("%w: runtime health is stale", ErrRuntimeUnavailable)
	}
	return nil
}

func validateDiagnosticRequest(request DiagnosticRequest) error {
	if !runtimeRequestIDPattern.MatchString(request.RequestID) {
		return fmt.Errorf("%w: request ID is invalid", ErrRuntimeRequestInvalid)
	}
	switch request.Profile {
	case "summary":
		if request.Prefab != "" || request.DurationSeconds != 0 || request.SampleLimit != 0 {
			return fmt.Errorf("%w: summary does not accept additional parameters", ErrRuntimeRequestInvalid)
		}
	case "prefab":
		if !regexp.MustCompile(`^[a-z0-9_]{1,80}$`).MatchString(request.Prefab) || request.SampleLimit < 0 || request.SampleLimit > 50 || request.DurationSeconds != 0 {
			return fmt.Errorf("%w: prefab parameters are invalid", ErrRuntimeRequestInvalid)
		}
	case "performance":
		if request.DurationSeconds < 1 || request.DurationSeconds > 5 || request.Prefab != "" || request.SampleLimit != 0 {
			return fmt.Errorf("%w: performance parameters are invalid", ErrRuntimeRequestInvalid)
		}
	default:
		return fmt.Errorf("%w: diagnostic profile is not allowed", ErrRuntimeRequestInvalid)
	}
	return nil
}

func (b *Bridge) resolveWorld(roomID, worldID string) (rooms.Room, rooms.World, string, error) {
	room, err := b.manager.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, rooms.World{}, "", err
	}
	if !room.Managed {
		return rooms.Room{}, rooms.World{}, "", rooms.ErrRoomNotManaged
	}
	world, err := b.manager.rooms.World(room.ID, worldID)
	if err != nil {
		return rooms.Room{}, rooms.World{}, "", err
	}
	path, err := b.manager.worldPath(room, world)
	return room, world, path, err
}

func (b *Bridge) worldLock(path string) *sync.Mutex {
	b.locksMu.Lock()
	defer b.locksMu.Unlock()
	if b.locks[path] == nil {
		b.locks[path] = &sync.Mutex{}
	}
	return b.locks[path]
}

func validateCommandRequest(request CommandRequest) error {
	if !runtimeRequestIDPattern.MatchString(request.RequestID) || !runtimeActionPattern.MatchString(request.Action) || !allowedRuntimeCommands[request.Action] {
		return errors.New("runtime command request is not allowed")
	}
	if request.Arguments == nil {
		request.Arguments = map[string]interface{}{}
	}
	data, err := json.Marshal(request.Arguments)
	if err != nil || len(data) > maxRuntimeRequestBytes || !utf8.Valid(data) {
		return errors.New("runtime command arguments are invalid")
	}
	return nil
}

func readCommandReceipt(worldPath, expectedSession string, request CommandRequest, now time.Time) (CommandReceipt, error) {
	candidates := make([]CommandReceipt, 0, 2)
	var invalid error
	root := filepath.Join(worldPath, "save", "mod_config_data", "dst-admin")
	for _, name := range []string{"command-receipt-a.json", "command-receipt-b.json"} {
		value, err := decodeCommandReceipt(filepath.Join(root, name), expectedSession, request, now)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrRuntimeResultAbsent) {
				invalid = errors.Join(invalid, fmt.Errorf("%s: %w", name, err))
			}
			continue
		}
		candidates = append(candidates, value)
	}
	if len(candidates) == 0 {
		if invalid != nil {
			return CommandReceipt{}, invalid
		}
		return CommandReceipt{}, ErrRuntimeResultAbsent
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Sequence > candidates[j].Sequence })
	return candidates[0], nil
}

func decodeCommandReceipt(path, expectedSession string, request CommandRequest, now time.Time) (CommandReceipt, error) {
	data, _, exists, err := readRegular(path, maxRuntimeResultBytes)
	if err != nil {
		return CommandReceipt{}, err
	}
	if !exists {
		return CommandReceipt{}, os.ErrNotExist
	}
	var value CommandReceipt
	if err := decodeStrictJSON(data, &value); err != nil {
		return CommandReceipt{}, fmt.Errorf("%w: decode command receipt: %v", ErrRuntimeResultInvalid, err)
	}
	if value.RequestID != request.RequestID || value.Action != request.Action {
		return CommandReceipt{}, ErrRuntimeResultAbsent
	}
	if value.SchemaVersion != 1 || value.ProducerVersion == "" || value.ProducerInstanceID == "" || value.SessionID == "" || value.ShardID == "" || value.Sequence < 1 ||
		!runtimeRequestIDPattern.MatchString(value.RequestID) || !runtimeActionPattern.MatchString(value.Action) || !runtimeCodePattern.MatchString(value.Code) || len([]rune(value.Message)) > 512 || value.CompletedAtUnix < 1 {
		return CommandReceipt{}, ErrRuntimeResultInvalid
	}
	value.CompletedAt = time.Unix(value.CompletedAtUnix, 0).UTC()
	if value.CompletedAt.After(now.Add(maxFutureSkew)) || now.Sub(value.CompletedAt) > 2*time.Minute {
		return CommandReceipt{}, ErrRuntimeResultStale
	}
	if expectedSession != "" && value.SessionID != expectedSession {
		return CommandReceipt{}, ErrRuntimeResultStale
	}
	return value, nil
}

func readEventBatch(worldPath, expectedSession string, now time.Time) (EventBatch, error) {
	candidates := make([]EventBatch, 0, 2)
	var invalid error
	root := filepath.Join(worldPath, "save", "mod_config_data", "dst-admin")
	for _, name := range []string{"events-a.json", "events-b.json"} {
		value, err := decodeEventBatch(filepath.Join(root, name), expectedSession, now)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				invalid = errors.Join(invalid, fmt.Errorf("%s: %w", name, err))
			}
			continue
		}
		candidates = append(candidates, value)
	}
	if len(candidates) == 0 {
		if invalid != nil {
			return EventBatch{}, invalid
		}
		return EventBatch{}, ErrRuntimeResultAbsent
	}
	sort.SliceStable(candidates, func(i, j int) bool { return eventBatchNewer(candidates[i], candidates[j]) })
	activeInstance := candidates[0].ProducerInstanceID
	eventsBySequence := make(map[int64]RuntimeEvent)
	var selected EventBatch
	for _, candidate := range candidates {
		if candidate.ProducerInstanceID != activeInstance {
			continue
		}
		if selected.ProducerInstanceID == "" || eventBatchNewer(candidate, selected) {
			selected = candidate
		}
		for _, event := range candidate.Events {
			eventsBySequence[event.Sequence] = event
		}
	}
	sequences := make([]int64, 0, len(eventsBySequence))
	for sequence := range eventsBySequence {
		sequences = append(sequences, sequence)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	selected.Events = make([]RuntimeEvent, 0, len(sequences))
	for _, sequence := range sequences {
		selected.Events = append(selected.Events, eventsBySequence[sequence])
	}
	selected.FirstSequence = sequences[0]
	selected.LastSequence = sequences[len(sequences)-1]
	return selected, nil
}

func eventBatchNewer(left, right EventBatch) bool {
	leftOccurred := left.Events[len(left.Events)-1].OccurredAt
	rightOccurred := right.Events[len(right.Events)-1].OccurredAt
	if !leftOccurred.Equal(rightOccurred) {
		return leftOccurred.After(rightOccurred)
	}
	return left.ReadAt.After(right.ReadAt)
}

func decodeEventBatch(path, expectedSession string, now time.Time) (EventBatch, error) {
	data, _, exists, err := readRegular(path, maxRuntimeResultBytes)
	if err != nil {
		return EventBatch{}, err
	}
	if !exists {
		return EventBatch{}, os.ErrNotExist
	}
	var value EventBatch
	if err := decodeStrictJSON(data, &value); err != nil {
		return EventBatch{}, fmt.Errorf("%w: decode event batch: %v", ErrRuntimeResultInvalid, err)
	}
	if value.SchemaVersion != 1 || value.ProducerVersion == "" || value.ProducerInstanceID == "" || value.SessionID == "" || value.ShardID == "" ||
		value.FirstSequence < 1 || value.LastSequence < value.FirstSequence || len(value.Events) == 0 || len(value.Events) > 128 {
		return EventBatch{}, ErrRuntimeResultInvalid
	}
	if expectedSession != "" && value.SessionID != expectedSession {
		return EventBatch{}, ErrRuntimeResultStale
	}
	previous := value.FirstSequence - 1
	for index := range value.Events {
		event := &value.Events[index]
		if event.Sequence != previous+1 || !allowedRuntimeEvents[event.Kind] || event.OccurredAtUnix < 1 || event.Fields == nil {
			return EventBatch{}, ErrRuntimeResultInvalid
		}
		event.OccurredAt = time.Unix(event.OccurredAtUnix, 0).UTC()
		if event.OccurredAt.After(now.Add(maxFutureSkew)) || now.Sub(event.OccurredAt) > 24*time.Hour {
			return EventBatch{}, ErrRuntimeResultStale
		}
		previous = event.Sequence
	}
	if previous != value.LastSequence {
		return EventBatch{}, ErrRuntimeResultInvalid
	}
	if info, statErr := os.Stat(path); statErr == nil {
		value.ReadAt = info.ModTime().UTC()
	} else {
		value.ReadAt = now
	}
	return value, nil
}

func readDiagnosticReport(worldPath, expectedSession string, request DiagnosticRequest, now time.Time) (DiagnosticReport, error) {
	return readDiagnosticReports(worldPath, expectedSession, now, func(value DiagnosticReport) bool {
		return value.RequestID == request.RequestID && value.Profile == request.Profile
	})
}

func readLatestDiagnosticReport(worldPath, expectedSession string, now time.Time) (DiagnosticReport, error) {
	return readDiagnosticReports(worldPath, expectedSession, now, func(DiagnosticReport) bool { return true })
}

func readDiagnosticReports(worldPath, expectedSession string, now time.Time, accept func(DiagnosticReport) bool) (DiagnosticReport, error) {
	candidates := make([]DiagnosticReport, 0, 2)
	var invalid error
	root := filepath.Join(worldPath, "save", "mod_config_data", "dst-admin")
	for _, name := range []string{"diagnostic-a.json", "diagnostic-b.json"} {
		value, err := decodeDiagnosticReport(filepath.Join(root, name), expectedSession, now)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				invalid = errors.Join(invalid, fmt.Errorf("%s: %w", name, err))
			}
			continue
		}
		if accept(value) {
			candidates = append(candidates, value)
		}
	}
	if len(candidates) == 0 {
		if invalid != nil {
			return DiagnosticReport{}, invalid
		}
		return DiagnosticReport{}, ErrRuntimeResultAbsent
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].ProducerInstanceID == candidates[j].ProducerInstanceID {
			return candidates[i].Sequence > candidates[j].Sequence
		}
		return candidates[i].CompletedAt.After(candidates[j].CompletedAt)
	})
	return candidates[0], nil
}

func decodeDiagnosticReport(path, expectedSession string, now time.Time) (DiagnosticReport, error) {
	data, _, exists, err := readRegular(path, maxRuntimeResultBytes)
	if err != nil {
		return DiagnosticReport{}, err
	}
	if !exists {
		return DiagnosticReport{}, os.ErrNotExist
	}
	var value DiagnosticReport
	if err := decodeStrictJSON(data, &value); err != nil {
		return DiagnosticReport{}, fmt.Errorf("%w: decode diagnostic report: %v", ErrRuntimeResultInvalid, err)
	}
	if value.SchemaVersion != 1 || value.ProducerVersion == "" || value.ProducerInstanceID == "" || value.SessionID == "" || value.ShardID == "" ||
		value.Sequence < 1 || !runtimeRequestIDPattern.MatchString(value.RequestID) || !runtimeCodePattern.MatchString(value.Code) || len([]rune(value.Message)) > 512 || value.CompletedAtUnix < 1 || value.Result == nil {
		return DiagnosticReport{}, ErrRuntimeResultInvalid
	}
	if value.Profile != "summary" && value.Profile != "prefab" && value.Profile != "performance" {
		return DiagnosticReport{}, ErrRuntimeResultInvalid
	}
	value.CompletedAt = time.Unix(value.CompletedAtUnix, 0).UTC()
	if value.CompletedAt.After(now.Add(maxFutureSkew)) || now.Sub(value.CompletedAt) > 24*time.Hour || expectedSession != "" && value.SessionID != expectedSession {
		return DiagnosticReport{}, ErrRuntimeResultStale
	}
	return value, nil
}

func quoteRuntimeLua(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, character := range value {
		switch character {
		case '\\':
			builder.WriteString(`\\`)
		case '"':
			builder.WriteString(`\"`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			builder.WriteRune(character)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
