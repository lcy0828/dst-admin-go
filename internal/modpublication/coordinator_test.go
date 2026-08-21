package modpublication

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type fakeCatalog struct {
	worlds []ManagedWorld
	err    error
}

func (f *fakeCatalog) ManagedWorlds(context.Context) ([]ManagedWorld, error) {
	return append([]ManagedWorld(nil), f.worlds...), f.err
}

type fakePlacements struct {
	snapshot PlacementSnapshot
	err      error
}

func (f *fakePlacements) AppliedPlacements(context.Context) (PlacementSnapshot, error) {
	return f.snapshot, f.err
}

type fakeContent struct {
	artifacts map[string]ContentArtifact
}

func (f *fakeContent) Resolve(_ context.Context, requirement ModRequirement) (ContentArtifact, error) {
	key := requirement.WorkshopID
	if requirement.TreeSHA256 != "" {
		key += ":" + requirement.TreeSHA256
	}
	artifact, exists := f.artifacts[key]
	if !exists {
		return ContentArtifact{}, fmt.Errorf("artifact %s not found", key)
	}
	return artifact, nil
}

type runtimeCall struct {
	action, target, installation, publication string
	idempotency, attempt                      string
	plan                                      TargetPlan
}

type fakeRuntime struct {
	mu           sync.Mutex
	observations map[string]RuntimeObservation
	failures     map[string]int
	calls        []runtimeCall
}

func (f *fakeRuntime) Observe(_ context.Context, placement AppliedPlacement) (RuntimeObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, exists := f.observations[targetIdentity(placement.TargetID, placement.InstallationID)]
	if !exists {
		return RuntimeObservation{}, errors.New("target missing")
	}
	return value, nil
}

func (f *fakeRuntime) EnsureCache(_ context.Context, target TargetPlan, operation RuntimeOperation) error {
	return f.invoke("ensure-cache", target, operation)
}
func (f *fakeRuntime) Prepare(_ context.Context, target TargetPlan, operation RuntimeOperation) error {
	return f.invoke("prepare", target, operation)
}
func (f *fakeRuntime) Publish(_ context.Context, target TargetPlan, operation RuntimeOperation) error {
	return f.invoke("publish", target, operation)
}
func (f *fakeRuntime) Rollback(_ context.Context, target TargetPlan, operation RuntimeOperation) error {
	return f.invoke("rollback", target, operation)
}
func (f *fakeRuntime) Complete(_ context.Context, target TargetPlan, operation RuntimeOperation) error {
	return f.invoke("complete", target, operation)
}

func (f *fakeRuntime) invoke(action string, target TargetPlan, operation RuntimeOperation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	attempt := ""
	if len(operation.Fences) > 0 {
		attempt = operation.Fences[0].OperationKey
	}
	f.calls = append(f.calls, runtimeCall{
		action: action, target: target.TargetID, installation: target.InstallationID,
		publication: operation.PublicationID, idempotency: operation.IdempotencyKey, attempt: attempt, plan: target,
	})
	key := action + ":" + targetIdentity(target.TargetID, target.InstallationID)
	if f.failures[key] > 0 {
		f.failures[key]--
		return errors.New("forced " + key + " failure")
	}
	return nil
}

func (f *fakeRuntime) count(action string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call.action == action {
			count++
		}
	}
	return count
}

func (f *fakeRuntime) has(action, target string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.action == action && call.target == target {
			return true
		}
	}
	return false
}

type fakeLease struct {
	mu       sync.Mutex
	next     uint64
	active   map[string]Fence
	renewals int
}

func (f *fakeLease) Acquire(_ context.Context, roomID, owner string, ttl time.Duration) (Fence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.active[roomID]; exists {
		return Fence{}, ErrConflict
	}
	f.next++
	fence := Fence{RoomID: roomID, LeaseID: fmt.Sprintf("lease-%08d", f.next), OperationKey: owner, FencingToken: f.next, ExpiresAt: time.Now().Add(ttl)}
	f.active[roomID] = fence
	return fence, nil
}

func (f *fakeLease) Renew(_ context.Context, fence Fence, ttl time.Duration) (Fence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, exists := f.active[fence.RoomID]
	if !exists || current.LeaseID != fence.LeaseID || current.FencingToken != fence.FencingToken {
		return Fence{}, ErrConflict
	}
	current.ExpiresAt = time.Now().Add(ttl)
	f.active[fence.RoomID] = current
	f.renewals++
	return current, nil
}

func (f *fakeLease) renewalCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewals
}

func (f *fakeLease) Release(fence Fence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, exists := f.active[fence.RoomID]
	if exists && current.LeaseID == fence.LeaseID {
		delete(f.active, fence.RoomID)
	}
	return nil
}

type fakeBackup struct {
	mu    sync.Mutex
	calls []ProtectionRequest
	err   error
}

func (f *fakeBackup) CreateProtection(_ context.Context, request ProtectionRequest) (ProtectionBackup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, request)
	if f.err != nil {
		return ProtectionBackup{}, f.err
	}
	ids := make([]string, 0, len(request.RoomIDs))
	for _, roomID := range request.RoomIDs {
		ids = append(ids, "backup-"+roomID)
	}
	return ProtectionBackup{IDs: ids}, nil
}

type testApplication struct {
	db          *gorm.DB
	catalog     *fakeCatalog
	placements  *fakePlacements
	content     *fakeContent
	runtime     *fakeRuntime
	leases      *fakeLease
	backups     *fakeBackup
	store       *Store
	planner     *Planner
	coordinator *Coordinator
}

func newTestApplication(t *testing.T, worlds []ManagedWorld, placements []AppliedPlacement) *testApplication {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.LogMode(false)
	app := &testApplication{
		db: db, catalog: &fakeCatalog{worlds: worlds},
		placements: &fakePlacements{snapshot: PlacementSnapshot{TopologyRevision: "topology-revision-0001", Placements: placements}},
		content:    &fakeContent{artifacts: make(map[string]ContentArtifact)},
		runtime:    &fakeRuntime{observations: make(map[string]RuntimeObservation), failures: make(map[string]int)},
		leases:     &fakeLease{active: make(map[string]Fence)}, backups: &fakeBackup{},
	}
	for _, world := range worlds {
		for _, mod := range world.Mods {
			key := mod.WorkshopID
			tree := mod.TreeSHA256
			if tree == "" {
				tree = hashBytes([]byte("tree-" + mod.WorkshopID))
			} else {
				key += ":" + tree
			}
			app.content.artifacts[key] = ContentArtifact{
				WorkshopID: mod.WorkshopID, TreeSHA256: tree, ManifestSHA256: hashBytes([]byte("manifest-" + tree)),
				Size: 1024, FileCount: 2, SourceRef: "content://" + mod.WorkshopID + "/" + tree,
			}
		}
	}
	for _, placement := range placements {
		key := targetIdentity(placement.TargetID, placement.InstallationID)
		app.runtime.observations[key] = RuntimeObservation{
			TargetID: placement.TargetID, NodeID: placement.NodeID, InstallationID: placement.InstallationID,
			Online: true, Capabilities: []string{RequiredCapability}, AvailableBytes: 1 << 30,
			Version: "2.5.0", CachedTreeSHA256: make(map[string]bool),
		}
	}
	app.store = NewStore(db, "test_")
	if err := app.store.Migrate(); err != nil {
		t.Fatal(err)
	}
	app.planner, err = NewPlanner(app.catalog, app.placements, app.content, app.runtime, "2.5.0")
	if err != nil {
		t.Fatal(err)
	}
	app.coordinator, err = NewCoordinator(app.planner, app.runtime, app.leases, app.backups, app.store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return app
}

func twoTargetWorlds() ([]ManagedWorld, []AppliedPlacement) {
	mod := ModRequirement{WorkshopID: "1392778117"}
	worlds := []ManagedWorld{
		{RoomID: "room-a", RoomDirectory: "Cluster_A", WorldID: "master", WorldDirectory: "Master", Mods: []ModRequirement{mod}, ModOverrides: []byte("return {master=true}\n")},
		{RoomID: "room-a", RoomDirectory: "Cluster_A", WorldID: "caves", WorldDirectory: "Caves", Mods: []ModRequirement{mod}, ModOverrides: []byte("return {caves=true}\n")},
	}
	placements := []AppliedPlacement{
		{RoomID: "room-a", WorldID: "master", TargetID: "target-a", NodeID: "node-a", InstallationID: "install-a"},
		{RoomID: "room-a", WorldID: "caves", TargetID: "target-b", NodeID: "node-b", InstallationID: "install-b"},
	}
	return worlds, placements
}

func TestPrepareFailureRollsBackEveryTouchedTarget(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	app.runtime.failures["prepare:"+targetIdentity("target-b", "install-b")] = 1
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-prepare-failure", SourceJobID: "job-prepare-failure", Plan: plan})
	if err == nil || publication.Status != StatusRolledBack || publication.Outcome != OutcomeNone || publication.CommitDecision {
		t.Fatalf("unexpected publication: %#v err=%v", publication, err)
	}
	if app.runtime.count("publish") != 0 || !app.runtime.has("rollback", "target-a") || !app.runtime.has("rollback", "target-b") {
		t.Fatalf("prepare rollback calls are incomplete: %#v", app.runtime.calls)
	}
}

func TestPartialPublishFailureRollsBackAllTargets(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	app.runtime.failures["publish:"+targetIdentity("target-b", "install-b")] = 1
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-publish-failure", Plan: plan})
	if err == nil || publication.Status != StatusRolledBack || publication.Outcome != OutcomeNone || publication.CommitDecision {
		t.Fatalf("unexpected publication: %#v err=%v", publication, err)
	}
	if app.runtime.count("publish") != 2 || app.runtime.count("rollback") != 2 {
		t.Fatalf("all prepared targets were not rolled back: %#v", app.runtime.calls)
	}
}

func TestCommitDecisionPreventsRollbackAndRecoveryCompletes(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	app.runtime.failures["complete:"+targetIdentity("target-b", "install-b")] = 1
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-complete-recovery", Plan: plan})
	if !errors.Is(err, ErrRecoveryRequired) || publication.Status != StatusRecoveryRequired || !publication.CommitDecision || publication.Outcome != OutcomeFull {
		t.Fatalf("unexpected committed failure: %#v err=%v", publication, err)
	}
	if app.runtime.count("rollback") != 0 {
		t.Fatalf("committed publication was rolled back: %#v", app.runtime.calls)
	}
	restarted, err := NewCoordinator(app.planner, app.runtime, app.leases, app.backups, app.store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	results, err := restarted.Recover(context.Background())
	if err != nil || len(results) != 1 || results[0].Status != StatusSucceeded || !results[0].CommitDecision {
		t.Fatalf("recovery did not complete commit: %#v err=%v", results, err)
	}
	if app.runtime.count("rollback") != 0 || app.runtime.count("complete") < 4 {
		t.Fatalf("recovery used wrong action: %#v", app.runtime.calls)
	}
}

func TestRepeatedRecoveryUsesFreshRuntimeIdempotencyDomain(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	app.runtime.failures["complete:"+targetIdentity("target-b", "install-b")] = 2
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	publication, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-repeat-recovery", Plan: plan})
	if !errors.Is(err, ErrRecoveryRequired) || publication.Status != StatusRecoveryRequired {
		t.Fatalf("initial publication did not require recovery: %#v err=%v", publication, err)
	}
	publication, err = app.coordinator.RecoverOne(context.Background(), publication.ID)
	if !errors.Is(err, ErrRecoveryRequired) || publication.Status != StatusRecoveryRequired {
		t.Fatalf("first recovery unexpectedly completed: %#v err=%v", publication, err)
	}
	publication, err = app.coordinator.RecoverOne(context.Background(), publication.ID)
	if err != nil || publication.Status != StatusSucceeded {
		t.Fatalf("second recovery did not complete: %#v err=%v", publication, err)
	}

	app.runtime.mu.Lock()
	defer app.runtime.mu.Unlock()
	seenAttempts := map[string]bool{}
	seenKeys := map[string]bool{}
	for _, call := range app.runtime.calls {
		if call.action != "complete" || call.target != "target-b" {
			continue
		}
		if seenAttempts[call.attempt] || seenKeys[call.idempotency] {
			t.Fatalf("recovery reused an Agent idempotency domain: %#v", app.runtime.calls)
		}
		seenAttempts[call.attempt], seenKeys[call.idempotency] = true, true
	}
	if len(seenAttempts) != 3 {
		t.Fatalf("expected initial completion and two recovery attempts, got %#v", app.runtime.calls)
	}
}

func TestControlPlaneRestartWithoutCommitRollsBackAllTargets(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	publication := Publication{
		ID: "publication-control-restart", RoomID: "room-a", Status: StatusPublishing, Outcome: OutcomePartial,
		Plan: plan, RestartRequired: true, CreatedAt: now, UpdatedAt: now,
	}
	for _, target := range plan.Targets {
		published := target.TargetID == "target-a"
		publication.Targets = append(publication.Targets, TargetResult{
			TargetID: target.TargetID, InstallationID: target.InstallationID, Status: StatusPublishing,
			CacheEnsured: true, Prepared: true, Published: published, UpdatedAt: now,
		})
	}
	if _, err := app.store.Create(publication); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewCoordinator(app.planner, app.runtime, app.leases, app.backups, app.store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	results, err := restarted.Recover(context.Background())
	if err != nil || len(results) != 1 || results[0].Status != StatusRolledBack || results[0].Outcome != OutcomeNone {
		t.Fatalf("uncommitted recovery failed: %#v err=%v", results, err)
	}
	if app.runtime.count("rollback") != 2 || app.runtime.count("complete") != 0 {
		t.Fatalf("unexpected recovery calls: %#v", app.runtime.calls)
	}
}

func TestPublishRejectsTopologyAndPlanDriftBeforeBackup(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	app.placements.snapshot.TopologyRevision = "topology-revision-0002"
	publication, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-topology-drift", Plan: plan})
	if !errors.Is(err, ErrTopologyChanged) || publication.Status != StatusFailed || len(app.backups.calls) != 0 || app.runtime.count("prepare") != 0 {
		t.Fatalf("topology drift was not blocked: %#v err=%v", publication, err)
	}

	app.placements.snapshot.TopologyRevision = plan.TopologyRevision
	plan, err = app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	app.catalog.worlds[0].ModOverrides = []byte("return {changed=true}\n")
	publication, err = app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-plan-drift", Plan: plan})
	if !errors.Is(err, ErrPlanChanged) || publication.Status != StatusFailed || len(app.backups.calls) != 0 {
		t.Fatalf("plan drift was not blocked: %#v err=%v", publication, err)
	}
}

func TestPreviewRejectsSameInstallationVersionConflict(t *testing.T) {
	first := hashBytes([]byte("version-one"))
	second := hashBytes([]byte("version-two"))
	worlds := []ManagedWorld{
		{RoomID: "room-a", RoomDirectory: "Cluster_A", WorldID: "master", WorldDirectory: "Master", Mods: []ModRequirement{{WorkshopID: "100", TreeSHA256: first}}, ModOverrides: []byte("return {}")},
		{RoomID: "room-b", RoomDirectory: "Cluster_B", WorldID: "master", WorldDirectory: "Master", Mods: []ModRequirement{{WorkshopID: "100", TreeSHA256: second}}, ModOverrides: []byte("return {}")},
	}
	placements := []AppliedPlacement{
		{RoomID: "room-a", WorldID: "master", TargetID: "target-a", NodeID: "node-a", InstallationID: "shared"},
		{RoomID: "room-b", WorldID: "master", TargetID: "target-a", NodeID: "node-a", InstallationID: "shared"},
	}
	app := newTestApplication(t, worlds, placements)
	if _, err := app.coordinator.Preview(context.Background(), "room-a"); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected version conflict, got %v", err)
	}
}

func TestPreviewIncludesSharedInstallationUnionAndIndependentWorldConfigs(t *testing.T) {
	worlds := []ManagedWorld{
		{RoomID: "room-a", RoomDirectory: "Cluster_A", WorldID: "master", WorldDirectory: "Master", Mods: []ModRequirement{{WorkshopID: "100"}}, ModOverrides: []byte("return {a=true}\n")},
		{RoomID: "room-b", RoomDirectory: "Cluster_B", WorldID: "master", WorldDirectory: "Master", Mods: []ModRequirement{{WorkshopID: "200"}}, ModOverrides: []byte("return {b=true}\n")},
	}
	placements := []AppliedPlacement{
		{RoomID: "room-a", WorldID: "master", TargetID: "target-a", NodeID: "node-a", InstallationID: "shared"},
		{RoomID: "room-b", WorldID: "master", TargetID: "target-a", NodeID: "node-a", InstallationID: "shared"},
	}
	app := newTestApplication(t, worlds, placements)
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Targets) != 1 || len(plan.Targets[0].Worlds) != 2 || len(plan.Targets[0].Mods) != 2 || strings.Join(plan.AffectedRoomIDs, ",") != "room-a,room-b" {
		t.Fatalf("shared installation union is incomplete: %#v", plan)
	}
	configs := map[string]string{}
	for _, world := range plan.Targets[0].Worlds {
		configs[world.RoomID] = string(world.ModOverrides)
	}
	if configs["room-a"] != "return {a=true}\n" || configs["room-b"] != "return {b=true}\n" {
		t.Fatalf("world configurations were merged: %#v", configs)
	}
	publication, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-shared-union", Plan: plan})
	if err != nil || publication.Status != StatusSucceeded || len(app.backups.calls) != 1 || len(app.backups.calls[0].RoomIDs) != 2 {
		t.Fatalf("shared union publish failed: %#v err=%v", publication, err)
	}
}

func TestEmptyModSetPublishesCompleteEmptyDesiredState(t *testing.T) {
	worlds := []ManagedWorld{{RoomID: "room-a", RoomDirectory: "Cluster_A", WorldID: "master", WorldDirectory: "Master", ModOverrides: []byte("return {}\n")}}
	placements := []AppliedPlacement{{RoomID: "room-a", WorldID: "master", TargetID: "target-a", NodeID: "node-a", InstallationID: "install-a"}}
	app := newTestApplication(t, worlds, placements)
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil || len(plan.Targets) != 1 || len(plan.Targets[0].Mods) != 0 || len(plan.Targets[0].Worlds[0].Mods) != 0 {
		t.Fatalf("empty Mod preview failed: %#v err=%v", plan, err)
	}
	publication, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-empty-mods", Plan: plan})
	if err != nil || publication.Status != StatusSucceeded || !publication.RestartRequired {
		t.Fatalf("empty Mod publish failed: %#v err=%v", publication, err)
	}
	app.runtime.mu.Lock()
	defer app.runtime.mu.Unlock()
	for _, call := range app.runtime.calls {
		if call.action == "prepare" && (len(call.plan.Mods) != 0 || string(call.plan.Worlds[0].ModOverrides) != "return {}\n") {
			t.Fatalf("runtime did not receive complete empty desired state: %#v", call.plan)
		}
	}
}

func TestPreviewReportsOnlineCapabilityDiskAndVersionBlockers(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	observation := app.runtime.observations[targetIdentity("target-a", "install-a")]
	observation.Online = false
	observation.Capabilities = nil
	observation.AvailableBytes = 0
	observation.Version = "2.4.0"
	app.runtime.observations[targetIdentity("target-a", "install-a")] = observation
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	codes := make([]string, 0, len(plan.Blockers))
	for _, blocker := range plan.Blockers {
		if blocker.TargetID == "target-a" {
			codes = append(codes, blocker.Code)
		}
	}
	sort.Strings(codes)
	if strings.Join(codes, ",") != "CAPABILITY_MISSING,DISK_INSUFFICIENT,RUNTIME_VERSION_BLOCKED,TARGET_OFFLINE" || plan.Ready {
		t.Fatalf("unexpected blockers: %#v", plan.Blockers)
	}
}

func TestStoreRejectsTrailingPlanJSON(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	publication := Publication{ID: "publication-json-integrity", RoomID: "room-a", Status: StatusPreviewed, Outcome: OutcomeNone, Plan: plan, CreatedAt: now, UpdatedAt: now}
	for _, target := range plan.Targets {
		publication.Targets = append(publication.Targets, TargetResult{TargetID: target.TargetID, InstallationID: target.InstallationID, Status: StatusPreviewed, UpdatedAt: now})
	}
	if _, err := app.store.Create(publication); err != nil {
		t.Fatal(err)
	}
	if err := app.db.Table(app.store.publicationTable).Where("id = ?", publication.ID).UpdateColumn("plan_json", gorm.Expr("plan_json || '{}'")).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.Get(publication.ID); err == nil {
		t.Fatal("expected strict JSON failure")
	}
}

func TestOperationAndSourceJobIdempotencyDoNotRepublish(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	request := PublishRequest{ID: "publication-idempotent-01", SourceJobID: "source-job-idempotent-01", Plan: plan}
	first, err := app.coordinator.Publish(context.Background(), request)
	if err != nil || first.Status != StatusSucceeded {
		t.Fatalf("initial publish failed: %#v err=%v", first, err)
	}
	calls := len(app.runtime.calls)
	second, err := app.coordinator.Publish(context.Background(), request)
	if err != nil || second.ID != first.ID || len(app.runtime.calls) != calls {
		t.Fatalf("operation ID was not idempotent: %#v err=%v", second, err)
	}
	request.ID = "publication-idempotent-02"
	third, err := app.coordinator.Publish(context.Background(), request)
	if err != nil || third.ID != first.ID || len(app.runtime.calls) != calls {
		t.Fatalf("source job ID was not idempotent: %#v err=%v", third, err)
	}
	conflictingPlan := plan
	conflictingPlan.Targets = append([]TargetPlan(nil), plan.Targets...)
	conflictingPlan.Targets[0].NodeID += "-conflict"
	conflictingPlan.PlanHash, err = calculatePlanHash(conflictingPlan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.coordinator.Publish(context.Background(), PublishRequest{
		ID: first.ID, Plan: conflictingPlan,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("operation ID accepted a different plan: %v", err)
	}
	if _, err := app.coordinator.Publish(context.Background(), PublishRequest{
		ID: "publication-idempotent-03", SourceJobID: request.SourceJobID, Plan: conflictingPlan,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("source job ID accepted a different plan: %v", err)
	}
	if len(app.runtime.calls) != calls {
		t.Fatalf("idempotency conflicts re-executed runtime calls: %#v", app.runtime.calls)
	}
}

func TestActiveRoomLeaseRejectsConcurrentPublication(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	fence, err := app.leases.Acquire(context.Background(), "room-a", "other-publication", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer app.leases.Release(fence)
	if _, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-concurrent-room", Plan: plan}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected room lease conflict, got %v", err)
	}
	if _, err := app.store.Get("publication-concurrent-room"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocked publication should not be persisted: %v", err)
	}
}

func TestInstallationLeaseRejectsPublicationFromAnotherRoom(t *testing.T) {
	worlds := []ManagedWorld{
		{RoomID: "room-a", RoomDirectory: "Cluster_A", WorldID: "master-a", WorldDirectory: "Master", ModOverrides: []byte("return {}\n")},
		{RoomID: "room-b", RoomDirectory: "Cluster_B", WorldID: "master-b", WorldDirectory: "Master", ModOverrides: []byte("return {}\n")},
	}
	placements := []AppliedPlacement{
		{RoomID: "room-a", WorldID: "master-a", TargetID: "target-shared", NodeID: "node-a", InstallationID: "install-shared"},
		{RoomID: "room-b", WorldID: "master-b", TargetID: "target-shared", NodeID: "node-a", InstallationID: "install-shared"},
	}
	app := newTestApplication(t, worlds, placements)
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	installationResource := ""
	for _, resource := range publicationLeaseResources(plan) {
		if strings.HasPrefix(resource, "@mod-installation/") {
			installationResource = resource
		}
	}
	if installationResource == "" {
		t.Fatal("installation lease resource missing")
	}
	fence, err := app.leases.Acquire(context.Background(), installationResource, "other-publication", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer app.leases.Release(fence)
	if _, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: "publication-installation-conflict", Plan: plan}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected installation conflict, got %v", err)
	}
}

func TestPublicationListIsRoomScopedAndNewestFirst(t *testing.T) {
	worlds, placements := twoTargetWorlds()
	app := newTestApplication(t, worlds, placements)
	plan, err := app.coordinator.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"publication-list-old", "publication-list-new"} {
		if _, err := app.coordinator.Publish(context.Background(), PublishRequest{ID: id, Plan: plan}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	items, total, err := app.coordinator.List("room-a", 1, 0)
	if err != nil || total != 2 || len(items) != 1 || items[0].ID != "publication-list-new" {
		t.Fatalf("items=%#v total=%d err=%v", items, total, err)
	}
}

func targetIdentity(targetID, installationID string) string {
	return targetID + "\x00" + installationID
}
