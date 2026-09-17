package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/backups"
	"dont/internal/configuration"
	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type configurationHandlerCatalog struct {
	room  rooms.Room
	world rooms.World
}

func (c configurationHandlerCatalog) Room(string) (rooms.Room, error) {
	return c.room, nil
}

func (c configurationHandlerCatalog) World(_, _ string) (rooms.World, error) {
	return c.world, nil
}

type configurationHandlerBackups struct {
	mu    sync.Mutex
	count int
}

type configurationHandlerPublisher struct{}

func (configurationHandlerPublisher) Publish(context.Context, configuration.PublicationRequest) (configuration.PublicationResult, error) {
	return configuration.PublicationResult{PublicationID: "publication", PublishedCount: 2}, nil
}

func (b *configurationHandlerBackups) Create(_ context.Context, roomID, _ string, kind backups.Kind, jobID string) (backups.Backup, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.count++
	return backups.Backup{ID: "protection-backup", RoomID: roomID, Kind: kind, SourceJobID: jobID}, nil
}

func (b *configurationHandlerBackups) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

type configurationHandlerApp struct {
	router   *gin.Engine
	jobs     *jobs.Service
	backups  *configurationHandlerBackups
	roomPath string
}

func newConfigurationHandlerApp(t *testing.T) configurationHandlerApp {
	t.Helper()
	root := t.TempDir()
	roomPath := filepath.Join(root, "Cluster")
	worldPath := filepath.Join(roomPath, "Master")
	if err := os.MkdirAll(worldPath, 0750); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(roomPath, "cluster.ini"):            "[NETWORK]\ncluster_name = Original\ncluster_description = Before\ncluster_password = existing-secret\n[GAMEPLAY]\ngame_mode = survival\nmax_players = 6\npvp = false\n",
		filepath.Join(roomPath, "adminlist.txt"):          "KU_ONE\nKU_TWO\n",
		filepath.Join(roomPath, "cluster_token.txt"):      "existing-cluster-token\n",
		filepath.Join(worldPath, "server.ini"):            "[NETWORK]\nserver_port = 10999\n[STEAM]\nauthentication_port = 8768\nmaster_server_port = 27018\n[ACCOUNT]\nencode_user_path = true\n",
		filepath.Join(worldPath, "leveldataoverride.lua"): "return { overrides = { day = \"default\" } }\n",
	}
	for path, data := range files {
		if err := os.WriteFile(path, []byte(data), 0640); err != nil {
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
	jobStore := jobs.NewStore(db, "configuration_http_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	catalog := configurationHandlerCatalog{
		room:  rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Original", Managed: true},
		world: rooms.World{ID: "world", RoomID: "room", DirectoryName: "Master", Name: "Master", IsMaster: true},
	}
	backupCreator := &configurationHandlerBackups{}
	service, err := configuration.NewService(root, catalog, backupCreator)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigurePublisher(configurationHandlerPublisher{}); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewConfigurationHandler(service, jobService).Register(v2)
	NewJobHandler(jobService).Register(v2)
	return configurationHandlerApp{router: router, jobs: jobService, backups: backupCreator, roomPath: roomPath}
}

func TestConfigurationHTTPRevisionFieldsAndApplyJob(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newConfigurationHandlerApp(t)

	response := performJSON(app.router, http.MethodGet, "/api/v2/rooms/room/configuration", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	current := responseData(t, response)
	revision, _ := current["revision"].(string)
	values, _ := current["values"].(map[string]interface{})
	if revision == "" || values == nil {
		t.Fatalf("invalid configuration response: %s", response.Body.String())
	}

	next := cloneJSONMap(t, values)
	next["clusterDescription"] = "After"
	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/configuration/preview", map[string]interface{}{
		"expectedRevision": "stale", "values": next,
	}, nil, "")
	assertAPIError(t, response, http.StatusConflict, "CONFIG_REVISION_CONFLICT")

	invalid := cloneJSONMap(t, values)
	invalid["maxPlayers"] = 0
	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/configuration/preview", map[string]interface{}{
		"expectedRevision": revision, "values": invalid,
	}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_CONFIGURATION")
	if !strings.Contains(response.Body.String(), "maxPlayers") {
		t.Fatalf("field error missing maxPlayers: %s", response.Body.String())
	}

	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/configuration/preview", map[string]interface{}{
		"expectedRevision": revision, "values": next,
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if strings.Contains(response.Body.String(), "existing-secret") {
		t.Fatalf("preview leaked password: %s", response.Body.String())
	}

	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/configuration/actions/apply", map[string]interface{}{
		"expectedRevision": revision, "values": next,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID, _ := responseData(t, response)["id"].(string)
	job := waitForConfigurationJob(t, app.jobs, jobID)
	if len(job.Targets) != 1 || !strings.Contains(job.Targets[0].Message, "已应用到 2 个 Runtime 目标") {
		t.Fatalf("configuration job did not report remote publication: %#v", job.Targets)
	}
	if app.backups.Count() != 0 {
		t.Fatalf("ordinary configuration save created %d world backups", app.backups.Count())
	}
	written, err := os.ReadFile(filepath.Join(app.roomPath, "cluster.ini"))
	if err != nil || !strings.Contains(string(written), "cluster_description = After") {
		t.Fatalf("cluster.ini was not applied: %v\n%s", err, written)
	}
}

func TestConfigurationHTTPAccessConfirmationAndTokenNoStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newConfigurationHandlerApp(t)

	response := performJSON(app.router, http.MethodGet, "/api/v2/rooms/room/access", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	revision, _ := responseData(t, response)["revision"].(string)
	accessRequest := map[string]interface{}{
		"expectedRevision": revision,
		"admins":           []string{"KU_ONE"},
		"blocked":          []string{},
		"whitelist":        []string{},
		"confirmation":     "",
	}
	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/access/preview", accessRequest, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["requiresConfirmation"] != true {
		t.Fatalf("removal preview did not require confirmation: %s", response.Body.String())
	}
	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/access/actions/apply", accessRequest, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED")
	accessRequest["confirmation"] = "Original"
	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/access/actions/apply", accessRequest, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	waitForConfigurationJob(t, app.jobs, responseData(t, response)["id"].(string))

	response = performJSON(app.router, http.MethodGet, "/api/v2/rooms/room/cluster-token", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if strings.Contains(response.Body.String(), "existing-cluster-token") || responseData(t, response)["maskedValue"] != "****oken" {
		t.Fatalf("token status is not safely masked: %s", response.Body.String())
	}
	tokenRevision, _ := responseData(t, response)["revision"].(string)
	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/cluster-token/reveal", map[string]string{"confirmation": "wrong"}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED")
	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/cluster-token/reveal", map[string]string{"confirmation": "Original"}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "existing-cluster-token") {
		t.Fatalf("token reveal cache contract failed: headers=%v body=%s", response.Header(), response.Body.String())
	}

	tokenRequest := map[string]string{
		"expectedRevision": tokenRevision,
		"token":            "replacement-token",
		"confirmation":     "Original",
	}
	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/cluster-token/preview", tokenRequest, nil, "")
	assertStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "replacement-token") || strings.Contains(response.Body.String(), "existing-cluster-token") {
		t.Fatalf("token preview leaked a secret or is cacheable: headers=%v body=%s", response.Header(), response.Body.String())
	}
	response = performJSON(app.router, http.MethodPost, "/api/v2/rooms/room/cluster-token/actions/apply", tokenRequest, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID, _ := responseData(t, response)["id"].(string)
	job := waitForConfigurationJob(t, app.jobs, jobID)
	encoded, _ := json.Marshal(job)
	if strings.Contains(string(encoded), "replacement-token") || strings.Contains(string(encoded), "existing-cluster-token") {
		t.Fatalf("configuration job leaked a token: %s", encoded)
	}
}

func TestConfigurationHTTPReportsAgentUpgradeRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "unsupported Agent action", err: agents.ErrUnsupportedAction},
		{name: "missing Runtime capability", err: runtimedriver.ErrCapabilityMissing},
		{name: "wrapped missing capability", err: errors.Join(errors.New("configuration read failed"), runtimedriver.ErrCapabilityMissing)},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v2/rooms/room/configuration", nil)
			configurationFailure(ctx, test.err)
			assertAPIError(t, response, http.StatusConflict, "AGENT_UPGRADE_REQUIRED")
			if jobError := configurationJobError(test.err); jobError.Code != "AGENT_UPGRADE_REQUIRED" {
				t.Fatalf("job error code = %q, want AGENT_UPGRADE_REQUIRED", jobError.Code)
			}
		})
	}
}

func cloneJSONMap(t *testing.T, value map[string]interface{}) map[string]interface{} {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]interface{}
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, expectedStatus int, expectedCode string) {
	t.Helper()
	if response.Code != expectedStatus || !strings.Contains(response.Body.String(), `"code":"`+expectedCode+`"`) {
		t.Fatalf("status/code = %d, want %d/%s: %s", response.Code, expectedStatus, expectedCode, response.Body.String())
	}
}

func waitForConfigurationJob(t *testing.T, service *jobs.Service, jobID string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(jobID)
		if err == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed || job.Status == jobs.StatusCanceled) {
			if job.Status != jobs.StatusSucceeded {
				t.Fatalf("configuration job failed: %#v", job)
			}
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("configuration job %q did not finish", jobID)
	return jobs.Job{}
}
