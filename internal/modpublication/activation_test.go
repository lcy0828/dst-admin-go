package modpublication

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/operationlease"
	"dont/shared"
)

type activationCall struct {
	action  string
	worldID string
}

type activationNotifier struct {
	roomIDs       []string
	action        string
	source        string
	jobID         string
	calls         int
	borrowedLease bool
	wait          time.Duration
	afterWait     func()
	err           error
}

func (n *activationNotifier) BeforeOperations(ctx context.Context, roomIDs []string, action, source, jobID string) error {
	n.roomIDs = append([]string(nil), roomIDs...)
	n.action, n.source, n.jobID = action, source, jobID
	for _, roomID := range roomIDs {
		if _, ok := operationlease.BorrowedLease(ctx, roomID); ok {
			n.borrowedLease = true
		}
	}
	n.calls++
	if n.wait > 0 {
		timer := time.NewTimer(n.wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if n.afterWait != nil {
		n.afterWait()
	}
	return n.err
}

type fakeActivationRuntime struct {
	mu      sync.Mutex
	states  map[string]ShardRuntimeObservation
	markers map[string]string
	fail    map[string]error
	calls   []activationCall
}

func (f *fakeActivationRuntime) Status(_ context.Context, world WorldPlan) (ShardRuntimeObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["status:"+world.WorldID]; err != nil {
		return ShardRuntimeObservation{}, err
	}
	return f.states[world.WorldID], nil
}

func (f *fakeActivationRuntime) CaptureLogCursor(_ context.Context, world WorldPlan) (LogCursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["cursor:"+world.WorldID]; err != nil {
		return LogCursor{}, err
	}
	return LogCursor{FileID: "before-" + world.WorldID, Cursor: 100}, nil
}

func (f *fakeActivationRuntime) Stop(_ context.Context, world WorldPlan, _ RuntimeOperation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, activationCall{action: "stop", worldID: world.WorldID})
	if err := f.fail["stop:"+world.WorldID]; err != nil {
		return err
	}
	f.states[world.WorldID] = ShardRuntimeObservation{State: "stopped"}
	return nil
}

func (f *fakeActivationRuntime) Start(_ context.Context, world WorldPlan, operation RuntimeOperation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, activationCall{action: "start", worldID: world.WorldID})
	if err := f.fail["start:"+world.WorldID]; err != nil {
		return err
	}
	f.states[world.WorldID] = ShardRuntimeObservation{State: "running", SessionExists: true, RuntimeMode: operation.RuntimeMode}
	return nil
}

func (f *fakeActivationRuntime) ReadLogs(_ context.Context, world WorldPlan, cursor LogCursor) (ShardLogObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["logs:"+world.WorldID]; err != nil {
		return ShardLogObservation{}, err
	}
	lines := []string{}
	if marker := f.markers[world.WorldID]; marker != "" {
		lines = append(lines, marker)
	}
	return ShardLogObservation{Cursor: LogCursor{FileID: cursor.FileID, Cursor: cursor.Cursor + 100}, Lines: lines}, nil
}

func activationRuntimeFor(worlds []ManagedWorld) *fakeActivationRuntime {
	runtime := &fakeActivationRuntime{
		states: make(map[string]ShardRuntimeObservation), markers: make(map[string]string), fail: make(map[string]error),
	}
	for _, world := range worlds {
		runtime.states[world.WorldID] = ShardRuntimeObservation{State: "running", SessionExists: true}
		lines := make([]string, 0, len(world.Mods)+1)
		for _, mod := range world.Mods {
			lines = append(lines, "[00:00:18]: Registering Mod workshop-"+mod.WorkshopID)
		}
		if world.IsMaster {
			lines = append(lines, "[00:00:22]: [Shard] Shard server started on port: 10888")
		} else {
			lines = append(lines, "[00:00:26]: [Shard] secondary shard LUA is now ready!")
		}
		runtime.markers[world.WorldID] = strings.Join(lines, "\n")
	}
	return runtime
}

func coordinatorWithActivation(t *testing.T, app *testApplication, runtime ActivationRuntime) *Coordinator {
	t.Helper()
	coordinator, err := NewCoordinator(app.planner, app.runtime, app.leases, app.backups, app.store, time.Minute, runtime)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.activationPollInterval = time.Millisecond
	return coordinator
}

func restartActivationPolicy() ActivationPolicy {
	return ActivationPolicy{Mode: ActivationModeRestart, LoadConfirmation: LoadConfirmationLogs, TimeoutSeconds: 30}
}

func TestPublicationActivationCoordinatesShardOrderAndConfirmsLogs(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	worlds[0].IsMaster = true
	activation := activationRuntimeFor(worlds)
	app := newTestApplication(t, worlds, placements)
	coordinator := coordinatorWithActivation(t, app, activation)
	notifier := &activationNotifier{}
	coordinator.ConfigureNotifier(notifier)
	plan, err := coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := coordinator.Publish(context.Background(), PublishRequest{
		ID: "publication-activated", SourceJobID: "job-publication-activated", Plan: plan, Activation: restartActivationPolicy(),
	})
	if err != nil || publication.Status != StatusSucceeded || publication.Activation.Status != ActivationStatusSucceeded || publication.RestartRequired {
		t.Fatalf("unexpected activated publication: %#v err=%v", publication, err)
	}
	activation.mu.Lock()
	calls := append([]activationCall(nil), activation.calls...)
	activation.mu.Unlock()
	expected := []activationCall{{"stop", "caves"}, {"stop", "master"}, {"start", "master"}, {"start", "caves"}}
	if len(calls) != len(expected) {
		t.Fatalf("unexpected activation calls: %#v", calls)
	}
	for index := range expected {
		if calls[index] != expected[index] {
			t.Fatalf("activation order mismatch: got %#v want %#v", calls, expected)
		}
	}
	markers := map[string]string{}
	for _, shard := range publication.Activation.Shards {
		if shard.Status != ActivationStatusSucceeded || shard.LoadConfirmedAt == nil || !shard.WasRunning {
			t.Fatalf("shard was not confirmed: %#v", shard)
		}
		markers[shard.WorldID] = shard.LoadMarker
	}
	if markers["master"] != "master-shard-server-started;mods=1/1" || markers["caves"] != "secondary-shard-lua-ready;mods=1/1" {
		t.Fatalf("unexpected load markers: %#v", markers)
	}
	if notifier.calls != 1 || notifier.action != "restart" || notifier.source != "mod_sync" || notifier.jobID != "job-publication-activated" || !notifier.borrowedLease || len(notifier.roomIDs) != 1 || notifier.roomIDs[0] != "room-a" {
		t.Fatalf("notifier=%#v", notifier)
	}
}

func TestOrdinaryStartConfirmationProjectsLoadedReplicaState(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	worlds[0].IsMaster = true
	activation := activationRuntimeFor(worlds)
	app := newTestApplication(t, worlds, placements)
	coordinator := coordinatorWithActivation(t, app, activation)
	replicas := NewReplicaStore(app.db, "ordinary_start_loaded_")
	if err := replicas.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ConfigureReplicaStore(replicas); err != nil {
		t.Fatal(err)
	}
	plan, err := coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := replicas.SetDesired(plan); err != nil {
		t.Fatal(err)
	}
	for _, target := range plan.Targets {
		if err := replicas.MarkCompleted(target, plan.PlanHash); err != nil {
			t.Fatal(err)
		}
	}
	if err := coordinator.ConfirmWorldLoaded(context.Background(), plan, worlds[0].RoomID, worlds[0].WorldID); err != nil {
		t.Fatal(err)
	}
	state, err := replicas.Room(worlds[0].RoomID)
	if err != nil {
		t.Fatal(err)
	}
	loaded := false
	for _, item := range state.Items {
		for _, target := range item.Targets {
			for _, world := range target.Worlds {
				if world.WorldID == worlds[0].WorldID && world.Loaded && world.ObservedRevision == plan.PlanHash {
					loaded = true
				}
			}
		}
	}
	if !loaded {
		t.Fatalf("ordinary start did not project loaded state: %#v", state)
	}
}

func TestActivationLoadConfirmationRequiresEveryExpectedMod(t *testing.T) {
	world := WorldPlan{
		RoomID: "room-a", WorldID: "master", WorldDirectory: "Master", IsMaster: true,
		Mods: []ContentArtifact{{WorkshopID: "111"}, {WorkshopID: "222"}},
	}
	runtime := &fakeActivationRuntime{
		states: map[string]ShardRuntimeObservation{"master": {State: "running", SessionExists: true}},
		markers: map[string]string{
			"master": "[00:00:18]: Loading mod: workshop-111\n[00:00:22]: [Shard] Shard server started on port: 10888",
		},
		fail: make(map[string]error),
	}
	coordinator := &Coordinator{activation: runtime, activationPollInterval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	_, _, err := coordinator.confirmShardLoaded(ctx, world, LogCursor{}, LoadConfirmationLogs)
	if err == nil || !strings.Contains(err.Error(), "222") || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing mod error=%v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("ready shard waited for confirmation timeout before reporting missing mod: %s", elapsed)
	}
}

func TestActivationLoadConfirmationRejectsNoModsRegistered(t *testing.T) {
	world := WorldPlan{
		RoomID: "room-a", WorldID: "master", WorldDirectory: "Master", IsMaster: true,
		Mods: []ContentArtifact{{WorkshopID: "111"}},
	}
	runtime := &fakeActivationRuntime{
		states:  map[string]ShardRuntimeObservation{"master": {State: "running", SessionExists: true}},
		markers: map[string]string{"master": "[00:00:18]: No mods registered.\n[00:00:22]: [Shard] Shard server started on port: 10888"},
		fail:    make(map[string]error),
	}
	coordinator := &Coordinator{activation: runtime, activationPollInterval: time.Millisecond}
	_, _, err := coordinator.confirmShardLoaded(context.Background(), world, LogCursor{}, LoadConfirmationLogs)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "no registered mods") {
		t.Fatalf("no-mod registration error=%v", err)
	}
}

func TestActivationLoadConfirmationAllowsWorldWithoutMods(t *testing.T) {
	world := WorldPlan{RoomID: "room-a", WorldID: "master", WorldDirectory: "Master", IsMaster: true}
	runtime := &fakeActivationRuntime{
		states:  map[string]ShardRuntimeObservation{"master": {State: "running", SessionExists: true}},
		markers: map[string]string{"master": "[00:00:18]: No mods registered.\n[00:00:22]: [Shard] Shard server started on port: 10888"},
		fail:    make(map[string]error),
	}
	coordinator := &Coordinator{activation: runtime, activationPollInterval: time.Millisecond}
	marker, _, err := coordinator.confirmShardLoaded(context.Background(), world, LogCursor{}, LoadConfirmationLogs)
	if err != nil || marker != "master-shard-server-started" {
		t.Fatalf("marker=%q error=%v", marker, err)
	}
}

func TestPublicationActivationCancellationPreventsShardRestart(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	worlds[0].IsMaster = true
	activation := activationRuntimeFor(worlds)
	app := newTestApplication(t, worlds, placements)
	coordinator := coordinatorWithActivation(t, app, activation)
	coordinator.ConfigureNotifier(&activationNotifier{err: context.Canceled})
	plan, err := coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := coordinator.Publish(context.Background(), PublishRequest{
		ID: "publication-notification-canceled", Plan: plan, Activation: restartActivationPolicy(),
	})
	if !errors.Is(err, context.Canceled) || len(activation.calls) != 0 || publication.Status != StatusSucceeded || !publication.RestartRequired {
		t.Fatalf("publication=%#v calls=%#v err=%v", publication, activation.calls, err)
	}
}

func TestPublicationActivationDoesNotStartStoppedShard(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	worlds[0].IsMaster = true
	activation := activationRuntimeFor(worlds)
	activation.states["caves"] = ShardRuntimeObservation{State: "stopped"}
	app := newTestApplication(t, worlds, placements)
	coordinator := coordinatorWithActivation(t, app, activation)
	plan, err := coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := coordinator.Publish(context.Background(), PublishRequest{ID: "publication-stopped-shard", Plan: plan, Activation: restartActivationPolicy()})
	if err != nil || publication.Activation.Status != ActivationStatusSucceeded || publication.RestartRequired {
		t.Fatalf("unexpected activation: %#v err=%v", publication, err)
	}
	for _, call := range activation.calls {
		if call.worldID == "caves" {
			t.Fatalf("stopped shard was mutated: %#v", activation.calls)
		}
	}
	for _, shard := range publication.Activation.Shards {
		if shard.WorldID == "caves" && (shard.Status != ActivationStatusSkipped || shard.WasRunning) {
			t.Fatalf("stopped shard was not recorded as skipped: %#v", shard)
		}
	}
}

func TestActivationFailurePreservesCommittedPublication(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	worlds[0].IsMaster = true
	activation := activationRuntimeFor(worlds)
	activation.markers["master"] = ""
	app := newTestApplication(t, worlds, placements)
	coordinator := coordinatorWithActivation(t, app, activation)
	plan, err := coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	publication, err := coordinator.Publish(ctx, PublishRequest{ID: "publication-activation-timeout", Plan: plan, Activation: restartActivationPolicy()})
	if !errors.Is(err, ErrActivationFailed) || publication.Status != StatusSucceeded || publication.Outcome != OutcomeFull ||
		!publication.CommitDecision || publication.Activation.Status != ActivationStatusFailed || !publication.RestartRequired {
		t.Fatalf("activation failure changed publication commit: %#v err=%v", publication, err)
	}
	if app.runtime.count("rollback") != 0 {
		t.Fatalf("committed Mod files were rolled back after activation failure: %#v", app.runtime.calls)
	}
}

func TestActivationRecoveryRestartsShardsFromPersistedRunningSnapshot(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	worlds[0].IsMaster = true
	activation := activationRuntimeFor(worlds)
	app := newTestApplication(t, worlds, placements)
	coordinator := coordinatorWithActivation(t, app, activation)
	notifier := &activationNotifier{}
	coordinator.ConfigureNotifier(notifier)
	plan, err := coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := coordinator.Publish(context.Background(), PublishRequest{ID: "publication-activation-crash", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	publication.RestartRequired = true
	publication.Activation = Activation{Policy: restartActivationPolicy(), Status: ActivationStatusRestarting, RequestedAt: &now, Shards: activationShards(plan, now)}
	for index := range publication.Activation.Shards {
		publication.Activation.Shards[index].WasRunning = true
	}
	if publication, err = app.store.Save(publication); err != nil {
		t.Fatal(err)
	}
	activation.mu.Lock()
	activation.calls = nil
	for _, world := range worlds {
		activation.states[world.WorldID] = ShardRuntimeObservation{State: "stopped"}
	}
	activation.mu.Unlock()

	recovered, err := coordinator.RecoverOne(context.Background(), publication.ID, "job-activation-recovery")
	if err != nil || recovered.Activation.Status != ActivationStatusSucceeded || recovered.RestartRequired {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	activation.mu.Lock()
	calls := append([]activationCall(nil), activation.calls...)
	activation.mu.Unlock()
	expected := []activationCall{{"stop", "caves"}, {"stop", "master"}, {"start", "master"}, {"start", "caves"}}
	if len(calls) != len(expected) {
		t.Fatalf("activation recovery calls=%#v", calls)
	}
	for index := range expected {
		if calls[index] != expected[index] {
			t.Fatalf("activation recovery order=%#v", calls)
		}
	}
	if notifier.calls != 1 || notifier.jobID != "job-activation-recovery" {
		t.Fatalf("recovery notifier=%#v", notifier)
	}
}

func TestManualActivationUsesActivationJobIDForNotification(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	worlds[0].IsMaster = true
	activation := activationRuntimeFor(worlds)
	app := newTestApplication(t, worlds, placements)
	coordinator := coordinatorWithActivation(t, app, activation)
	notifier := &activationNotifier{}
	coordinator.ConfigureNotifier(notifier)
	plan, err := coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := coordinator.Publish(context.Background(), PublishRequest{ID: "publication-manual-restart", SourceJobID: "original-publication-job", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	publication, err = coordinator.Activate(context.Background(), publication.ID, "activation-job", restartActivationPolicy())
	if err != nil || publication.Activation.Status != ActivationStatusSucceeded {
		t.Fatalf("publication=%#v err=%v", publication, err)
	}
	if notifier.calls != 1 || notifier.jobID != "activation-job" {
		t.Fatalf("activation notifier=%#v", notifier)
	}
}

func TestActivationNotificationRenewsBorrowedLeaseDuringCountdown(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	worlds[0].IsMaster = true
	activation := activationRuntimeFor(worlds)
	app := newTestApplication(t, worlds, placements)
	coordinator := coordinatorWithActivation(t, app, activation)
	coordinator.leaseTTL = 30 * time.Millisecond
	plan, err := coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := coordinator.Publish(context.Background(), PublishRequest{ID: "publication-lease-renewal", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	baseline := app.leases.renewalCount()
	renewalsDuringNotification := 0
	coordinator.ConfigureNotifier(&activationNotifier{
		wait: 35 * time.Millisecond,
		afterWait: func() {
			renewalsDuringNotification = app.leases.renewalCount() - baseline
		},
	})
	if _, err := coordinator.Activate(context.Background(), publication.ID, "activation-renewal-job", restartActivationPolicy()); err != nil {
		t.Fatal(err)
	}
	if renewalsDuringNotification < 2 {
		t.Fatalf("lease was not renewed throughout notification wait: %d renewals", renewalsDuringNotification)
	}
}

func TestManualActivationPolicyPreservesExistingPublicationFlow(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-manual-activation", Plan: plan})
	if err != nil || publication.Status != StatusSucceeded || publication.Activation.Status != ActivationStatusSkipped ||
		publication.Activation.Policy.Mode != ActivationModeManual || !publication.RestartRequired {
		t.Fatalf("manual compatibility flow changed: %#v err=%v", publication, err)
	}
}

func TestModActivationRetainsRuntimeModesDuringRecovery(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	worlds[0].IsMaster = true
	activation := activationRuntimeFor(worlds)
	modes := map[string]shared.RuntimePerformanceMode{"master": shared.RuntimePerformanceModeLuaJIT, "caves": shared.RuntimePerformanceModeArenaGC}
	for key, mode := range modes {
		status := activation.states[key]
		status.RuntimeMode = mode
		activation.states[key] = status
	}
	app := newTestApplication(t, worlds, placements)
	coordinator := coordinatorWithActivation(t, app, activation)
	plan, err := coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := coordinator.Publish(context.Background(), PublishRequest{ID: "publication-modes", Plan: plan, Activation: restartActivationPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	for _, shard := range publication.Activation.Shards {
		if shard.RuntimeMode != modes[shard.WorldID] || activation.states[shard.WorldID].RuntimeMode != modes[shard.WorldID] {
			t.Fatalf("mode lost: %#v", shard)
		}
	}
	publication.RestartRequired = true
	publication.Activation.Status = ActivationStatusRestarting
	if _, err = app.store.Save(publication); err != nil {
		t.Fatal(err)
	}
	for key := range activation.states {
		activation.states[key] = ShardRuntimeObservation{State: "stopped"}
	}
	recovered, err := coordinator.RecoverOne(context.Background(), publication.ID)
	if err != nil || recovered.Activation.Status != ActivationStatusSucceeded {
		t.Fatalf("recovery=%#v err=%v", recovered, err)
	}
	for key, mode := range modes {
		if activation.states[key].RuntimeMode != mode {
			t.Fatalf("recovery mode %s=%s", key, activation.states[key].RuntimeMode)
		}
	}
}
