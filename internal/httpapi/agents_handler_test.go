package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	localRoot := t.TempDir()
	service.ConfigureLocalRuntime(agents.RuntimeConfig{DisplayName: "本机", SavePath: localRoot, ServerPath: localRoot, LuaBinary: "lua", ServerMode: "64"})
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewAgentHandler(service).Register(v2)
	NewJobHandler(jobService).Register(v2)

	response := performJSON(router, http.MethodGet, "/api/v2/runtime-targets", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	runtimeData := responseData(t, response)
	if runtimeData["defaultTargetId"] != "local" || runtimeData["total"] != float64(3) {
		t.Fatalf("unexpected runtime targets: %s", response.Body.String())
	}
	runtimeItems := runtimeData["items"].([]interface{})
	localTarget := runtimeItems[0].(map[string]interface{})
	if localTarget["id"] != "local" || localTarget["default"] != true {
		t.Fatalf("local runtime is not first/default: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPatch, "/api/v2/runtime-targets/local", map[string]interface{}{
		"displayName": "本地游戏机",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["name"] != "本地游戏机" || data["hostname"] == "" {
		t.Fatalf("unexpected renamed local machine: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPut, "/api/v2/runtime-targets/local/display-address", map[string]interface{}{
		"displayAddress": "games.example.com",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["displayAddress"] != "games.example.com" || data["name"] != "本地游戏机" || data["id"] != "local" {
		t.Fatalf("unexpected display metadata: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPut, "/api/v2/runtime-targets/local/display-address", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusBadRequest)
	response = performJSON(router, http.MethodPut, "/api/v2/runtime-targets/local/display-address", map[string]interface{}{"displayAddress": "0.0.0.0"}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = performJSON(router, http.MethodPatch, "/api/v2/runtime-targets/agent%3Aagent-primary", map[string]interface{}{
		"displayName": "远程游戏机",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["name"] != "远程游戏机" || data["hostname"] == "" {
		t.Fatalf("unexpected renamed remote machine: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPatch, "/api/v2/runtime-targets/local", map[string]interface{}{
		"displayName": "bad\nname",
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = performJSON(router, http.MethodPut, "/api/v2/runtime-targets/agents/agent-primary", map[string]interface{}{
		"displayName": "远程生产节点", "savePath": "/srv/dst/save", "serverPath": "/srv/dst/server", "serverMode": "64",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["id"] != "agent:agent-primary" || data["configured"] != true || data["name"] != "远程游戏机" {
		t.Fatalf("unexpected saved runtime: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodGet, "/api/v2/agents/agent-primary/inventory", nil, nil, "")
	assertStatus(t, response, http.StatusNotFound)
	response = performJSON(router, http.MethodPost, "/api/v2/agents/agent-primary/inventory/actions/refresh", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	inventoryJobID := responseData(t, response)["id"].(string)
	waitHTTPJobStatus(t, jobService, inventoryJobID, jobs.StatusSucceeded)
	response = performJSON(router, http.MethodGet, "/api/v2/agents/agent-primary/inventory", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["stale"] != false || data["capacity"].(map[string]interface{})["recommendedShardLimit"] != float64(7) {
		t.Fatalf("unexpected inventory: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPut, "/api/v2/runtime-targets/agents/agent-primary", map[string]interface{}{
		"displayName": "无效节点", "savePath": "relative", "serverPath": "/srv/dst/server",
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)

	response = performJSON(router, http.MethodGet, "/api/v2/agents", nil, nil, "")
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

func waitHTTPJobStatus(t *testing.T, service *jobs.Service, id string, status jobs.Status) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := service.Get(id)
		if job.Status == status {
			return
		}
		if job.Status == jobs.StatusFailed || job.Status == jobs.StatusCanceled {
			t.Fatalf("job %s status=%s", id, job.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s", id, status)
}

func TestAgentReleaseHTTPUploadDownloadUpgradeAndDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	agentStore := agents.NewStore(db, "agent_release_http_")
	jobStore := jobs.NewStore(db, "agent_release_http_")
	if err := agentStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	service, err := agents.NewService(agentStore, jobService, agents.NewMemoryTransport())
	if err != nil {
		t.Fatal(err)
	}
	releaseStore, err := agents.NewReleaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureReleaseStore(releaseStore); err != nil {
		t.Fatal(err)
	}

	binaryPath := filepath.Join(t.TempDir(), "dst-admin-agent")
	build := exec.Command("go", "build", "-trimpath", "-ldflags", "-X=dont/agent.AgentVersion=9.9.9", "-o", binaryPath, "../../agent/cmd/agent")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Agent fixture: %v\n%s", err, output)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	handler := NewAgentHandler(service)
	handler.Register(router.Group("/api/v2"))
	handler.RegisterDownloads(router)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "dst-admin-agent-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("version", "9.9.9"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v2/agent-releases", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assertStatus(t, response, http.StatusCreated)
	releaseID, _ := responseData(t, response)["id"].(string)
	if releaseID == "" {
		t.Fatalf("missing release ID: %s", response.Body.String())
	}

	response = performJSON(router, http.MethodGet, "/api/v2/agent-releases", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["total"] != float64(1) || data["maxUploadBytes"] != float64(agents.MaxAgentReleaseBytes) {
		t.Fatalf("unexpected release list: %s", response.Body.String())
	}

	token, err := releaseStore.IssueDownloadToken("agent-primary", releaseID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/agent-updates/"+releaseID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assertStatus(t, response, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), binary) || response.Header().Get("X-Agent-Release-SHA256") == "" {
		t.Fatal("downloaded Agent binary or digest header does not match")
	}
	releaseStore.RevokeDownloadToken(token)

	response = performJSON(router, http.MethodPost, "/api/v2/agents/agent-primary/actions/upgrade", map[string]interface{}{"releaseId": releaseID}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID, _ := responseData(t, response)["id"].(string)
	waitHTTPJobStatus(t, jobService, jobID, jobs.StatusSucceeded)

	response = performJSON(router, http.MethodDelete, "/api/v2/agent-releases/"+releaseID, nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodGet, "/api/v2/agent-releases", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["total"] != float64(0) {
		t.Fatalf("release was not deleted: %s", response.Body.String())
	}
}

func TestAgentReleaseTooLargeMapsToHTTP413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	agentFailure(context, agents.ErrReleaseTooLarge)
	if context.Writer.Status() != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", context.Writer.Status())
	}
}
