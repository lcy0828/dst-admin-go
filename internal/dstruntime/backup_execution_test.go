package dstruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProtectionBackupRetainsEditedExecutionScript(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	world := catalog.worlds[0]
	first, err := manager.InstallWorld(context.Background(), catalog.room.ID, world.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Cluster_1", "Master", executionDirectory, "bootstrap.lua")
	edited := []byte("-- operator modification\nreturn true\n")
	if err := os.WriteFile(path, edited, 0640); err != nil {
		t.Fatal(err)
	}
	rolled, err := manager.RollbackWorld(context.Background(), catalog.room.ID, world.ID, filepath.Base(first.BackupPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RollbackWorld(context.Background(), catalog.room.ID, world.ID, filepath.Base(rolled.BackupPath)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(edited) {
		t.Fatalf("protection backup lost edited execution file; error=%v bytes=%d expected=%d", err, len(got), len(edited))
	}
}

// Old on-disk backups do not have activity.lua entries. Restore their exact
// layout while leaving activity data and game saves outside Runtime assets.
func TestSevenModuleBackupRemainsRestorableAfterActivityUpgrade(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Cluster_1", "Master")
	id, err := manager.createStateBackup(path, "historical layout")
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(path, backupDirectory, id, backupManifestName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var value backupManifest
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	files := value.Files[:0]
	for _, file := range value.Files {
		if filepath.Base(file.Path) != "activity.lua" {
			files = append(files, file)
		}
	}
	value.Files = files
	raw, _ = json.Marshal(value)
	if err = os.WriteFile(manifestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(path, "save", "mod_config_data", "dst-admin", "activity-01.json")
	if err = os.WriteFile(protected, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = manager.restoreStateBackup(path, id); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{managedDirectory, executionDirectory} {
		if _, err = os.Stat(filepath.Join(path, directory, "activity.lua")); !os.IsNotExist(err) {
			t.Fatalf("new module retained after old layout restore: %v", err)
		}
	}
	if got, err := os.ReadFile(protected); err != nil || string(got) != "keep" {
		t.Fatal("activity spool modified")
	}
}
