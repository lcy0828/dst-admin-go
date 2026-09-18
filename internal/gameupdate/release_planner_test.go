package gameupdate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	dstinstall "dont/internal/dstserver"
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
	mu           sync.Mutex
	observations map[string]shared.RuntimeGameVersionResult
	statuses     map[string]shared.ShardRuntimeStatus
	errors       map[string]error
	logs         map[string]shared.RuntimeLogChunk
	calls        []string
}

func (f *fakeReleaseRuntime) ObserveInstallation(_ context.Context, target ReleaseInstallationPlan) (shared.RuntimeGameVersionResult, error) {
	key := releaseInstallationKey(target.TargetID, target.InstallationID)
	f.mu.Lock()
	f.calls = append(f.calls, "observe:"+key)
	f.mu.Unlock()
	return f.observations[key], f.errors["observe:"+key]
}

func (f *fakeReleaseRuntime) UpdateInstallation(_ context.Context, target ReleaseInstallationPlan, _ runtimedriver.Operation, _ string, _ bool) (shared.RuntimeGameVersionResult, error) {
	key := releaseInstallationKey(target.TargetID, target.InstallationID)
	f.mu.Lock()
	f.calls = append(f.calls, "update:"+key)
	f.mu.Unlock()
	return f.observations[key], f.errors["update:"+key]
}

func (f *fakeReleaseRuntime) Status(_ context.Context, shard ReleaseShardPlan) (shared.ShardRuntimeStatus, error) {
	key := releaseShardKey(shard)
	f.mu.Lock()
	f.calls = append(f.calls, "status:"+key)
	f.mu.Unlock()
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
func (f *fakeReleaseRuntime) ReadLogs(_ context.Context, shard ReleaseShardPlan, _ string, _ int64) (shared.RuntimeLogChunk, error) {
	key := releaseShardKey(shard)
	f.mu.Lock()
	f.calls = append(f.calls, "logs:"+key)
	f.mu.Unlock()
	return f.logs[key], f.errors["logs:"+key]
}

func TestReleasePlannerReadsOfficialGameVersionFromShardLog(t *testing.T) {
	snapshot := ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "local", "default", true),
	}}
	runtime := &fakeReleaseRuntime{
		observations: map[string]shared.RuntimeGameVersionResult{
			releaseInstallationKey("local", "default"): {
				Installed: true, CurrentVersion: "24700692", SteamBuild: "24700692", AvailableBytes: 8 << 30,
				SteamCMDAvailable: true, UpdateSupported: true,
			},
		},
		statuses: map[string]shared.ShardRuntimeStatus{"room-a\x00Master": {State: "stopped"}},
		logs: map[string]shared.RuntimeLogChunk{
			"room-a\x00Master": {Lines: []shared.RuntimeLogLine{
				{Text: "[00:00:01]: Version: 747465"},
				{Text: "[00:00:01]: Don't Starve Together: 747465 OSX"},
			}},
		},
		errors: map[string]error{},
	}
	planner, err := NewReleasePlanner(fakeReleaseSnapshots{snapshot}, runtime, fixedLatest{version: "24700692"}, 2<<30)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Installations) != 1 || plan.Installations[0].GameVersion != "747465" || plan.Installations[0].SteamBuild != "24700692" {
		t.Fatalf("installations=%#v", plan.Installations)
	}
}

func TestParseReleaseGameVersionLogRejectsUnrelatedNumbers(t *testing.T) {
	chunk := shared.RuntimeLogChunk{Data: []byte("Steam Build: 24700692\nVersion: 747465\n")}
	if version := parseReleaseGameVersionLog(chunk); version != "747465" {
		t.Fatalf("version=%q", version)
	}
	if version := parseReleaseGameVersionLog(shared.RuntimeLogChunk{Data: []byte("Steam Build: 24700692\n")}); version != "" {
		t.Fatalf("unexpected version=%q", version)
	}
}

type fixedLatest struct {
	version string
	err     error
}

type fixedOfficialRelease struct {
	version string
	err     error
	stale   bool
}

func (f fixedOfficialRelease) Check(context.Context) (OfficialRelease, error) {
	return OfficialRelease{Version: f.version, Stale: f.stale}, f.err
}

func TestReleasePlannerIgnoresStaleOrFailedOfficialResults(t *testing.T) {
	for _, official := range []fixedOfficialRelease{
		{version: "747465", stale: true},
		{version: "747465", err: errors.New("HTTP 503")},
	} {
		planner := &ReleasePlanner{official: official}
		if got := planner.latestOfficialGameVersion(context.Background()); got != "" {
			t.Fatalf("unconfirmed release used as current: %q", got)
		}
	}
}

func TestReleasePlannerDoesNotUpdateTestBranchAsStable(t *testing.T) {
	snapshot := ReleasePlacementSnapshot{Shards: []ReleaseShardSnapshot{releaseSnapshotShard("room-a", "Master", "agent:node-a", "native", true)}}
	runtime := &fakeReleaseRuntime{observations: map[string]shared.RuntimeGameVersionResult{
		releaseInstallationKey("agent:node-a", "native"): {Installed: true, Branch: "updatebeta", GameVersion: "751622", CurrentVersion: "25135001"},
	}}
	latest := &trackingLatest{version: "24700372"}
	planner, err := NewReleasePlanner(fakeReleaseSnapshots{snapshot}, runtime, latest, 0, fixedOfficialRelease{version: "747465"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	blocked := false
	for _, blocker := range plan.Blockers {
		if blocker.Code == "BRANCH_UNSUPPORTED" {
			blocked = true
		}
	}
	if plan.Ready || !blocked || latest.count() != 0 {
		t.Fatalf("plan=%#v latest calls=%d", plan, latest.count())
	}
}

func (f fixedLatest) Check(context.Context, string, string) (string, bool, error) {
	return f.version, false, f.err
}

func TestReleasePlannerUsesOfficialGameVersionBeforeSteamBuildLookup(t *testing.T) {
	snapshot := ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "local", "default", true),
		releaseSnapshotShard("room-b", "Master", "agent:node-a", "primary", true),
	}}
	runtime := &fakeReleaseRuntime{
		observations: map[string]shared.RuntimeGameVersionResult{
			releaseInstallationKey("local", "default"): {
				Installed: true, GameVersion: "747465", CurrentVersion: "24700692", SteamBuild: "24700692",
				AppID: dstinstall.AppIDGame, UpdateMethod: dstinstall.UpdateMethodSteamClient,
				AvailableBytes: 8 << 30, SteamCMDAvailable: true, UpdateSupported: false,
			},
			releaseInstallationKey("agent:node-a", "primary"): {
				Installed: true, GameVersion: "747465", CurrentVersion: "24700372", SteamBuild: "24700372",
				AppID: dstinstall.AppIDDedicatedServer, UpdateMethod: dstinstall.UpdateMethodSteamCMD,
				AvailableBytes: 8 << 30, SteamCMDAvailable: true, UpdateSupported: true,
			},
		},
		statuses: map[string]shared.ShardRuntimeStatus{
			"room-a\x00Master": {State: "stopped"},
			"room-b\x00Master": {State: "stopped"},
		},
		errors: map[string]error{},
	}
	latest := &trackingLatest{version: "should-not-be-used"}
	planner, err := NewReleasePlanner(
		fakeReleaseSnapshots{snapshot}, runtime, latest, 2<<30,
		fixedOfficialRelease{version: "747465"},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || plan.UpdateRequired || len(plan.Installations) != 2 {
		t.Fatalf("official version plan = %#v", plan)
	}
	desiredByTarget := make(map[string]string, len(plan.Installations))
	for _, installation := range plan.Installations {
		if !installation.UpToDate || installation.GameVersion != "747465" {
			t.Fatalf("official version installation = %#v", installation)
		}
		desiredByTarget[installation.TargetID] = installation.DesiredVersion
	}
	if desiredByTarget["local"] != "24700692" || desiredByTarget["agent:node-a"] != "24700372" {
		t.Fatalf("platform builds = %#v", desiredByTarget)
	}
	if latest.count() != 0 {
		t.Fatalf("Steam latest build lookups = %d, want 0", latest.count())
	}
}

type trackingLatest struct {
	mu      sync.Mutex
	version string
	calls   []string
}

func (f *trackingLatest) Check(_ context.Context, appID, _ string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, appID)
	return f.version, false, nil
}

func (f *trackingLatest) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type boundedPreviewRuntime struct {
	*fakeReleaseRuntime
	gateMu    sync.Mutex
	active    int
	maxActive int
	delay     time.Duration
	slow      string
}

func (r *boundedPreviewRuntime) ObserveInstallation(ctx context.Context, target ReleaseInstallationPlan) (shared.RuntimeGameVersionResult, error) {
	r.gateMu.Lock()
	r.active++
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	r.gateMu.Unlock()
	defer func() {
		r.gateMu.Lock()
		r.active--
		r.gateMu.Unlock()
	}()
	if target.TargetID == r.slow {
		<-ctx.Done()
		return shared.RuntimeGameVersionResult{}, ctx.Err()
	}
	if r.delay > 0 {
		timer := time.NewTimer(r.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return shared.RuntimeGameVersionResult{}, ctx.Err()
		}
	}
	return shared.RuntimeGameVersionResult{
		Installed: true, CurrentVersion: "700", AvailableBytes: 8 << 30,
		SteamCMDAvailable: true, UpdateSupported: true,
	}, nil
}

func releaseSnapshotShard(roomID, worldID, targetID, installationID string, master bool) ReleaseShardSnapshot {
	return ReleaseShardSnapshot{
		Room:  rooms.Room{ID: roomID, Name: roomID, DirectoryName: "Cluster_" + roomID, Managed: true},
		World: rooms.World{ID: worldID, Name: worldID, DirectoryName: worldID, IsMaster: master},
		Target: agents.RuntimeTarget{
			ID: targetID, Name: targetID, OS: "linux", Arch: "amd64", Online: true, Configured: true,
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
	if plan.Installations[0].OS != "linux" || plan.Installations[0].Arch != "amd64" {
		t.Fatalf("target platform=%s/%s", plan.Installations[0].OS, plan.Installations[0].Arch)
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

func TestReleasePlannerBoundsParallelInstallationChecks(t *testing.T) {
	shards := []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "agent:node-a", "primary", true),
		releaseSnapshotShard("room-b", "Master", "agent:node-b", "primary", true),
		releaseSnapshotShard("room-c", "Master", "agent:node-c", "primary", true),
	}
	statuses := map[string]shared.ShardRuntimeStatus{
		"room-a\x00Master": {State: "stopped"},
		"room-b\x00Master": {State: "stopped"},
		"room-c\x00Master": {State: "stopped"},
	}
	runtime := &boundedPreviewRuntime{
		fakeReleaseRuntime: &fakeReleaseRuntime{statuses: statuses, errors: map[string]error{}},
		delay:              30 * time.Millisecond,
	}
	planner, _ := NewReleasePlanner(
		fakeReleaseSnapshots{ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: shards}},
		runtime, fixedLatest{version: "701"}, 1,
	)
	planner.previewConcurrency = 2
	planner.targetTimeout = time.Second
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || len(plan.Installations) != 3 || runtime.maxActive != 2 {
		t.Fatalf("plan=%#v maxActive=%d", plan, runtime.maxActive)
	}
}

func TestReleasePlannerReturnsPartialPlanWhenOneInstallationTimesOut(t *testing.T) {
	shards := []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "agent:fast", "primary", true),
		releaseSnapshotShard("room-b", "Master", "agent:slow", "primary", true),
	}
	runtime := &boundedPreviewRuntime{
		fakeReleaseRuntime: &fakeReleaseRuntime{
			statuses: map[string]shared.ShardRuntimeStatus{"room-a\x00Master": {State: "stopped"}},
			errors:   map[string]error{},
		},
		slow: "agent:slow",
	}
	planner, _ := NewReleasePlanner(
		fakeReleaseSnapshots{ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: shards}},
		runtime, fixedLatest{version: "701"}, 1,
	)
	planner.targetTimeout = 20 * time.Millisecond
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || !plan.UpdateRequired || len(plan.Installations) != 2 {
		t.Fatalf("plan=%#v", plan)
	}
	var fast, slow *ReleaseInstallationPlan
	for index := range plan.Installations {
		target := &plan.Installations[index]
		switch target.TargetID {
		case "agent:fast":
			fast = target
		case "agent:slow":
			slow = target
		}
	}
	if fast == nil || fast.CurrentVersion != "700" || fast.DesiredVersion != "701" || len(fast.Blockers) != 0 {
		t.Fatalf("fast target=%#v", fast)
	}
	if slow == nil || len(slow.Blockers) != 1 || slow.Blockers[0].Code != "VERSION_CHECK_TIMEOUT" {
		t.Fatalf("slow target=%#v", slow)
	}
}

func TestReleasePlannerSkipsLatestBuildLookupWhenInstallationCannotBeObserved(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*ReleaseShardSnapshot, *fakeReleaseRuntime)
		blockerCode string
	}{
		{
			name: "offline target",
			configure: func(shard *ReleaseShardSnapshot, _ *fakeReleaseRuntime) {
				shard.Target.Online = false
			},
			blockerCode: "TARGET_OFFLINE",
		},
		{
			name: "observation failure",
			configure: func(shard *ReleaseShardSnapshot, runtime *fakeReleaseRuntime) {
				runtime.errors["observe:"+releaseInstallationKey(shard.Target.ID, shard.Target.Config.InstallationID)] = errors.New("agent unavailable")
			},
			blockerCode: "VERSION_OBSERVE_FAILED",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			shard := releaseSnapshotShard("room-a", "Master", "agent:node-a", "primary", true)
			runtime := &fakeReleaseRuntime{
				observations: map[string]shared.RuntimeGameVersionResult{},
				statuses:     map[string]shared.ShardRuntimeStatus{},
				errors:       map[string]error{},
			}
			test.configure(&shard, runtime)
			latest := &trackingLatest{version: "701"}
			planner, _ := NewReleasePlanner(
				fakeReleaseSnapshots{ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{shard}}},
				runtime, latest, 1,
			)
			plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Ready || len(plan.Installations) != 1 || len(plan.Installations[0].Blockers) == 0 || plan.Installations[0].Blockers[0].Code != test.blockerCode {
				t.Fatalf("plan=%#v", plan)
			}
			if latest.count() != 0 {
				t.Fatalf("latest build lookup calls=%d", latest.count())
			}
		})
	}
}

func TestReleasePlannerScopesPreviewToSelectedRuntimeTargets(t *testing.T) {
	snapshot := ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "agent:node-a", "primary", true),
		releaseSnapshotShard("room-b", "Master", "agent:node-b", "primary", true),
	}}
	runtime := &fakeReleaseRuntime{
		observations: map[string]shared.RuntimeGameVersionResult{
			releaseInstallationKey("agent:node-a", "primary"): {Installed: true, CurrentVersion: "700", AvailableBytes: 8 << 30, SteamCMDAvailable: true, UpdateSupported: true},
			releaseInstallationKey("agent:node-b", "primary"): {Installed: true, CurrentVersion: "699", AvailableBytes: 8 << 30, SteamCMDAvailable: true, UpdateSupported: true},
		},
		statuses: map[string]shared.ShardRuntimeStatus{
			"room-a\x00Master": {State: "running", SessionExists: true},
			"room-b\x00Master": {State: "running", SessionExists: true},
		},
		errors: map[string]error{},
	}
	planner, _ := NewReleasePlanner(fakeReleaseSnapshots{snapshot}, runtime, fixedLatest{version: "701"}, 1)
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{TargetIDs: []string{"agent:node-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Installations) != 1 || plan.Installations[0].TargetID != "agent:node-b" || len(plan.AffectedRoomIDs) != 1 || plan.AffectedRoomIDs[0] != "room-b" {
		t.Fatalf("scoped plan=%#v", plan)
	}
	for _, call := range runtime.calls {
		if strings.Contains(call, "agent:node-a") {
			t.Fatalf("unselected target was observed: %v", runtime.calls)
		}
	}
	if _, err := planner.Preview(context.Background(), ReleasePreviewRequest{TargetIDs: []string{"invalid target"}}); !errors.Is(err, ErrReleaseInvalid) {
		t.Fatalf("invalid target error=%v", err)
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

func TestReleasePlannerRequiresAdvertisedCapabilityForLocalRuntime(t *testing.T) {
	local := releaseSnapshotShard("room-a", "Master", "local", "default", true)
	local.Target.Capabilities = []string{"runtime.local"}
	runtime := &fakeReleaseRuntime{observations: map[string]shared.RuntimeGameVersionResult{}, statuses: map[string]shared.ShardRuntimeStatus{}, errors: map[string]error{}}
	planner, _ := NewReleasePlanner(
		fakeReleaseSnapshots{ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{local}}},
		runtime,
		fixedLatest{version: "701"},
		1,
	)
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || len(plan.Blockers) != 1 || plan.Blockers[0].Code != "CAPABILITY_MISSING" {
		t.Fatalf("local capability preflight=%#v", plan)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("local target without capability was observed: %v", runtime.calls)
	}
}

func TestReleasePlannerRejectsChangedDesiredBuild(t *testing.T) {
	snapshot := ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "agent:node-a", "primary", true),
	}}
	runtime := &fakeReleaseRuntime{
		observations: map[string]shared.RuntimeGameVersionResult{
			releaseInstallationKey("agent:node-a", "primary"): {Installed: true, CurrentVersion: "699", AvailableBytes: 8 << 30, SteamCMDAvailable: true, UpdateSupported: true},
		},
		statuses: map[string]shared.ShardRuntimeStatus{"room-a\x00Master": {State: "stopped"}}, errors: map[string]error{},
	}
	planner, _ := NewReleasePlanner(fakeReleaseSnapshots{snapshot}, runtime, fixedLatest{version: "701"}, 1)
	_, err := planner.Preview(context.Background(), ReleasePreviewRequest{DesiredVersion: "700"})
	if !errors.Is(err, ErrDesiredVersionChanged) {
		t.Fatalf("error=%v", err)
	}
}

func TestReleasePlannerClassifiesLatestBuildLookupFailures(t *testing.T) {
	snapshot := ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "agent:node-a", "primary", true),
	}}
	runtime := &fakeReleaseRuntime{
		observations: map[string]shared.RuntimeGameVersionResult{
			releaseInstallationKey("agent:node-a", "primary"): {Installed: true, CurrentVersion: "700", AvailableBytes: 8 << 30, SteamCMDAvailable: true, UpdateSupported: true},
		},
		statuses: map[string]shared.ShardRuntimeStatus{"room-a\x00Master": {State: "stopped"}}, errors: map[string]error{},
	}
	planner, _ := NewReleasePlanner(fakeReleaseSnapshots{snapshot}, runtime, fixedLatest{err: errors.New("Steam unavailable")}, 1)
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || len(plan.Installations) != 1 || len(plan.Blockers) != 1 || plan.Blockers[0].Code != "LATEST_BUILD_UNAVAILABLE" || plan.Blockers[0].TargetID != "agent:node-a" {
		t.Fatalf("plan=%#v", plan)
	}
}

func TestReleasePlannerTreatsCurrentSteamClientInstallationAsInformational(t *testing.T) {
	snapshot := ReleasePlacementSnapshot{TopologyRevision: string(make([]byte, 64)), Shards: []ReleaseShardSnapshot{
		releaseSnapshotShard("room-a", "Master", "local", "default", true),
	}}
	snapshot.Shards[0].Target.OS, snapshot.Shards[0].Target.Arch = "darwin", "arm64"
	runtime := &fakeReleaseRuntime{
		observations: map[string]shared.RuntimeGameVersionResult{
			releaseInstallationKey("local", "default"): {
				Installed: true, AppID: dstinstall.AppIDGame, UpdateMethod: dstinstall.UpdateMethodSteamClient,
				CurrentVersion: "24700692", AvailableBytes: 23 << 30, UpdateSupported: false,
			},
		},
		statuses: map[string]shared.ShardRuntimeStatus{"room-a\x00Master": {State: "running", SessionExists: true}}, errors: map[string]error{},
	}
	planner, _ := NewReleasePlanner(fakeReleaseSnapshots{snapshot}, runtime, fixedLatest{version: "24700692"}, 2<<30)
	plan, err := planner.Preview(context.Background(), ReleasePreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || plan.UpdateRequired || plan.DesiredVersion != "24700692" || len(plan.Blockers) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
	target := plan.Installations[0]
	if !target.UpToDate || target.AppID != dstinstall.AppIDGame || target.UpdateMethod != dstinstall.UpdateMethodSteamClient || target.UpdateSupported || target.OS != "darwin" || target.Arch != "arm64" {
		t.Fatalf("target = %#v", target)
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
	previewed := value
	previewed.ID, previewed.Stage = "release-previewed", ReleaseStagePreviewed
	previewed.SourceJobID = "job-previewed"
	if _, err := store.Create(previewed); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Get(value.ID)
	if err != nil || recovered.Stage != ReleaseStageRecoveryRequired || recovered.ErrorCode != "SERVICE_RESTARTED" {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	if recovered, err := store.Get(previewed.ID); err != nil || recovered.Stage != ReleaseStageRecoveryRequired {
		t.Fatalf("interruption before protection has no recovery entry: %#v %v", recovered, err)
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
