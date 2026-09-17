package gameupdate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"dont/internal/operationlease"
	"dont/internal/runtimedriver"
	"dont/shared"
)

type coordinatorRuntime struct {
	mu             sync.Mutex
	versions       map[string]string
	states         map[string]shared.ShardRuntimeStatus
	updateErrors   map[string]error
	updateVersions map[string]string
	logErrors      map[string]error
	events         []string
	updateCounts   map[string]int
}

func (r *coordinatorRuntime) ObserveInstallation(_ context.Context, target ReleaseInstallationPlan) (shared.RuntimeGameVersionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := releaseInstallationKey(target.TargetID, target.InstallationID)
	return shared.RuntimeGameVersionResult{
		Installed: true, CurrentVersion: r.versions[key], AvailableBytes: 8 << 30,
		SteamCMDAvailable: true, UpdateSupported: true,
	}, nil
}

func (r *coordinatorRuntime) UpdateInstallation(_ context.Context, target ReleaseInstallationPlan, _ runtimedriver.Operation, expected string, _ bool) (shared.RuntimeGameVersionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := releaseInstallationKey(target.TargetID, target.InstallationID)
	r.events = append(r.events, "update:"+key)
	r.updateCounts[key]++
	if err := r.updateErrors[key]; err != nil {
		return shared.RuntimeGameVersionResult{Installed: true, CurrentVersion: r.versions[key]}, err
	}
	version := expected
	if overridden := r.updateVersions[key]; overridden != "" {
		version = overridden
	}
	r.versions[key] = version
	return shared.RuntimeGameVersionResult{Installed: true, CurrentVersion: version}, nil
}

func (r *coordinatorRuntime) Status(_ context.Context, shard ReleaseShardPlan) (shared.ShardRuntimeStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.states[releaseShardKey(shard)], nil
}

func (r *coordinatorRuntime) Stop(_ context.Context, shard ReleaseShardPlan, _ runtimedriver.Operation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := releaseShardKey(shard)
	r.events = append(r.events, "stop:"+key)
	r.states[key] = shared.ShardRuntimeStatus{State: "stopped"}
	return nil
}

func (r *coordinatorRuntime) Start(_ context.Context, shard ReleaseShardPlan, _ runtimedriver.Operation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := releaseShardKey(shard)
	r.events = append(r.events, "start:"+key)
	r.states[key] = shared.ShardRuntimeStatus{State: "running", SessionExists: true}
	return nil
}

func (r *coordinatorRuntime) CaptureLogCursor(context.Context, ReleaseShardPlan) (string, int64, error) {
	return "log", 0, nil
}

func (r *coordinatorRuntime) ReadLogs(_ context.Context, shard ReleaseShardPlan, _ string, _ int64) (shared.RuntimeLogChunk, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.logErrors[releaseShardKey(shard)]; err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	marker := "[Shard] Secondary shard Lua is now ready!"
	if shard.IsMaster {
		marker = "[Shard] Shard server started on port: 10999"
	}
	return shared.RuntimeLogChunk{FileID: "log", Cursor: 1, Lines: []shared.RuntimeLogLine{{Text: marker}}}, nil
}

func (r *coordinatorRuntime) mutationEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type coordinatorLeases struct {
	mu       sync.Mutex
	next     uint64
	renewals int
}

func (l *coordinatorLeases) Acquire(_ context.Context, roomID, operationKey string, ttl time.Duration) (operationlease.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	return operationlease.Lease{
		RoomID: roomID, LeaseID: fmt.Sprintf("lease-%d", l.next), OperationKey: operationKey,
		FencingToken: l.next, ExpiresAt: time.Now().UTC().Add(ttl),
	}, nil
}

func (l *coordinatorLeases) Renew(_ context.Context, lease operationlease.Lease, ttl time.Duration) (operationlease.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renewals++
	lease.ExpiresAt = time.Now().UTC().Add(ttl)
	return lease, nil
}

func (*coordinatorLeases) Release(operationlease.Lease) error { return nil }

type coordinatorBackups struct {
	mu  sync.Mutex
	ids []string
}

type releaseMutationObserver struct {
	mu      sync.Mutex
	targets []string
}

func (o *releaseMutationObserver) RuntimeTargetChanged(targetID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.targets = append(o.targets, targetID)
}

func (b *coordinatorBackups) CreateProtection(_ context.Context, roomID, _, _ string, _ *operationlease.Lease) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := "backup-" + roomID
	b.ids = append(b.ids, id)
	return id, nil
}

type releaseCoordinatorFixture struct {
	coordinator *ReleaseCoordinator
	runtime     *coordinatorRuntime
	leases      *coordinatorLeases
	plan        ReleasePlan
}

func newReleaseCoordinatorFixture(t *testing.T, confirmation ReleaseLoadConfirmation) releaseCoordinatorFixture {
	t.Helper()
	snapshot := ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "agent:node-a", "primary", true),
		releaseSnapshotShard("room-a", "Caves", "agent:node-a", "primary", false),
		releaseSnapshotShard("room-b", "Master", "agent:node-b", "primary", true),
		releaseSnapshotShard("room-b", "Archive", "agent:node-b", "primary", false),
	}}
	runtime := &coordinatorRuntime{
		versions: map[string]string{
			releaseInstallationKey("agent:node-a", "primary"): "700",
			releaseInstallationKey("agent:node-b", "primary"): "700",
		},
		states: map[string]shared.ShardRuntimeStatus{
			"room-a\x00Master":  {State: "running", SessionExists: true},
			"room-a\x00Caves":   {State: "running", SessionExists: true},
			"room-b\x00Master":  {State: "running", SessionExists: true},
			"room-b\x00Archive": {State: "stopped"},
		},
		updateErrors: make(map[string]error), updateVersions: make(map[string]string),
		logErrors: make(map[string]error), updateCounts: make(map[string]int),
	}
	planner, err := NewReleasePlanner(fakeReleaseSnapshots{snapshot}, runtime, fixedLatest{version: "701"}, 2<<30)
	if err != nil {
		t.Fatal(err)
	}
	restart := true
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{Policy: ReleasePolicyInput{
		RestartRunning: &restart, LoadConfirmation: confirmation, TimeoutSeconds: 30,
	}})
	if err != nil {
		t.Fatal(err)
	}
	store := NewReleaseStore(openGameUpdateTestDB(t), "coordinator_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	leases := &coordinatorLeases{}
	coordinator, err := NewReleaseCoordinator(planner, runtime, leases, &coordinatorBackups{}, store)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.pollInterval = time.Millisecond
	coordinator.renewInterval = time.Millisecond
	return releaseCoordinatorFixture{coordinator: coordinator, runtime: runtime, leases: leases, plan: plan}
}

func TestReleaseCoordinatorOrdersStopUpdateAndRestart(t *testing.T) {
	fixture := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	notifier := &updateNotifier{}
	fixture.coordinator.ConfigureNotifier(notifier)
	mutations := &releaseMutationObserver{}
	if err := fixture.coordinator.ConfigureMutationObserver(mutations); err != nil {
		t.Fatal(err)
	}
	value, err := fixture.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "release-order", Plan: fixture.plan})
	if err != nil || value.Stage != ReleaseStageSucceeded {
		t.Fatalf("release=%#v error=%v", value, err)
	}
	events := fixture.runtime.mutationEvents()
	assertEventBefore(t, events, "stop:room-a\x00Caves", "stop:room-a\x00Master")
	assertEventBefore(t, events, "update:agent:node-a\x00primary", "start:room-a\x00Master")
	assertEventBefore(t, events, "update:agent:node-b\x00primary", "start:room-a\x00Master")
	assertEventBefore(t, events, "start:room-a\x00Master", "start:room-a\x00Caves")
	for _, event := range events {
		if event == "start:room-b\x00Archive" {
			t.Fatalf("originally stopped shard was started: %v", events)
		}
	}
	if len(notifier.calls) != 1 || notifier.calls[0].source != "game_update" || notifier.calls[0].action != "restart" ||
		!reflect.DeepEqual(notifier.calls[0].roomIDs, []string{"room-a", "room-b"}) {
		t.Fatalf("notifier calls=%#v", notifier.calls)
	}
	mutations.mu.Lock()
	defer mutations.mu.Unlock()
	if !reflect.DeepEqual(mutations.targets, []string{"agent:node-a", "agent:node-b"}) {
		t.Fatalf("mutation targets=%v", mutations.targets)
	}
}

func TestReleaseCoordinatorKeepsAllShardsStoppedWhenAnyInstallationFails(t *testing.T) {
	fixture := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	mutations := &releaseMutationObserver{}
	if err := fixture.coordinator.ConfigureMutationObserver(mutations); err != nil {
		t.Fatal(err)
	}
	failedKey := releaseInstallationKey("agent:node-b", "primary")
	fixture.runtime.updateErrors[failedKey] = errors.New("steamcmd failed")
	value, err := fixture.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "release-failure", Plan: fixture.plan})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) || value.Stage != ReleaseStageRecoveryRequired {
		t.Fatalf("release=%#v error=%v", value, err)
	}
	for _, event := range fixture.runtime.mutationEvents() {
		if len(event) >= 6 && event[:6] == "start:" {
			t.Fatalf("shard restarted before every installation succeeded: %v", fixture.runtime.mutationEvents())
		}
	}
	for key, status := range fixture.runtime.states {
		if key != "room-b\x00Archive" && (status.State != "stopped" || status.SessionExists) {
			t.Fatalf("shard %q was not kept stopped: %#v", key, status)
		}
	}
	mutations.mu.Lock()
	defer mutations.mu.Unlock()
	if !reflect.DeepEqual(mutations.targets, []string{"agent:node-a", "agent:node-b"}) {
		t.Fatalf("failed release mutation targets=%v", mutations.targets)
	}
}

func TestReleaseCoordinatorRejectsInexactUpdatedVersion(t *testing.T) {
	fixture := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	key := releaseInstallationKey("agent:node-a", "primary")
	fixture.runtime.updateVersions[key] = "700"
	value, err := fixture.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "release-mismatch", Plan: fixture.plan})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) || value.Stage != ReleaseStageRecoveryRequired {
		t.Fatalf("release=%#v error=%v", value, err)
	}
	result := releaseInstallationResult(&value, "agent:node-a", "primary")
	if result == nil || result.ErrorCode != "UPDATE_FAILED" {
		t.Fatalf("installation result=%#v", result)
	}
}

func TestReleaseCoordinatorRetrySkipsVerifiedInstallation(t *testing.T) {
	fixture := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	firstKey := releaseInstallationKey("agent:node-a", "primary")
	failedKey := releaseInstallationKey("agent:node-b", "primary")
	fixture.runtime.updateErrors[failedKey] = errors.New("temporary failure")
	if _, err := fixture.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "release-retry", Plan: fixture.plan}); !errors.Is(err, ErrReleaseRecoveryNeeded) {
		t.Fatalf("first publish error=%v", err)
	}
	notifier := &updateNotifier{}
	fixture.coordinator.ConfigureNotifier(notifier)
	delete(fixture.runtime.updateErrors, failedKey)
	value, err := fixture.coordinator.Retry(context.Background(), "release-retry", "retry-job")
	if err != nil || value.Stage != ReleaseStageSucceeded {
		t.Fatalf("retry release=%#v error=%v", value, err)
	}
	if fixture.runtime.updateCounts[firstKey] != 1 || fixture.runtime.updateCounts[failedKey] != 2 {
		t.Fatalf("update counts=%v", fixture.runtime.updateCounts)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].jobID != "retry-job" {
		t.Fatalf("retry notifier calls=%#v", notifier.calls)
	}
}

func TestReleaseCoordinatorPersistsLogConfirmationFailure(t *testing.T) {
	fixture := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationLogs)
	fixture.runtime.logErrors["room-a\x00Master"] = errors.New("log unavailable")
	value, err := fixture.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "release-log", Plan: fixture.plan})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) || value.Stage != ReleaseStageRecoveryRequired {
		t.Fatalf("release=%#v error=%v", value, err)
	}
	result := releaseShardResult(&value, "room-a", "Master")
	if result == nil || result.ErrorCode != "LOAD_CONFIRMATION_FAILED" {
		t.Fatalf("shard result=%#v", result)
	}
	stored, getErr := fixture.coordinator.Get(value.ID)
	if getErr != nil || stored.Stage != ReleaseStageRecoveryRequired || releaseShardResult(&stored, "room-a", "Master").ErrorCode != "LOAD_CONFIRMATION_FAILED" {
		t.Fatalf("stored=%#v error=%v", stored, getErr)
	}
}

func assertEventBefore(t *testing.T, events []string, first, second string) {
	t.Helper()
	firstIndex, secondIndex := -1, -1
	for index, event := range events {
		if event == first && firstIndex < 0 {
			firstIndex = index
		}
		if event == second && secondIndex < 0 {
			secondIndex = index
		}
	}
	if firstIndex < 0 || secondIndex < 0 || firstIndex >= secondIndex {
		t.Fatalf("expected %q before %q in %v", first, second, events)
	}
}
