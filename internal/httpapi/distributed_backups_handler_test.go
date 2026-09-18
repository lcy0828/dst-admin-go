package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dont/internal/distributedbackup"
	"dont/internal/jobs"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type distributedBackupHTTPFixture struct {
	value       distributedbackup.Set
	operations  []distributedbackup.Operation
	createdMode string
}

type coldOnlyDistributedBackupService struct{ DistributedBackupService }

func (f *distributedBackupHTTPFixture) List(string) ([]distributedbackup.Set, error) {
	return []distributedbackup.Set{f.value}, nil
}

func (f *distributedBackupHTTPFixture) Get(string) (distributedbackup.Set, error) {
	return f.value, nil
}

func (f *distributedBackupHTTPFixture) Operations(string) ([]distributedbackup.Operation, error) {
	return append([]distributedbackup.Operation(nil), f.operations...), nil
}

func (f *distributedBackupHTTPFixture) Operation(id string) (distributedbackup.Operation, error) {
	for _, operation := range f.operations {
		if operation.ID == id {
			return operation, nil
		}
	}
	return distributedbackup.Operation{}, distributedbackup.ErrNotFound
}

func (f *distributedBackupHTTPFixture) Create(_ context.Context, roomID, name, kind, jobID string) (distributedbackup.Set, error) {
	f.value.RoomID, f.value.Name, f.value.Kind, f.value.SourceJobID = roomID, name, kind, jobID
	f.value.Mode = distributedbackup.ModeCold
	f.value.Status = distributedbackup.StatusVerified
	f.createdMode = distributedbackup.ModeCold
	return f.value, nil
}

func (f *distributedBackupHTTPFixture) CreateWithMode(_ context.Context, roomID, name, kind, jobID, mode string) (distributedbackup.Set, error) {
	f.value.RoomID, f.value.Name, f.value.Kind, f.value.SourceJobID = roomID, name, kind, jobID
	f.value.Mode = mode
	f.value.Status = distributedbackup.StatusVerified
	f.createdMode = mode
	return f.value, nil
}

func (f *distributedBackupHTTPFixture) Restore(_ context.Context, setID, _ string, _ string) (distributedbackup.RestoreResult, error) {
	operationID := "00000000-0000-4000-8000-000000000001"
	f.operations = []distributedbackup.Operation{{
		ID: operationID, SetID: setID, RoomID: f.value.RoomID, Kind: "restore", Phase: "completed", Status: distributedbackup.OperationRecoveryRequired,
		Failure: "清理待重试", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	return distributedbackup.RestoreResult{SetID: setID, OperationID: operationID, ProtectionSetID: "protection", Warnings: []string{"清理待重试"}}, nil
}

func (f *distributedBackupHTTPFixture) RecoverOperation(_ context.Context, id string) (distributedbackup.Operation, error) {
	for index, operation := range f.operations {
		if operation.ID != id {
			continue
		}
		operation.Status, operation.Phase, operation.Failure = distributedbackup.OperationSucceeded, "completed", ""
		operation.UpdatedAt = time.Now().UTC()
		f.operations[index] = operation
		return operation, nil
	}
	return distributedbackup.Operation{}, distributedbackup.ErrNotFound
}

func TestDistributedBackupHandlerCreatesAndRestoresJobs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	jobStore := jobs.NewStore(db, "distributed_http_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	fixture := &distributedBackupHTTPFixture{value: distributedbackup.Set{ID: "set-1", RoomID: "room", RoomName: "测试房间", Name: "备份集", Restorable: true, ContentKind: "game-save"}}
	handler, err := NewDistributedBackupHandler(fixture, jobService)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	created := performDistributedBackupRequest(t, router, http.MethodPost, "/api/v2/rooms/room/backup-sets", map[string]string{"name": "夜间备份"})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	createJobID := distributedBackupJobID(t, created.Body.Bytes())
	createJob := waitDistributedBackupJob(t, jobService, createJobID)
	if createJob.Status != jobs.StatusSucceeded || createJob.Kind != "backup-set.create" {
		t.Fatalf("create job=%#v", createJob)
	}
	if fixture.createdMode != distributedbackup.ModeHot {
		t.Fatalf("default create mode=%q, want %q", fixture.createdMode, distributedbackup.ModeHot)
	}

	coldCreated := performDistributedBackupRequest(t, router, http.MethodPost, "/api/v2/rooms/room/backup-sets", map[string]string{"name": "停服备份", "mode": distributedbackup.ModeCold})
	if coldCreated.Code != http.StatusAccepted {
		t.Fatalf("cold create status=%d body=%s", coldCreated.Code, coldCreated.Body.String())
	}
	coldCreateJob := waitDistributedBackupJob(t, jobService, distributedBackupJobID(t, coldCreated.Body.Bytes()))
	if coldCreateJob.Status != jobs.StatusSucceeded || fixture.createdMode != distributedbackup.ModeCold {
		t.Fatalf("cold create job=%#v mode=%q", coldCreateJob, fixture.createdMode)
	}

	automaticCreated := performDistributedBackupRequest(t, router, http.MethodPost, "/api/v2/rooms/room/backup-sets", map[string]string{"name": "自动选择模式", "mode": distributedbackup.ModeAutomatic})
	if automaticCreated.Code != http.StatusAccepted {
		t.Fatalf("automatic create status=%d body=%s", automaticCreated.Code, automaticCreated.Body.String())
	}
	automaticJob := waitDistributedBackupJob(t, jobService, distributedBackupJobID(t, automaticCreated.Body.Bytes()))
	if automaticJob.Status != jobs.StatusSucceeded || fixture.createdMode != distributedbackup.ModeAutomatic {
		t.Fatalf("automatic create job=%#v mode=%q", automaticJob, fixture.createdMode)
	}
	fixture.createdMode = ""
	coldOnlyHandler, err := NewDistributedBackupHandler(coldOnlyDistributedBackupService{DistributedBackupService: fixture}, jobService)
	if err != nil {
		t.Fatal(err)
	}
	coldOnlyRouter := gin.New()
	coldOnlyHandler.Register(coldOnlyRouter.Group("/api/v2"))
	unsupported := performDistributedBackupRequest(t, coldOnlyRouter, http.MethodPost, "/api/v2/rooms/room/backup-sets", map[string]string{"name": "不支持热备份"})
	if unsupported.Code != http.StatusAccepted {
		t.Fatalf("unsupported create status=%d body=%s", unsupported.Code, unsupported.Body.String())
	}
	unsupportedJob := waitDistributedBackupJob(t, jobService, distributedBackupJobID(t, unsupported.Body.Bytes()))
	if unsupportedJob.Status != jobs.StatusFailed || fixture.createdMode != "" || len(unsupportedJob.Targets) != 1 ||
		unsupportedJob.Targets[0].Error == nil || unsupportedJob.Targets[0].Error.Code != "BACKUP_HOT_UNAVAILABLE" {
		t.Fatalf("unsupported create job=%#v mode=%q", unsupportedJob, fixture.createdMode)
	}

	restored := performDistributedBackupRequest(t, router, http.MethodPost, "/api/v2/backup-sets/set-1/actions/restore", map[string]string{"confirmation": "测试房间"})
	if restored.Code != http.StatusAccepted {
		t.Fatalf("restore status=%d body=%s", restored.Code, restored.Body.String())
	}
	restoreJob := waitDistributedBackupJob(t, jobService, distributedBackupJobID(t, restored.Body.Bytes()))
	if restoreJob.Status != jobs.StatusSucceeded || restoreJob.Kind != "backup-set.restore" {
		t.Fatalf("restore job=%#v", restoreJob)
	}

	operations := performDistributedBackupRequest(t, router, http.MethodGet, "/api/v2/rooms/room/backup-operations", nil)
	if operations.Code != http.StatusOK ||
		!bytes.Contains(operations.Body.Bytes(), []byte(`"status":"recovery_required"`)) ||
		!bytes.Contains(operations.Body.Bytes(), []byte(`"fencingToken":0`)) {
		t.Fatalf("operations status=%d body=%s", operations.Code, operations.Body.String())
	}
	recovered := performDistributedBackupRequest(t, router, http.MethodPost, "/api/v2/backup-operations/00000000-0000-4000-8000-000000000001/actions/recover", nil)
	if recovered.Code != http.StatusAccepted {
		t.Fatalf("recover status=%d body=%s", recovered.Code, recovered.Body.String())
	}
	recoverJob := waitDistributedBackupJob(t, jobService, distributedBackupJobID(t, recovered.Body.Bytes()))
	if recoverJob.Status != jobs.StatusSucceeded || recoverJob.Kind != "backup-set.recover" {
		t.Fatalf("recover job=%#v", recoverJob)
	}
}

func performDistributedBackupRequest(t *testing.T, router http.Handler, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func distributedBackupJobID(t *testing.T, payload []byte) string {
	t.Helper()
	var envelope struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Data.ID == "" {
		t.Fatalf("job payload=%s err=%v", payload, err)
	}
	return envelope.Data.ID
}

func waitDistributedBackupJob(t *testing.T, service *jobs.Service, id string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		value, err := service.Get(id)
		if err == nil && (value.Status == jobs.StatusSucceeded || value.Status == jobs.StatusFailed) {
			return value
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return jobs.Job{}
}
