package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/jobs"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func TestAgentHTTPListCommandFailureAndKeyRotation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := agents.NewStore(db, "agent_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobStore := jobs.NewStore(db, "agent_http_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	service, err := agents.NewService(store, jobService, agents.NewMemoryTransport())
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewAgentHandler(service).Register(v2)
	NewJobHandler(jobService).Register(v2)

	response := performJSON(router, http.MethodGet, "/api/v2/agents", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["total"] != float64(2) || data["transportAvailable"] != true {
		t.Fatalf("unexpected list: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/agents/agent-offline/commands", map[string]interface{}{"action": "system.refresh", "timeoutSeconds": 30}, nil, "")
	assertStatus(t, response, http.StatusConflict)
	response = performJSON(router, http.MethodPost, "/api/v2/agents/agent-primary/commands", map[string]interface{}{"action": "disk.inspect", "timeoutSeconds": 30}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID := responseData(t, response)["id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := jobService.Get(jobID)
		if job.Status == jobs.StatusFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	response = performJSON(router, http.MethodGet, "/api/v2/agents/commands?status=failed&limit=25", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["total"] != float64(1) {
		t.Fatalf("unexpected commands: %s", response.Body.String())
	}
	commandID := responseData(t, response)["items"].([]interface{})[0].(map[string]interface{})["id"].(string)
	response = performJSON(router, http.MethodGet, "/api/v2/agents/commands/"+commandID, nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["id"] != commandID {
		t.Fatalf("unexpected command detail: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodGet, "/api/v2/agents/commands?query=disk&startDate=2020-01-01&endDate=2030-01-01&limit=25", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["total"] != float64(1) {
		t.Fatalf("unexpected filtered commands: %s", response.Body.String())
	}

	response = performJSON(router, http.MethodGet, "/api/v2/agents/security", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "memory-agent-key") {
		t.Fatalf("security response leaked key: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/agents/security/actions/rotate", map[string]interface{}{"confirmation": "wrong"}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = performJSON(router, http.MethodPost, "/api/v2/agents/security/actions/rotate", map[string]interface{}{"confirmation": "ROTATE AGENT KEY"}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" || responseData(t, response)["newKey"] == "" {
		t.Fatalf("unexpected rotation: %s", response.Body.String())
	}
}
