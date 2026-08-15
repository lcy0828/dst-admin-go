package modpublication

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type activationCall struct {
	action  string
	worldID string
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

func (f *fakeActivationRuntime) Start(_ context.Context, world WorldPlan, _ RuntimeOperation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, activationCall{action: "start", worldID: world.WorldID})
	if err := f.fail["start:"+world.WorldID]; err != nil {
		return err
	}
	f.states[world.WorldID] = ShardRuntimeObservation{State: "running", SessionExists: true}
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
		if world.IsMaster {
			runtime.markers[world.WorldID] = "[00:00:22]: [Shard] Shard server started on port: 10888"
		} else {
			runtime.markers[world.WorldID] = "[00:00:26]: [Shard] secondary shard LUA is now ready!"
		}
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
	if markers["master"] != "master-shard-server-started" || markers["caves"] != "secondary-shard-lua-ready" {
		t.Fatalf("unexpected load markers: %#v", markers)
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
