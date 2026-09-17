package modcontrol

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dont/internal/mods"
)

func TestRoomUpdatesDownloadOncePerAssignedInstallationWithoutPublishing(t *testing.T) {
	for _, scenario := range []string{"local", "remote", "split", "partial-failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSnapshotFixture(t)
			executions := f.placements.executions[f.room.ID]
			switch scenario {
			case "local":
				executions[1] = executions[0]
				executions[1].World = f.caves
			case "remote":
				executions[0] = executions[1]
				executions[0].World = f.master
				f.driver.overrides[f.room.DirectoryName+"\x00"+f.master.DirectoryName] = f.local
			}
			f.mods.localList = mods.ModList{Items: []mods.ModState{
				{SteamMod: mods.SteamMod{ID: "100"}, Enabled: true, EnabledWorlds: []string{f.master.ID, f.caves.ID}},
				{SteamMod: mods.SteamMod{ID: "200"}, Enabled: true, EnabledWorlds: []string{f.master.ID}},
				{SteamMod: mods.SteamMod{ID: "300"}, Enabled: true, EnabledWorlds: []string{f.caves.ID}},
				{SteamMod: mods.SteamMod{ID: "400"}, Enabled: false},
			}}
			fetcher := &parallelModFetcher{}
			if scenario == "partial-failure" {
				fetcher.fail = "target-2"
			}
			coordinator := &snapshotCoordinator{source: f.source}
			service, err := NewService(f.source, f.mods, coordinator)
			if err != nil {
				t.Fatal(err)
			}
			publisher := &modConfigurationPublisherFixture{}
			service.installer, service.configuration = fetcher, publisher
			result, err := service.UpdateRoomMods(context.Background(), f.room.ID, []string{"100", "200", "300", "400", "500"}, io.Discard)
			if scenario == "partial-failure" {
				if err == nil || !strings.Contains(err.Error(), "1/2") ||
					!strings.Contains(err.Error(), "target-2/install-2") || !strings.Contains(err.Error(), "Workshop 100, 300") {
					t.Fatalf("partial failure missing details: %v", err)
				}
			} else if err != nil || !reflect.DeepEqual(result.ModIDs, []string{"100", "200", "300"}) {
				t.Fatalf("updates=%#v err=%v", result, err)
			}
			want := map[string][]string{"local/default": {"100", "200"}, "target-2/install-2": {"100", "300"}}
			if scenario == "local" {
				want = map[string][]string{"local/default": {"100", "200", "300"}}
			}
			if scenario == "remote" {
				want = map[string][]string{"target-2/install-2": {"100", "200", "300"}}
			}
			if !reflect.DeepEqual(fetcher.modIDsByInstallation, want) || fetcher.calls != len(want) {
				t.Fatalf("wrong destinations or duplicate downloads: %#v calls=%d", fetcher.modIDsByInstallation, fetcher.calls)
			}
			if len(coordinator.published) != 0 || len(publisher.writes) != 0 {
				t.Fatal("update published room configuration")
			}
			for world, before := range map[string][]byte{"Master": f.local, "Caves": f.decoy} {
				after, readErr := os.ReadFile(filepath.Join(f.root, f.room.DirectoryName, world, "modoverrides.lua"))
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatalf("configuration changed: %s %v", world, readErr)
				}
			}
			if !bytes.Equal(f.driver.overrides[f.room.DirectoryName+"\x00"+f.caves.DirectoryName], f.remote) {
				t.Fatal("remote configuration changed")
			}
		})
	}
}
