package modcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
	"dont/shared"
)

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
		calls: make(map[string]int),
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
	if _, err := service.Configuration(context.Background(), fixture.room.ID, fixture.caves.ID, "200"); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(fixture.mods.configurationContent, fixture.decoy) || !bytes.Equal(fixture.mods.configurationContent, fixture.remote) {
		t.Fatalf("configuration used wrong placement content: %s", fixture.mods.configurationContent)
	}
	list, err := service.RoomList(context.Background(), fixture.room.ID)
	if err != nil || list.Total != 2 {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	if !bytes.Equal(fixture.mods.listContents[fixture.caves.ID], fixture.remote) {
		t.Fatalf("room list used wrong content: %#v", fixture.mods.listContents)
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
	if value, err := service.Retry(context.Background(), "job-2", "publication-1"); err != nil || value.Status != modpublication.StatusSucceeded || len(coordinator.published) != 1 {
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
