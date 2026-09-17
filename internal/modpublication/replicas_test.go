package modpublication

import (
	"context"
	"errors"
	"testing"
	"time"
)

func sameInstallationWorlds() ([]ManagedWorld, []AppliedPlacement) {
	mod := ModRequirement{WorkshopID: "1392778117"}
	worlds := []ManagedWorld{
		{RoomID: "room-a", RoomDirectory: "Cluster_A", WorldID: "master", WorldDirectory: "Master", IsMaster: true, Mods: []ModRequirement{mod}, ModOverrides: []byte("return {master=true}\n")},
		{RoomID: "room-a", RoomDirectory: "Cluster_A", WorldID: "caves", WorldDirectory: "Caves", Mods: []ModRequirement{mod}, ModOverrides: []byte("return {caves=true}\n")},
	}
	placements := []AppliedPlacement{
		{RoomID: "room-a", WorldID: "master", TargetID: "target-a", NodeID: "node-a", InstallationID: "install-a"},
		{RoomID: "room-a", WorldID: "caves", TargetID: "target-a", NodeID: "node-a", InstallationID: "install-a"},
	}
	return worlds, placements
}

func TestReplicaStoreTracksInstallationAndWorldStateSeparately(t *testing.T) {
	worlds, placements := sameInstallationWorlds()
	app := newTestApplication(t, worlds, placements)
	store := NewReplicaStore(app.db, "replica_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 8, 30, 6, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	plan, err := app.planner.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Targets) != 1 || len(plan.Targets[0].Worlds) != 2 {
		t.Fatalf("plan targets = %#v", plan.Targets)
	}
	if err := store.SetDesired(plan); err != nil {
		t.Fatal(err)
	}
	state, err := store.Room("room-a")
	if err != nil {
		t.Fatal(err)
	}
	if state.Coverage.TotalTargets != 1 || state.Coverage.TotalWorlds != 2 || state.Coverage.ReadyTargets != 0 {
		t.Fatalf("initial coverage = %#v", state.Coverage)
	}

	clock = clock.Add(time.Minute)
	if err := store.MarkCache(plan.Targets[0], plan.PlanHash); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPublished(plan.Targets[0], plan.PlanHash); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWorldLoaded(plan, "room-a", "master", true, nil); err != nil {
		t.Fatal(err)
	}
	state, err = store.Room("room-a")
	if err != nil {
		t.Fatal(err)
	}
	if state.Coverage.ReadyTargets != 1 || state.Coverage.CachedTargets != 1 || state.Coverage.PublishedTargets != 1 ||
		state.Coverage.ConfiguredWorlds != 2 || state.Coverage.LoadedWorlds != 1 {
		t.Fatalf("published coverage = %#v", state.Coverage)
	}
	if len(state.Items) != 1 || len(state.Items[0].Targets) != 1 || len(state.Items[0].Targets[0].Worlds) != 2 {
		t.Fatalf("replica state = %#v", state)
	}
}

func TestReplicaStorePreservesExactObservationsAndReplacesChangedWorldDesiredState(t *testing.T) {
	worlds, placements := sameInstallationWorlds()
	app := newTestApplication(t, worlds, placements)
	store := NewReplicaStore(app.db, "replica_replace_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	plan, err := app.planner.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDesired(plan); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPublished(plan.Targets[0], plan.PlanHash); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWorldLoaded(plan, "room-a", "master", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDesired(plan); err != nil {
		t.Fatal(err)
	}
	unchanged, err := store.Room("room-a")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Coverage.ReadyTargets != 1 || unchanged.Coverage.LoadedWorlds != 1 {
		t.Fatalf("repeated desired state lost observations: %#v", unchanged.Coverage)
	}

	app.catalog.worlds[1].ModOverrides = []byte("return {caves=true, changed=true}\n")
	changed, err := app.planner.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if changed.PlanHash == plan.PlanHash {
		t.Fatal("changed configuration kept the same plan hash")
	}
	if err := store.SetDesired(changed); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Room("room-a")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Coverage.ReadyTargets != 0 || pending.Coverage.ConfiguredWorlds != 1 || pending.Coverage.LoadedWorlds != 1 {
		t.Fatalf("changed desired state = %#v", pending.Coverage)
	}
}

func TestReplicaStoreRollbackKeepsImmutableCacheButClearsPublication(t *testing.T) {
	worlds, placements := sameInstallationWorlds()
	app := newTestApplication(t, worlds, placements)
	store := NewReplicaStore(app.db, "replica_rollback_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	plan, err := app.planner.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDesired(plan); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPublished(plan.Targets[0], plan.PlanHash); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRolledBack(plan.Targets[0], plan.PlanHash, errors.New("publish failed")); err != nil {
		t.Fatal(err)
	}
	state, err := store.Room("room-a")
	if err != nil {
		t.Fatal(err)
	}
	target := state.Items[0].Targets[0]
	if !target.Cached || target.Published || target.Ready || target.ErrorStage != string(ReplicaStageRollback) {
		t.Fatalf("rolled back target = %#v", target)
	}
	if state.Coverage.ConfiguredWorlds != 0 || state.Coverage.LoadedWorlds != 0 {
		t.Fatalf("rolled back coverage = %#v", state.Coverage)
	}
}

func TestReplicaStoreUsesExactRuntimeObservationAndRejectsOlderSnapshots(t *testing.T) {
	worlds, placements := sameInstallationWorlds()
	app := newTestApplication(t, worlds, placements)
	store := NewReplicaStore(app.db, "replica_observe_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	plan, err := app.planner.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDesired(plan); err != nil {
		t.Fatal(err)
	}
	target := plan.Targets[0]
	workshopID, treeSHA := target.Mods[0].WorkshopID, target.Mods[0].TreeSHA256
	observedAt := time.Now().UTC().Add(-time.Hour)
	matching := &InstallationReplicaObservation{
		Mods: map[string]string{workshopID: treeSHA}, ObservedAt: observedAt,
		Worlds: map[string]WorldReplicaObservation{},
	}
	for _, world := range target.Worlds {
		matching.Worlds[world.RoomID+"/"+world.WorldID] = WorldReplicaObservation{
			ConfigSHA256: hashBytes(world.ModOverrides), Mods: map[string]string{workshopID: treeSHA},
		}
	}
	if err := store.ObserveTarget(target, plan.PlanHash, matching); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWorldLoaded(plan, "room-a", "master", true, nil); err != nil {
		t.Fatal(err)
	}

	staleMismatch := &InstallationReplicaObservation{
		Mods:   map[string]string{workshopID: hashBytes([]byte("old-tree"))},
		Worlds: map[string]WorldReplicaObservation{}, ObservedAt: observedAt.Add(-time.Minute),
	}
	if err := store.ObserveTarget(target, plan.PlanHash, staleMismatch); err != nil {
		t.Fatal(err)
	}
	state, err := store.Room("room-a")
	if err != nil {
		t.Fatal(err)
	}
	if state.Coverage.ReadyTargets != 1 || state.Coverage.LoadedWorlds != 1 {
		t.Fatalf("older observation replaced current state: %#v", state.Coverage)
	}

	newerMismatch := &InstallationReplicaObservation{
		Mods:   map[string]string{workshopID: hashBytes([]byte("other-tree"))},
		Worlds: map[string]WorldReplicaObservation{}, ObservedAt: time.Now().UTC().Add(time.Hour),
	}
	if err := store.ObserveTarget(target, plan.PlanHash, newerMismatch); err != nil {
		t.Fatal(err)
	}
	state, err = store.Room("room-a")
	if err != nil {
		t.Fatal(err)
	}
	if state.Coverage.ReadyTargets != 0 || state.Coverage.ConfiguredWorlds != 0 || state.Coverage.LoadedWorlds != 0 {
		t.Fatalf("newer mismatch was not projected: %#v", state.Coverage)
	}
}

func TestReplicaStoreReturnsExactCachedPeerCandidatesAndFetchSource(t *testing.T) {
	worlds, placements := sameInstallationWorlds()
	app := newTestApplication(t, worlds, placements)
	store := NewReplicaStore(app.db, "replica_peer_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	plan, err := app.planner.Preview(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDesired(plan); err != nil {
		t.Fatal(err)
	}
	target, artifact := plan.Targets[0], plan.Targets[0].Mods[0]
	attemptedAt := clock.Add(-time.Second)
	attempts := []ReplicaFetchAttempt{{
		Source: "steam", Feasibility: "available", Status: "succeeded", Selected: true,
		Bytes: 8 << 20, DurationMillis: 500, BytesPerSecond: 16 << 20, ObservedAt: &attemptedAt,
	}}
	if err := store.MarkArtifactCacheWithAttempts(target, artifact, plan.PlanHash, "steam", attempts); err != nil {
		t.Fatal(err)
	}
	newer := clock.Add(time.Minute)
	peer := artifactReplicaRecord{
		ID:       artifactReplicaID("target-b", "install-b", artifact.WorkshopID, artifact.TreeSHA256),
		TargetID: "target-b", InstallationID: "install-b", WorkshopID: artifact.WorkshopID,
		TreeSHA256: artifact.TreeSHA256, ManifestSHA256: artifact.ManifestSHA256,
		Size: artifact.Size, FileCount: artifact.FileCount, DesiredRevision: plan.PlanHash,
		Cached: true, FetchSource: "controller", ObservedAt: &newer, CreatedAt: newer, UpdatedAt: newer,
	}
	if err := app.db.Table(store.artifactTable).Create(&peer).Error; err != nil {
		t.Fatal(err)
	}
	candidates, err := store.CachedCandidates(artifact.WorkshopID, artifact.TreeSHA256, target.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].TargetID != "target-b" || candidates[0].InstallationID != "install-b" {
		t.Fatalf("candidates=%#v", candidates)
	}
	state, err := store.Room("room-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 1 || len(state.Items[0].Targets) != 1 || state.Items[0].Targets[0].FetchSource != "steam" {
		t.Fatalf("room replica source=%#v", state)
	}
	observedAttempts := state.Items[0].Targets[0].FetchAttempts
	if len(observedAttempts) != 1 || observedAttempts[0].Source != "steam" || !observedAttempts[0].Selected ||
		observedAttempts[0].BytesPerSecond != 16<<20 || observedAttempts[0].ObservedAt == nil || !observedAttempts[0].ObservedAt.Equal(attemptedAt) {
		t.Fatalf("room replica fetch attempts=%#v", observedAttempts)
	}
}
