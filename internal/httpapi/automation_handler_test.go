package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"dont/internal/automation"
	"dont/internal/jobs"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type automationHTTPRooms struct{}

func (automationHTTPRooms) Room(id string) (rooms.Room, error) {
	if id != "room" {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return rooms.Room{ID: id, Name: "Room", Managed: true}, nil
}

type automationHTTPExecutor struct{}

func (automationHTTPExecutor) Validate(automation.Task) error { return nil }
func (automationHTTPExecutor) Execute(context.Context, automation.Task, string) (automation.ExecutionResult, error) {
	return automation.ExecutionResult{Message: "done"}, nil
}

type automationHTTPScheduler struct{ reloads int }

func (s *automationHTTPScheduler) Reload() error { s.reloads++; return nil }

func TestAutomationHTTPCRUDRunAndImportPreview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := automation.NewStore(db, "automation_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobStore := jobs.NewStore(db, "automation_http_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	service, err := automation.NewService(automationHTTPRooms{}, store, jobService, automationHTTPExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := &automationHTTPScheduler{}
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewAutomationHandler(service, scheduler).Register(v2)
	NewJobHandler(jobService).Register(v2)

	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room/automation/groups", map[string]interface{}{"name": "Daily", "enabled": true}, nil, "")
	assertStatus(t, response, http.StatusCreated)
	group := responseData(t, response)
	groupID := group["id"].(string)
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/automation/tasks", map[string]interface{}{"groupId": groupID, "name": "Refresh", "enabled": true, "schedule": "0 10 * * *", "timezone": "Asia/Shanghai", "action": "world.state.refresh", "worldIds": []string{}, "parameters": map[string]interface{}{}, "timeoutSeconds": 30}, nil, "")
	assertStatus(t, response, http.StatusCreated)
	taskID := responseData(t, response)["id"].(string)
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/automation/tasks/"+taskID+"/actions/run", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID := responseData(t, response)["id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, getErr := jobService.Get(jobID)
		if getErr == nil && job.Status == jobs.StatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/automation/runs?limit=25", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["total"] != float64(1) {
		t.Fatalf("unexpected runs: %s", response.Body.String())
	}
	document := map[string]interface{}{"version": 1, "groups": []map[string]interface{}{{"key": "ops", "name": "Ops", "enabled": true}}, "tasks": []map[string]interface{}{{"groupKey": "ops", "name": "Refresh logs", "enabled": true, "schedule": "0 10 * * *", "timezone": "Asia/Shanghai", "action": "log.structured.refresh", "worldIds": []string{}, "parameters": map[string]interface{}{}, "timeoutSeconds": 30}}}
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/automation/imports/preview", document, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["valid"] != true {
		t.Fatalf("unexpected import preview: %s", response.Body.String())
	}
	if scheduler.reloads != 2 {
		t.Fatalf("scheduler reloads=%d, want 2", scheduler.reloads)
	}
}
