package gameupdate

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/shared"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type fakeReleaseSnapshots struct{ value ReleasePlacementSnapshot }

func (f fakeReleaseSnapshots) Snapshot(context.Context) (ReleasePlacementSnapshot, error) {
	return f.value, nil
}

type fakeReleaseRuntime struct {
	observations map[string]shared.RuntimeGameVersionResult
	statuses     map[string]shared.ShardRuntimeStatus
	errors       map[string]error
	calls        []string
}

func (f *fakeReleaseRuntime) ObserveInstallation(_ context.Context, target ReleaseInstallationPlan) (shared.RuntimeGameVersionResult, error) {
	key := releaseInstallationKey(target.TargetID, target.InstallationID)
	f.calls = append(f.calls, "observe:"+key)
	return f.observations[key], f.errors["observe:"+key]
}

func (f *fakeReleaseRuntime) UpdateInstallation(_ context.Context, target ReleaseInstallationPlan, _ runtimedriver.Operation, _ string, _ bool) (shared.RuntimeGameVersionResult, error) {
	key := releaseInstallationKey(target.TargetID, target.InstallationID)
	f.calls = append(f.calls, "update:"+key)
	return f.observations[key], f.errors["update:"+key]
}

func (f *fakeReleaseRuntime) Status(_ context.Context, shard ReleaseShardPlan) (shared.ShardRuntimeStatus, error) {
	key := releaseShardKey(shard)
	f.calls = append(f.calls, "status:"+key)
	return f.statuses[key], f.errors["status:"+key]
}

func (f *fakeReleaseRuntime) Stop(context.Context, ReleaseShardPlan, runtimedriver.Operation) error {
	return nil
}
func (f *fakeReleaseRuntime) Start(context.Context, ReleaseShardPlan, runtimedriver.Operation) error {
	return nil
}
func (f *fakeReleaseRuntime) CaptureLogCursor(context.Context, ReleaseShardPlan) (string, int64, error) {
	return "log", 0, nil
}
func (f *fakeReleaseRuntime) ReadLogs(context.Context, ReleaseShardPlan, string, int64) (shared.RuntimeLogChunk, error) {
	return shared.RuntimeLogChunk{}, nil
}

type fixedLatest struct {
	version string
	err     error
}

func (f fixedLatest) Check(context.Context, string, string) (string, bool, error) {
	return f.version, false, f.err
}

func releaseSnapshotShard(roomID, worldID, targetID, installationID string, master bool) ReleaseShardSnapshot {
	return ReleaseShardSnapshot{
		Room:  rooms.Room{ID: roomID, Name: roomID, DirectoryName: "Cluster_" + roomID, Managed: true},
		World: rooms.World{ID: worldID, Name: worldID, DirectoryName: worldID, IsMaster: master},
		Target: agents.RuntimeTarget{
			ID: targetID, Name: targetID, Online: true, Configured: true,
			Capabilities: []string{RequiredUpdateCapability, "shard.control.v1"}, Config: agents.RuntimeConfig{InstallationID: installationID},
		},
		InventoryAvailable: true, InventoryHasShard: true, TopologyRevision: "revision-" + roomID,
	}
}

func TestReleasePlannerDeduplicatesSharedInstallationAcrossRooms(t *testing.T) {
	snapshot := ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "agent:node-a", "primary", true),
		releaseSnapshotShard("room-a", "Caves", "agent:node-a", "primary", false),
		releaseSnapshotShard("room-b", "Master", "agent:node-a", "primary", true),
		releaseSnapshotShard("room-b", "Forest", "agent:node-b", "secondary", false),
	}}
	runtime := &fakeReleaseRuntime{
		observations: map[string]shared.RuntimeGameVersionResult{
			releaseInstallationKey("agent:node-a", "primary"):   {Installed: true, CurrentVersion: "700", AvailableBytes: 8 << 30, SteamCMDAvailable: true, UpdateSupported: true},
			releaseInstallationKey("agent:node-b", "secondary"): {Installed: true, CurrentVersion: "699", AvailableBytes: 8 << 30, SteamCMDAvailable: true, UpdateSupported: true},
		},
		statuses: map[string]shared.ShardRuntimeStatus{
			"room-a\x00Master": {State: "running", SessionExists: true}, "room-a\x00Caves": {State: "stopped"},
			"room-b\x00Master": {State: "running", SessionExists: true}, "room-b\x00Forest": {State: "running", SessionExists: true},
		}, errors: map[string]error{},
	}
	planner, err := NewReleasePlanner(fakeReleaseSnapshots{snapshot}, runtime, fixedLatest{version: "701"}, 2<<30)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || len(plan.Installations) != 2 || len(plan.AffectedRoomIDs) != 2 || !plan.Policy.RestartRunning || plan.Policy.LoadConfirmation != ReleaseLoadConfirmationLogs {
		t.Fatalf("plan=%#v", plan)
	}
	if len(plan.Installations[0].Shards) != 3 || plan.Installations[0].RunningShards != 2 {
		t.Fatalf("deduplicated target=%#v", plan.Installations[0])
	}
	observeCalls := 0
	for _, call := range runtime.calls {
		if len(call) > 8 && call[:8] == "observe:" {
			observeCalls++
		}
	}
	if observeCalls != 2 {
		t.Fatalf("observe calls=%v", runtime.calls)
	}
	if err := validateReleasePlan(plan); err != nil {
		t.Fatalf("validate plan: %v", err)
	}
}

func TestReleasePlannerReportsIndependentPreflightBlockers(t *testing.T) {
	offline := releaseSnapshotShard("room-a", "Master", "agent:offline", "primary", true)
	offline.Target.Online = false
	offline.InventoryAvailable = false
	offline.InventoryStale = true
	offline.InventoryHasShard = false
	limited := releaseSnapshotShard("room-b", "Master", "agent:limited", "primary", true)
	limited.Target.Capabilities = []string{"shard.control.v1"}
	diskLow := releaseSnapshotShard("room-c", "Master", "agent:disk", "primary", true)
	runtime := &fakeReleaseRuntime{
		observations: map[string]shared.RuntimeGameVersionResult{
			releaseInstallationKey("agent:disk", "primary"): {Installed: true, CurrentVersion: "700", AvailableBytes: 1, SteamCMDAvailable: true, UpdateSupported: true},
		},
		statuses: map[string]shared.ShardRuntimeStatus{"room-c\x00Master": {State: "stopped"}}, errors: map[string]error{},
	}
	planner, _ := NewReleasePlanner(fakeReleaseSnapshots{ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{offline, limited, diskLow}}}, runtime, fixedLatest{version: "701"}, 1024)
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	codes := make(map[string]bool)
	for _, blocker := range plan.Blockers {
		codes[blocker.Code] = true
	}
	for _, code := range []string{"TARGET_OFFLINE", "INVENTORY_STALE", "SHARD_INVENTORY_MISSING", "CAPABILITY_MISSING", "DISK_INSUFFICIENT"} {
		if !codes[code] {
			t.Fatalf("missing blocker %s in %#v", code, plan.Blockers)
		}
	}
	if plan.Ready || validateReleasePlan(plan) == nil {
		t.Fatalf("blocked plan accepted: %#v", plan)
	}
}

func TestReleasePlannerRejectsChangedDesiredBuild(t *testing.T) {
	planner, _ := NewReleasePlanner(fakeReleaseSnapshots{}, &fakeReleaseRuntime{}, fixedLatest{version: "701"}, 1)
	_, err := planner.Preview(context.Background(), ReleasePreviewRequest{DesiredVersion: "700"})
	if !errors.Is(err, ErrDesiredVersionChanged) {
		t.Fatalf("error=%v", err)
	}
}

func TestReleasePlanHashIgnoresObservedRuntimeStateButPreservesRestartIntent(t *testing.T) {
	plan := readyReleasePlanForStore(t)
	original := plan.PlanHash
	plan.Installations[0].Shards[0].RuntimeState = "starting"
	hash, err := calculateReleasePlanHash(plan)
	if err != nil {
		t.Fatal(err)
	}
	if hash != original {
		t.Fatalf("runtime observation changed plan hash: original=%s current=%s", original, hash)
	}
	plan.Installations[0].Shards[0].WasRunning = false
	hash, err = calculateReleasePlanHash(plan)
	if err != nil {
		t.Fatal(err)
	}
	if hash == original {
		t.Fatal("restart intent did not change plan hash")
	}
}

func TestReleaseStoreRecoversInterruptedStages(t *testing.T) {
	db := openGameUpdateTestDB(t)
	store := NewReleaseStore(db, "release_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	plan := readyReleasePlanForStore(t)
	now := time.Now().UTC()
	value := Release{ID: "release-1", Stage: ReleaseStageUpdating, Plan: plan, CreatedAt: now, UpdatedAt: now}
	for _, target := range plan.Installations {
		value.Installations = append(value.Installations, ReleaseInstallationResult{TargetID: target.TargetID, InstallationID: target.InstallationID, Stage: ReleaseStageUpdating, UpdatedAt: now})
		for _, shard := range target.Shards {
			value.Shards = append(value.Shards, ReleaseShardResult{RoomID: shard.RoomID, WorldID: shard.WorldID, TargetID: shard.TargetID, InstallationID: shard.InstallationID, IsMaster: shard.IsMaster, WasRunning: shard.WasRunning, Stage: ReleaseStageStaged, UpdatedAt: now})
		}
	}
	if _, err := store.Create(value); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Get(value.ID)
	if err != nil || recovered.Stage != ReleaseStageRecoveryRequired || recovered.ErrorCode != "SERVICE_RESTARTED" {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
}

func openGameUpdateTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func readyReleasePlanForStore(t *testing.T) ReleasePlan {
	t.Helper()
	shard := ReleaseShardPlan{RoomID: "room-a", RoomName: "Room", RoomDirectory: "Cluster_1", WorldID: "Master", WorldName: "Master", WorldDirectory: "Master", IsMaster: true, TargetID: "agent:node", InstallationID: "primary", TopologyRevision: "revision-a", RuntimeState: "running", WasRunning: true}
	target := ReleaseInstallationPlan{TargetID: "agent:node", TargetName: "Node", InstallationID: "primary", Online: true, InventoryFresh: true, Capabilities: []string{RequiredUpdateCapability}, Installed: true, CurrentVersion: "700", DesiredVersion: "701", AvailableBytes: 8 << 30, RequiredBytes: 2 << 30, SteamCMDAvailable: true, UpdateSupported: true, RunningShards: 1, Shards: []ReleaseShardPlan{shard}}
	plan := ReleasePlan{Version: ReleasePlanVersion, DesiredVersion: "701", TopologyRevision: string(make([]byte, 64)), Policy: ReleasePolicy{RestartRunning: true, LoadConfirmation: ReleaseLoadConfirmationLogs, TimeoutSeconds: 300}, AffectedRoomIDs: []string{"room-a"}, Installations: []ReleaseInstallationPlan{target}, Ready: true, UpdateRequired: true, CreatedAt: time.Now().UTC()}
	var err error
	plan.PlanHash, err = calculateReleasePlanHash(plan)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
