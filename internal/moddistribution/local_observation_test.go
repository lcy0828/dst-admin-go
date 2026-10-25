package moddistribution

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalInventorySeparatesNumericDirectoriesAndIgnoresHelpersAndLinks(t *testing.T) {
	e := newTestEnvironment(t, 0)
	for _, name := range []string{"123", "workshop-123", "fishdried_custom", "鱼干_自用", "workshop-custom"} {
		dir := filepath.Join(e.server, "mods", name)
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "modinfo.lua"), []byte("name='local name'\nversion='1'\n"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(e.server, "mods", "helper"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.server, "mods", "fishdried_custom"), filepath.Join(e.server, "mods", "escape")); err != nil {
		t.Fatal(err)
	}
	result, err := e.manager.InventoryMods(context.Background(), "primary")
	if err != nil || len(result.Mods) != 5 {
		t.Fatalf("%+v %v", result, err)
	}
	for _, id := range []string{"123", "local:123", "local:fishdried_custom", "local:鱼干_自用", "local:workshop-custom"} {
		if result.Mods[id].Status != FileReady {
			t.Fatalf("missing %s: %+v", id, result.Mods)
		}
	}
	legacy, err := e.manager.InventoryFiles(context.Background(), "primary")
	if err != nil || len(legacy.Mods) != 1 || legacy.Mods["123"].Status != FileReady {
		t.Fatalf("legacy payload changed: %+v %v", legacy, err)
	}
}

func TestPlanDoesNotReconstructExistingMixedConfigurationFromWorkshopSubset(t *testing.T) {
	e := newTestEnvironment(t, 0)
	original := []byte(`return {fishdried_custom={enabled=true},operator_value={keep=1}}`)
	writeWorldConfig(t, e.saves, "Master", original)
	input := PlanInput{OperationID: "local-guard-plan-01", NodeID: "node-main", Shards: []ShardRelease{{InstallationID: "primary", RoomID: "room-1", RoomDirectory: "Cluster_1", WorldID: "master", WorldDirectory: "Master"}}}
	if _, err := e.manager.BuildPlan(context.Background(), input); err == nil {
		t.Fatal("reconstructed an existing file")
	}
	assertFileContent(t, filepath.Join(e.saves, "Cluster_1", "Master", "modoverrides.lua"), original)
	input.Shards[0].ModOverrides = original
	if _, err := e.manager.BuildPlan(context.Background(), input); err != nil {
		t.Fatal(err)
	}
}

func TestReadOverridesRejectsEmptyAndSymlinkParent(t *testing.T) {
	e := newTestEnvironment(t, 0)
	if data, err := ReadOverrides(context.Background(), e.saves, "Cluster_1", "Master"); err != nil || data != nil {
		t.Fatalf("missing=%q %v", data, err)
	}
	writeWorldConfig(t, e.saves, "Master", nil)
	if _, err := ReadOverrides(context.Background(), e.saves, "Cluster_1", "Master"); err == nil {
		t.Fatal("existing empty file accepted")
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.saves, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOverrides(context.Background(), e.saves, "linked", "Master"); err == nil {
		t.Fatal("symlink parent accepted as missing file")
	}
}
