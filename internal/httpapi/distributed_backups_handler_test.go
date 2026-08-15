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
	value distributedbackup.Set
}

func (f *distributedBackupHTTPFixture) List(string) ([]distributedbackup.Set, error) {
	return []distributedbackup.Set{f.value}, nil
}

func (f *distributedBackupHTTPFixture) Get(string) (distributedbackup.Set, error) {
	return f.value, nil
}

func (f *distributedBackupHTTPFixture) Create(_ context.Context, roomID, name, kind, jobID string) (distributedbackup.Set, error) {
	f.value.RoomID, f.value.Name, f.value.Kind, f.value.SourceJobID = roomID, name, kind, jobID
	f.value.Status = distributedbackup.StatusVerified
	return f.value, nil
}

func (f *distributedBackupHTTPFixture) Restore(_ context.Context, setID, _ string, _ string) (distributedbackup.RestoreResult, error) {
	return distributedbackup.RestoreResult{SetID: setID, OperationID: "operation", ProtectionSetID: "protection"}, nil
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
	fixture := &distributedBackupHTTPFixture{value: distributedbackup.Set{ID: "set-1", RoomID: "room", RoomName: "测试房间", Name: "备份集"}}
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

	restored := performDistributedBackupRequest(t, router, http.MethodPost, "/api/v2/backup-sets/set-1/actions/restore", map[string]string{"confirmation": "测试房间"})
	if restored.Code != http.StatusAccepted {
		t.Fatalf("restore status=%d body=%s", restored.Code, restored.Body.String())
	}
	restoreJob := waitDistributedBackupJob(t, jobService, distributedBackupJobID(t, restored.Body.Bytes()))
	if restoreJob.Status != jobs.StatusSucceeded || restoreJob.Kind != "backup-set.restore" {
		t.Fatalf("restore job=%#v", restoreJob)
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
