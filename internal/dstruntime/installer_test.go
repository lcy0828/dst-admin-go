package dstruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/rooms"
)

type runtimeTestCatalog struct {
	room   rooms.Room
	worlds []rooms.World
}

func (c runtimeTestCatalog) Room(id string) (rooms.Room, error) {
	if id != c.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return c.room, nil
}

func (c runtimeTestCatalog) World(roomID, worldID string) (rooms.World, error) {
	if roomID != c.room.ID {
		return rooms.World{}, rooms.ErrRoomNotFound
	}
	for _, world := range c.worlds {
		if world.ID == worldID {
			return world, nil
		}
	}
	return rooms.World{}, rooms.ErrWorldNotFound
}

func (c runtimeTestCatalog) Worlds(roomID string) ([]rooms.World, error) {
	if roomID != c.room.ID {
		return nil, rooms.ErrRoomNotFound
	}
	return append([]rooms.World(nil), c.worlds...), nil
}

func newRuntimeTestManager(t *testing.T) (*Manager, runtimeTestCatalog, string) {
	t.Helper()
	root := t.TempDir()
	catalog := runtimeTestCatalog{
		room: rooms.Room{ID: rooms.EncodeID("Cluster_1"), DirectoryName: "Cluster_1", Name: "测试房间", Managed: true},
		worlds: []rooms.World{
			{ID: rooms.EncodeID("Master"), RoomID: rooms.EncodeID("Cluster_1"), DirectoryName: "Master", Name: "地面"},
			{ID: rooms.EncodeID("Caves"), RoomID: rooms.EncodeID("Cluster_1"), DirectoryName: "Caves", Name: "洞穴"},
		},
	}
	for _, world := range catalog.worlds {
		if err := os.MkdirAll(filepath.Join(root, catalog.room.DirectoryName, world.DirectoryName, "save"), 0750); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := NewManager(root, catalog)
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC) }
	return manager, catalog, root
}

func TestInstallRoomPreservesUserCommandsAndIsIdempotent(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	userSource := []byte("-- user comment\nfunction hello() return 'world' end\n")
	masterCustom := filepath.Join(root, "Cluster_1", "Master", customCommandsName)
	if err := os.WriteFile(masterCustom, userSource, 0600); err != nil {
		t.Fatal(err)
	}
	statuses, err := manager.InstallRoom(context.Background(), catalog.room.ID)
	if err != nil || len(statuses) != 2 || !statuses[0].Changed || !statuses[1].Changed {
		t.Fatalf("install statuses = %#v, error = %v", statuses, err)
	}
	installed, err := os.ReadFile(masterCustom)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(installed, userSource) || bytes.Count(installed, []byte("DST-ADMIN MANAGED BLOCK BEGIN")) != 1 {
		t.Fatalf("user source was not preserved: %q", installed)
	}
	if mode := fileMode(t, masterCustom); mode != 0600 {
		t.Fatalf("customcommands mode = %o", mode)
	}
	for _, world := range catalog.worlds {
		for _, name := range append(managedAssetNames, manifestName) {
			path := filepath.Join(root, "Cluster_1", world.DirectoryName, managedDirectory, name)
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("managed asset %s missing: %v", path, err)
			}
		}
		output := filepath.Join(root, "Cluster_1", world.DirectoryName, "save", "mod_config_data", managedDirectory)
		if info, err := os.Stat(output); err != nil || !info.IsDir() || info.Mode().Perm() != 0750 {
			t.Fatalf("runtime output directory %s invalid: info=%v error=%v", output, info, err)
		}
	}
	second, err := manager.InstallRoom(context.Background(), catalog.room.ID)
	if err != nil || second[0].Changed || second[1].Changed {
		t.Fatalf("idempotent install = %#v, error = %v", second, err)
	}
	after, _ := os.ReadFile(masterCustom)
	if !bytes.Equal(installed, after) {
		t.Fatal("idempotent install rewrote customcommands.lua")
	}
}

func TestInstallRepairsMissingRuntimeOutputDirectoryWithoutRewritingAssets(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	world := catalog.worlds[0]
	first, err := manager.InstallWorld(context.Background(), catalog.room.ID, world.ID)
	if err != nil || !first.Changed {
		t.Fatalf("first install = %#v, error = %v", first, err)
	}
	output := filepath.Join(root, catalog.room.DirectoryName, world.DirectoryName, "save", "mod_config_data", managedDirectory)
	if err := os.RemoveAll(output); err != nil {
		t.Fatal(err)
	}
	second, err := manager.InstallWorld(context.Background(), catalog.room.ID, world.ID)
	if err != nil || second.Changed {
		t.Fatalf("repair install = %#v, error = %v", second, err)
	}
	if info, err := os.Stat(output); err != nil || !info.IsDir() {
		t.Fatalf("runtime output directory was not repaired: info=%v error=%v", info, err)
	}
}

func TestInstallRejectsSymlinkedRuntimeOutputDirectory(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	world := catalog.worlds[0]
	outputParent := filepath.Join(root, catalog.room.DirectoryName, world.DirectoryName, "save", "mod_config_data")
	if err := os.MkdirAll(outputParent, 0750); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(outputParent, managedDirectory)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, world.ID); !errors.Is(err, ErrUnsafeRuntimePath) {
		t.Fatalf("symlinked runtime output error = %v", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target was modified: entries=%v error=%v", entries, err)
	}
}

func TestInstallRejectsBrokenMarkersAndModifiedManagedAsset(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	custom := filepath.Join(root, "Cluster_1", "Master", customCommandsName)
	broken := "-- DST-ADMIN MANAGED BLOCK BEGIN protocol=2\n-- no end\n"
	if err := os.WriteFile(custom, []byte(broken), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrManagedBlockInvalid) {
		t.Fatalf("broken marker error = %v", err)
	}
	data, _ := os.ReadFile(custom)
	if string(data) != broken {
		t.Fatal("broken customcommands.lua was modified")
	}
	if err := os.Remove(custom); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(root, "Cluster_1", "Master", managedDirectory, "telemetry.lua")
	if err := os.WriteFile(asset, []byte("return {}\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrManagedFileChanged) {
		t.Fatalf("modified asset error = %v", err)
	}
	modified, _ := os.ReadFile(asset)
	if string(modified) != "return {}\n" {
		t.Fatal("modified managed asset was silently overwritten")
	}
}

func TestUninstallRemovesOnlyManagedContentAndCreatesBackup(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	custom := filepath.Join(root, "Cluster_1", "Master", customCommandsName)
	userSource := []byte("-- mine\nprint('keep')\n")
	if err := os.WriteFile(custom, userSource, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	status, err := manager.UninstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || !status.Changed || status.BackupPath == "" {
		t.Fatalf("uninstall status = %#v, error = %v", status, err)
	}
	after, err := os.ReadFile(custom)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(after)) != strings.TrimSpace(string(userSource)) || bytes.Contains(after, []byte("DST-ADMIN MANAGED")) {
		t.Fatalf("uninstall changed user source: %q", after)
	}
	if _, err := os.Stat(filepath.Join(root, "Cluster_1", "Master", managedDirectory)); !os.IsNotExist(err) {
		t.Fatalf("managed directory remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Cluster_1", "Master", status.BackupPath)); err != nil {
		t.Fatalf("uninstall backup missing: %v", err)
	}
}

func TestInstallRejectsSymlinkedCustomCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("symlink test")
	}
	manager, catalog, root := newRuntimeTestManager(t)
	target := filepath.Join(t.TempDir(), "outside.lua")
	if err := os.WriteFile(target, []byte("print('outside')\n"), 0640); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(root, "Cluster_1", "Master", customCommandsName)
	if err := os.Symlink(target, custom); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrUnsafeRuntimePath) {
		t.Fatalf("symlink error = %v", err)
	}
	outside, _ := os.ReadFile(target)
	if string(outside) != "print('outside')\n" {
		t.Fatal("symlink target was modified")
	}
}

func TestInstallRejectsInvalidLuaWithoutPublishingRuntime(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	custom := filepath.Join(root, "Cluster_1", "Master", customCommandsName)
	invalid := []byte("function broken(\n")
	if err := os.WriteFile(custom, invalid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err == nil || !strings.Contains(err.Error(), "validate Lua syntax") {
		t.Fatalf("invalid Lua was accepted: %v", err)
	}
	after, err := os.ReadFile(custom)
	if err != nil || !bytes.Equal(after, invalid) {
		t.Fatalf("invalid user file changed: %q err=%v", after, err)
	}
	if _, err := os.Stat(filepath.Join(root, "Cluster_1", "Master", managedDirectory, manifestName)); !os.IsNotExist(err) {
		t.Fatalf("runtime was partially published: %v", err)
	}
}

func TestRollbackRestoresCompletePreInstallState(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	custom := filepath.Join(root, "Cluster_1", "Master", customCommandsName)
	userSource := []byte("-- original\nfunction hello() return 'world' end\n")
	if err := os.WriteFile(custom, userSource, 0600); err != nil {
		t.Fatal(err)
	}
	installed, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	backups, err := manager.Backups(catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || len(backups) != 1 || installed.BackupPath != filepath.Join(backupDirectory, backups[0].ID) {
		t.Fatalf("install backup = %#v status=%#v err=%v", backups, installed, err)
	}
	rolledBack, err := manager.RollbackWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID, backups[0].ID)
	if err != nil || !rolledBack.Changed || rolledBack.State != InstallStateMissing {
		t.Fatalf("rollback status=%#v err=%v", rolledBack, err)
	}
	after, err := os.ReadFile(custom)
	if err != nil || !bytes.Equal(after, userSource) || fileMode(t, custom) != 0600 {
		t.Fatalf("user source was not restored: %q err=%v", after, err)
	}
	if _, err := os.Stat(filepath.Join(root, "Cluster_1", "Master", managedDirectory)); !os.IsNotExist(err) {
		t.Fatalf("managed runtime remains after rollback: %v", err)
	}
	backups, err = manager.Backups(catalog.room.ID, catalog.worlds[0].ID)
	hasProtection := false
	for _, backup := range backups {
		hasProtection = hasProtection || backup.Reason == "before-rollback"
	}
	if err != nil || len(backups) != 2 || !hasProtection {
		t.Fatalf("rollback protection backup missing: %#v err=%v", backups, err)
	}
}

func TestRollbackRejectsTamperedBackupWithoutChangingCurrentState(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	status, err := manager.UninstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	backupID := filepath.Base(status.BackupPath)
	payload := filepath.Join(root, "Cluster_1", "Master", backupDirectory, backupID, "files", managedDirectory, "telemetry.lua")
	if err := os.WriteFile(payload, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(root, "Cluster_1", "Master", customCommandsName)
	if _, err := manager.RollbackWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID, backupID); err == nil {
		t.Fatal("tampered backup was accepted")
	}
	if _, err := os.Stat(custom); !os.IsNotExist(err) {
		t.Fatalf("failed rollback changed current uninstall state: %v", err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
