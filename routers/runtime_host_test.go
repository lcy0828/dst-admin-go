package routers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/systemsettings"
	"dont/models"
	"dont/pkg/setting"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"github.com/google/uuid"
)

func TestRuntimeHostAppliesPathsAndRolesAndRestoresFailedConfiguration(t *testing.T) {
	configureRouterTestEnvironment(t)
	t.Setenv("DST_ADMIN_TEST_SYSTEM_SETTINGS", "")
	// The Runtime uses a Unix socket under saves. t.TempDir includes the test
	// name, which exceeds the socket path limit for this integration test.
	root, err := os.MkdirTemp("/tmp", "dst-rt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	var original bytes.Buffer
	_, _ = setting.Cfg.WriteTo(&original)
	file, err := ini.Load(original.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	saves := filepath.Join(root, "saves")
	if err := os.MkdirAll(saves, 0700); err != nil {
		t.Fatal(err)
	}
	file.Section("paths").Key("DST_SAVE_PATH").SetValue(saves)
	file.Section("fleet").Key("LOCAL_EXECUTOR_ENABLED").SetValue("true")
	file.Section("fleet").Key("CONTROLLER_ENABLED").SetValue("true")
	file.Section("fleet").Key("MEMBER_ENABLED").SetValue("false")
	file.Section("operator_custom").Key("keep").SetValue("untouched")
	path := filepath.Join(root, "app.conf")
	if err := file.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DST_ADMIN_SAVE_PATH", "")
	config := setting.CurrentSnapshot()
	config.ConfigPath = path
	config, err = config.Reload()
	if err != nil {
		t.Fatal(err)
	}
	build := func(c setting.Snapshot) (*Application, error) { return initApplicationConfig(true, false, c) }
	app, err := build(config)
	if err != nil {
		t.Fatal(err)
	}
	host := &RuntimeHost{app: app, active: make(map[*requestTicket]struct{}), changed: make(chan struct{}, 1), build: build}
	app.settings.SetRuntimeApplier(host.apply)
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	db := models.DB()
	if err := db.Exec("DELETE FROM dont_admin_session").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DELETE FROM dont_auth").Error; err != nil {
		t.Fatal(err)
	}
	response := request(host, "POST", "/api/v2/auth/setup", map[string]string{"username": "runtime-test", "password": "Runtime123!"}, nil, "")
	if response.Code != 201 {
		t.Fatalf("setup: %d %s", response.Code, response.Body)
	}
	cookie := response.Result().Cookies()[0]
	var auth struct {
		Data struct {
			CSRF string `json:"csrfToken"`
		}
	}
	_ = json.Unmarshal(response.Body.Bytes(), &auth)
	getSettings := func() systemsettings.Settings {
		t.Helper()
		response := request(host, "GET", "/api/v2/system/settings", nil, cookie, "")
		var value struct{ Data systemsettings.Settings }
		_ = json.Unmarshal(response.Body.Bytes(), &value)
		if response.Code != 200 {
			t.Fatalf("settings: %d %s", response.Code, response.Body)
		}
		return value.Data
	}
	apply := func(values map[string]string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(systemsettings.Input{Revision: getSettings().Revision, Values: values, Confirmation: systemsettings.ApplyConfirmation})
		req := httptest.NewRequest("POST", "/api/v2/system/settings/actions/apply", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req.Header.Set("X-CSRF-Token", auth.Data.CSRF)
		req.AddCookie(cookie)
		response := httptest.NewRecorder()
		host.ServeHTTP(response, req)
		return response
	}
	sentinel := filepath.Join(saves, "existing-save")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	nextSaves := filepath.Join(root, "new-saves")
	response = apply(map[string]string{"paths.save": nextSaves, "fleet.controllerEnabled": "false"})
	if response.Code != 200 {
		t.Fatalf("apply: %d %s", response.Code, response.Body)
	}
	if getSettings().RestartRequired {
		t.Fatal("runtime still requires restart")
	}
	if models.DB() != db || host.app == app {
		t.Fatal("database or application generation lifetime is incorrect")
	}
	response = request(host, "GET", "/api/v2/system/capabilities", nil, cookie, "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"role":"standalone"`) || !strings.Contains(response.Body.String(), nextSaves) {
		t.Fatalf("new config not effective: %s", response.Body)
	}
	response = request(host, "GET", "/api/v2/auth/session", nil, cookie, "")
	if !strings.Contains(response.Body.String(), `"authenticated":true`) || !strings.Contains(response.Body.String(), `"required":true`) {
		t.Fatalf("session/onboarding lost: %s", response.Body)
	}
	badPath := filepath.Join(root, "not-a-directory")
	_ = os.WriteFile(badPath, []byte("keep"), 0600)
	response = apply(map[string]string{"paths.save": badPath})
	if response.Code != 500 {
		t.Fatalf("invalid runtime accepted: %d %s", response.Code, response.Body)
	}
	response = request(host, "GET", "/api/v2/system/capabilities", nil, cookie, "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), nextSaves) {
		t.Fatalf("rollback failed: %d %s", response.Code, response.Body)
	}
	response = apply(map[string]string{"paths.save": saves, "fleet.controllerEnabled": "true"})
	if response.Code != 200 {
		t.Fatalf("second reload: %d %s", response.Code, response.Body)
	}
	if data, _ := os.ReadFile(sentinel); string(data) != "preserve" {
		t.Fatal("existing saves changed")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "untouched") {
		t.Fatal("unknown configuration removed")
	}
}

func TestRuntimeBarrierDrainsReadStreamsAndPreservesInflightWrites(t *testing.T) {
	streamEntered, streamLeft := make(chan struct{}), make(chan struct{})
	writeEntered, finishWrite := make(chan struct{}), make(chan struct{})
	engine := gin.New()
	engine.GET("/stream", func(c *gin.Context) { close(streamEntered); <-c.Request.Context().Done(); close(streamLeft) })
	engine.POST("/write", func(c *gin.Context) { close(writeEntered); <-finishWrite; c.Status(204) })
	host := &RuntimeHost{app: newApplication(engine, applicationHooks{}), active: make(map[*requestTicket]struct{}), changed: make(chan struct{}, 1)}
	barrierEntered, barrierDone := make(chan struct{}), make(chan struct{})
	engine.POST("/apply", func(c *gin.Context) {
		close(barrierEntered)
		release, err := host.barrier(c.Request.Context())
		if err != nil {
			t.Error(err)
			return
		}
		defer release()
		close(barrierDone)
	})
	streamDone, writeDone, applyDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		host.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/stream", nil))
		close(streamDone)
	}()
	<-streamEntered
	go func() {
		host.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/write", nil))
		close(writeDone)
	}()
	<-writeEntered
	go func() {
		host.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/apply", nil))
		close(applyDone)
	}()
	<-barrierEntered
	select {
	case <-streamLeft:
	case <-time.After(time.Second):
		t.Fatal("read stream did not drain")
	}
	select {
	case <-barrierDone:
		t.Fatal("barrier passed an active write")
	default:
	}
	response := httptest.NewRecorder()
	host.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if response.Code != 503 {
		t.Fatalf("admitted during reload: %d", response.Code)
	}
	close(finishWrite)
	select {
	case <-applyDone:
	case <-time.After(time.Second):
		t.Fatal("barrier did not release")
	}
	<-writeDone
	<-streamDone
}
