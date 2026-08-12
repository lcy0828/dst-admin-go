package saveimport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	backupapi "dont/internal/backups"
	modapi "dont/internal/mods"
	"dont/internal/rooms"

	"github.com/go-ini/ini"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type applyRuntime struct {
	running map[string]bool
}

func (r *applyRuntime) IsRunning(_ context.Context, _, world string) (bool, error) {
	return r.running[world], nil
}

func (r *applyRuntime) Send(context.Context, string, string, string) error { return nil }

type applyModDownloader struct {
	root      string
	downloads []string
}

func (d *applyModDownloader) Download(_ context.Context, request modapi.DownloadRequest, _ io.Writer) (modapi.ActionResult, error) {
	d.downloads = append(d.downloads, request.ModID)
	if err := os.MkdirAll(filepath.Join(d.root, request.ModID), 0750); err != nil {
		return modapi.ActionResult{}, err
	}
	return modapi.ActionResult{ModIDs: []string{request.ModID}}, nil
}

func (d *applyModDownloader) EnsureLibrarySetup([]string) error { return nil }

type applyTestApp struct {
	service      *Service
	store        *Store
	rooms        *rooms.Service
	backups      *backupapi.Service
	runtime      *applyRuntime
	downloader   *applyModDownloader
	saveRoot     string
	backupRoot   string
	importRoot   string
	workshopRoot string
}

func newApplyTestApp(t *testing.T) applyTestApp {
	t.Helper()
	root := t.TempDir()
	saveRoot := filepath.Join(root, "saves")
	backupRoot := filepath.Join(root, "backups")
	importRoot := filepath.Join(backupRoot, ".imports")
	workshopRoot := filepath.Join(root, "workshop")
	for _, directory := range []string{saveRoot, backupRoot, workshopRoot} {
		if err := os.MkdirAll(directory, 0750); err != nil {
			t.Fatal(err)
		}
	}
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	roomStore := rooms.NewStore(db, "apply_")
	if err := roomStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	catalog, err := rooms.NewCatalog(saveRoot, roomStore)
	if err != nil {
		t.Fatal(err)
	}
	roomService := rooms.NewService(catalog, roomStore)
	runtime := &applyRuntime{running: make(map[string]bool)}
	backupStore := backupapi.NewStore(db, "apply_")
	if err := backupStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	backupService, err := backupapi.NewService(saveRoot, backupRoot, roomService, runtime, backupStore)
	if err != nil {
		t.Fatal(err)
	}
	importStore := NewStore(db, "apply_")
	if err := importStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	downloader := &applyModDownloader{root: workshopRoot}
	service, err := NewService(Config{SaveRoot: saveRoot, ImportRoot: importRoot, WorkshopRoot: workshopRoot}, importStore, roomService, runtime, backupService, downloader)
	if err != nil {
		t.Fatal(err)
	}
	return applyTestApp{
		service: service, store: importStore, rooms: roomService, backups: backupService, runtime: runtime,
		downloader: downloader, saveRoot: saveRoot, backupRoot: backupRoot, importRoot: importRoot, workshopRoot: workshopRoot,
	}
}

func TestApplyCreatesManagedRoomAndAllocatesConflictFreePorts(t *testing.T) {
	app := newApplyTestApp(t)
	createManagedRoom(t, app, "Existing", "Existing Room", "target-token-1234567890", 10999, "old")
	managedRoomID := ""
	app.rooms.SetManagedRoomLifecycle(func(roomID string) {
		managedRoomID = roomID
	}, nil)
	archive := createZIP(t, []archiveTestEntry{
		{name: "Cluster_1/cluster.ini", content: clusterINI("Source Room")},
		{name: "Cluster_1/cluster_token.txt", content: "source-token-1234567890\n"},
		{name: "Cluster_1/Master/server.ini", content: serverINI(true, 1, 10999)},
		{name: "Cluster_1/Master/save/session/source/0000000001", content: "new-save"},
		{name: "Cluster_1/Master/modoverrides.lua", content: `return {["workshop-1392778117"] = { enabled = true }}`},
	})
	value, err := app.service.Upload(context.Background(), "外部存档", "Cluster_1.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.service.Apply(context.Background(), value.ID, "job-new", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeNew, DirectoryName: "Imported",
		RoomName: "Imported Room", TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsInstallMissing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.DirectoryName != "Imported" || result.RoomName != "Imported Room" || len(result.InstalledMods) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if managedRoomID != result.RoomID {
		t.Fatalf("managed room lifecycle got %q, want %q", managedRoomID, result.RoomID)
	}
	room, err := app.rooms.Room(rooms.EncodeID("Imported"))
	if err != nil || !room.Managed {
		t.Fatalf("room = %#v, %v", room, err)
	}
	serverConfig, err := ini.Load(filepath.Join(app.saveRoot, "Imported", "Master", "server.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if port := serverConfig.Section("NETWORK").Key("server_port").MustInt(0); port == 10999 || port == 0 {
		t.Fatalf("allocated server port = %d", port)
	}
	modContent, err := os.ReadFile(filepath.Join(app.saveRoot, "Imported", "Master", "modoverrides.lua"))
	if err != nil || !bytes.Contains(modContent, []byte("workshop-1392778117")) {
		t.Fatalf("mod config = %q, %v", modContent, err)
	}
}

func TestApplyReplacePreservesTargetTokenAndPortsAndCreatesProtectionBackup(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Source Room")},
		{name: "cluster_token.txt", content: "source-token-1234567890\n"},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
		{name: "Master/save/session/source/0000000001", content: "new-save"},
	})
	value, err := app.service.Upload(context.Background(), "替换包", "source.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.service.Apply(context.Background(), value.ID, "job-replace", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: target.ID,
		RoomName: "Target Room", Confirmation: "Target Room", TokenPolicy: TokenPreserve,
		NetworkPolicy: NetworkPreserve, ModPolicy: ModsPreserve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProtectionBackupID == "" {
		t.Fatalf("result = %#v", result)
	}
	token, err := os.ReadFile(filepath.Join(app.saveRoot, "Target", "cluster_token.txt"))
	if err != nil || strings.TrimSpace(string(token)) != "target-token-1234567890" {
		t.Fatalf("token = %q, %v", token, err)
	}
	serverConfig, err := ini.Load(filepath.Join(app.saveRoot, "Target", "Master", "server.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if port := serverConfig.Section("NETWORK").Key("server_port").MustInt(0); port != 12001 {
		t.Fatalf("preserved port = %d", port)
	}
	content, err := os.ReadFile(filepath.Join(app.saveRoot, "Target", "Master", "save", "session", "source", "0000000001"))
	if err != nil || string(content) != "new-save" {
		t.Fatalf("restored content = %q, %v", content, err)
	}
	backups, err := app.backups.List(target.ID)
	if err != nil || len(backups) != 1 || backups[0].Kind != backupapi.KindProtection {
		t.Fatalf("backups = %#v, %v", backups, err)
	}
}

func TestApplyReplaceRefusesRunningRoomWithoutChangingFiles(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
	app.runtime.running["Master"] = true
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Source")},
		{name: "cluster_token.txt", content: "source-token-1234567890\n"},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
	})
	value, err := app.service.Upload(context.Background(), "运行中替换", "source.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.service.Apply(context.Background(), value.ID, "job", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: target.ID,
		Confirmation: "Target Room", TokenPolicy: TokenPreserve, NetworkPolicy: NetworkPreserve, ModPolicy: ModsPreserve,
	})
	if !errors.Is(err, ErrRoomRunning) {
		t.Fatalf("error = %v", err)
	}
	content, readErr := os.ReadFile(filepath.Join(app.saveRoot, "Target", "Master", "save", "session", "old", "0000000001"))
	if readErr != nil || string(content) != "old-save" {
		t.Fatalf("original content = %q, %v", content, readErr)
	}
}

func TestApplyRejectsSourcePortsUsedByAnotherRoom(t *testing.T) {
	app := newApplyTestApp(t)
	createManagedRoom(t, app, "Existing", "Existing Room", "target-token-1234567890", 10999, "old")
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Imported")},
		{name: "cluster_token.txt", content: "source-token-1234567890\n"},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
	})
	value, err := app.service.Upload(context.Background(), "端口冲突", "source.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.service.Apply(context.Background(), value.ID, "job", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeNew, DirectoryName: "Imported",
		TokenPolicy: TokenSource, NetworkPolicy: NetworkSource, ModPolicy: ModsPreserve,
	})
	if !errors.Is(err, ErrPortConflict) {
		t.Fatalf("error = %v, want port conflict", err)
	}
	if _, statErr := os.Stat(filepath.Join(app.saveRoot, "Imported")); !os.IsNotExist(statErr) {
		t.Fatalf("conflicting room was published: %v", statErr)
	}
}

func TestApplyPreservesPortsByShardIDWhenDirectoryNamesDiffer(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old")
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Source")},
		{name: "cluster_token.txt", content: "source-token-1234567890\n"},
		{name: "Forest/server.ini", content: serverINI(true, 1, 10999)},
	})
	value, err := app.service.Upload(context.Background(), "改名分片", "source.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.service.Apply(context.Background(), value.ID, "job", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: target.ID,
		Confirmation: target.Name, TokenPolicy: TokenPreserve, NetworkPolicy: NetworkPreserve, ModPolicy: ModsPreserve,
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := ini.Load(filepath.Join(app.saveRoot, "Target", "Forest", "server.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if port := config.Section("NETWORK").Key("server_port").MustInt(0); port != 12001 {
		t.Fatalf("preserved port = %d, want 12001", port)
	}
}

func TestServiceRecoversInterruptedReplacementByRestoringOriginalRoom(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
	value := analyzedImportForRecovery(t, app)
	staging := createRecoveryRoom(t, app.saveRoot, ".dst-admin-import-test", "new-save")
	rollback := filepath.Join(app.saveRoot, ".dst-admin-import-rollback-test")
	if err := app.store.BeginApply(value.ID, ApplyModeReplace, target.ID, "Target", filepath.Base(staging), filepath.Base(rollback)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(app.saveRoot, "Target"), rollback); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, filepath.Join(app.saveRoot, "Target")); err != nil {
		t.Fatal(err)
	}

	if _, err := NewService(Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot}, app.store, app.rooms, app.runtime, app.backups, app.downloader); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(app.saveRoot, "Target", "Master", "save", "session", "old", "0000000001"))
	if err != nil || string(content) != "old-save" {
		t.Fatalf("restored content = %q, %v", content, err)
	}
	recovered, err := app.store.Get(value.ID)
	if err != nil || recovered.Status != StatusReady || recovered.ErrorCode != "SERVER_RESTARTED" {
		t.Fatalf("recovered import = %#v, %v", recovered, err)
	}
}

func TestServiceRecoveryUnmanagesInterruptedNewRoom(t *testing.T) {
	app := newApplyTestApp(t)
	value := analyzedImportForRecovery(t, app)
	target := createRecoveryRoom(t, app.saveRoot, "Imported", "new-save")
	roomID := rooms.EncodeID(filepath.Base(target))
	stagingName := ".dst-admin-import-interrupted-new"
	if err := app.store.BeginApply(value.ID, ApplyModeNew, roomID, filepath.Base(target), stagingName, ""); err != nil {
		t.Fatal(err)
	}
	managedRoomID := ""
	unmanagedRoomID := ""
	app.rooms.SetManagedRoomLifecycle(func(value string) {
		managedRoomID = value
	}, func(value string) {
		unmanagedRoomID = value
	})
	if _, err := app.rooms.Adopt(roomID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot}, app.store, app.rooms, app.runtime, app.backups, app.downloader); err != nil {
		t.Fatal(err)
	}
	if managedRoomID != roomID || unmanagedRoomID != roomID {
		t.Fatalf("lifecycle managed=%q unmanaged=%q, want %q", managedRoomID, unmanagedRoomID, roomID)
	}
	if _, err := app.rooms.Room(roomID); !errors.Is(err, rooms.ErrRoomNotFound) {
		t.Fatalf("interrupted imported room still exists: %v", err)
	}
	recovered, err := app.store.Get(value.ID)
	if err != nil || recovered.Status != StatusReady || recovered.ErrorCode != "SERVER_RESTARTED" {
		t.Fatalf("recovered import = %#v, %v", recovered, err)
	}
}

func TestServiceFinishesCommittedReplacementAfterRestart(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
	value := analyzedImportForRecovery(t, app)
	staging := createRecoveryRoom(t, app.saveRoot, ".dst-admin-import-committed", "new-save")
	rollback := filepath.Join(app.saveRoot, ".dst-admin-import-rollback-committed")
	if err := app.store.BeginApply(value.ID, ApplyModeReplace, target.ID, "Target", filepath.Base(staging), filepath.Base(rollback)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(app.saveRoot, "Target"), rollback); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, filepath.Join(app.saveRoot, "Target")); err != nil {
		t.Fatal(err)
	}
	if err := app.store.MarkApplyCommitted(value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot}, app.store, app.rooms, app.runtime, app.backups, app.downloader); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(app.saveRoot, "Target", "Master", "save", "session", "new", "0000000001"))
	if err != nil || string(content) != "new-save" {
		t.Fatalf("committed content = %q, %v", content, err)
	}
	recovered, err := app.store.Get(value.ID)
	if err != nil || recovered.Status != StatusApplied {
		t.Fatalf("recovered import = %#v, %v", recovered, err)
	}
	if _, err := os.Stat(rollback); !os.IsNotExist(err) {
		t.Fatalf("committed rollback was not removed: %v", err)
	}
}

func TestServiceFinishesAppliedJournalCleanupAfterRestart(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
	value := analyzedImportForRecovery(t, app)
	staging := createRecoveryRoom(t, app.saveRoot, ".dst-admin-import-applied", "new-save")
	rollback := filepath.Join(app.saveRoot, ".dst-admin-import-rollback-applied")
	if err := app.store.BeginApply(value.ID, ApplyModeReplace, target.ID, "Target", filepath.Base(staging), filepath.Base(rollback)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(app.saveRoot, "Target"), rollback); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, filepath.Join(app.saveRoot, "Target")); err != nil {
		t.Fatal(err)
	}
	if err := app.store.MarkApplyCommitted(value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.MarkApplied(value.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := NewService(Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot}, app.store, app.rooms, app.runtime, app.backups, app.downloader); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rollback); !os.IsNotExist(err) {
		t.Fatalf("applied rollback was not removed: %v", err)
	}
	var record importRecord
	if err := app.store.db.Table(app.store.table).Where("id = ?", value.ID).First(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.Status != string(StatusApplied) || record.ApplyPhase != "" || record.ApplyTarget != "" || record.ApplyRollback != "" {
		t.Fatalf("apply journal was not cleared: %#v", record)
	}
}

func analyzedImportForRecovery(t *testing.T, app applyTestApp) Session {
	t.Helper()
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Source")},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
	})
	value, err := app.service.Upload(context.Background(), "恢复测试", "source.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func createRecoveryRoom(t *testing.T, saveRoot, name, save string) string {
	t.Helper()
	root := filepath.Join(saveRoot, name)
	world := filepath.Join(root, "Master")
	if err := os.MkdirAll(filepath.Join(world, "save", "session", "new"), 0750); err != nil {
		t.Fatal(err)
	}
	for filePath, content := range map[string]string{
		filepath.Join(root, "cluster.ini"):                           clusterINI("Imported"),
		filepath.Join(world, "server.ini"):                           serverINI(true, 1, 10999),
		filepath.Join(world, "save", "session", "new", "0000000001"): save,
	} {
		if err := os.WriteFile(filePath, []byte(content), 0640); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func createManagedRoom(t *testing.T, app applyTestApp, directory, name, token string, port int, save string) rooms.Room {
	t.Helper()
	roomRoot := filepath.Join(app.saveRoot, directory)
	worldRoot := filepath.Join(roomRoot, "Master")
	if err := os.MkdirAll(filepath.Join(worldRoot, "save", "session", "old"), 0750); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(roomRoot, "cluster.ini"):                           clusterINI(name),
		filepath.Join(roomRoot, "cluster_token.txt"):                     token + "\n",
		filepath.Join(worldRoot, "server.ini"):                           serverINI(true, 1, port),
		filepath.Join(worldRoot, "save", "session", "old", "0000000001"): save,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0640); err != nil {
			t.Fatal(err)
		}
	}
	room, err := app.rooms.Adopt(rooms.EncodeID(directory))
	if err != nil {
		t.Fatal(err)
	}
	return room
}
