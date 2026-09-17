package modcontrol

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"dont/internal/mods"
	"dont/shared"
)

type unavailableMetadataCatalog struct {
	*testModCatalog
	calls int
}

func TestCheckUpdatesReportsBatchFailureOnceAndKeepsPartialUpdates(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			f := newSnapshotFixture(t)
			cause := fmt.Errorf("load Steam Workshop details: %w", context.DeadlineExceeded)
			f.mods.describeErr = cause
			if partial {
				f.mods.described = map[string]mods.SteamMod{"100": {ID: "100", SteamManifestID: "new"}}
			}
			files := make(map[string]shared.RuntimeModFileState)
			for i := 100; i < 121; i++ {
				id := fmt.Sprint(i)
				f.mods.localList.Items = append(f.mods.localList.Items, mods.ModState{
					SteamMod: mods.SteamMod{ID: id}, Enabled: true, Configured: true,
					EnabledWorlds: []string{f.master.ID, f.caves.ID},
				})
				files[id] = shared.RuntimeModFileState{Status: shared.RuntimeModFileReady, SteamManifestID: "old"}
			}
			observer := &testRuntimeFileObserver{observations: map[string]*shared.RuntimeModFilesObservation{
				"local\x00default":      {InstallationID: "default", Mods: files},
				"target-2\x00install-2": {InstallationID: "install-2", Mods: files},
			}}
			service, err := NewService(f.source, f.mods, &snapshotCoordinator{source: f.source})
			if err != nil {
				t.Fatal(err)
			}
			if err := service.ConfigureRuntimeFileObserver(observer); err != nil {
				t.Fatal(err)
			}
			result, err := service.CheckUpdates(context.Background(), f.room.ID)
			if !errors.Is(err, context.DeadlineExceeded) || err.Error() != cause.Error() {
				t.Fatalf("batch error expanded into per-Mod failures: %v", err)
			}
			want := 0
			if partial {
				want = 1
			}
			if len(result.ModIDs) != want || partial && result.ModIDs[0] != "100" {
				t.Fatalf("lost confirmed updates: %#v", result)
			}
		})
	}
}

func (c *unavailableMetadataCatalog) Describe(context.Context, []string) (map[string]mods.SteamMod, error) {
	c.calls++
	return nil, errors.New("Steam unavailable")
}

func TestModFactsUseRuntimeWithoutWorkshopMetadata(t *testing.T) {
	fixture := newSnapshotFixture(t)
	fixture.mods.localList = mods.ModList{Items: []mods.ModState{{
		SteamMod: mods.SteamMod{ID: "100"}, Configured: true, Enabled: true,
		EnabledWorlds: []string{fixture.master.ID, fixture.caves.ID},
	}}}
	catalog := &unavailableMetadataCatalog{testModCatalog: fixture.mods}
	service, err := NewService(fixture.source, catalog, &snapshotCoordinator{source: fixture.source})
	if err != nil {
		t.Fatal(err)
	}
	observer := &testRuntimeFileObserver{observation: &shared.RuntimeModFilesObservation{
		InstallationID: "install-2", Mods: map[string]shared.RuntimeModFileState{
			"100": {Status: shared.RuntimeModFileReady, Name: "Runtime name", Version: "1.2", SteamManifestID: "10"},
		},
	}}
	if err := service.ConfigureRuntimeFileObserver(observer); err != nil {
		t.Fatal(err)
	}
	list, err := service.RoomFacts(context.Background(), fixture.room.ID)
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("facts=%#v, %v", list, err)
	}
	item := list.Items[0]
	if item.Name != "Runtime name" || item.RuntimeVersion != "1.2" || item.RuntimeFileStatus != "ready" || item.RuntimeVersionStatus != "unknown" || item.RuntimeOutdatedTargets != 0 {
		t.Fatalf("facts confused installation and latest metadata: %#v", item)
	}
	inventory, err := service.InstallationModFacts(context.Background(), "target-2", "install-2")
	if err != nil || len(inventory.Items) != 1 || inventory.Items[0].Name != "Runtime name" || inventory.Items[0].VersionStatus != "unknown" {
		t.Fatalf("inventory=%#v, %v", inventory, err)
	}
	if catalog.calls != 0 {
		t.Fatalf("facts called Steam %d times", catalog.calls)
	}
	observer.err, observer.observation = errors.New("node offline"), nil
	list, err = service.RoomFacts(context.Background(), fixture.room.ID)
	if err != nil || list.Items[0].RuntimeFileStatus != "unavailable" || list.Items[0].RuntimeReadyTargets != 0 {
		t.Fatalf("remote failure replaced by local files: %#v, %v", list, err)
	}
}
