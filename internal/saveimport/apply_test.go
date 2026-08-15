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
	"dont/internal/runtimeguard"
	"dont/internal/topology"

	"github.com/go-ini/ini"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type applyRuntime struct {
	running map[string]bool
}

type deniedSaveImportMutationGuard struct{}

func (deniedSaveImportMutationGuard) RequireRoom(string) error {
	return runtimeguard.ErrRemoteMutationUnavailable
}

func (deniedSaveImportMutationGuard) RequireWorld(string, string) error {
	return runtimeguard.ErrRemoteMutationUnavailable
}

func (r *applyRuntime) IsRunning(_ context.Context, _, world string) (bool, error) {
	return r.running[world], nil
}

func (r *applyRuntime) Send(context.Context, string, string, string) error { return nil }

type applyModDownloader struct {
	root        string
	downloads   []string
	downloadErr error
}

type applyPortAllocator struct {
	allocation  topology.PortAllocation
	request     topology.PortAllocationRequest
	activated   string
	released    string
	activateErr error
	releaseErr  error
}

func (allocator *applyPortAllocator) ReservePorts(_ context.Context, request topology.PortAllocationRequest) (topology.PortAllocation, error) {
	allocator.request = request
	return allocator.allocation, nil
}

func (allocator *applyPortAllocator) ActivatePorts(_ context.Context, leaseID string) error {
	allocator.activated = leaseID
	return allocator.activateErr
}

func (allocator *applyPortAllocator) ReleasePorts(_ context.Context, leaseID string) error {
	allocator.released = leaseID
	return allocator.releaseErr
}

func (d *applyModDownloader) Download(_ context.Context, request modapi.DownloadRequest, _ io.Writer) (modapi.ActionResult, error) {
	d.downloads = append(d.downloads, request.ModID)
	if d.downloadErr != nil {
		return modapi.ActionResult{}, d.downloadErr
	}
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

func TestApplyUsesDurablePortLeaseAndActivatesAfterPublication(t *testing.T) {
	app := newApplyTestApp(t)
	archive := createZIP(t, []archiveTestEntry{
		{name: "Cluster_1/cluster.ini", content: clusterINI("Source Room")},
		{name: "Cluster_1/cluster_token.txt", content: "source-token-1234567890\n"},
		{name: "Cluster_1/Master/server.ini", content: serverINI(true, 1, 10999)},
		{name: "Cluster_1/Master/save/session/source/0000000001", content: "save"},
	})
	value, err := app.service.Upload(context.Background(), "带端口租约", "Cluster_1.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	masterID := rooms.EncodeID("Master")
	allocator := &applyPortAllocator{allocation: topology.PortAllocation{LeaseID: "lease-import", Reservations: []topology.PortReservation{
		{WorldID: masterID, Purpose: topology.PortClusterMaster, Port: 11889},
		{WorldID: masterID, Purpose: topology.PortDSTServer, Port: 11999},
		{WorldID: masterID, Purpose: topology.PortSteamAuth, Port: 9767},
		{WorldID: masterID, Purpose: topology.PortSteamMasterServer, Port: 28017},
	}}}
	if err := app.service.ConfigurePortAllocator(allocator); err != nil {
		t.Fatal(err)
	}
	result, err := app.service.Apply(context.Background(), value.ID, "job", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeNew, DirectoryName: "ImportedLease",
		RoomName: "Imported Lease", TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsPreserve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocator.activated != "lease-import" || allocator.released != "" || allocator.request.RoomID != result.RoomID || allocator.request.TargetID != "local" {
		t.Fatalf("allocator=%#v result=%#v", allocator, result)
	}
	cluster, err := ini.Load(filepath.Join(app.saveRoot, "ImportedLease", "cluster.ini"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := ini.Load(filepath.Join(app.saveRoot, "ImportedLease", "Master", "server.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if cluster.Section("SHARD").Key("master_port").MustInt(0) != 11889 || server.Section("NETWORK").Key("server_port").MustInt(0) != 11999 {
		t.Fatalf("allocated configuration cluster=%s server=%s", cluster.Section("SHARD").Key("master_port").String(), server.Section("NETWORK").Key("server_port").String())
	}
}

func TestApplyReleasesPortLeaseWhenModInstallFails(t *testing.T) {
	app := newApplyTestApp(t)
	value := uploadAnalyzedImportWithMod(t, app, "mod-failure")
	allocator := importTestPortAllocator()
	if err := app.service.ConfigurePortAllocator(allocator); err != nil {
		t.Fatal(err)
	}
	app.downloader.downloadErr = errors.New("steamcmd failed")
	_, err := app.service.Apply(context.Background(), value.ID, "job-mod-failure", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeNew, DirectoryName: "FailedModImport",
		RoomName: "Failed Mod Import", TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsInstallMissing,
	})
	if err == nil || !strings.Contains(err.Error(), "steamcmd failed") {
		t.Fatalf("apply error=%v", err)
	}
	if allocator.released != allocator.allocation.LeaseID || allocator.activated != "" {
		t.Fatalf("allocator after Mod failure=%#v", allocator)
	}
	if _, statErr := os.Stat(filepath.Join(app.saveRoot, "FailedModImport")); !os.IsNotExist(statErr) {
		t.Fatalf("failed import was published: %v", statErr)
	}
}

func TestApplyReleasesPortLeaseWhenModInstallIsCanceled(t *testing.T) {
	app := newApplyTestApp(t)
	value := uploadAnalyzedImportWithMod(t, app, "mod-cancel")
	allocator := importTestPortAllocator()
	if err := app.service.ConfigurePortAllocator(allocator); err != nil {
		t.Fatal(err)
	}
	app.downloader.downloadErr = context.Canceled
	_, err := app.service.Apply(context.Background(), value.ID, "job-cancel", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeNew, DirectoryName: "CanceledModImport",
		RoomName: "Canceled Mod Import", TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsInstallMissing,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("apply error=%v want context cancellation", err)
	}
	if allocator.released != allocator.allocation.LeaseID || allocator.activated != "" {
		t.Fatalf("allocator after cancellation=%#v", allocator)
	}
}

func TestApplyReleasesPortLeaseWhenActivationFails(t *testing.T) {
	app := newApplyTestApp(t)
	value := uploadAnalyzedImportWithMod(t, app, "activation-failure")
	allocator := importTestPortAllocator()
	allocator.activateErr = errors.New("activation failed")
	if err := app.service.ConfigurePortAllocator(allocator); err != nil {
		t.Fatal(err)
	}
	_, err := app.service.Apply(context.Background(), value.ID, "job-activation-failure", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeNew, DirectoryName: "FailedActivationImport",
		RoomName: "Failed Activation Import", TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsPreserve,
	})
	if err == nil || !strings.Contains(err.Error(), "activation failed") {
		t.Fatalf("apply error=%v", err)
	}
	if allocator.activated != allocator.allocation.LeaseID || allocator.released != allocator.allocation.LeaseID {
		t.Fatalf("allocator after activation failure=%#v", allocator)
	}
	if _, statErr := os.Stat(filepath.Join(app.saveRoot, "FailedActivationImport")); !os.IsNotExist(statErr) {
		t.Fatalf("activation-failed import was not rolled back: %v", statErr)
	}
}

func TestApplyPreservesJournalWhenActivationAndPortReleaseFail(t *testing.T) {
	app := newApplyTestApp(t)
	value := uploadAnalyzedImportWithMod(t, app, "activation-release-failure")
	activationErr := errors.New("activation failed")
	releaseErr := errors.New("release failed")
	allocator := importTestPortAllocator()
	allocator.activateErr = activationErr
	allocator.releaseErr = releaseErr
	if err := app.service.ConfigurePortAllocator(allocator); err != nil {
		t.Fatal(err)
	}
	_, err := app.service.Apply(context.Background(), value.ID, "job-activation-release-failure", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeNew, DirectoryName: "FailedActivationReleaseImport",
		RoomName: "Failed Activation Release Import", TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsPreserve,
	})
	if !errors.Is(err, activationErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("apply error=%v", err)
	}
	var blocked importRecord
	if dbErr := app.store.db.Table(app.store.table).Where("id = ?", value.ID).First(&blocked).Error; dbErr != nil {
		t.Fatal(dbErr)
	}
	if blocked.Status != string(StatusApplying) || blocked.ApplyPhase != applyPhasePublishing || blocked.ApplyPortLeaseID != allocator.allocation.LeaseID || !strings.Contains(blocked.ErrorMessage, "release failed") {
		t.Fatalf("blocked apply journal=%#v", blocked)
	}
	if _, statErr := os.Stat(filepath.Join(app.saveRoot, "FailedActivationReleaseImport")); !os.IsNotExist(statErr) {
		t.Fatalf("failed import target was not rolled back: %v", statErr)
	}

	allocator.activateErr = nil
	allocator.releaseErr = nil
	if err := app.service.recoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	var recovered importRecord
	if dbErr := app.store.db.Table(app.store.table).Where("id = ?", value.ID).First(&recovered).Error; dbErr != nil {
		t.Fatal(dbErr)
	}
	if recovered.Status != string(StatusReady) || recovered.ApplyPhase != "" || recovered.ApplyPortLeaseID != "" {
		t.Fatalf("recovered apply journal=%#v", recovered)
	}
}

func TestApplyPreservesJournalWhenPrePublishPortReleaseFails(t *testing.T) {
	app := newApplyTestApp(t)
	value := uploadAnalyzedImportWithMod(t, app, "mod-release-failure")
	modErr := errors.New("steamcmd failed")
	releaseErr := errors.New("release failed")
	allocator := importTestPortAllocator()
	allocator.releaseErr = releaseErr
	if err := app.service.ConfigurePortAllocator(allocator); err != nil {
		t.Fatal(err)
	}
	app.downloader.downloadErr = modErr
	_, err := app.service.Apply(context.Background(), value.ID, "job-mod-release-failure", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeNew, DirectoryName: "FailedModReleaseImport",
		RoomName: "Failed Mod Release Import", TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsInstallMissing,
	})
	if !errors.Is(err, modErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("apply error=%v", err)
	}
	var blocked importRecord
	if dbErr := app.store.db.Table(app.store.table).Where("id = ?", value.ID).First(&blocked).Error; dbErr != nil {
		t.Fatal(dbErr)
	}
	if blocked.Status != string(StatusApplying) || blocked.ApplyPhase != applyPhasePublishing || blocked.ApplyPortLeaseID != allocator.allocation.LeaseID {
		t.Fatalf("pre-publish journal=%#v", blocked)
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

func TestRemoteRoomGuardBlocksReplaceButAllowsNewAndClone(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Source Room")},
		{name: "cluster_token.txt", content: "source-token-1234567890\n"},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
		{name: "Master/save/session/source/0000000001", content: "new-save"},
		{name: "Master/modoverrides.lua", content: `return {["workshop-1392778117"] = { enabled = true }}`},
	})
	value, err := app.service.Upload(context.Background(), "远端替换门禁", "source.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	originalPath := filepath.Join(app.saveRoot, "Target", "Master", "save", "session", "old", "0000000001")
	original, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	app.service.guard = deniedSaveImportMutationGuard{}

	_, err = app.service.Apply(context.Background(), value.ID, "job-remote", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: target.ID,
		Confirmation: target.Name, TokenPolicy: TokenPreserve, NetworkPolicy: NetworkPreserve, ModPolicy: ModsInstallMissing,
	})
	if !errors.Is(err, runtimeguard.ErrRemoteMutationUnavailable) {
		t.Fatalf("replace error = %v", err)
	}
	if code := ErrorCode(err); code != runtimeguard.ErrorCode {
		t.Fatalf("replace error code = %q", code)
	}
	unchanged, err := os.ReadFile(originalPath)
	if err != nil || string(unchanged) != string(original) {
		t.Fatalf("target save changed: before=%q after=%q err=%v", original, unchanged, err)
	}
	backups, err := app.backups.List(target.ID)
	if err != nil || len(backups) != 0 {
		t.Fatalf("replace created backups: value=%#v err=%v", backups, err)
	}
	if len(app.downloader.downloads) != 0 {
		t.Fatalf("replace downloaded Mods: %v", app.downloader.downloads)
	}
	staging, err := filepath.Glob(filepath.Join(app.saveRoot, ".dst-admin-import-*"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("replace created staging directories: value=%v err=%v", staging, err)
	}
	stored, err := app.store.Get(value.ID)
	if err != nil || stored.Status != StatusReady {
		t.Fatalf("replace changed import record: value=%#v err=%v", stored, err)
	}

	for _, local := range []struct {
		mode      ApplyMode
		directory string
	}{
		{mode: ApplyModeNew, directory: "ImportedNew"},
		{mode: ApplyModeClone, directory: "ImportedClone"},
	} {
		result, err := app.service.Apply(context.Background(), value.ID, "job-local", ApplyRequest{
			CandidateID: value.Manifest.Candidates[0].ID, Mode: local.mode, DirectoryName: local.directory,
			RoomName: local.directory, TokenPolicy: TokenSource, NetworkPolicy: NetworkAuto, ModPolicy: ModsPreserve,
		})
		if err != nil {
			t.Fatalf("%s apply error = %v", local.mode, err)
		}
		if result.DirectoryName != local.directory {
			t.Fatalf("%s result = %#v", local.mode, result)
		}
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

func TestNetworkSourceAndPreserveRequestsAreStrict(t *testing.T) {
	app := newApplyTestApp(t)
	root := createRecoveryRoom(t, app.saveRoot, "StrictNetwork", "save")
	requests, err := networkPortRequests(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) == 0 {
		t.Fatalf("strict network requests=%#v", requests)
	}
	for _, request := range requests {
		if !request.Strict {
			t.Fatalf("source/preserve request was allowed to drift: %#v", request)
		}
	}
	automatic, err := networkPortRequests(root, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range automatic {
		if request.Strict {
			t.Fatalf("automatic request unexpectedly strict: %#v", request)
		}
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
	if err := app.store.BeginApply(value.ID, ApplyModeReplace, target.ID, "Target", filepath.Base(staging), filepath.Base(rollback), ""); err != nil {
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

func TestServiceDefersRemoteReplacementRecoveryWithoutTouchingLocalPaths(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
	value := analyzedImportForRecovery(t, app)
	staging := createRecoveryRoom(t, app.saveRoot, ".dst-admin-import-remote", "new-save")
	rollback := filepath.Join(app.saveRoot, ".dst-admin-import-rollback-remote")
	if err := app.store.BeginApply(value.ID, ApplyModeReplace, target.ID, "Target", filepath.Base(staging), filepath.Base(rollback), ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(app.saveRoot, "Target"), rollback); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, filepath.Join(app.saveRoot, "Target")); err != nil {
		t.Fatal(err)
	}

	service, err := NewService(
		Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot},
		app.store, app.rooms, app.runtime, app.backups, app.downloader, deniedSaveImportMutationGuard{},
	)
	if err != nil || service == nil {
		t.Fatalf("service startup failed: service=%v err=%v", service, err)
	}
	current, err := os.ReadFile(filepath.Join(app.saveRoot, "Target", "Master", "save", "session", "new", "0000000001"))
	if err != nil || string(current) != "new-save" {
		t.Fatalf("current target changed: value=%q err=%v", current, err)
	}
	original, err := os.ReadFile(filepath.Join(rollback, "Master", "save", "session", "old", "0000000001"))
	if err != nil || string(original) != "old-save" {
		t.Fatalf("rollback changed: value=%q err=%v", original, err)
	}
	var record importRecord
	if err := app.store.db.Table(app.store.table).Where("id = ?", value.ID).First(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.Status != string(StatusApplying) || record.ApplyPhase != applyPhasePublishing || record.ErrorCode != runtimeguard.ErrorCode {
		t.Fatalf("blocked recovery record = %#v", record)
	}
}

func TestServiceRecoveryUnmanagesInterruptedNewRoom(t *testing.T) {
	app := newApplyTestApp(t)
	value := analyzedImportForRecovery(t, app)
	target := createRecoveryRoom(t, app.saveRoot, "Imported", "new-save")
	roomID := rooms.EncodeID(filepath.Base(target))
	stagingName := ".dst-admin-import-interrupted-new"
	if err := app.store.BeginApply(value.ID, ApplyModeNew, roomID, filepath.Base(target), stagingName, "", ""); err != nil {
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

func TestServiceReleasesInterruptedApplyLeaseAfterAllocatorIsConfigured(t *testing.T) {
	app := newApplyTestApp(t)
	value := analyzedImportForRecovery(t, app)
	target := createRecoveryRoom(t, app.saveRoot, "ImportedLeaseRecovery", "new-save")
	roomID := rooms.EncodeID(filepath.Base(target))
	stagingName := ".dst-admin-import-interrupted-lease"
	if err := app.store.BeginApply(value.ID, ApplyModeNew, roomID, filepath.Base(target), stagingName, "", "lease-restart"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.rooms.Adopt(roomID); err != nil {
		t.Fatal(err)
	}

	service, err := NewService(
		Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot},
		app.store, app.rooms, app.runtime, app.backups, app.downloader,
	)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfigure, err := app.store.Get(value.ID)
	if err != nil || beforeConfigure.Status != StatusApplying {
		t.Fatalf("lease recovery ran before allocator was available: value=%#v err=%v", beforeConfigure, err)
	}
	allocator := &applyPortAllocator{}
	if err := service.ConfigurePortAllocator(allocator); err != nil {
		t.Fatal(err)
	}
	if allocator.released != "lease-restart" || allocator.activated != "" {
		t.Fatalf("allocator after restart recovery=%#v", allocator)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("interrupted imported room still exists: %v", err)
	}
	recovered, err := app.store.Get(value.ID)
	if err != nil || recovered.Status != StatusReady || recovered.ErrorCode != "SERVER_RESTARTED" {
		t.Fatalf("recovered import=%#v err=%v", recovered, err)
	}
}

func TestServiceActivatesCommittedApplyLeaseAfterAllocatorIsConfigured(t *testing.T) {
	app := newApplyTestApp(t)
	value := analyzedImportForRecovery(t, app)
	target := createRecoveryRoom(t, app.saveRoot, "CommittedLeaseRecovery", "new-save")
	roomID := rooms.EncodeID(filepath.Base(target))
	if err := app.store.BeginApply(value.ID, ApplyModeNew, roomID, filepath.Base(target), ".dst-admin-import-committed-lease", "", "lease-committed-restart"); err != nil {
		t.Fatal(err)
	}
	if err := app.store.MarkApplyCommitted(value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.rooms.Adopt(roomID); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(
		Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot},
		app.store, app.rooms, app.runtime, app.backups, app.downloader,
	)
	if err != nil {
		t.Fatal(err)
	}
	allocator := &applyPortAllocator{}
	if err := service.ConfigurePortAllocator(allocator); err != nil {
		t.Fatal(err)
	}
	if allocator.activated != "lease-committed-restart" || allocator.released != "" {
		t.Fatalf("allocator after committed recovery=%#v", allocator)
	}
	recovered, err := app.store.Get(value.ID)
	if err != nil || recovered.Status != StatusApplied {
		t.Fatalf("recovered import=%#v err=%v", recovered, err)
	}
	var record importRecord
	if err := app.store.db.Table(app.store.table).Where("id = ?", value.ID).First(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.ApplyPhase != "" || record.ApplyPortLeaseID != "" {
		t.Fatalf("committed recovery journal not cleared: %#v", record)
	}
}

func TestServiceFinishesCommittedReplacementAfterRestart(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
	value := analyzedImportForRecovery(t, app)
	staging := createRecoveryRoom(t, app.saveRoot, ".dst-admin-import-committed", "new-save")
	rollback := filepath.Join(app.saveRoot, ".dst-admin-import-rollback-committed")
	if err := app.store.BeginApply(value.ID, ApplyModeReplace, target.ID, "Target", filepath.Base(staging), filepath.Base(rollback), ""); err != nil {
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
	if err := app.store.BeginApply(value.ID, ApplyModeReplace, target.ID, "Target", filepath.Base(staging), filepath.Base(rollback), ""); err != nil {
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

func uploadAnalyzedImportWithMod(t *testing.T, app applyTestApp, name string) Session {
	t.Helper()
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Imported")},
		{name: "cluster_token.txt", content: "source-token-1234567890\n"},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
		{name: "Master/save/session/source/0000000001", content: "save"},
		{name: "Master/modoverrides.lua", content: `return {["workshop-1392778117"] = { enabled = true }}`},
	})
	value, err := app.service.Upload(context.Background(), name, name+".zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func importTestPortAllocator() *applyPortAllocator {
	masterID := rooms.EncodeID("Master")
	return &applyPortAllocator{allocation: topology.PortAllocation{LeaseID: "lease-import", Reservations: []topology.PortReservation{
		{WorldID: masterID, Purpose: topology.PortClusterMaster, Port: 11889},
		{WorldID: masterID, Purpose: topology.PortDSTServer, Port: 11999},
		{WorldID: masterID, Purpose: topology.PortSteamAuth, Port: 9767},
		{WorldID: masterID, Purpose: topology.PortSteamMasterServer, Port: 28017},
	}}}
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
