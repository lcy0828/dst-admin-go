package moddistribution

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadedModsRequireActualLoadAndExactID(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "server_log.txt")
	data := "[00:00:00]: ModIndex: workshop-100 registered\n" +
		"[00:00:00]: Loading mod: workshop-1000 (Test) Version:1\n" +
		"[00:00:00]: Mod: workshop-200 (Test)\tLoading modmain.lua\n" +
		"[00:00:00]: WARNING loading modinfo.lua: workshop-300\n"
	if err := os.WriteFile(path, []byte(data), 0o640); err != nil {
		t.Fatal(err)
	}
	loaded, observed := observeLoadedMods(path, []string{"100", "1000", "200", "300"})
	if !observed || !slices.Equal(loaded, []string{"1000", "200"}) {
		t.Fatalf("loaded=%v observed=%v", loaded, observed)
	}
}

func TestDownloadedModWithoutLocalEntryIsNotRuntimeReady(t *testing.T) {
	server, content := localModFixture(t)
	observation, err := ObserveInstallationFiles(context.Background(), TrustedInstallation{
		ID: "native", ServerPath: server, SavePath: server, WorkshopContentPath: content,
	}, []string{"123"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := observation.Mods["123"]
	if state.Status != FileReady || state.Reason != "local_mod_entry_missing" {
		t.Fatalf("downloaded content must report its missing load entry: %#v", state)
	}
}

func TestObserveFilesReadsDiskWithoutInstallationState(t *testing.T) {
	environment := newTestEnvironment(t, 0)
	readyRoot := filepath.Join(environment.server, "mods", "workshop-100")
	partialRoot := filepath.Join(environment.server, "mods", "workshop-200")
	if err := os.MkdirAll(readyRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readyRoot, "modinfo.lua"), []byte("name='ready'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(partialRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(environment.saves, "Cluster_1", "Master", "server_log.txt")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("[00:00:18]: Loading mod: workshop-100\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	observed, err := environment.manager.ObserveFiles(context.Background(), "primary", []string{"100", "200", "300"}, []ObserveWorld{{
		RoomID: "room-1", RoomDirectory: "Cluster_1", WorldID: "master", WorldDirectory: "Master",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Mods["100"].Status != FileReady || observed.Mods["200"].Status != FileInvalid || observed.Mods["300"].Status != FileMissing {
		t.Fatalf("unexpected file states: %#v", observed.Mods)
	}
	world := observed.Worlds["room-1/master"]
	if !world.LogObserved || len(world.LoadedModIDs) != 1 || world.LoadedModIDs[0] != "100" {
		t.Fatalf("unexpected world observation: %#v", world)
	}
	if _, err := os.Stat(environment.manager.statePath()); !os.IsNotExist(err) {
		t.Fatalf("read-only observation created publication state: %v", err)
	}
}

func TestObserveFilesReportsInstalledVersionAndWorkshopManifest(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(root, "server")
	saves := filepath.Join(root, "saves")
	workshopRoot := filepath.Join(root, "workshop", "steamapps", "workshop")
	content := filepath.Join(workshopRoot, "content", "322330")
	modRoot := filepath.Join(content, "376333686")
	for _, directory := range []string{server, saves, modRoot} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(modRoot, "modinfo.lua"), []byte("name='简易血条(重构版)'\nversion = '1.9.6'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifest := strings.ReplaceAll(`
"AppWorkshop"
{
  "appid" "322330"
  "WorkshopItemsInstalled"
  {
    "376333686"
    {
      "manifest" "3421201230228906491"
      "size" "12345"
      "timeupdated" "1785022469"
    }
  }
}`, "  ", "\t")
	if err := os.WriteFile(filepath.Join(workshopRoot, "appworkshop_322330.acf"), []byte(manifest), 0o640); err != nil {
		t.Fatal(err)
	}

	observed, err := ObserveInstallationFiles(context.Background(), TrustedInstallation{
		ID: "native", ServerPath: server, SavePath: saves, WorkshopContentPath: content,
		WorkshopManifestPath: filepath.Join(workshopRoot, "appworkshop_322330.acf"),
	}, []string{"376333686"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := observed.Mods["376333686"]
	expectedTime := time.Unix(1785022469, 0).UTC()
	if state.Status != FileReady || state.Name != "简易血条(重构版)" || state.Version != "1.9.6" || state.SteamManifestID != "3421201230228906491" ||
		state.SteamUpdatedAt == nil || !state.SteamUpdatedAt.Equal(expectedTime) || state.InstalledSize != 12345 || state.MetadataReason != "" {
		t.Fatalf("observed Mod version = %#v", state)
	}
}

func TestInventoryInstallationFilesEnumeratesOnlyInstalledWorkshopDirectories(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(root, "server")
	saves := filepath.Join(root, "saves")
	workshopRoot := filepath.Join(root, "workshop", "steamapps", "workshop")
	content := filepath.Join(workshopRoot, "content", "322330")
	for _, directory := range []string{
		server, saves,
		filepath.Join(content, "376333686"),
		filepath.Join(content, "378160973"),
		filepath.Join(content, "not-a-workshop-id"),
	} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(content, "376333686", "modinfo.lua"), []byte("version = '1.9.8'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifest := `"AppWorkshop"
{
  "appid" "322330"
  "WorkshopItemsInstalled"
  {
    "376333686" { "manifest" "101" "size" "4096" "timeupdated" "1785022469" }
    "999999999" { "manifest" "102" "size" "8192" "timeupdated" "1785022470" }
  }
}`
	if err := os.WriteFile(filepath.Join(workshopRoot, "appworkshop_322330.acf"), []byte(manifest), 0o640); err != nil {
		t.Fatal(err)
	}

	observed, err := InventoryInstallationFiles(context.Background(), TrustedInstallation{
		ID: "native", ServerPath: server, SavePath: saves, WorkshopContentPath: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Mods) != 2 {
		t.Fatalf("inventory should follow actual directories, got %#v", observed.Mods)
	}
	ready := observed.Mods["376333686"]
	if ready.Status != FileReady || ready.Version != "1.9.8" || ready.InstalledSize != 4096 || ready.SteamManifestID != "101" {
		t.Fatalf("ready inventory item = %#v", ready)
	}
	invalid := observed.Mods["378160973"]
	if invalid.Status != FileInvalid || invalid.Reason != "missing_modinfo" {
		t.Fatalf("invalid inventory item = %#v", invalid)
	}
	if _, exists := observed.Mods["999999999"]; exists {
		t.Fatal("manifest-only item must not be reported as installed content")
	}
	if len(observed.Worlds) != 0 || observed.ObservedAt.IsZero() {
		t.Fatalf("unexpected inventory envelope: %#v", observed)
	}
}

func TestInventoryInstallationFilesUsesServerModsFallback(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(root, "server")
	saves := filepath.Join(root, "saves")
	modRoot := filepath.Join(server, "mods", "workshop-1392778117")
	for _, directory := range []string{modRoot, saves} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(modRoot, "modinfo.lua"), []byte("version = '7.6.5'\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	observed, err := InventoryInstallationFiles(context.Background(), TrustedInstallation{
		ID: "default", ServerPath: server, SavePath: saves,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Mods) != 1 || observed.Mods["1392778117"].Version != "7.6.5" {
		t.Fatalf("fallback inventory = %#v", observed.Mods)
	}
}
