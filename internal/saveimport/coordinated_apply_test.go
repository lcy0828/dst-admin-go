package saveimport

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dont/internal/distributedbackup"
	"dont/internal/rooms"
	"dont/internal/runtimeguard"

	"github.com/go-ini/ini"
)

type coordinatedRestorerStub struct {
	imported       []distributedbackup.DirectoryImportRequest
	restoredSetIDs []string
	importSet      distributedbackup.Set
	importErr      error
	restoreResult  distributedbackup.RestoreResult
	restoreErr     error
	operations     []distributedbackup.Operation
	operationsErr  error
	recovered      distributedbackup.Operation
	recoverErr     error
	recoveredIDs   []string
	token          string
	ports          map[string]int
}

func (stub *coordinatedRestorerStub) ImportDirectory(_ context.Context, request distributedbackup.DirectoryImportRequest) (distributedbackup.Set, error) {
	request.Worlds = append([]distributedbackup.DirectoryImportWorld(nil), request.Worlds...)
	stub.imported = append(stub.imported, request)
	if stub.importErr != nil {
		return distributedbackup.Set{}, stub.importErr
	}
	token, err := os.ReadFile(filepath.Join(request.SourceRoot, "cluster_token.txt"))
	if err != nil {
		return distributedbackup.Set{}, err
	}
	stub.token = strings.TrimSpace(string(token))
	stub.ports = make(map[string]int, len(request.Worlds))
	for _, world := range request.Worlds {
		config, err := ini.Load(filepath.Join(request.SourceRoot, world.DirectoryName, "server.ini"))
		if err != nil {
			return distributedbackup.Set{}, err
		}
		stub.ports[world.DirectoryName] = config.Section("NETWORK").Key("server_port").MustInt(0)
	}
	value := stub.importSet
	if value.ID == "" {
		value.ID = "imported-backup-set"
	}
	value.RoomID = request.RoomID
	return value, nil
}

func (stub *coordinatedRestorerStub) Restore(_ context.Context, setID, _ string, _ string) (distributedbackup.RestoreResult, error) {
	stub.restoredSetIDs = append(stub.restoredSetIDs, setID)
	return stub.restoreResult, stub.restoreErr
}

func (stub *coordinatedRestorerStub) Operations(string) ([]distributedbackup.Operation, error) {
	return append([]distributedbackup.Operation(nil), stub.operations...), stub.operationsErr
}

func (stub *coordinatedRestorerStub) RecoverOperation(_ context.Context, operationID string) (distributedbackup.Operation, error) {
	stub.recoveredIDs = append(stub.recoveredIDs, operationID)
	return stub.recovered, stub.recoverErr
}

func TestCoordinatedReplaceSupportsRunningRemoteAndMixedRooms(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedTwoWorldRoom(t, app, "Target", "Target Room")
	value := uploadTwoWorldImport(t, app, "Forest", 1, true, "Underground", 2, false)
	operationID := "restore-operation"
	coordinator := &coordinatedRestorerStub{
		restoreResult: distributedbackup.RestoreResult{OperationID: operationID, ProtectionSetID: "protection-set"},
		operations: []distributedbackup.Operation{{
			ID: operationID, SetID: "imported-backup-set", RoomID: target.ID,
			Kind: "restore", Status: distributedbackup.OperationSucceeded,
		}},
	}
	service := coordinatedApplyService(t, app, coordinator, deniedSaveImportMutationGuard{})
	app.runtime.running["Master"] = true
	app.runtime.running["Caves"] = true

	result, err := service.Apply(context.Background(), value.ID, "import-job", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: target.ID,
		Confirmation: target.Name, TokenPolicy: TokenPreserve, NetworkPolicy: NetworkPreserve, ModPolicy: ModsPreserve,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProtectionBackupID != "protection-set" || len(coordinator.imported) != 1 || len(coordinator.restoredSetIDs) != 1 {
		t.Fatalf("result=%#v imports=%d restores=%v", result, len(coordinator.imported), coordinator.restoredSetIDs)
	}
	if coordinator.token != "target-token-1234567890" {
		t.Fatalf("preserved token=%q", coordinator.token)
	}
	if coordinator.ports["Forest"] != 12001 || coordinator.ports["Underground"] != 12002 {
		t.Fatalf("preserved ports=%v", coordinator.ports)
	}
	worlds, err := app.rooms.Worlds(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantDirectories := map[string]string{}
	for _, world := range worlds {
		if world.DirectoryName == "Master" {
			wantDirectories[world.ID] = "Forest"
		} else if world.DirectoryName == "Caves" {
			wantDirectories[world.ID] = "Underground"
		}
	}
	for _, world := range coordinator.imported[0].Worlds {
		if wantDirectories[world.WorldID] != world.DirectoryName {
			t.Fatalf("world mapping=%#v want=%v", coordinator.imported[0].Worlds, wantDirectories)
		}
	}
	oldSave, err := os.ReadFile(filepath.Join(app.saveRoot, "Target", "Master", "save", "session", "old", "0000000001"))
	if err != nil || string(oldSave) != "old-master" {
		t.Fatalf("controller target was modified: value=%q err=%v", oldSave, err)
	}
}

func TestCoordinatedWorldMappingFallsBackToMasterRole(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedTwoWorldRoom(t, app, "Target", "Target Room")
	importedRoot := filepath.Join(t.TempDir(), "Imported")
	if err := os.MkdirAll(filepath.Join(importedRoot, "Overworld"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(importedRoot, "Caves"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(importedRoot, "Overworld", "server.ini"), []byte(serverINI(true, 9, 10999)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(importedRoot, "Caves", "server.ini"), []byte(serverINI(false, 8, 11000)), 0o640); err != nil {
		t.Fatal(err)
	}

	mapping, err := app.service.coordinatedWorldMapping(target.ID, filepath.Join(app.saveRoot, "Target"), importedRoot)
	if err != nil {
		t.Fatal(err)
	}
	worlds, err := app.rooms.Worlds(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, world := range worlds {
		if world.DirectoryName == "Master" {
			want[world.ID] = "Overworld"
		} else {
			want[world.ID] = "Caves"
		}
	}
	for _, world := range mapping {
		if want[world.WorldID] != world.DirectoryName {
			t.Fatalf("mapping=%#v want=%v", mapping, want)
		}
	}
}

func TestCoordinatedReplaceRejectsDifferentWorldCount(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedTwoWorldRoom(t, app, "Target", "Target Room")
	value := uploadAnalyzedImportWithMod(t, app, "single-world")
	coordinator := &coordinatedRestorerStub{}
	service := coordinatedApplyService(t, app, coordinator)

	_, err := service.Apply(context.Background(), value.ID, "import-job", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: target.ID,
		Confirmation: target.Name, TokenPolicy: TokenPreserve, NetworkPolicy: NetworkPreserve, ModPolicy: ModsPreserve,
	})
	if !errors.Is(err, ErrWorldMismatch) || len(coordinator.imported) != 0 {
		t.Fatalf("error=%v imports=%d", err, len(coordinator.imported))
	}
}

func TestCoordinatedRestoreFailureReturnsImportToReady(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
	value := uploadAnalyzedImportWithMod(t, app, "restore-failure")
	coordinator := &coordinatedRestorerStub{restoreErr: errors.New("restore failed")}
	service := coordinatedApplyService(t, app, coordinator)

	_, err := service.Apply(context.Background(), value.ID, "import-job", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: target.ID,
		Confirmation: target.Name, TokenPolicy: TokenPreserve, NetworkPolicy: NetworkPreserve, ModPolicy: ModsPreserve,
	})
	if err == nil {
		t.Fatal("expected restore failure")
	}
	stored, err := app.store.Get(value.ID)
	if err != nil || stored.Status != StatusReady {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	assertCoordinatedJournal(t, app.store, value.ID, StatusReady, "")
}

func TestCoordinatedRestoreRetainsJournalWhenRecoveryIsIncomplete(t *testing.T) {
	app := newApplyTestApp(t)
	target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
	value := uploadAnalyzedImportWithMod(t, app, "restore-recovery")
	operationID := "restore-recovery-operation"
	coordinator := &coordinatedRestorerStub{
		restoreResult: distributedbackup.RestoreResult{OperationID: operationID},
		restoreErr:    errors.New("publish result uncertain"),
		operations: []distributedbackup.Operation{{
			ID: operationID, SetID: "imported-backup-set", RoomID: target.ID,
			Kind: "restore", Status: distributedbackup.OperationRecoveryRequired,
		}},
		recoverErr: distributedbackup.ErrRecoveryIncomplete,
	}
	service := coordinatedApplyService(t, app, coordinator)

	_, err := service.Apply(context.Background(), value.ID, "import-job", ApplyRequest{
		CandidateID: value.Manifest.Candidates[0].ID, Mode: ApplyModeReplace, TargetRoomID: target.ID,
		Confirmation: target.Name, TokenPolicy: TokenPreserve, NetworkPolicy: NetworkPreserve, ModPolicy: ModsPreserve,
	})
	if !errors.Is(err, distributedbackup.ErrRecoveryIncomplete) || len(coordinator.recoveredIDs) != 1 {
		t.Fatalf("error=%v recovered=%v", err, coordinator.recoveredIDs)
	}
	assertCoordinatedJournal(t, app.store, value.ID, StatusApplying, "imported-backup-set")
}

func TestServiceRecoversCoordinatedApplyAfterRestart(t *testing.T) {
	for _, test := range []struct {
		name        string
		operation   distributedbackup.Operation
		recovered   distributedbackup.Operation
		wantRecover bool
	}{
		{
			name:      "already completed",
			operation: distributedbackup.Operation{ID: "restore-complete", SetID: "set-restart", Kind: "restore", Status: distributedbackup.OperationSucceeded},
		},
		{
			name:        "resume active restore",
			operation:   distributedbackup.Operation{ID: "restore-active", SetID: "set-restart", Kind: "restore", Status: distributedbackup.OperationRunning},
			recovered:   distributedbackup.Operation{ID: "restore-active", SetID: "set-restart", Kind: "restore", Status: distributedbackup.OperationSucceeded},
			wantRecover: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := newApplyTestApp(t)
			target := createManagedRoom(t, app, "Target", "Target Room", "target-token-1234567890", 12001, "old-save")
			value := analyzedImportForRecovery(t, app)
			if err := app.store.BeginCoordinatedApply(value.ID, target.ID, "set-restart"); err != nil {
				t.Fatal(err)
			}
			test.operation.RoomID = target.ID
			test.recovered.RoomID = target.ID
			coordinator := &coordinatedRestorerStub{operations: []distributedbackup.Operation{test.operation}, recovered: test.recovered}
			_ = coordinatedApplyService(t, app, coordinator)

			stored, err := app.store.Get(value.ID)
			if err != nil || stored.Status != StatusApplied {
				t.Fatalf("stored=%#v err=%v", stored, err)
			}
			if got := len(coordinator.recoveredIDs) > 0; got != test.wantRecover {
				t.Fatalf("recover called=%v want=%v", got, test.wantRecover)
			}
			assertCoordinatedJournal(t, app.store, value.ID, StatusApplied, "")
		})
	}
}

func coordinatedApplyService(t *testing.T, app applyTestApp, coordinator CoordinatedRestorer, guards ...runtimeguard.MutationGuard) *Service {
	t.Helper()
	var service *Service
	var err error
	if len(guards) == 0 {
		service, err = NewServiceWithCoordinator(
			Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot},
			app.store, app.rooms, app.runtime, app.backups, app.downloader, coordinator,
		)
	} else {
		service, err = NewServiceWithCoordinator(
			Config{SaveRoot: app.saveRoot, ImportRoot: app.importRoot, WorkshopRoot: app.workshopRoot},
			app.store, app.rooms, app.runtime, app.backups, app.downloader, coordinator, guards[0],
		)
	}
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func createManagedTwoWorldRoom(t *testing.T, app applyTestApp, directory, name string) rooms.Room {
	t.Helper()
	room := createManagedRoom(t, app, directory, name, "target-token-1234567890", 12001, "old-master")
	cavesRoot := filepath.Join(app.saveRoot, directory, "Caves")
	if err := os.MkdirAll(filepath.Join(cavesRoot, "save", "session", "old"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cavesRoot, "server.ini"), []byte(serverINI(false, 2, 12002)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cavesRoot, "save", "session", "old", "0000000001"), []byte("old-caves"), 0o640); err != nil {
		t.Fatal(err)
	}
	return room
}

func uploadTwoWorldImport(t *testing.T, app applyTestApp, first string, firstID int, firstMaster bool, second string, secondID int, secondMaster bool) Session {
	t.Helper()
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Imported Room")},
		{name: "cluster_token.txt", content: "source-token-1234567890\n"},
		{name: first + "/server.ini", content: serverINI(firstMaster, firstID, 10999)},
		{name: first + "/save/session/source/0000000001", content: "imported-first"},
		{name: second + "/server.ini", content: serverINI(secondMaster, secondID, 11000)},
		{name: second + "/save/session/source/0000000001", content: "imported-second"},
	})
	value, err := app.service.Upload(context.Background(), "two-world-import", "two-world.zip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	value, err = app.service.Analyze(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func assertCoordinatedJournal(t *testing.T, store *Store, id string, status Status, backupSetID string) {
	t.Helper()
	var record importRecord
	if err := store.db.Table(store.table).Where("id = ?", id).First(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.Status != string(status) || record.ApplyBackupSetID != backupSetID {
		t.Fatalf("journal=%#v want status=%s set=%q", record, status, backupSetID)
	}
	if backupSetID == "" && record.ApplyPhase != "" {
		t.Fatalf("journal phase was not cleared: %#v", record)
	}
	if backupSetID != "" && record.ApplyPhase != applyPhaseCoordinated {
		t.Fatalf("journal phase=%q", record.ApplyPhase)
	}
}
