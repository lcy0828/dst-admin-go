package dstruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/operationlease"
	"dont/internal/requesttiming"
	"dont/internal/runtimefiles"
	"dont/shared"
)

// DistributedRuntime is the Placement-aware boundary used by the control
// plane. It resolves every room/world pair to its applied local or Agent
// target before reading files or sending console input.
type DistributedRuntime interface {
	Status(context.Context, string, string) (shared.ShardRuntimeStatus, error)
	SendID(context.Context, string, string, shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error)
	ReadArtifacts(context.Context, string, string, shared.ArtifactKind) (shared.RuntimeArtifactBundle, error)
}

type DistributedBridge struct {
	local        *Bridge
	runtime      DistributedRuntime
	now          func() time.Time
	timeout      time.Duration
	pollInterval time.Duration
	locksMu      sync.Mutex
	locks        map[string]*sync.Mutex
}

func NewDistributedBridge(local *Bridge, runtime DistributedRuntime) (*DistributedBridge, error) {
	if local == nil || runtime == nil {
		return nil, errors.New("local runtime bridge and distributed runtime are required")
	}
	return &DistributedBridge{
		local: local, runtime: runtime, now: time.Now, timeout: defaultCommandTimeout,
		pollInterval: defaultPollInterval, locks: make(map[string]*sync.Mutex),
	}, nil
}

func (b *DistributedBridge) IsLocalPlacement(roomID, worldID string) (bool, error) {
	locality, ok := b.runtime.(interface {
		IsLocalPlacement(string, string) (bool, error)
	})
	if !ok {
		return true, nil
	}
	return locality.IsLocalPlacement(roomID, worldID)
}

func (b *DistributedBridge) ReadPlayers(ctx context.Context, roomID, worldID string) (Snapshot, error) {
	health, err := b.Health(ctx, roomID, worldID)
	if err != nil {
		return Snapshot{}, err
	}
	return b.readPlayersForHealth(ctx, roomID, worldID, health)
}

func (b *DistributedBridge) readPlayersForHealth(ctx context.Context, roomID, worldID string, health Health) (Snapshot, error) {
	defer requesttiming.Start(ctx, "runtime.players_read")()
	bundle, err := b.runtime.ReadArtifacts(ctx, roomID, worldID, shared.ArtifactRuntimePlayers)
	if err != nil {
		return Snapshot{}, artifactReadError(err, ErrSnapshotUnavailable)
	}
	artifacts, err := validatedArtifacts(bundle, shared.ArtifactRuntimePlayers, "players-a.json", "players-b.json")
	if err != nil {
		return Snapshot{}, err
	}
	candidates := make([]Snapshot, 0, len(artifacts))
	var failures error
	for _, artifact := range artifacts {
		value, decodeErr := decodeSnapshotData(artifact.Data, health.SessionID, health.ShardID, b.now().UTC())
		if decodeErr != nil {
			failures = errors.Join(failures, fmt.Errorf("%s: %w", artifact.Name, decodeErr))
			continue
		}
		if value.ProducerVersion != RuntimeVersion || value.ProducerInstanceID != health.ProducerInstanceID || value.Sequence != health.Sequence {
			failures = errors.Join(failures, fmt.Errorf("%s: %w", artifact.Name, ErrSnapshotStale))
			continue
		}
		candidates = append(candidates, value)
	}
	if len(candidates) == 0 {
		return Snapshot{}, unavailableWithFailures(ErrSnapshotUnavailable, failures)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Sequence != candidates[j].Sequence {
			return candidates[i].Sequence > candidates[j].Sequence
		}
		return candidates[i].CapturedAt.After(candidates[j].CapturedAt)
	})
	return candidates[0], nil
}

func (b *DistributedBridge) ReadWorldState(ctx context.Context, roomID, worldID string) (WorldStateSnapshot, error) {
	return b.readWorldState(ctx, roomID, worldID, true)
}

func (b *DistributedBridge) ReadStoppedWorldState(ctx context.Context, roomID, worldID string) (WorldStateSnapshot, error) {
	return b.readWorldState(ctx, roomID, worldID, false)
}

func (b *DistributedBridge) readWorldState(ctx context.Context, roomID, worldID string, requireFresh bool) (WorldStateSnapshot, error) {
	health, err := b.readHealth(ctx, roomID, worldID, requireFresh)
	if err != nil {
		return WorldStateSnapshot{}, err
	}
	return b.readWorldStateForHealth(ctx, roomID, worldID, health, requireFresh)
}

func (b *DistributedBridge) readWorldStateForHealth(ctx context.Context, roomID, worldID string, health Health, requireFresh bool) (WorldStateSnapshot, error) {
	defer requesttiming.Start(ctx, "runtime.worldstate_read")()
	bundle, err := b.runtime.ReadArtifacts(ctx, roomID, worldID, shared.ArtifactRuntimeWorldState)
	if err != nil {
		return WorldStateSnapshot{}, artifactReadError(err, ErrSnapshotUnavailable)
	}
	artifacts, err := validatedArtifacts(bundle, shared.ArtifactRuntimeWorldState, "worldstate-a.json", "worldstate-b.json")
	if err != nil {
		return WorldStateSnapshot{}, err
	}
	module := health.Modules["worldstate"]
	candidates := make([]WorldStateSnapshot, 0, len(artifacts))
	var failures error
	for _, artifact := range artifacts {
		value, decodeErr := decodeWorldStateData(artifact.Data, health.SessionID, health.ShardID, b.now().UTC(), requireFresh)
		if decodeErr != nil {
			failures = errors.Join(failures, fmt.Errorf("%s: %w", artifact.Name, decodeErr))
			continue
		}
		// Health can precede the final worldstate write during shutdown.
		if requireFresh && (value.ProducerVersion != RuntimeVersion || module.Sequence > 0 && value.Sequence != module.Sequence) {
			failures = errors.Join(failures, fmt.Errorf("%s: %w", artifact.Name, ErrSnapshotStale))
			continue
		}
		candidates = append(candidates, value)
	}
	if len(candidates) == 0 {
		return WorldStateSnapshot{}, unavailableWithFailures(ErrSnapshotUnavailable, failures)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].ProducerInstanceID == candidates[j].ProducerInstanceID && candidates[i].Sequence != candidates[j].Sequence {
			return candidates[i].Sequence > candidates[j].Sequence
		}
		return candidates[i].CapturedAt.After(candidates[j].CapturedAt)
	})
	return candidates[0], nil
}

func (b *DistributedBridge) Health(ctx context.Context, roomID, worldID string) (Health, error) {
	return b.readHealth(ctx, roomID, worldID, true)
}

func (b *DistributedBridge) readHealth(ctx context.Context, roomID, worldID string, requireCurrentVersion bool) (Health, error) {
	bundle, err := b.runtime.ReadArtifacts(ctx, roomID, worldID, shared.ArtifactRuntimeHealth)
	if err != nil {
		return Health{}, artifactReadError(err, ErrSnapshotUnavailable)
	}
	artifacts, err := validatedArtifacts(bundle, shared.ArtifactRuntimeHealth, "health.json")
	if err != nil {
		return Health{}, err
	}
	if len(artifacts) != 1 {
		return Health{}, ErrSnapshotUnavailable
	}
	health, err := decodeHealthData(artifacts[0].Data, "", "", artifacts[0].UpdatedAt)
	if err != nil {
		return Health{}, err
	}
	if requireCurrentVersion && health.ProducerVersion != RuntimeVersion {
		return Health{}, fmt.Errorf("%w: runtime version %q does not match %q", ErrRuntimeUnavailable, health.ProducerVersion, RuntimeVersion)
	}
	return health, nil
}

func (b *DistributedBridge) Activate(ctx context.Context, roomID, worldID string) (LifecycleResult, error) {
	room, err := b.local.manager.rooms.Room(roomID)
	if err != nil {
		return LifecycleResult{}, err
	}
	world, err := b.local.manager.rooms.World(room.ID, worldID)
	if err != nil {
		return LifecycleResult{}, err
	}
	lock := b.worldLock(room.ID, world.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := b.requireRunning(ctx, room.ID, world.ID); err != nil {
		return LifecycleResult{}, err
	}
	if health, healthErr := b.Health(ctx, room.ID, world.ID); healthErr == nil && b.lifecycleHealthReady(health, time.Time{}) {
		return LifecycleResult{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, Mode: LifecycleModeCurrent, Health: health, Message: "DST Admin 运行时已在当前分片健康运行"}, nil
	}
	startedAt := b.now().UTC()
	if _, err := b.send(ctx, room.ID, world.ID, managedActivationScript, shared.ConsoleModeManaged, ""); err != nil {
		return LifecycleResult{}, fmt.Errorf("%w: send managed bootstrap: %v", ErrRuntimeActivation, err)
	}
	health, err := b.waitForLifecycleHealth(ctx, room.ID, world.ID, startedAt, "")
	if err != nil {
		return LifecycleResult{}, err
	}
	return LifecycleResult{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, Mode: LifecycleModeActivate, Health: health, Message: "DST Admin 运行时已通过目标节点激活"}, nil
}

func (b *DistributedBridge) Reload(ctx context.Context, roomID, worldID string) (LifecycleResult, error) {
	room, err := b.local.manager.rooms.Room(roomID)
	if err != nil {
		return LifecycleResult{}, err
	}
	world, err := b.local.manager.rooms.World(room.ID, worldID)
	if err != nil {
		return LifecycleResult{}, err
	}
	lock := b.worldLock(room.ID, world.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := b.requireRunning(ctx, room.ID, world.ID); err != nil {
		return LifecycleResult{}, err
	}
	previous, err := b.Health(ctx, room.ID, world.ID)
	if err != nil || !b.lifecycleHealthReady(previous, time.Time{}) {
		return LifecycleResult{}, fmt.Errorf("%w: current runtime health is unavailable", ErrRuntimeUnavailable)
	}
	startedAt := b.now().UTC()
	const reloadScript = `local runtime=rawget(_G,"DSTAdmin"); if runtime~=nil and type(runtime.Reload)=="function" then runtime.Reload() else print("[DST-ADMIN-RUNTIME ERROR] code=RELOAD_UNAVAILABLE") end`
	if _, err := b.send(ctx, room.ID, world.ID, reloadScript, shared.ConsoleModeManaged, ""); err != nil {
		return LifecycleResult{}, fmt.Errorf("%w: send managed reload: %v", ErrRuntimeActivation, err)
	}
	health, err := b.waitForLifecycleHealth(ctx, room.ID, world.ID, startedAt, previous.ProducerInstanceID)
	if err != nil {
		return LifecycleResult{}, err
	}
	return LifecycleResult{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, Mode: LifecycleModeReload, Health: health, Message: "DST Admin 运行时已通过目标节点热重载"}, nil
}

func (b *DistributedBridge) RefreshSnapshots(ctx context.Context, roomID, worldID string) (SnapshotRefreshResult, error) {
	finishLock := requesttiming.Start(ctx, "refresh.lock_wait")
	lock := b.worldLock(roomID, worldID)
	lock.Lock()
	finishLock()
	defer lock.Unlock()
	finishStatus := requesttiming.Start(ctx, "refresh.status")
	statusErr := b.requireRunning(ctx, roomID, worldID)
	finishStatus()
	if statusErr != nil {
		return SnapshotRefreshResult{}, statusErr
	}
	finishPrevious := requesttiming.Start(ctx, "refresh.previous_health")
	previous, _ := b.Health(ctx, roomID, worldID)
	finishPrevious()
	startedAt := b.now().UTC()
	finishSend := requesttiming.Start(ctx, "refresh.console_send")
	_, err := b.send(ctx, roomID, worldID, managedRefreshScript, shared.ConsoleModeProbe, "runtime.snapshot.refresh:"+worldID)
	finishSend()
	if err != nil {
		if errors.Is(err, operationlease.ErrBusy) {
			return SnapshotRefreshResult{}, fmt.Errorf("%w: room operation in progress: %w", ErrRuntimeRefreshDeferred, err)
		}
		return SnapshotRefreshResult{}, fmt.Errorf("%w: send managed refresh: %w", ErrRuntimeRefresh, err)
	}
	deadline := time.NewTimer(b.timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		finishHealth := requesttiming.Start(ctx, "refresh.health_read")
		health, healthErr := b.Health(ctx, roomID, worldID)
		finishHealth()
		if healthErr == nil && validRefreshHealth(health, health.SessionID, health.ShardID, startedAt, previous) {
			finishOutputs := requesttiming.Start(ctx, "refresh.parallel_outputs")
			var players Snapshot
			var playersErr error
			var reads sync.WaitGroup
			reads.Add(1)
			go func() {
				defer reads.Done()
				players, playersErr = b.readPlayersForHealth(ctx, roomID, worldID, health)
			}()
			worldState, worldErr := b.readWorldStateForHealth(ctx, roomID, worldID, health, true)
			reads.Wait()
			finishOutputs()
			if playersErr == nil && worldErr == nil && refreshOutputsMatch(health, players, worldState, health.SessionID, health.ShardID) {
				return SnapshotRefreshResult{Players: players, WorldState: worldState, Health: health}, nil
			}
			lastErr = errors.Join(playersErr, worldErr)
		} else {
			lastErr = healthErr
		}
		finishWait := requesttiming.Start(ctx, "refresh.poll_wait")
		select {
		case <-ctx.Done():
			finishWait()
			return SnapshotRefreshResult{}, ctx.Err()
		case <-deadline.C:
			finishWait()
			return SnapshotRefreshResult{}, fmt.Errorf("%w: target runtime did not publish coherent fresh snapshots: %v", ErrRuntimeRefresh, lastErr)
		case <-ticker.C:
			finishWait()
		}
	}
}

var ErrRuntimeCommandBusy = errors.New("world is processing another runtime operation")

func (b *DistributedBridge) ExecuteCommand(ctx context.Context, roomID, worldID string, request CommandRequest) (CommandReceipt, error) {
	if err := validateCommandRequest(request); err != nil {
		return CommandReceipt{}, err
	}
	lock := b.worldLock(roomID, worldID)
	// Optional catalogue browsing must not queue behind gameplay operations or
	// other browsers. The caller can explicitly try again when the world is free.
	if request.Action == "catalog.entities" {
		if !lock.TryLock() {
			return CommandReceipt{}, ErrRuntimeCommandBusy
		}
	} else {
		lock.Lock()
	}
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return CommandReceipt{}, err
	}
	health, err := b.requireModule(ctx, roomID, worldID, "commands")
	if err != nil {
		return CommandReceipt{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil || len(payload) > maxRuntimeRequestBytes {
		return CommandReceipt{}, fmt.Errorf("%w: invalid command payload", ErrRuntimeRequestInvalid)
	}
	command, document := commandDelivery(request, payload)
	sentAt := b.now().UTC()
	if _, err := b.runtime.SendID(ctx, roomID, worldID, shared.RuntimeConsoleRequest{
		Mode: shared.ConsoleModeManaged, Command: command, CommandDocument: document,
	}); err != nil {
		// Different worlds in one room can briefly contend on the room's
		// delivery lease. Only catalog reads may retry this pre-send rejection.
		if request.Action == "catalog.entities" && errors.Is(err, operationlease.ErrBusy) {
			return CommandReceipt{}, fmt.Errorf("%w: %w", ErrRuntimeCommandBusy, err)
		}
		return CommandReceipt{}, fmt.Errorf("send runtime command: %w", err)
	}
	return b.waitForCommandReceipt(ctx, roomID, worldID, health, request, sentAt)
}

func (b *DistributedBridge) ReadEvents(ctx context.Context, roomID, worldID string) (EventBatch, error) {
	health, err := b.Health(ctx, roomID, worldID)
	if err != nil {
		return EventBatch{}, err
	}
	bundle, err := b.runtime.ReadArtifacts(ctx, roomID, worldID, shared.ArtifactRuntimeEvents)
	if err != nil {
		return EventBatch{}, artifactReadError(err, ErrRuntimeResultAbsent)
	}
	artifacts, err := validatedArtifacts(bundle, shared.ArtifactRuntimeEvents, "events-a.json", "events-b.json")
	if err != nil {
		return EventBatch{}, err
	}
	candidates := make([]EventBatch, 0, len(artifacts))
	var failures error
	for _, artifact := range artifacts {
		value, decodeErr := decodeEventBatchData(artifact.Data, health.SessionID, health.ShardID, b.now().UTC())
		if decodeErr != nil {
			failures = errors.Join(failures, fmt.Errorf("%s: %w", artifact.Name, decodeErr))
			continue
		}
		value.ReadAt = artifact.UpdatedAt.UTC()
		candidates = append(candidates, value)
	}
	return mergeEventBatches(candidates, failures)
}

func (b *DistributedBridge) LatestDiagnostic(ctx context.Context, roomID, worldID string) (DiagnosticReport, error) {
	health, err := b.Health(ctx, roomID, worldID)
	if err != nil {
		return DiagnosticReport{}, err
	}
	return b.readDiagnostic(ctx, roomID, worldID, health, time.Time{}, func(DiagnosticReport) bool { return true })
}

func (b *DistributedBridge) CaptureDiagnostic(ctx context.Context, roomID, worldID string, request DiagnosticRequest) (DiagnosticReport, error) {
	if err := validateDiagnosticRequest(request); err != nil {
		return DiagnosticReport{}, err
	}
	lock := b.worldLock(roomID, worldID)
	lock.Lock()
	defer lock.Unlock()
	health, err := b.requireModule(ctx, roomID, worldID, "diagnostics")
	if err != nil {
		return DiagnosticReport{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil || len(payload) > maxRuntimeRequestBytes {
		return DiagnosticReport{}, ErrRuntimeRequestInvalid
	}
	sentAt := b.now().UTC()
	if _, err := b.send(ctx, roomID, worldID, `DSTAdmin.Diagnostics.CaptureJSON(`+quoteRuntimeLua(string(payload))+`)`, shared.ConsoleModeManaged, ""); err != nil {
		return DiagnosticReport{}, fmt.Errorf("send runtime diagnostic: %w", err)
	}
	deadline := time.NewTimer(defaultDiagnosticTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	for {
		report, readErr := b.readDiagnostic(ctx, roomID, worldID, health, sentAt, func(value DiagnosticReport) bool {
			return value.RequestID == request.RequestID && value.Profile == request.Profile
		})
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

func (b *DistributedBridge) requireRunning(ctx context.Context, roomID, worldID string) error {
	status, err := b.runtime.Status(ctx, roomID, worldID)
	if err != nil {
		return fmt.Errorf("%w: inspect target runtime: %v", ErrRuntimeUnavailable, err)
	}
	if status.State != "running" {
		return fmt.Errorf("%w: shard state is %s", ErrRuntimeUnavailable, status.State)
	}
	return nil
}

func (b *DistributedBridge) requireModule(ctx context.Context, roomID, worldID, moduleName string) (Health, error) {
	if err := b.requireRunning(ctx, roomID, worldID); err != nil {
		return Health{}, err
	}
	health, err := b.Health(ctx, roomID, worldID)
	if err != nil {
		return Health{}, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	module, exists := health.Modules[moduleName]
	if !health.Running || !health.Ready || !exists || !module.Running || !module.Ready || b.now().UTC().Sub(health.ReadAt) > defaultFreshFor {
		return Health{}, fmt.Errorf("%w: %s module is not ready", ErrRuntimeUnavailable, moduleName)
	}
	if module.Busy {
		return Health{}, fmt.Errorf("%w: %w: %s", ErrRuntimeUnavailable, ErrRuntimeCommandBusy, moduleName)
	}
	return health, nil
}

func (b *DistributedBridge) send(ctx context.Context, roomID, worldID, command string, mode shared.ConsoleMode, coalesceKey string) (shared.RuntimeOperationResult, error) {
	return b.runtime.SendID(ctx, roomID, worldID, shared.RuntimeConsoleRequest{Mode: mode, CoalesceKey: coalesceKey, Command: command})
}

func (b *DistributedBridge) waitForLifecycleHealth(ctx context.Context, roomID, worldID string, startedAt time.Time, previousInstanceID string) (Health, error) {
	deadline := time.NewTimer(defaultLifecycleTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	for {
		health, err := b.Health(ctx, roomID, worldID)
		if err == nil && b.lifecycleHealthReady(health, startedAt) && (previousInstanceID == "" || health.ProducerInstanceID != previousInstanceID) {
			return health, nil
		}
		select {
		case <-ctx.Done():
			return Health{}, ctx.Err()
		case <-deadline.C:
			return Health{}, fmt.Errorf("%w: target runtime did not publish fresh healthy state", ErrRuntimeActivation)
		case <-ticker.C:
		}
	}
}

func (b *DistributedBridge) lifecycleHealthReady(health Health, startedAt time.Time) bool {
	if health.ProducerVersion != RuntimeVersion || !health.Running || !health.Ready || health.Writing || health.LastError != nil || health.ConsecutiveFailures != 0 {
		return false
	}
	if !startedAt.IsZero() && health.ReadAt.Before(startedAt) || b.now().UTC().Sub(health.ReadAt) > defaultFreshFor {
		return false
	}
	for _, name := range []string{"worldstate", "commands", "events", "diagnostics", "barriers"} {
		module, exists := health.Modules[name]
		if !exists || !module.Running || !module.Ready || module.Busy || module.LastError != nil {
			return false
		}
	}
	return true
}

func (b *DistributedBridge) waitForCommandReceipt(ctx context.Context, roomID, worldID string, health Health, request CommandRequest, sentAt time.Time) (CommandReceipt, error) {
	timeout := b.timeout
	if request.Action == "catalog.entities" && timeout == defaultCommandTimeout {
		// Catalogue collection yields between game frames and has a 10s Runtime
		// deadline. Allow time for its final receipt without extending Lua commands.
		timeout = 15 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	var lastReadErr error
	for {
		bundle, err := b.runtime.ReadArtifacts(ctx, roomID, worldID, shared.ArtifactRuntimeCommand)
		if err == nil {
			artifacts, bundleErr := validatedArtifacts(bundle, shared.ArtifactRuntimeCommand, "command-receipt-a.json", "command-receipt-b.json")
			if bundleErr != nil {
				return CommandReceipt{}, bundleErr
			}
			candidates := make([]CommandReceipt, 0, len(artifacts))
			var failures error
			for _, artifact := range artifacts {
				value, decodeErr := decodeCommandReceiptData(artifact.Data, health.SessionID, health.ShardID, request, b.now().UTC(), sentAt)
				if decodeErr == nil {
					candidates = append(candidates, value)
				} else if !errors.Is(decodeErr, ErrRuntimeResultAbsent) {
					failures = errors.Join(failures, decodeErr)
				}
			}
			if len(candidates) > 0 {
				sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Sequence > candidates[j].Sequence })
				return candidates[0], nil
			}
			// Klei replaces receipt files asynchronously. A partially written JSON
			// slot is not evidence that the command failed. Keep validating reads
			// until this request's receipt arrives; never resend the command.
			lastReadErr = failures
		} else if errors.Is(err, runtimefiles.ErrArtifactChanging) {
			lastReadErr = err
		} else if !errors.Is(err, os.ErrNotExist) {
			return CommandReceipt{}, err
		}
		select {
		case <-ctx.Done():
			return CommandReceipt{}, ctx.Err()
		case <-deadline.C:
			return CommandReceipt{}, fmt.Errorf("%w: command %s may have executed; it will not be retried automatically (last receipt read: %v)", ErrRuntimeResultAbsent, request.RequestID, lastReadErr)
		case <-ticker.C:
		}
	}
}

func (b *DistributedBridge) readDiagnostic(ctx context.Context, roomID, worldID string, health Health, notBefore time.Time, accept func(DiagnosticReport) bool) (DiagnosticReport, error) {
	bundle, err := b.runtime.ReadArtifacts(ctx, roomID, worldID, shared.ArtifactRuntimeDiagnostics)
	if err != nil {
		return DiagnosticReport{}, artifactReadError(err, ErrRuntimeResultAbsent)
	}
	artifacts, err := validatedArtifacts(bundle, shared.ArtifactRuntimeDiagnostics, "diagnostic-a.json", "diagnostic-b.json")
	if err != nil {
		return DiagnosticReport{}, err
	}
	candidates := make([]DiagnosticReport, 0, len(artifacts))
	var failures error
	for _, artifact := range artifacts {
		value, decodeErr := decodeDiagnosticReportData(artifact.Data, health.SessionID, health.ShardID, b.now().UTC(), notBefore)
		if decodeErr != nil {
			if !errors.Is(decodeErr, ErrRuntimeResultAbsent) {
				failures = errors.Join(failures, fmt.Errorf("%s: %w", artifact.Name, decodeErr))
			}
			continue
		}
		if accept(value) {
			candidates = append(candidates, value)
		}
	}
	if len(candidates) == 0 {
		return DiagnosticReport{}, unavailableWithFailures(ErrRuntimeResultAbsent, failures)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].ProducerInstanceID == candidates[j].ProducerInstanceID {
			return candidates[i].Sequence > candidates[j].Sequence
		}
		return candidates[i].CompletedAt.After(candidates[j].CompletedAt)
	})
	return candidates[0], nil
}

func (b *DistributedBridge) worldLock(roomID, worldID string) *sync.Mutex {
	key := roomID + "\x00" + worldID
	b.locksMu.Lock()
	defer b.locksMu.Unlock()
	if b.locks[key] == nil {
		b.locks[key] = &sync.Mutex{}
	}
	return b.locks[key]
}

func validatedArtifacts(bundle shared.RuntimeArtifactBundle, kind shared.ArtifactKind, allowed ...string) ([]shared.RuntimeArtifact, error) {
	if bundle.Kind != kind || len(bundle.Artifacts) == 0 || len(bundle.Artifacts) > len(allowed) {
		return nil, fmt.Errorf("%w: artifact bundle does not match %s", ErrRuntimeResultInvalid, kind)
	}
	allow := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		allow[name] = true
	}
	seen := make(map[string]bool, len(bundle.Artifacts))
	result := make([]shared.RuntimeArtifact, 0, len(bundle.Artifacts))
	for _, artifact := range bundle.Artifacts {
		if !allow[artifact.Name] || seen[artifact.Name] || artifact.Size != int64(len(artifact.Data)) || artifact.Size < 1 || artifact.UpdatedAt.IsZero() {
			return nil, fmt.Errorf("%w: artifact metadata is invalid", ErrRuntimeResultInvalid)
		}
		sum := sha256.Sum256(artifact.Data)
		if !strings.EqualFold(artifact.SHA256, hex.EncodeToString(sum[:])) {
			return nil, fmt.Errorf("%w: artifact checksum does not match", ErrRuntimeResultInvalid)
		}
		seen[artifact.Name] = true
		result = append(result, artifact)
	}
	return result, nil
}

func mergeEventBatches(candidates []EventBatch, failures error) (EventBatch, error) {
	if len(candidates) == 0 {
		return EventBatch{}, unavailableWithFailures(ErrRuntimeResultAbsent, failures)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return eventBatchNewer(candidates[i], candidates[j]) })
	activeInstance := candidates[0].ProducerInstanceID
	eventsBySequence := make(map[int64]RuntimeEvent)
	selected := candidates[0]
	for _, candidate := range candidates {
		if candidate.ProducerInstanceID != activeInstance {
			continue
		}
		if eventBatchNewer(candidate, selected) {
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
	selected.FirstSequence, selected.LastSequence = sequences[0], sequences[len(sequences)-1]
	return selected, nil
}

func artifactReadError(err, fallback error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fallback
	}
	return err
}

func unavailableWithFailures(base, failures error) error {
	if failures == nil {
		return base
	}
	return fmt.Errorf("%w: %v", base, failures)
}
