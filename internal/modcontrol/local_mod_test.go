package modcontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/mods"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
	"dont/shared"
)

type firstOverridePublisher struct {
	updates []runtimedriver.ModOverridesUpdate
}

func (p *firstOverridePublisher) PublishModOverrides(_ context.Context, _ string, updates []runtimedriver.ModOverridesUpdate) (int, error) {
	p.updates = append(p.updates, updates...)
	for _, update := range updates {
		if update.ExpectedSHA256 != shaHex(nil) {
			return 0, errors.New("missing file was compared against a synthetic document")
		}
	}
	return len(updates), nil
}

func TestFirstModConfigurationPreservesAbsenceAndRawDigest(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "agent"}[remote], func(t *testing.T) {
			f := newSnapshotFixture(t)
			world := f.master
			if remote {
				world = f.caves
				f.driver.overrideHook = func(_ runtimedriver.Target, room, world string, offset int64) (shared.RuntimeModOverridesChunk, error) {
					return shared.RuntimeModOverridesChunk{Missing: true, RoomDirectory: room, WorldDirectory: world, SHA256: shaHex(nil), Complete: true}, nil
				}
			} else if err := os.Remove(filepath.Join(f.root, f.room.DirectoryName, world.DirectoryName, "modoverrides.lua")); err != nil {
				t.Fatal(err)
			}
			service, _ := NewService(f.source, f.mods, &snapshotCoordinator{source: f.source})
			file, err := service.ConfigurationFile(context.Background(), f.room.ID, world.ID)
			if err != nil || file.Exists || file.Content != "" {
				t.Fatalf("missing file=%+v err=%v", file, err)
			}
			publisher := &firstOverridePublisher{}
			service.ConfigureConfigurationPublisher(publisher)
			result, err := service.applyPreparedOverrides(context.Background(), proposal{roomID: f.room.ID, roomOnly: true, action: mods.OverrideActionAdd, modID: "400", modIDs: []string{"400"}, worldIDs: map[string]bool{world.ID: true}, enabled: true}, "")
			if err != nil || result.PublishedTargets != 1 || len(publisher.updates) != 1 || !strings.Contains(string(publisher.updates[0].Content), "workshop-400") {
				t.Fatalf("first save failed: %+v %v", result, err)
			}
			ctx := withPublicationSnapshot(context.Background(), proposal{roomID: f.room.ID, roomOnly: true, readOnly: true})
			worlds, err := f.source.ManagedWorlds(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range worlds {
				if item.WorldID == world.ID && string(item.ModOverrides) != "return {}\n" {
					t.Fatalf("missing publication payload: %q", item.ModOverrides)
				}
			}
		})
	}
}

func TestLocalInstallationReadNeverFallsBackFromUnknownScope(t *testing.T) {
	f := localMixedSnapshot(t)
	execution := f.placements.executions[f.room.ID][0]
	execution.Target.Config.InstallationID = "other"
	placement := publicationPlacement(execution, f.room.ID, f.master.ID)
	if _, err := f.source.readOverrides(context.Background(), execution, placement); !errors.Is(err, runtimedriver.ErrInvalidTarget) {
		t.Fatalf("unknown scope read default data: %v", err)
	}
	if _, err := f.source.configurationForExecution(context.Background(), execution, "local:fishdried_custom", f.local); !errors.Is(err, runtimedriver.ErrInvalidTarget) {
		t.Fatalf("unknown schema read default data: %v", err)
	}
	execution.Target.Installations = []agents.RuntimeInstallation{{ID: "unrelated", SavePath: f.root}}
	placement.InstallationID = "default"
	if _, err := f.source.readOverrides(context.Background(), execution, placement); !errors.Is(err, runtimedriver.ErrInvalidTarget) {
		t.Fatalf("registered scope read default data: %v", err)
	}
	otherRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	execution.Target.Installations = []agents.RuntimeInstallation{{ID: "other", SavePath: otherRoot}}
	placement.InstallationID = "other"
	data, err := f.source.readOverrides(context.Background(), execution, placement)
	if err != nil || data != nil {
		t.Fatalf("read another installation: %q %v", data, err)
	}
}

func TestDisabledLocalModRetainsFileFactsWithoutClaimingLoaded(t *testing.T) {
	f := localMixedSnapshot(t)
	f.mods.localList = mods.ModList{Items: []mods.ModState{{SteamMod: mods.SteamMod{ID: "local:fishdried_custom", Source: "local"}, Configured: true, ConfiguredWorlds: []string{f.master.ID}}}}
	service, _ := NewService(f.source, f.mods, &snapshotCoordinator{source: f.source})
	observer := &testRuntimeFileObserver{observation: &shared.RuntimeModFilesObservation{Mods: map[string]shared.RuntimeModFileState{"local:fishdried_custom": {Status: shared.RuntimeModFileReady, Name: "鱼干", Version: "2"}}}}
	service.ConfigureRuntimeFileObserver(observer)
	result, err := service.RoomFacts(context.Background(), f.room.ID)
	if err != nil || len(result.Items) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	item := result.Items[0]
	if item.Name != "鱼干" || !item.Installed || item.Loaded || item.RuntimeFileStatus != "disabled" || item.RuntimeVersionStatus != "local" || len(observer.requests) != 1 {
		t.Fatalf("disabled local facts lost: %+v", item)
	}
}

func TestPublicationLocalAddRequiresFilesOnEverySelectedInstallation(t *testing.T) {
	f := localMixedSnapshot(t)
	service, _ := NewService(f.source, f.mods, &snapshotCoordinator{source: f.source})
	observer := &testRuntimeFileObserver{observations: map[string]*shared.RuntimeModFilesObservation{
		"local\x00default":      {Mods: map[string]shared.RuntimeModFileState{"local:fishdried_custom": {Status: shared.RuntimeModFileReady}}},
		"target-2\x00install-2": {Mods: map[string]shared.RuntimeModFileState{}},
	}}
	service.ConfigureRuntimeFileObserver(observer)
	_, err := service.Preview(context.Background(), f.room.ID, Request{Action: "add", ModID: "local:fishdried_custom", WorldIDs: []string{f.master.ID, f.caves.ID}, Enabled: true})
	if !errors.Is(err, mods.ErrModNotDownloaded) {
		t.Fatalf("publication accepted missing remote local files: %v", err)
	}
}

func localMixedSnapshot(t *testing.T) snapshotFixture {
	t.Helper()
	f := newSnapshotFixture(t)
	content := `return { fishdried_custom={enabled=true,configuration_options={mode="easy",future={keep=42}}},custom="preserve",["workshop-100"]={enabled=true} }`
	f.local = []byte(content)
	if err := os.WriteFile(filepath.Join(f.root, f.room.DirectoryName, "Master", "modoverrides.lua"), f.local, 0640); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLocalModToggleChangesOnlySelectedWorldAndRetainsWorkshopContent(t *testing.T) {
	f := localMixedSnapshot(t)
	service, _ := NewService(f.source, f.mods, &snapshotCoordinator{source: f.source})
	publisher := &modConfigurationPublisherFixture{}
	service.ConfigureConfigurationPublisher(publisher)
	state, _ := mods.InspectModOverride(f.local)
	result, err := service.SetEnabled(context.Background(), f.room.ID, "fishdried_custom", mods.EnableRequest{WorldIDs: []string{f.master.ID}, Enabled: false, ExpectedRevision: state.Revision})
	if err != nil || result.PublishedTargets != 1 || len(publisher.writes) != 1 {
		t.Fatalf("%+v %v writes=%d", result, err, len(publisher.writes))
	}
	next := string(publisher.writes[f.room.ID+"/"+f.master.ID])
	for _, want := range []string{"fishdried_custom", "enabled = false", "workshop-100", "keep = 42", `custom = "preserve"`} {
		if !strings.Contains(next, want) {
			t.Fatalf("lost %q: %s", want, next)
		}
	}
	data, _ := os.ReadFile(filepath.Join(f.root, f.room.DirectoryName, "Caves", "modoverrides.lua"))
	if string(data) != string(f.decoy) {
		t.Fatal("changed controller decoy or unselected world")
	}
	// Content publication is still Workshop-only, with the entire Lua document.
	ctx := withPublicationSnapshot(context.Background(), proposal{roomID: f.room.ID, roomOnly: true, readOnly: true})
	worlds, err := f.source.ManagedWorlds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, world := range worlds {
		for _, mod := range world.Mods {
			if !mods.ValidID(mod.WorkshopID) {
				t.Fatal("local identity entered Workshop content", mod)
			}
		}
	}
}

func TestLocalModOldAgentPreflightBlocksAllWrites(t *testing.T) {
	f := localMixedSnapshot(t)
	f.driver.overrides[f.room.DirectoryName+"\x00"+f.caves.DirectoryName] = append([]byte(nil), f.local...)
	service, _ := NewService(f.source, f.mods, &snapshotCoordinator{source: f.source})
	publisher := &modConfigurationPublisherFixture{}
	service.ConfigureConfigurationPublisher(publisher)
	state, _ := mods.InspectModOverride(f.local)
	_, err := service.SetEnabled(context.Background(), f.room.ID, "local:fishdried_custom", mods.EnableRequest{WorldIDs: []string{f.master.ID, f.caves.ID}, ExpectedRevisions: map[string]string{f.master.ID: state.Revision, f.caves.ID: state.Revision}})
	if !errors.Is(err, runtimedriver.ErrCapabilityMissing) || len(publisher.writes) != 0 {
		t.Fatalf("old Agent received writes: %v %+v", err, publisher.writes)
	}
}

func TestMigrationWithLocalReferencesFailsBeforePreviewOrWrite(t *testing.T) {
	f := localMixedSnapshot(t)
	coordinator := &snapshotCoordinator{source: f.source}
	service, _ := NewService(f.source, f.mods, coordinator)
	execution := f.placements.executions[f.room.ID][1]
	_, err := service.PreparePlacementMigration(context.Background(), topology.MigrationPlacement{
		Room: f.room, World: f.master, Revision: "migration-1", SourceTargetID: "local", SourceInstallationID: "default",
		TargetTargetID: execution.AppliedTargetID, TargetInstallationID: "install-2", Target: execution.Target,
	}, reconciliationLease(f.room.ID))
	if err == nil || !strings.Contains(err.Error(), "本地模组") || len(coordinator.published) != 0 {
		t.Fatalf("unsafe migration: %v", err)
	}
}

func TestLoadedLocalModRequiresCurrentProcessAndLogGeneration(t *testing.T) {
	f := localMixedSnapshot(t)
	started := time.Now().UTC().Truncate(time.Second)
	execution := &f.placements.executions[f.room.ID][0]
	execution.Inventory = agents.RuntimeTargetInventory{Available: true, Inventory: shared.RuntimeInventoryReport{Processes: []shared.ShardProcessReport{{PID: 123, Cluster: f.room.DirectoryName, Shard: f.master.DirectoryName, StorageRoot: f.root, StartedAt: &started}}}}
	service, _ := NewService(f.source, f.mods, &snapshotCoordinator{source: f.source})
	ctx := withPublicationSnapshot(context.Background(), proposal{roomID: f.room.ID, roomOnly: true, readOnly: true})
	if _, err := f.source.ManagedWorlds(ctx); err != nil {
		t.Fatal(err)
	}
	request := RuntimeFileRequest{TargetID: "local", InstallationID: "default", Worlds: []RuntimeFileWorld{{WorldID: f.master.ID, WorldDirectory: f.master.DirectoryName}}}
	world := shared.RuntimeModWorldFileState{WorldID: f.master.ID, LogObserved: true, LogStartedAt: started.Add(time.Second)}
	if !service.currentModLog(ctx, f.room, request, world) {
		t.Fatal("current boot not recognized")
	}
	world.LogStartedAt = started.Add(-time.Hour)
	if service.currentModLog(ctx, f.room, request, world) {
		t.Fatal("previous boot counted as loaded")
	}
	execution.Inventory.Inventory.Processes = nil
	ctx = withPublicationSnapshot(context.Background(), proposal{roomID: f.room.ID, roomOnly: true, readOnly: true})
	if _, err := f.source.ManagedWorlds(ctx); err != nil {
		t.Fatal(err)
	}
	world.LogStartedAt = started
	if service.currentModLog(ctx, f.room, request, world) {
		t.Fatal("stopped world counted as loaded")
	}
}
