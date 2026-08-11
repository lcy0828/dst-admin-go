package mods

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type lifecycleRunner struct {
	root    string
	content string
	err     error
}

type blockingLifecycleRunner struct {
	root    string
	started chan struct{}
	release chan struct{}
}

type existingDirectoryRunner struct {
	root     string
	observed bool
}

func (r *existingDirectoryRunner) Download(ctx context.Context, ids []string, validate bool, output io.Writer) error {
	for _, id := range ids {
		if !directoryExists(filepath.Join(r.root, id)) {
			return errors.New("existing Workshop directory was hidden from SteamCMD")
		}
	}
	r.observed = true
	return lifecycleRunner{root: r.root, content: `name = "updated"`}.Download(ctx, ids, validate, output)
}

func (r blockingLifecycleRunner) Download(_ context.Context, ids []string, _ bool, _ io.Writer) error {
	close(r.started)
	<-r.release
	return lifecycleRunner{root: r.root, content: `name = "updated"`}.Download(context.Background(), ids, true, io.Discard)
}

func TestInstallFailureRestoresExistingCacheAndRemovesPartialDependencies(t *testing.T) {
	service, backupService, overridesPath := newConfigTestService(t)
	originalCache, err := os.ReadFile(filepath.Join(service.downloadedPath("378160973"), "modinfo.lua"))
	if err != nil {
		t.Fatal(err)
	}
	originalConfig, err := os.ReadFile(overridesPath)
	if err != nil {
		t.Fatal(err)
	}
	service.runner = lifecycleRunner{root: service.config.WorkshopContentRoot, content: "partial", err: errors.New("dependency download failed")}

	_, err = service.Install(context.Background(), "job", "room-1", InstallRequest{
		ModID: "378160973", WorldIDs: []string{"world-1"}, Enabled: true, IncludeDependencies: true,
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "dependency download failed") {
		t.Fatalf("expected install failure, got %v", err)
	}
	restoredCache, err := os.ReadFile(filepath.Join(service.downloadedPath("378160973"), "modinfo.lua"))
	if err != nil || string(restoredCache) != string(originalCache) {
		t.Fatalf("existing cache was not restored: value=%q err=%v", restoredCache, err)
	}
	if _, err := os.Stat(service.downloadedPath("123456789")); !os.IsNotExist(err) {
		t.Fatalf("partial dependency was not removed: %v", err)
	}
	config, err := os.ReadFile(overridesPath)
	if err != nil || string(config) != string(originalConfig) {
		t.Fatalf("configuration changed after failed download: value=%q err=%v", config, err)
	}
	if backupService.count != 0 {
		t.Fatalf("failed download created a backup: %d", backupService.count)
	}
}

func (r lifecycleRunner) Download(_ context.Context, ids []string, _ bool, _ io.Writer) error {
	for _, id := range ids {
		path := filepath.Join(r.root, id)
		if err := os.MkdirAll(path, 0750); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "modinfo.lua"), []byte(r.content), 0640); err != nil {
			return err
		}
	}
	return r.err
}

func TestRepairRestoresWorkshopAndUGCCacheWhenDownloadFails(t *testing.T) {
	service, _, _ := newConfigTestService(t)
	modPath := service.downloadedPath("378160973")
	original, err := os.ReadFile(filepath.Join(modPath, "modinfo.lua"))
	if err != nil {
		t.Fatal(err)
	}
	room, err := service.rooms.Room("room-1")
	if err != nil {
		t.Fatal(err)
	}
	worlds, err := service.rooms.Worlds("room-1")
	if err != nil {
		t.Fatal(err)
	}
	ugcPath := filepath.Join(service.ugcCandidates(room, worlds[0])[0], "378160973")
	if err := os.MkdirAll(ugcPath, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ugcPath, "ugc-marker"), []byte("original"), 0640); err != nil {
		t.Fatal(err)
	}
	service.runner = lifecycleRunner{root: service.config.WorkshopContentRoot, content: "partial download", err: errors.New("network interrupted")}

	_, err = service.Repair(context.Background(), "room-1", "378160973", ModActionRequest{
		WorldIDs: []string{"world-1"}, Confirmation: "测试房间",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "network interrupted") {
		t.Fatalf("expected download failure, got %v", err)
	}
	restored, err := os.ReadFile(filepath.Join(modPath, "modinfo.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("Workshop cache was not restored: %q", restored)
	}
	marker, err := os.ReadFile(filepath.Join(ugcPath, "ugc-marker"))
	if err != nil || string(marker) != "original" {
		t.Fatalf("UGC cache was not restored: value=%q err=%v", marker, err)
	}
	assertNoRepairStagingDirectories(t, filepath.Dir(modPath), filepath.Dir(ugcPath))
}

func TestRepairReplacesWorkshopAndDropsStaleUGCCache(t *testing.T) {
	service, _, _ := newConfigTestService(t)
	room, _ := service.rooms.Room("room-1")
	worlds, _ := service.rooms.Worlds("room-1")
	ugcPath := filepath.Join(service.ugcCandidates(room, worlds[0])[0], "378160973")
	if err := os.MkdirAll(ugcPath, 0750); err != nil {
		t.Fatal(err)
	}
	service.runner = lifecycleRunner{root: service.config.WorkshopContentRoot, content: `name = "repaired"`}

	if _, err := service.Repair(context.Background(), "room-1", "378160973", ModActionRequest{
		WorldIDs: []string{"world-1"}, Confirmation: "测试房间",
	}, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(service.downloadedPath("378160973"), "modinfo.lua"))
	if err != nil || string(data) != `name = "repaired"` {
		t.Fatalf("repaired Workshop cache missing: value=%q err=%v", data, err)
	}
	if _, err := os.Stat(ugcPath); !os.IsNotExist(err) {
		t.Fatalf("stale UGC cache still exists: %v", err)
	}
	assertNoRepairStagingDirectories(t, filepath.Dir(service.downloadedPath("378160973")), filepath.Dir(ugcPath))
}

func TestUpdateKeepsExistingWorkshopDirectoryVisibleToRunner(t *testing.T) {
	service, _, _ := newConfigTestService(t)
	runner := &existingDirectoryRunner{root: service.config.WorkshopContentRoot}
	service.runner = runner

	if _, err := service.Update(context.Background(), "room-1", "378160973", io.Discard); err != nil {
		t.Fatal(err)
	}
	if !runner.observed {
		t.Fatal("runner did not inspect the existing Workshop directory")
	}
	assertNoRepairStagingDirectories(t, filepath.Dir(service.downloadedPath("378160973")))
}

func TestUninstallNoChangeDoesNotCreateProtectionBackup(t *testing.T) {
	service, backupService, _ := newConfigTestService(t)
	request := ModActionRequest{WorldIDs: []string{"world-1"}, Confirmation: "测试房间", RemoveFiles: false}
	if _, err := service.Uninstall(context.Background(), "job-1", "room-1", "378160973", request); err != nil {
		t.Fatal(err)
	}
	if backupService.count != 1 {
		t.Fatalf("first semantic change should create one backup, got %d", backupService.count)
	}
	if _, err := service.Uninstall(context.Background(), "job-2", "room-1", "378160973", request); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("expected no changes, got %v", err)
	}
	if backupService.count != 1 {
		t.Fatalf("no-change uninstall created a backup: %d", backupService.count)
	}
}

func TestUninstallPreservesFilesReferencedByManualSetup(t *testing.T) {
	service, backupService, _ := newConfigTestService(t)
	setupPath := service.setupPath()
	if err := os.WriteFile(setupPath, []byte("ServerModSetup(\"378160973\")\n"), 0640); err != nil {
		t.Fatal(err)
	}
	result, err := service.Uninstall(context.Background(), "job", "room-1", "378160973", ModActionRequest{
		WorldIDs: []string{"world-1"}, Confirmation: "测试房间", RemoveFiles: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProtectionBackupID == "" || backupService.count != 1 {
		t.Fatalf("configuration change was not protected: %#v", result)
	}
	if !directoryExists(service.downloadedPath("378160973")) {
		t.Fatal("files referenced by the operator-managed setup entry were removed")
	}
	data, err := os.ReadFile(setupPath)
	if err != nil || !strings.Contains(string(data), `ServerModSetup("378160973")`) {
		t.Fatalf("manual setup entry was lost: %q err=%v", data, err)
	}
}

func TestModCollectionsAreNeverNull(t *testing.T) {
	service, _, _ := newConfigTestService(t)
	list, err := service.List(context.Background(), "room-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) == 0 || list.Items[0].Dependencies == nil || list.Items[0].Tags == nil {
		t.Fatalf("list returned nullable collections: %#v", list.Items)
	}
	search, err := service.Search(context.Background(), SearchOptions{Query: "123456789", Page: 1, PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Items) != 1 || search.Items[0].Dependencies == nil || search.Items[0].Tags == nil {
		t.Fatalf("search returned nullable collections: %#v", search.Items)
	}
}

func TestNodeDownloadDoesNotChangeRoomConfiguration(t *testing.T) {
	service, backupService, overridesPath := newConfigTestService(t)
	beforeOverrides, err := os.ReadFile(overridesPath)
	if err != nil {
		t.Fatal(err)
	}
	setupPath := service.setupPath()
	beforeSetup, err := os.ReadFile(setupPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	service.runner = lifecycleRunner{root: service.config.WorkshopContentRoot, content: `name = "Shared Library"`}
	result, err := service.Download(context.Background(), DownloadRequest{ModID: "123456789"}, io.Discard)
	if err != nil || len(result.ModIDs) != 1 || result.ModIDs[0] != "123456789" {
		t.Fatalf("node download result=%#v err=%v", result, err)
	}
	afterOverrides, _ := os.ReadFile(overridesPath)
	afterSetup, afterSetupErr := os.ReadFile(setupPath)
	if os.IsNotExist(afterSetupErr) {
		afterSetup = nil
	} else if afterSetupErr != nil {
		t.Fatal(afterSetupErr)
	}
	if string(afterOverrides) != string(beforeOverrides) || string(afterSetup) != string(beforeSetup) {
		t.Fatal("node download changed room or dedicated server configuration")
	}
	if backupService.count != 0 {
		t.Fatalf("node download created a room backup: %d", backupService.count)
	}
	roomList, err := service.List(context.Background(), "room-1")
	if err != nil || len(roomList.Items) != 1 || roomList.Items[0].ID != "378160973" {
		t.Fatalf("room list contains node-only Mods: value=%#v err=%v", roomList, err)
	}
	library, err := service.Library(context.Background())
	if err != nil || library.Total != 2 {
		t.Fatalf("node library did not include both downloads: value=%#v err=%v", library, err)
	}
}

func TestAddDownloadedModToRoomIsConfigurationOnly(t *testing.T) {
	service, backupService, overridesPath := newConfigTestService(t)
	modPath := service.downloadedPath("123456789")
	if err := os.MkdirAll(modPath, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modPath, "modinfo.lua"), []byte(`name = "Shared Library"`), 0640); err != nil {
		t.Fatal(err)
	}
	result, err := service.AddToRoom(context.Background(), "job", "room-1", "123456789", AddToRoomRequest{
		WorldIDs: []string{"world-1"}, Enabled: true,
	})
	if err != nil || result.ProtectionBackupID == "" || backupService.count != 1 {
		t.Fatalf("add result=%#v backups=%d err=%v", result, backupService.count, err)
	}
	overrides, _ := os.ReadFile(overridesPath)
	setup, _ := os.ReadFile(service.setupPath())
	if !strings.Contains(string(overrides), `workshop-123456789`) || !strings.Contains(string(setup), `ServerModSetup("123456789")`) {
		t.Fatalf("room or server setup was not updated:\n%s\n%s", overrides, setup)
	}
}

func TestRoomListWaitsForCoherentNodeLibraryState(t *testing.T) {
	service, _, _ := newConfigTestService(t)
	runner := blockingLifecycleRunner{
		root: service.config.WorkshopContentRoot, started: make(chan struct{}), release: make(chan struct{}),
	}
	service.runner = runner
	updateDone := make(chan error, 1)
	go func() {
		_, err := service.Update(context.Background(), "room-1", "378160973", io.Discard)
		updateDone <- err
	}()
	<-runner.started

	listDone := make(chan error, 1)
	go func() {
		_, err := service.List(context.Background(), "room-1")
		listDone <- err
	}()
	select {
	case err := <-listDone:
		t.Fatalf("room list observed an in-progress node update: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(runner.release)
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
	if err := <-listDone; err != nil {
		t.Fatal(err)
	}
}

func TestMergeLocalModInfoPrefersPackagedMetadata(t *testing.T) {
	item := SteamMod{Name: "Steam Name", Author: "Steam Author", Version: "1.0", Description: "Steam description"}
	mergeLocalModInfo(&item, map[string]interface{}{
		"name": "[DST] 棱镜", "author": "ti_Tout", "version": "7.6.5", "description": "本地描述",
	})
	if item.Name != "[DST] 棱镜" || item.Author != "ti_Tout" || item.Version != "7.6.5" || item.Description != "本地描述" {
		t.Fatalf("local modinfo metadata was not preferred: %#v", item)
	}
}

func assertNoRepairStagingDirectories(t *testing.T, roots ...string) {
	t.Helper()
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".dst-admin-repair-") {
				t.Fatalf("repair staging directory was not cleaned: %s", filepath.Join(root, entry.Name()))
			}
		}
	}
}
