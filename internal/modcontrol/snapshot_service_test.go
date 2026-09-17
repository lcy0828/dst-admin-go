package modcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
	"dont/shared"
)

func reconciliationPlan(fixture snapshotFixture, withMods bool) modpublication.Plan {
	world := modpublication.WorldPlan{
		RoomID: fixture.room.ID, RoomDirectory: fixture.room.DirectoryName,
		WorldID: fixture.master.ID, WorldDirectory: fixture.master.DirectoryName,
		ModOverrides: fixture.local,
	}
	target := modpublication.TargetPlan{TargetID: "local", NodeID: "local", InstallationID: "default", Worlds: []modpublication.WorldPlan{world}}
	if withMods {
		artifact := modpublication.ContentArtifact{WorkshopID: "100", TreeSHA256: testSHA([]byte("mod"))}
		target.Mods = []modpublication.ContentArtifact{artifact}
		target.Worlds[0].Mods = []modpublication.ContentArtifact{artifact}
	}
	return modpublication.Plan{
		Version: modpublication.PlanVersion, RoomID: fixture.room.ID, TopologyRevision: "topology-reconcile-1",
		PlanHash: testSHA([]byte("reconcile-plan")), AffectedRoomIDs: []string{fixture.room.ID},
		Targets: []modpublication.TargetPlan{target}, Ready: true, RestartRequired: true, CreatedAt: time.Now().UTC(),
	}
}

func reconciliationLease(roomID string) operationlease.Lease {
	return operationlease.Lease{
		RoomID: roomID, LeaseID: "migration-lease", OperationKey: "placement.migrate:world",
		FencingToken: 7, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
}

func TestMigrationReconciliationSkipsPublicationWithoutDesiredMods(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	plan := reconciliationPlan(fixture, false)
	coordinator.previewPlan = &plan
	convergence := &testPlanConvergence{}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureConvergence(convergence); err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcilePlacementMigration(context.Background(), fixture.room.ID, []string{fixture.master.ID}, reconciliationLease(fixture.room.ID)); err != nil {
		t.Fatal(err)
	}
	if convergence.calls != 0 || len(coordinator.published) != 0 {
		t.Fatalf("empty Mod migration performed work: convergence=%d publications=%d", convergence.calls, len(coordinator.published))
	}
}

func TestMigrationReconciliationPublishesOnlyWhenRuntimeDrifts(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	plan := reconciliationPlan(fixture, true)
	coordinator.previewPlan = &plan
	convergence := &testPlanConvergence{converged: true}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureConvergence(convergence); err != nil {
		t.Fatal(err)
	}
	lease := reconciliationLease(fixture.room.ID)
	if err := service.ReconcilePlacementMigration(context.Background(), fixture.room.ID, []string{fixture.master.ID}, lease); err != nil {
		t.Fatal(err)
	}
	if convergence.calls != 1 || len(coordinator.published) != 0 {
		t.Fatalf("converged migration published unexpectedly: convergence=%d publications=%d", convergence.calls, len(coordinator.published))
	}

	convergence.converged = false
	if err := service.ReconcilePlacementMigration(context.Background(), fixture.room.ID, []string{fixture.master.ID}, lease); err != nil {
		t.Fatal(err)
	}
	if convergence.calls != 2 || len(coordinator.published) != 1 || !coordinator.published[0].SkipProtectionBackup {
		t.Fatalf("drift reconciliation=%#v convergence=%d", coordinator.published, convergence.calls)
	}
}

type remoteSnapshotConvergence struct {
	source    *SnapshotSource
	targetID  string
	installID string
	calls     int
}

type installationContentFetcherFixture struct {
	err            error
	targetID       string
	installationID string
	modIDs         []string
	calls          int
}

type modConfigurationPublisherFixture struct {
	writes map[string][]byte
	err    error
}

func (f *modConfigurationPublisherFixture) PublishModOverrides(_ context.Context, roomID string, updates []runtimedriver.ModOverridesUpdate) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.writes == nil {
		f.writes = make(map[string][]byte)
	}
	for _, update := range updates {
		f.writes[roomID+"/"+update.Target.WorldID] = append([]byte(nil), update.Content...)
	}
	return len(updates), nil
}

func (f *installationContentFetcherFixture) UpdateInstallationMods(_ context.Context, targetID, installationID string, modIDs []string, _ io.Writer) (mods.ActionResult, error) {
	f.calls++
	f.targetID, f.installationID = targetID, installationID
	f.modIDs = append([]string(nil), modIDs...)
	if f.err != nil {
		return mods.ActionResult{}, f.err
	}
	return mods.ActionResult{ModIDs: append([]string(nil), modIDs...)}, nil
}

func (f *installationContentFetcherFixture) LinkInstallationMods(context.Context, string, string, []string) error {
	return f.err
}

func TestInstallDownloadsOnSelectedRuntimeAndWritesSelectedWorld(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fetcher := &installationContentFetcherFixture{}
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureInstallationContentFetcher(fetcher); err != nil {
		t.Fatal(err)
	}
	publisher := &modConfigurationPublisherFixture{}
	if err := service.ConfigureConfigurationPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	result, err := service.Install(context.Background(), "job-install-1", fixture.room.ID, mods.InstallRequest{
		ModID: "400", WorldIDs: []string{fixture.caves.ID}, Enabled: true,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 1 || fetcher.targetID != "target-2" || fetcher.installationID != "install-2" || !sameStringSet(fetcher.modIDs, map[string]bool{"400": true}) {
		t.Fatalf("fetch=%#v", fetcher)
	}
	if len(coordinator.published) != 0 || len(result.ModIDs) != 1 || result.ModIDs[0] != "400" {
		t.Fatalf("publication=%#v result=%#v", coordinator.published, result)
	}
	content := publisher.writes[fixture.room.ID+"/"+fixture.caves.ID]
	if !bytes.Contains(content, []byte(`workshop-400`)) || !bytes.Contains(content, []byte(`enabled = true`)) {
		t.Fatalf("selected world config=%s", content)
	}
}

func TestInstallSkipsSteamCMDWhenSelectedRuntimeAlreadyHasCurrentMod(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.mods.described = map[string]mods.SteamMod{"400": {SteamManifestID: "manifest-current"}}
	fetcher := &installationContentFetcherFixture{}
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureInstallationContentFetcher(fetcher); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureRuntimeFileObserver(&testRuntimeFileObserver{observations: map[string]*shared.RuntimeModFilesObservation{
		"target-2\x00install-2": {
			InstallationID: "install-2",
			Mods: map[string]shared.RuntimeModFileState{"400": {
				Status: shared.RuntimeModFileReady, SteamManifestID: "manifest-current",
			}},
			Worlds: map[string]shared.RuntimeModWorldFileState{},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	publisher := &modConfigurationPublisherFixture{}
	if err := service.ConfigureConfigurationPublisher(publisher); err != nil {
		t.Fatal(err)
	}

	result, err := service.Install(context.Background(), "job-install-current", fixture.room.ID, mods.InstallRequest{
		ModID: "400", WorldIDs: []string{fixture.caves.ID}, Enabled: true,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("current Runtime Mod unexpectedly invoked SteamCMD: %#v", fetcher)
	}
	if !strings.Contains(result.Message, "已有最新模组") || !bytes.Contains(publisher.writes[fixture.room.ID+"/"+fixture.caves.ID], []byte(`workshop-400`)) {
		t.Fatalf("result=%#v writes=%#v", result, publisher.writes)
	}
}

func TestInstallDownloadsWhenSelectedRuntimeModIsOutdated(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.mods.described = map[string]mods.SteamMod{"400": {SteamManifestID: "manifest-latest"}}
	fetcher := &installationContentFetcherFixture{}
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureInstallationContentFetcher(fetcher); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureRuntimeFileObserver(&testRuntimeFileObserver{observations: map[string]*shared.RuntimeModFilesObservation{
		"target-2\x00install-2": {
			InstallationID: "install-2",
			Mods: map[string]shared.RuntimeModFileState{"400": {
				Status: shared.RuntimeModFileReady, SteamManifestID: "manifest-old",
			}},
			Worlds: map[string]shared.RuntimeModWorldFileState{},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureConfigurationPublisher(&modConfigurationPublisherFixture{}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Install(context.Background(), "job-install-outdated", fixture.room.ID, mods.InstallRequest{
		ModID: "400", WorldIDs: []string{fixture.caves.ID}, Enabled: true,
	}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("outdated Runtime Mod download calls=%d", fetcher.calls)
	}
}

func TestSetEnabledPublishesOnlySelectedWorldWithRevisionCheck(t *testing.T) {
	fixture := newSnapshotFixture(t)
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &modConfigurationPublisherFixture{}
	if err := service.ConfigureConfigurationPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	current, err := mods.InspectModOverride(fixture.remote)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.SetEnabled(context.Background(), fixture.room.ID, "200", mods.EnableRequest{
		WorldIDs: []string{fixture.caves.ID}, Enabled: true, ExpectedRevision: current.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := publisher.writes[fixture.room.ID+"/"+fixture.caves.ID]
	if result.PublishedTargets != 1 || result.Revision == current.Revision || result.Revisions[fixture.caves.ID] != result.Revision ||
		!bytes.Contains(content, []byte(`workshop-200`)) || !bytes.Contains(content, []byte(`enabled = true`)) {
		t.Fatalf("result=%#v content=%s", result, content)
	}
	if _, exists := publisher.writes[fixture.room.ID+"/"+fixture.master.ID]; exists {
		t.Fatalf("unselected world was written: %#v", publisher.writes)
	}

	_, err = service.SetEnabled(context.Background(), fixture.room.ID, "200", mods.EnableRequest{
		WorldIDs: []string{fixture.caves.ID}, Enabled: false, ExpectedRevision: "stale",
	})
	if !errors.Is(err, mods.ErrRevisionConflict) {
		t.Fatalf("stale enable revision error=%v", err)
	}
}

func TestInstallDownloadFailureNamesTargetRuntime(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fetcher := &installationContentFetcherFixture{err: errors.New("SteamCMD I/O Operation Failed")}
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureInstallationContentFetcher(fetcher); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureConfigurationPublisher(&modConfigurationPublisherFixture{}); err != nil {
		t.Fatal(err)
	}
	_, err = service.Install(context.Background(), "job-install-2", fixture.room.ID, mods.InstallRequest{
		ModID: "400", WorldIDs: []string{fixture.caves.ID}, Enabled: true,
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "target-2/install-2") || !strings.Contains(err.Error(), "I/O Operation Failed") {
		t.Fatalf("download error did not identify its runtime: %v", err)
	}
}

func (c *remoteSnapshotConvergence) Converged(ctx context.Context, _ modpublication.Plan) (bool, error) {
	c.calls++
	_, exists, err := c.source.Execution(ctx, c.targetID, c.installID)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, errors.New("remote placement missing from start reconciliation snapshot")
	}
	return true, nil
}

func TestMigrationReconciliationCarriesRemotePlacementSnapshot(t *testing.T) {
	fixture := newSnapshotFixture(t)
	artifact := modpublication.ContentArtifact{WorkshopID: "200", TreeSHA256: testSHA([]byte("mod"))}
	plan := modpublication.Plan{
		Version: modpublication.PlanVersion, RoomID: fixture.room.ID, TopologyRevision: "topology-reconcile-remote",
		PlanHash: testSHA([]byte("reconcile-remote-plan")), AffectedRoomIDs: []string{fixture.room.ID},
		Targets: []modpublication.TargetPlan{{
			TargetID: "target-2", NodeID: "agent-2", InstallationID: "install-2",
			Mods: []modpublication.ContentArtifact{artifact},
			Worlds: []modpublication.WorldPlan{{
				RoomID: fixture.room.ID, RoomDirectory: fixture.room.DirectoryName,
				WorldID: fixture.caves.ID, WorldDirectory: fixture.caves.DirectoryName,
				Mods: []modpublication.ContentArtifact{artifact}, ModOverrides: fixture.remote,
			}},
		}},
		Ready: true, RestartRequired: true, CreatedAt: time.Now().UTC(),
	}
	coordinator := &snapshotCoordinator{source: fixture.source, previewPlan: &plan}
	convergence := &remoteSnapshotConvergence{source: fixture.source, targetID: "target-2", installID: "install-2"}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureConvergence(convergence); err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcilePlacementMigration(context.Background(), fixture.room.ID, []string{fixture.caves.ID}, reconciliationLease(fixture.room.ID)); err != nil {
		t.Fatal(err)
	}
	if convergence.calls != 1 || fixture.placements.count(fixture.room.ID) != 1 || len(coordinator.published) != 0 {
		t.Fatalf("remote reconciliation snapshot was not reused: convergence=%d topologyReads=%d publications=%d", convergence.calls, fixture.placements.count(fixture.room.ID), len(coordinator.published))
	}
}

func TestMigrationReconciliationBorrowsRoomFence(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	plan := reconciliationPlan(fixture, true)
	coordinator.previewPlan = &plan
	convergence := &testPlanConvergence{converged: false}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureConvergence(convergence); err != nil {
		t.Fatal(err)
	}
	lease := operationlease.Lease{
		RoomID: fixture.room.ID, LeaseID: "migration-lease-1", OperationKey: "placement.migrate:master",
		FencingToken: 7, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	if err := service.ReconcilePlacementMigration(context.Background(), fixture.room.ID, []string{fixture.master.ID}, lease); err != nil {
		t.Fatal(err)
	}
	if len(coordinator.published) != 1 || len(coordinator.published[0].BorrowedFences) != 1 ||
		coordinator.published[0].BorrowedFences[0].LeaseID != lease.LeaseID || !coordinator.published[0].SkipProtectionBackup {
		t.Fatalf("migration publication=%#v", coordinator.published)
	}
}

type snapshotFixture struct {
	root       string
	room       rooms.Room
	master     rooms.World
	caves      rooms.World
	local      []byte
	remote     []byte
	decoy      []byte
	rooms      *testRoomCatalog
	placements *testPlacementResolver
	mods       *testModCatalog
	driver     *testModDriver
	source     *SnapshotSource
}

func newSnapshotFixture(t *testing.T) snapshotFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room-1", DirectoryName: "Cluster_1", Name: "Room 1", Managed: true}
	master := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true}
	caves := rooms.World{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Name: "Caves"}
	local := []byte("return { [\"workshop-100\"] = { enabled = true, configuration_options = { mode = \"easy\" } } }\n")
	remote := []byte("return { [\"workshop-200\"] = { enabled = false } }\n")
	decoy := []byte("return { [\"workshop-999\"] = { enabled = true } }\n")
	for world, content := range map[string][]byte{"Master": local, "Caves": decoy} {
		path := filepath.Join(root, room.DirectoryName, world, "modoverrides.lua")
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	roomCatalog := &testRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {master, caves}}}
	resolver := &testPlacementResolver{
		executions: map[string][]topology.ExecutionPlacement{room.ID: {
			testExecution(room, master, "room-revision-1", "local", "local", "default"),
			testExecution(room, caves, "room-revision-1", "target-2", "agent-2", "install-2"),
		}},
		calls: make(map[string]int), cachedCalls: make(map[string]int),
	}
	driver := &testModDriver{
		overrides: map[string][]byte{room.DirectoryName + "\x00" + caves.DirectoryName: remote},
		inspect:   make(map[string]shared.RuntimeModCacheManifest), availableBytes: 1 << 30, runtimeVersion: "1.0.0",
	}
	modCatalog := &testModCatalog{dependencyIDs: []string{"400", "401"}}
	source, err := NewSnapshotSource(roomCatalog, resolver, modCatalog, driver, root)
	if err != nil {
		t.Fatal(err)
	}
	return snapshotFixture{
		root: root, room: room, master: master, caves: caves, local: local, remote: remote, decoy: decoy,
		rooms: roomCatalog, placements: resolver, mods: modCatalog, driver: driver, source: source,
	}
}

func TestSnapshotReadsEachRoomTopologyOnceAndUsesAppliedPlacement(t *testing.T) {
	fixture := newSnapshotFixture(t)
	ctx := withPublicationSnapshot(context.Background(), proposal{roomID: fixture.room.ID, action: "reconcile", worldIDs: map[string]bool{}})
	worlds, err := fixture.source.ManagedWorlds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	placements, err := fixture.source.AppliedPlacements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := fixture.source.Contents(ctx, fixture.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.placements.count(fixture.room.ID) != 1 {
		t.Fatalf("room topology resolved %d times", fixture.placements.count(fixture.room.ID))
	}
	if len(worlds) != 2 || len(placements.Placements) != 2 || placements.TopologyRevision == "" {
		t.Fatalf("incomplete snapshot: worlds=%#v placements=%#v", worlds, placements)
	}
	if !bytes.Equal(contents[fixture.master.ID], fixture.local) || !bytes.Equal(contents[fixture.caves.ID], fixture.remote) {
		t.Fatalf("placement-aware content mismatch: %#v", contents)
	}
	if bytes.Equal(contents[fixture.caves.ID], fixture.decoy) {
		t.Fatal("remote world fell back to the controller's same-name directory")
	}
	if len(fixture.driver.overrideCalls) != 1 || fixture.driver.overrideCalls[0].TargetID != "target-2" || fixture.driver.overrideCalls[0].TopologyRevision != "room-revision-1" {
		t.Fatalf("unexpected remote read target: %#v", fixture.driver.overrideCalls)
	}
}

func TestRoomListSnapshotUsesCachedTopologyResolution(t *testing.T) {
	fixture := newSnapshotFixture(t)
	ctx := withPublicationSnapshot(context.Background(), proposal{
		roomID: fixture.room.ID, roomOnly: true, action: "reconcile", worldIDs: map[string]bool{}, readOnly: true,
	})
	if _, err := fixture.source.Contents(ctx, fixture.room.ID); err != nil {
		t.Fatal(err)
	}
	if fixture.placements.count(fixture.room.ID) != 0 {
		t.Fatalf("read-only snapshot used strict topology resolution %d times", fixture.placements.count(fixture.room.ID))
	}
	cachedCalls := fixture.placements.cachedCount(fixture.room.ID)
	if cachedCalls != 1 {
		t.Fatalf("cached topology resolved %d times", cachedCalls)
	}
}

func TestMigrationSnapshotReadsSourceConfigButPlansOnlyTargetInstallation(t *testing.T) {
	fixture := newSnapshotFixture(t)
	targetExecution := fixture.placements.executions[fixture.room.ID][1]
	targetExecution.World = fixture.master
	placement := publicationPlacement(targetExecution, fixture.room.ID, fixture.master.ID)
	installationKey := placement.TargetID + "\x00" + placement.InstallationID
	ctx := withPublicationSnapshot(context.Background(), proposal{
		roomID: fixture.room.ID, action: "reconcile", worldIDs: map[string]bool{},
		placements:       map[string]modpublication.AppliedPlacement{fixture.room.ID + "\x00" + fixture.master.ID: placement},
		executions:       map[string]topology.ExecutionPlacement{installationKey: targetExecution},
		installationOnly: map[string]bool{installationKey: true},
	})
	worlds, err := fixture.source.ManagedWorlds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	placements, err := fixture.source.AppliedPlacements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := fixture.source.Contents(ctx, fixture.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, exists, err := fixture.source.Execution(ctx, "target-2", "install-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(worlds) != 2 || len(placements.Placements) != 2 || !exists || resolved.World.ID != fixture.master.ID ||
		!bytes.Equal(contents[fixture.master.ID], fixture.local) {
		t.Fatalf("migration snapshot worlds=%#v placements=%#v execution=%#v contents=%#v", worlds, placements, resolved, contents)
	}
	for _, value := range placements.Placements {
		if value.TargetID != "target-2" || value.InstallationID != "install-2" {
			t.Fatalf("source installation leaked into migration publication: %#v", placements)
		}
	}
}

func TestSnapshotConcurrentReadersShareOneImmutableBuild(t *testing.T) {
	fixture := newSnapshotFixture(t)
	ctx := withPublicationSnapshot(context.Background(), proposal{roomID: fixture.room.ID, action: "reconcile", worldIDs: map[string]bool{}})
	start := make(chan struct{})
	errs := make(chan error, 24)
	var group sync.WaitGroup
	for index := 0; index < 24; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			if index%3 == 0 {
				_, err := fixture.source.ManagedWorlds(ctx)
				errs <- err
				return
			}
			if index%3 == 1 {
				_, err := fixture.source.AppliedPlacements(ctx)
				errs <- err
				return
			}
			_, err := fixture.source.Contents(ctx, fixture.room.ID)
			errs <- err
		}(index)
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if fixture.placements.count(fixture.room.ID) != 1 {
		t.Fatalf("concurrent snapshot built %d times", fixture.placements.count(fixture.room.ID))
	}
}

func TestSnapshotRejectsInconsistentRoomTopologyRevision(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.placements.executions[fixture.room.ID][1].Revision = "room-revision-2"
	ctx := withPublicationSnapshot(context.Background(), proposal{roomID: fixture.room.ID, action: "reconcile", worldIDs: map[string]bool{}})
	if _, err := fixture.source.ManagedWorlds(ctx); !errors.Is(err, ErrTopologyChanged) {
		t.Fatalf("expected topology change, got %v", err)
	}
}

func TestRemoteOverridesChunkValidation(t *testing.T) {
	fixture := newSnapshotFixture(t)
	execution := fixture.placements.executions[fixture.room.ID][1]
	placement := publicationPlacement(execution, fixture.room.ID, fixture.caves.ID)
	tests := []struct {
		name string
		hook func(runtimedriver.Target, string, string, int64) (shared.RuntimeModOverridesChunk, error)
	}{
		{
			name: "oversized declared file",
			hook: func(_ runtimedriver.Target, room, world string, _ int64) (shared.RuntimeModOverridesChunk, error) {
				return shared.RuntimeModOverridesChunk{RoomDirectory: room, WorldDirectory: world, Size: (8 << 20) + 1, SHA256: testSHA(nil), Complete: true}, nil
			},
		},
		{
			name: "oversized chunk",
			hook: func(_ runtimedriver.Target, room, world string, _ int64) (shared.RuntimeModOverridesChunk, error) {
				data := make([]byte, shared.MaxChunkBytes+1)
				return shared.RuntimeModOverridesChunk{RoomDirectory: room, WorldDirectory: world, NextOffset: int64(len(data)), Size: int64(len(data)), SHA256: testSHA(data), Data: data, Complete: true}, nil
			},
		},
		{
			name: "wrong offset",
			hook: func(_ runtimedriver.Target, room, world string, _ int64) (shared.RuntimeModOverridesChunk, error) {
				return shared.RuntimeModOverridesChunk{RoomDirectory: room, WorldDirectory: world, Offset: 1, NextOffset: 1, Size: 1, SHA256: testSHA([]byte("x")), Complete: true}, nil
			},
		},
		{
			name: "tampered digest",
			hook: func(_ runtimedriver.Target, room, world string, _ int64) (shared.RuntimeModOverridesChunk, error) {
				return shared.RuntimeModOverridesChunk{RoomDirectory: room, WorldDirectory: world, NextOffset: 1, Size: 1, SHA256: testSHA([]byte("y")), Data: []byte("x"), Complete: true}, nil
			},
		},
		{
			name: "truncated transfer",
			hook: func(_ runtimedriver.Target, room, world string, _ int64) (shared.RuntimeModOverridesChunk, error) {
				return shared.RuntimeModOverridesChunk{RoomDirectory: room, WorldDirectory: world, Size: 1, SHA256: testSHA([]byte("x"))}, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture.driver.overrideHook = test.hook
			if _, err := fixture.source.readOverrides(context.Background(), execution, placement); err == nil {
				t.Fatal("invalid remote transfer was accepted")
			}
		})
	}
}

func TestServiceActionsProduceIndependentWorldConfigurations(t *testing.T) {
	tests := []struct {
		name    string
		request func(snapshotFixture) Request
		assert  func(*testing.T, []mods.OverrideModState, []byte)
	}{
		{
			name: "add with dependencies",
			request: func(f snapshotFixture) Request {
				return Request{Action: "add", ModID: "400", WorldIDs: []string{f.master.ID}, IncludeDependencies: true, Enabled: false}
			},
			assert: func(t *testing.T, states []mods.OverrideModState, _ []byte) {
				if len(states) != 3 || states[1] != (mods.OverrideModState{ModID: "400", Enabled: false}) || states[2] != (mods.OverrideModState{ModID: "401", Enabled: false}) {
					t.Fatalf("dependencies were not added: %#v", states)
				}
			},
		},
		{
			name: "disable",
			request: func(f snapshotFixture) Request {
				return Request{Action: "enable", ModID: "100", WorldIDs: []string{f.master.ID}, Enabled: false}
			},
			assert: func(t *testing.T, states []mods.OverrideModState, _ []byte) {
				if len(states) != 1 || states[0].Enabled {
					t.Fatalf("mod was not disabled: %#v", states)
				}
			},
		},
		{
			name: "remove",
			request: func(f snapshotFixture) Request {
				return Request{Action: "remove", ModID: "100", WorldIDs: []string{f.master.ID}}
			},
			assert: func(t *testing.T, states []mods.OverrideModState, _ []byte) {
				if len(states) != 0 {
					t.Fatalf("mod was not removed: %#v", states)
				}
			},
		},
		{
			name: "configure",
			request: func(f snapshotFixture) Request {
				snapshot, err := mods.InspectModOverride(f.local)
				if err != nil {
					panic(err)
				}
				return Request{
					Action: "configure", ModID: "100", WorldIDs: []string{f.master.ID}, Enabled: false,
					ExpectedConfigurationRevision: snapshot.Revision,
					Patch:                         map[string]json.RawMessage{"mode": json.RawMessage(`"hard"`)},
				}
			},
			assert: func(t *testing.T, states []mods.OverrideModState, content []byte) {
				if len(states) != 1 || states[0].Enabled || !bytes.Contains(content, []byte(`mode = "hard"`)) {
					t.Fatalf("configuration was not applied: states=%#v content=%s", states, content)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSnapshotFixture(t)
			coordinator := &snapshotCoordinator{source: fixture.source}
			service, err := NewService(fixture.source, fixture.mods, coordinator)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Preview(context.Background(), fixture.room.ID, test.request(fixture)); err != nil {
				t.Fatal(err)
			}
			var master, caves []byte
			for _, world := range coordinator.lastWorlds {
				switch world.WorldID {
				case fixture.master.ID:
					master = world.ModOverrides
				case fixture.caves.ID:
					caves = world.ModOverrides
				}
			}
			test.assert(t, sortedModStates(t, master), master)
			if !bytes.Equal(caves, fixture.remote) {
				t.Fatalf("unselected world changed: %s", caves)
			}
		})
	}
}

func TestRoomDefaultConfigurationUpdatesAllSelectedWorldsAtomically(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.remote = []byte("return { [\"workshop-100\"] = { enabled = false, configuration_options = { mode = \"easy\" } } }\n")
	fixture.driver.overrides[fixture.room.DirectoryName+"\x00"+fixture.caves.DirectoryName] = fixture.remote
	masterSnapshot, err := mods.InspectModOverride(fixture.local)
	if err != nil {
		t.Fatal(err)
	}
	cavesSnapshot, err := mods.InspectModOverride(fixture.remote)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Preview(context.Background(), fixture.room.ID, Request{
		Action: "configure", ModID: "100", WorldIDs: []string{fixture.master.ID, fixture.caves.ID}, Enabled: false, PreserveEnabled: true,
		ExpectedConfigurationRevisions: map[string]string{
			fixture.master.ID: masterSnapshot.Revision,
			fixture.caves.ID:  cavesSnapshot.Revision,
		},
		Patch: map[string]json.RawMessage{"mode": json.RawMessage(`"hard"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(coordinator.lastWorlds) != 2 {
		t.Fatalf("unexpected planned worlds: %#v", coordinator.lastWorlds)
	}
	for _, world := range coordinator.lastWorlds {
		if !bytes.Contains(world.ModOverrides, []byte(`mode = "hard"`)) {
			t.Fatalf("world %s did not receive the room default: %s", world.WorldID, world.ModOverrides)
		}
		states := sortedModStates(t, world.ModOverrides)
		wantEnabled := world.WorldID == fixture.master.ID
		if len(states) != 1 || states[0].Enabled != wantEnabled {
			t.Fatalf("world %s has unexpected state: %#v", world.WorldID, states)
		}
	}
}

func TestRoomDefaultConfigurationRejectsIncompleteRevisionMap(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Preview(context.Background(), fixture.room.ID, Request{
		Action: "configure", ModID: "100", WorldIDs: []string{fixture.master.ID, fixture.caves.ID},
		ExpectedConfigurationRevisions: map[string]string{fixture.master.ID: "revision"},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("incomplete revision map was accepted: %v", err)
	}
}

func TestSharedConfigurationUsesSelectedSourceAndRejectsStaleSource(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			fixture := newSnapshotFixture(t)
			fixture.remote = []byte(`return {["workshop-100"]={enabled=false,configuration_options={mode="easy",source_only=42}}}`)
			fixture.driver.overrides[fixture.room.DirectoryName+"\x00"+fixture.caves.DirectoryName] = fixture.remote
			master, _ := mods.InspectModOverride(fixture.local)
			caves, _ := mods.InspectModOverride(fixture.remote)
			if stale {
				caves.Revision = "stale"
			}
			coordinator := &snapshotCoordinator{source: fixture.source}
			service, err := NewService(fixture.source, fixture.mods, coordinator)
			if err != nil {
				t.Fatal(err)
			}
			publisher := &modConfigurationPublisherFixture{}
			if err := service.ConfigureConfigurationPublisher(publisher); err != nil {
				t.Fatal(err)
			}
			_, err = service.ApplyConfiguration(context.Background(), fixture.room.ID, fixture.caves.ID, "100", mods.ConfigUpdateRequest{
				WorldIDs: []string{fixture.master.ID, fixture.caves.ID}, SourceWorldID: fixture.caves.ID, PreserveEnabled: true,
				ExpectedRevisions: map[string]string{fixture.master.ID: master.Revision, fixture.caves.ID: caves.Revision},
				Patch:             map[string]json.RawMessage{"mode": json.RawMessage(`"hard"`)},
			})
			if stale {
				if !errors.Is(err, mods.ErrRevisionConflict) || len(publisher.writes) != 0 {
					t.Fatalf("stale source published: %v %#v", err, publisher)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(publisher.writes) != 2 {
				t.Fatalf("updates=%#v", publisher)
			}
			for _, content := range publisher.writes {
				if !bytes.Contains(content, []byte("source_only = 42")) || !bytes.Contains(content, []byte(`mode = "hard"`)) {
					t.Fatalf("source not copied: %s", content)
				}
			}
		})
	}
}

func TestServiceAcceptsRoomRevisionForPreviewAndCompositeRevisionForPublish(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	input := Request{
		Action: "reconcile", WorldIDs: []string{fixture.master.ID, fixture.caves.ID},
		ExpectedTopologyRevision: "room-revision-1",
	}
	plan, err := service.Preview(context.Background(), fixture.room.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TopologyRevision == input.ExpectedTopologyRevision || plan.TopologyRevision == "" {
		t.Fatalf("expected a distinct composite revision, got %#v", plan)
	}
	input.ExpectedTopologyRevision = plan.TopologyRevision
	input.PlanHash, input.Confirmation = plan.PlanHash, plan.PlanHash
	publication, err := service.Publish(context.Background(), "job-publication-1", fixture.room.ID, input)
	if err != nil || publication.Status != modpublication.StatusSucceeded {
		t.Fatalf("publication=%#v err=%v", publication, err)
	}
	if len(coordinator.published) != 1 || !coordinator.published[0].SkipProtectionBackup {
		t.Fatalf("interactive Mod publication requested a disruptive protection backup: %#v", coordinator.published)
	}
	input.ExpectedTopologyRevision = "stale-room-revision"
	if _, err := service.Preview(context.Background(), fixture.room.ID, input); !errors.Is(err, ErrTopologyChanged) {
		t.Fatalf("stale room revision was accepted: %v", err)
	}
}

func TestReconcileRejectsStaleVisibleModSet(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	base := Request{
		Action: "reconcile", ModIDs: []string{"100", "200"},
		WorldIDs: []string{fixture.master.ID, fixture.caves.ID},
	}
	if _, err := service.Preview(context.Background(), fixture.room.ID, base); err != nil {
		t.Fatalf("current visible Mod set was rejected: %v", err)
	}
	base.ModIDs = []string{"100"}
	if _, err := service.Preview(context.Background(), fixture.room.ID, base); !errors.Is(err, modpublication.ErrPlanChanged) {
		t.Fatalf("stale visible Mod set was accepted: %v", err)
	}
	base.ModIDs = []string{"invalid"}
	if _, err := service.Preview(context.Background(), fixture.room.ID, base); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid visible Mod set was accepted: %v", err)
	}
}

func TestPlacementAwareReadAPIsNeverUseRemoteLocalDecoy(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	file, err := service.ConfigurationFile(context.Background(), fixture.room.ID, fixture.caves.ID)
	if err != nil || file.Content != string(fixture.remote) || !file.Exists {
		t.Fatalf("configuration file=%#v err=%v", file, err)
	}
	configuration, err := service.Configuration(context.Background(), fixture.room.ID, fixture.caves.ID, "200")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := mods.InspectModOverride(fixture.remote)
	if err != nil || configuration.Revision != remote.Revision || configuration.Enabled || len(fixture.mods.configurationContent) != 0 || len(fixture.driver.schemaCalls) != 1 {
		t.Fatalf("configuration used wrong placement: %#v, err=%v", configuration, err)
	}
	list, err := service.RoomList(context.Background(), fixture.room.ID)
	if err != nil || list.Total != 2 {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	if !bytes.Equal(fixture.mods.listContents[fixture.caves.ID], fixture.remote) {
		t.Fatalf("room list used wrong content: %#v", fixture.mods.listContents)
	}
}

func TestPlacementAwareModConfigurationRejectsExternalRuntimeEdit(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	publisher := &modConfigurationPublisherFixture{}
	if err := service.ConfigureConfigurationPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	current, err := service.Configuration(context.Background(), fixture.room.ID, fixture.caves.ID, "200")
	if err != nil {
		t.Fatal(err)
	}
	external := []byte(`return {
  ["workshop-200"] = {
    enabled = true,
    configuration_options = {
      mode = "hard",
      manual_value = { preserved = true },
    },
    operator_field = "keep",
  },
}` + "\n")
	key := fixture.room.DirectoryName + "\x00" + fixture.caves.DirectoryName
	fixture.driver.mu.Lock()
	fixture.driver.overrides[key] = append([]byte(nil), external...)
	fixture.driver.mu.Unlock()

	request := mods.ConfigUpdateRequest{
		ExpectedRevision: current.Revision,
		Enabled:          false,
		Patch:            map[string]json.RawMessage{"mode": json.RawMessage(`"easy"`)},
	}
	if _, err := service.PreviewConfiguration(context.Background(), fixture.room.ID, fixture.caves.ID, "200", request); !errors.Is(err, mods.ErrRevisionConflict) {
		t.Fatalf("stale placement-aware configuration preview error = %v", err)
	}
	if _, err := service.ApplyConfiguration(context.Background(), fixture.room.ID, fixture.caves.ID, "200", request); !errors.Is(err, mods.ErrRevisionConflict) {
		t.Fatalf("stale direct configuration apply error = %v", err)
	}
	if len(publisher.writes) != 0 {
		t.Fatalf("stale configuration was published: %#v", publisher.writes)
	}
	if _, err := service.Preview(context.Background(), fixture.room.ID, Request{
		Action: "configure", ModID: "200", WorldIDs: []string{fixture.caves.ID}, Enabled: false,
		ExpectedConfigurationRevision: current.Revision,
		Patch:                         map[string]json.RawMessage{"mode": json.RawMessage(`"easy"`)},
	}); !errors.Is(err, mods.ErrRevisionConflict) {
		t.Fatalf("stale Mod publication preview error = %v", err)
	}
	latest, err := service.ConfigurationFile(context.Background(), fixture.room.ID, fixture.caves.ID)
	if err != nil || latest.Revision == current.Revision || latest.Content != string(external) {
		t.Fatalf("external Runtime edit was not preserved on reread: file=%#v err=%v", latest, err)
	}
}

func TestDirectModConfigurationPublishesOnlySelectedWorldFiles(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.remote = append([]byte(nil), fixture.local...)
	fixture.driver.overrides[fixture.room.DirectoryName+"\x00"+fixture.caves.DirectoryName] = fixture.remote
	masterState, err := mods.InspectModOverride(fixture.local)
	if err != nil {
		t.Fatal(err)
	}
	cavesState, err := mods.InspectModOverride(fixture.remote)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &modConfigurationPublisherFixture{}
	if err := service.ConfigureConfigurationPublisher(publisher); err != nil {
		t.Fatal(err)
	}
	result, err := service.ApplyConfiguration(context.Background(), fixture.room.ID, fixture.master.ID, "100", mods.ConfigUpdateRequest{
		WorldIDs: []string{fixture.master.ID, fixture.caves.ID},
		ExpectedRevisions: map[string]string{
			fixture.master.ID: masterState.Revision,
			fixture.caves.ID:  cavesState.Revision,
		},
		ExpectedTopologyRevision: "room-revision-1",
		Enabled:                  false,
		Patch:                    map[string]json.RawMessage{"mode": json.RawMessage(`"hard"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.PublishedTargets != 2 || len(publisher.writes) != 2 {
		t.Fatalf("result=%#v writes=%#v", result, publisher.writes)
	}
	for key, content := range publisher.writes {
		if !bytes.Contains(content, []byte(`mode = "hard"`)) || !bytes.Contains(content, []byte(`enabled = false`)) {
			t.Fatalf("%s content=%s", key, content)
		}
	}
}

func TestRoomListUsesPlacementRuntimeFilesForLocalWorlds(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.placements.executions[fixture.room.ID][1] = testExecution(
		fixture.room, fixture.caves, "room-revision-1", "local", "local", "default",
	)
	fixture.mods.localList = mods.ModList{
		Items: []mods.ModState{{
			SteamMod: mods.SteamMod{ID: "100"}, Configured: true, Downloaded: true, Enabled: true,
			EnabledWorlds: []string{fixture.master.ID, fixture.caves.ID},
		}},
		Total: 1, Healthy: 1,
	}
	observer := &testRuntimeFileObserver{observation: &shared.RuntimeModFilesObservation{
		InstallationID: "default",
		Mods:           map[string]shared.RuntimeModFileState{"100": {Status: shared.RuntimeModFileReady}},
		Worlds:         map[string]shared.RuntimeModWorldFileState{},
		ObservedAt:     time.Now().UTC(),
	}}
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureRuntimeFileObserver(observer); err != nil {
		t.Fatal(err)
	}

	list, err := service.RoomList(context.Background(), fixture.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || !list.Items[0].Downloaded || !list.Items[0].Installed || !list.Items[0].RuntimeObserved {
		t.Fatalf("local room facts=%#v", list)
	}
	if fixture.mods.localListCalls != 0 || fixture.placements.cachedCount(fixture.room.ID) != 1 || len(observer.requests) != 1 {
		t.Fatalf("local list calls=%d cached topology reads=%d", fixture.mods.localListCalls, fixture.placements.cachedCount(fixture.room.ID))
	}
}

func TestRoomListAggregatesActualFilesAcrossRuntimePlacements(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.mods.localList = mods.ModList{
		Items: []mods.ModState{{
			SteamMod: mods.SteamMod{ID: "100"}, Configured: true, Enabled: true,
			EnabledWorlds: []string{fixture.master.ID, fixture.caves.ID},
		}}, Total: 1,
	}
	observer := &testRuntimeFileObserver{observations: map[string]*shared.RuntimeModFilesObservation{
		"local\x00default": {
			InstallationID: "default",
			Mods:           map[string]shared.RuntimeModFileState{"100": {Status: shared.RuntimeModFileReady}},
			Worlds:         map[string]shared.RuntimeModWorldFileState{},
		},
		"target-2\x00install-2": {
			InstallationID: "install-2",
			Mods:           map[string]shared.RuntimeModFileState{"100": {Status: shared.RuntimeModFileMissing}},
			Worlds:         map[string]shared.RuntimeModWorldFileState{},
		},
	}}
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureRuntimeFileObserver(observer); err != nil {
		t.Fatal(err)
	}

	list, err := service.RoomList(context.Background(), fixture.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := list.Items[0]
	if item.RuntimeFileStatus != "pending" || item.RuntimeTotalTargets != 2 || item.RuntimeReadyTargets != 1 ||
		item.RuntimePendingTargets != 1 || item.RuntimeUnavailableTargets != 0 || item.Installed {
		t.Fatalf("pending runtime aggregation=%#v", item)
	}

	observer.errors = map[string]error{"target-2\x00install-2": errors.New("Agent offline")}
	list, err = service.RoomList(context.Background(), fixture.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	item = list.Items[0]
	if item.RuntimeFileStatus != "unavailable" || item.RuntimeReadyTargets != 1 || item.RuntimeUnavailableTargets != 1 || item.RuntimePendingTargets != 0 {
		t.Fatalf("unavailable runtime aggregation=%#v", item)
	}
}

func TestRoomListDetectsVersionsAcrossRuntimePlacements(t *testing.T) {
	fixture := newSnapshotFixture(t)
	latest := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	current := latest
	old := latest.Add(-48 * time.Hour)
	fixture.mods.described = map[string]mods.SteamMod{"100": {ID: "100", Version: "1.9.8", UpdatedAt: latest}}
	fixture.mods.localList = mods.ModList{
		Items: []mods.ModState{{
			SteamMod: mods.SteamMod{ID: "100", Version: "1.9.8", UpdatedAt: latest}, LatestVersion: "1.9.8",
			Configured: true, Enabled: true, EnabledWorlds: []string{fixture.master.ID, fixture.caves.ID},
		}}, Total: 1,
	}
	observer := &testRuntimeFileObserver{observations: map[string]*shared.RuntimeModFilesObservation{
		"local\x00default": {
			InstallationID: "default",
			Mods: map[string]shared.RuntimeModFileState{"100": {
				Status: shared.RuntimeModFileReady, Version: "1.9.8", SteamManifestID: "new", SteamUpdatedAt: &current,
			}},
			Worlds: map[string]shared.RuntimeModWorldFileState{},
		},
		"target-2\x00install-2": {
			InstallationID: "install-2",
			Mods: map[string]shared.RuntimeModFileState{"100": {
				Status: shared.RuntimeModFileReady, Version: "1.9.6", SteamManifestID: "old", SteamUpdatedAt: &old,
			}},
			Worlds: map[string]shared.RuntimeModWorldFileState{},
		},
	}}
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureRuntimeFileObserver(observer); err != nil {
		t.Fatal(err)
	}

	list, err := service.RoomList(context.Background(), fixture.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := list.Items[0]
	if item.RuntimeVersion != "" || item.RuntimeVersionStatus != "mixed" || item.RuntimeCurrentTargets != 1 ||
		item.RuntimeOutdatedTargets != 1 || item.RuntimeUnknownTargets != 0 || len(item.RuntimeVersions) != 2 ||
		item.Health != mods.HealthUpdateAvailable {
		t.Fatalf("runtime versions = %#v", item)
	}
	updates, err := service.CheckUpdates(context.Background(), fixture.room.ID)
	if err != nil || len(updates.ModIDs) != 1 || updates.ModIDs[0] != "100" {
		t.Fatalf("updates = %#v, error = %v", updates, err)
	}
	fixture.mods.describeErr = errors.New("Steam version API unavailable")
	fixture.mods.described = nil
	if _, err := service.CheckUpdates(context.Background(), fixture.room.ID); err == nil {
		t.Fatal("Steam failure must not become a successful zero-update check")
	}
	fixture.mods.describeErr = nil
	if _, err := service.CheckUpdates(context.Background(), fixture.room.ID); err == nil {
		t.Fatal("missing Steam version evidence must not become a successful check")
	}
	fixture.mods.described = map[string]mods.SteamMod{"100": {ID: "100", SteamManifestID: "new", Version: "1.9.8", UpdatedAt: latest}}
	delete(observer.observations, "local\x00default")
	updates, err = service.CheckUpdates(context.Background(), fixture.room.ID)
	if err == nil || len(updates.ModIDs) != 1 || updates.ModIDs[0] != "100" {
		t.Fatalf("unavailable node must preserve known remote update and return an error: %#v, %v", updates, err)
	}
}

func TestInstallationModInventoryCombinesActualFilesWithPlacementReferences(t *testing.T) {
	fixture := newSnapshotFixture(t)
	observedAt := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	steamUpdatedAt := observedAt.Add(-time.Hour)
	observer := &testRuntimeFileObserver{observations: map[string]*shared.RuntimeModFilesObservation{
		"target-2\x00install-2": {
			InstallationID: "install-2",
			Mods: map[string]shared.RuntimeModFileState{
				"200": {
					Status: shared.RuntimeModFileReady, Version: "2.0.0", InstalledSize: 8192,
					SteamManifestID: "123456", SteamUpdatedAt: &steamUpdatedAt,
				},
			},
			Worlds: map[string]shared.RuntimeModWorldFileState{}, ObservedAt: observedAt,
		},
	}}
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureRuntimeFileObserver(observer); err != nil {
		t.Fatal(err)
	}

	inventory, err := service.InstallationModInventory(context.Background(), "target-2", "install-2")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.TargetID != "target-2" || inventory.InstallationID != "install-2" || inventory.Total != 1 || inventory.Unknown != 1 || !inventory.ObservedAt.Equal(observedAt) {
		t.Fatalf("inventory summary = %#v", inventory)
	}
	item := inventory.Items[0]
	if item.ID != "200" || item.Name != "Workshop 200" || item.CurrentVersion != "2.0.0" || item.InstalledSize != 8192 || item.SteamManifestID != "123456" {
		t.Fatalf("inventory item = %#v", item)
	}
	if len(item.RoomReferences) != 1 || item.RoomReferences[0].RoomID != fixture.room.ID || item.RoomReferences[0].WorldID != fixture.caves.ID {
		t.Fatalf("placement references = %#v", item.RoomReferences)
	}
}

func TestRuntimeModVersionStatusPrefersSteamManifestIdentity(t *testing.T) {
	latest := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	older := latest.Add(-48 * time.Hour)
	tests := []struct {
		name             string
		state            shared.RuntimeModFileState
		latestManifestID string
		latestVersion    string
		latestUpdatedAt  time.Time
		want             string
	}{
		{
			name: "matching manifest ignores Workshop metadata timestamp drift",
			state: shared.RuntimeModFileState{
				Status: shared.RuntimeModFileReady, SteamManifestID: "manifest-current", SteamUpdatedAt: &older,
			},
			latestManifestID: "manifest-current", latestUpdatedAt: latest, want: "current",
		},
		{
			name: "different manifest remains outdated even when version label matches",
			state: shared.RuntimeModFileState{
				Status: shared.RuntimeModFileReady, Version: "1.0.0", SteamManifestID: "manifest-old", SteamUpdatedAt: &latest,
			},
			latestManifestID: "manifest-current", latestVersion: "1.0.0", latestUpdatedAt: latest, want: "outdated",
		},
		{
			name: "timestamp remains fallback without manifest identity",
			state: shared.RuntimeModFileState{
				Status: shared.RuntimeModFileReady, SteamUpdatedAt: &older,
			},
			latestUpdatedAt: latest, want: "outdated",
		},
		{
			name: "version remains final fallback",
			state: shared.RuntimeModFileState{
				Status: shared.RuntimeModFileReady, Version: "1.0.0",
			},
			latestVersion: "1.0.0", want: "current",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := runtimeModVersionStatus(test.state, test.latestManifestID, test.latestVersion, test.latestUpdatedAt)
			if got != test.want {
				t.Fatalf("version status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestInstallationRoomIDsUsesAppliedInstallationPlacement(t *testing.T) {
	fixture := newSnapshotFixture(t)
	service, err := NewService(fixture.source, fixture.mods, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	roomIDs, err := service.InstallationRoomIDs(context.Background(), "target-2", "install-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(roomIDs) != 1 || roomIDs[0] != fixture.room.ID {
		t.Fatalf("installation rooms = %v", roomIDs)
	}
	roomIDs, err = service.InstallationRoomIDs(context.Background(), "local", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(roomIDs) != 1 || roomIDs[0] != fixture.room.ID {
		t.Fatalf("local installation rooms = %v", roomIDs)
	}
}

func TestPlacementAwareReadSurvivesLocalWorldRemovalAfterMigration(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.rooms.worlds[fixture.room.ID] = nil
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}

	file, err := service.ConfigurationFile(context.Background(), fixture.room.ID, fixture.caves.ID)
	if err != nil || file.Content != string(fixture.remote) || !file.Exists {
		t.Fatalf("configuration file=%#v err=%v", file, err)
	}
	configuration, err := service.Configuration(context.Background(), fixture.room.ID, fixture.caves.ID, "200")
	if err != nil || configuration.ModID != "200" {
		t.Fatalf("configuration=%#v err=%v", configuration, err)
	}
}

func TestPlacementAwareReadIsIsolatedFromUnrelatedBrokenRoom(t *testing.T) {
	fixture := newSnapshotFixture(t)
	broken := rooms.Room{ID: "room-broken", DirectoryName: "Cluster_Broken", Name: "Broken", Managed: true}
	fixture.rooms.rooms = append(fixture.rooms.rooms, broken)
	fixture.rooms.worlds[broken.ID] = []rooms.World{{ID: "master-broken", RoomID: broken.ID, DirectoryName: "Master", Name: "Master"}}
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	file, err := service.ConfigurationFile(context.Background(), fixture.room.ID, fixture.caves.ID)
	if err != nil || file.Content != string(fixture.remote) {
		t.Fatalf("unrelated room broke configuration read: file=%#v err=%v", file, err)
	}
	if _, err := service.RoomList(context.Background(), fixture.room.ID); err != nil {
		t.Fatalf("unrelated room broke Mod list: %v", err)
	}
	if fixture.placements.count(broken.ID) != 0 {
		t.Fatalf("read-only query resolved unrelated room %d times", fixture.placements.count(broken.ID))
	}
}

func TestPublicationSkipsUnrelatedInstallationConfiguration(t *testing.T) {
	fixture := newSnapshotFixture(t)
	otherRoom := rooms.Room{ID: "room-other", DirectoryName: "Cluster_Other", Name: "Other", Managed: true}
	otherWorld := rooms.World{ID: "master-other", RoomID: otherRoom.ID, DirectoryName: "Master", Name: "Master"}
	fixture.rooms.rooms = append(fixture.rooms.rooms, otherRoom)
	fixture.rooms.worlds[otherRoom.ID] = []rooms.World{otherWorld}
	fixture.placements.executions[otherRoom.ID] = []topology.ExecutionPlacement{
		testExecution(otherRoom, otherWorld, "other-revision-1", "local", "local", "other-installation"),
	}
	invalidPath := filepath.Join(fixture.root, otherRoom.DirectoryName, otherWorld.DirectoryName, "modoverrides.lua")
	if err := os.MkdirAll(filepath.Dir(invalidPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalidPath, []byte("this is not Lua"), 0o640); err != nil {
		t.Fatal(err)
	}
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Preview(context.Background(), fixture.room.ID, Request{
		Action: "reconcile", WorldIDs: []string{fixture.master.ID, fixture.caves.ID},
	})
	if err != nil {
		t.Fatalf("unrelated installation blocked publication: %v", err)
	}
	for _, world := range coordinator.lastWorlds {
		if world.RoomID == otherRoom.ID {
			t.Fatalf("unrelated installation leaked into publication: %#v", world)
		}
	}
}

func TestPublicationPreservesOtherRoomOnSharedInstallation(t *testing.T) {
	fixture := newSnapshotFixture(t)
	otherRoom := rooms.Room{ID: "room-shared", DirectoryName: "Cluster_Shared", Name: "Shared", Managed: true}
	otherWorld := rooms.World{ID: "master-shared", RoomID: otherRoom.ID, DirectoryName: "Master", Name: "Master"}
	otherContent := []byte("return { [\"workshop-700\"] = { enabled = true } }\n")
	fixture.rooms.rooms = append(fixture.rooms.rooms, otherRoom)
	fixture.rooms.worlds[otherRoom.ID] = []rooms.World{otherWorld}
	fixture.placements.executions[otherRoom.ID] = []topology.ExecutionPlacement{
		testExecution(otherRoom, otherWorld, "shared-revision-1", "target-2", "agent-2", "install-2"),
	}
	fixture.driver.overrides[otherRoom.DirectoryName+"\x00"+otherWorld.DirectoryName] = otherContent
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Preview(context.Background(), fixture.room.ID, Request{
		Action: "enable", ModID: "100", WorldIDs: []string{fixture.master.ID}, Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, world := range coordinator.lastWorlds {
		if world.RoomID == otherRoom.ID && world.WorldID == otherWorld.ID {
			found = true
			if !bytes.Equal(world.ModOverrides, otherContent) {
				t.Fatalf("shared world configuration changed: %s", world.ModOverrides)
			}
		}
	}
	if !found {
		t.Fatalf("shared installation world was omitted: %#v", coordinator.lastWorlds)
	}
}

func TestServiceRejectsUnknownSelectedWorld(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Preview(context.Background(), fixture.room.ID, Request{Action: "add", ModID: "400", WorldIDs: []string{"missing"}})
	if !errors.Is(err, rooms.ErrWorldNotFound) {
		t.Fatalf("expected world not found, got %v", err)
	}
}

func TestPlacementAwareReadReturnsWorldNotFound(t *testing.T) {
	fixture := newSnapshotFixture(t)
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfigurationFile(context.Background(), fixture.room.ID, "missing"); !errors.Is(err, rooms.ErrWorldNotFound) {
		t.Fatalf("configuration file error=%v", err)
	}
	if _, err := service.Configuration(context.Background(), fixture.room.ID, "missing", "100"); !errors.Is(err, rooms.ErrWorldNotFound) {
		t.Fatalf("configuration error=%v", err)
	}
	if _, err := service.PreviewConfiguration(context.Background(), fixture.room.ID, "missing", "100", mods.ConfigUpdateRequest{}); !errors.Is(err, rooms.ErrWorldNotFound) {
		t.Fatalf("configuration preview error=%v", err)
	}
}

func TestRetrySeparatesRecoveryAndExactTerminalRetry(t *testing.T) {
	fixture := newSnapshotFixture(t)
	basePlan := modpublication.Plan{
		Version: modpublication.PlanVersion, RoomID: fixture.room.ID, TopologyRevision: "topology-1",
		PlanHash: testSHA([]byte("plan-1")), Ready: true, RestartRequired: true,
		Targets: []modpublication.TargetPlan{{
			TargetID: "local", NodeID: "local", InstallationID: "default",
			Worlds: []modpublication.WorldPlan{{
				RoomID: fixture.room.ID, RoomDirectory: fixture.room.DirectoryName,
				WorldID: fixture.master.ID, WorldDirectory: fixture.master.DirectoryName, ModOverrides: fixture.local,
			}},
		}},
	}
	coordinator := &snapshotCoordinator{source: fixture.source, current: modpublication.Publication{ID: "publication-1", RoomID: fixture.room.ID, Status: modpublication.StatusPublishing, Plan: basePlan}}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := service.Retry(context.Background(), "job-1", "publication-1"); err != nil || value.Status != modpublication.StatusSucceeded || len(coordinator.recoveredIDs) != 1 {
		t.Fatalf("active recovery failed: value=%#v err=%v", value, err)
	}
	if len(coordinator.recoveryJobs) != 1 || coordinator.recoveryJobs[0] != "job-1" {
		t.Fatalf("active recovery source jobs=%#v", coordinator.recoveryJobs)
	}

	coordinator.current.Status = modpublication.StatusFailed
	coordinator.previewPlan = &basePlan
	if value, err := service.Retry(context.Background(), "job-2", "publication-1"); err != nil || value.Status != modpublication.StatusSucceeded || len(coordinator.published) != 1 || !coordinator.published[0].SkipProtectionBackup {
		t.Fatalf("exact terminal retry failed: value=%#v err=%v", value, err)
	}

	drifted := basePlan
	drifted.TopologyRevision = "topology-2"
	coordinator.previewPlan = &drifted
	if _, err := service.Retry(context.Background(), "job-3", "publication-1"); !errors.Is(err, ErrTopologyChanged) {
		t.Fatalf("topology drift was accepted: %v", err)
	}
	drifted = basePlan
	drifted.PlanHash = testSHA([]byte("plan-2"))
	coordinator.previewPlan = &drifted
	if _, err := service.Retry(context.Background(), "job-4", "publication-1"); !errors.Is(err, modpublication.ErrPlanChanged) {
		t.Fatalf("plan drift was accepted: %v", err)
	}
}
