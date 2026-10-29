package routers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/buildinfo"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/internal/softwareupdate"
	"dont/internal/systemsettings"
	"dont/models"
	"dont/pkg/setting"
	"dont/shared"
	dsttmux "dont/tmux"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"github.com/google/uuid"
)

type blockedRuntimeSoftwareDownload struct{}

func (blockedRuntimeSoftwareDownload) Latest(ctx context.Context, _, _ string) (*softwareupdate.Release, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockedRuntimeSoftwareDownload) Checksum(context.Context, softwareupdate.Asset, string) ([]byte, error) {
	return nil, nil
}
func (blockedRuntimeSoftwareDownload) Download(context.Context, softwareupdate.Asset, string, io.Writer) error {
	return nil
}

func TestRuntimeConfigurationApplyDoesNotCancelSoftwareDownload(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "releases"), 0700); err != nil {
		t.Fatal(err)
	}
	software := softwareupdate.New(softwareupdate.Config{Root: root, Current: buildinfo.Info{Version: "v1.0.0"}, Platform: "linux-amd64", Managed: true, Ready: func() bool { return true }, Client: blockedRuntimeSoftwareDownload{}})
	t.Cleanup(func() { _ = software.Close(context.Background()) })
	if _, err := software.Start("v1.1.0", "auto"); err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	app := newApplication(engine, applicationHooks{})
	app.software = software
	app.prepareReload = func(context.Context, []string) (func(), error) {
		t.Error("configuration reload proceeded during software download")
		return func() {}, nil
	}
	host := &RuntimeHost{app: app, active: make(map[*requestTicket]struct{}), changed: make(chan struct{}, 1)}
	persisted := false
	engine.POST("/apply", func(c *gin.Context) {
		err := host.apply(c.Request.Context(), []string{"paths.save"}, func() error { persisted = true; return nil }, func() error { return nil })
		if err == systemsettings.ErrRuntimeBusy {
			c.Status(409)
		} else {
			t.Errorf("unexpected apply result: %v", err)
			c.Status(500)
		}
	})
	response := httptest.NewRecorder()
	host.ServeHTTP(response, httptest.NewRequest("POST", "/apply", nil))
	status, err := software.Snapshot()
	if response.Code != 409 || persisted || host.app != app || err != nil || status.Operation.Phase != "downloading" {
		t.Fatalf("reload canceled or replaced updater: HTTP %d persisted=%v status=%+v err=%v", response.Code, persisted, status, err)
	}
}

func TestRuntimeHostAppliesPathsAndRolesAndRestoresFailedConfiguration(t *testing.T) {
	configureRouterTestEnvironment(t)
	t.Setenv("DST_ADMIN_TEST_SYSTEM_SETTINGS", "")
	t.Setenv("DST_ADMIN_TEST_CONTROL", "")
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
	t.Run("role switches preserve live tmux sessions", func(t *testing.T) {
		if _, err := exec.LookPath("tmux"); err != nil {
			t.Skip("tmux unavailable")
		}
		roomPath := filepath.Join(nextSaves, "live-room")
		if err := os.MkdirAll(roomPath, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(roomPath, "cluster.ini"), []byte("[NETWORK]\ncluster_name = Online switch\n"), 0600); err != nil {
			t.Fatal(err)
		}
		socket, err := shards.NativeConsoleSocketPath(nextSaves)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
		identities := map[string]string{}
		identity := func(world string) string {
			t.Helper()
			output, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "-t", "="+dsttmux.V2SessionName("live-room", world)+":0.0", "#{session_id}|#{session_created}|#{pane_pid}|#{pane_dead}").CombinedOutput()
			if err != nil {
				t.Fatalf("inspect %s: %s %v", world, output, err)
			}
			return string(output)
		}
		for _, world := range []string{"Master", "Caves"} {
			worldPath := filepath.Join(roomPath, world)
			if err := os.MkdirAll(worldPath, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worldPath, "server.ini"), []byte("[NETWORK]\nserver_port = 11000\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("tmux", "-S", socket, "new-session", "-d", "-s", dsttmux.V2SessionName("live-room", world), "/bin/sleep", "180").CombinedOutput(); err != nil {
				t.Fatalf("create %s: %s %v", world, output, err)
			}
			identities[world] = identity(world)
		}
		// Combining the gateway switch with a path change must still be refused.
		blocked := apply(map[string]string{"fleet.controllerEnabled": "true", "paths.save": filepath.Join(root, "blocked-saves")})
		if blocked.Code != 409 || !strings.Contains(blocked.Body.String(), "SYSTEM_SETTINGS_WORLDS_RUNNING") {
			t.Fatalf("path change bypassed live-world guard: %d %s", blocked.Code, blocked.Body)
		}
		for _, enabled := range []string{"true", "false"} {
			response := apply(map[string]string{"fleet.localExecutorEnabled": "true", "fleet.controllerEnabled": enabled, "fleet.memberEnabled": "false"})
			if response.Code != 200 {
				t.Fatalf("online role switch %s: %d %s", enabled, response.Code, response.Body)
			}
			for world, before := range identities {
				if after := identity(world); before != after {
					t.Fatalf("%s was replaced: before=%q after=%q", world, before, after)
				}
			}
		}
		// Offline remote rooms still depend on this Controller. Refusal must
		// preserve the config and generation, then release job admission so a
		// later switch can succeed once the dependency has been removed.
		if response := apply(map[string]string{"fleet.controllerEnabled": "true"}); response.Code != 200 {
			t.Fatalf("enable Controller: %d %s", response.Code, response.Body)
		}
		catalog, err := rooms.NewCatalog(nextSaves, nil)
		if err != nil {
			t.Fatal(err)
		}
		store := rooms.NewStore(db, file.Section("database").Key("TABLE_PREFIX").String())
		catalogService := rooms.NewService(catalog, store)
		remoteSource := rooms.RuntimeCatalogSource{
			TargetID: "agent:offline", Available: true, Stale: true,
			Inventory: shared.RuntimeInventoryReport{Rooms: []shared.RoomInventoryReport{{
				Directory: "remote-room", Name: "Offline room", Shards: []shared.ShardInventoryReport{{Directory: "Master"}},
			}}},
		}
		if err := catalogService.SyncRuntimeCatalog([]rooms.RuntimeCatalogSource{remoteSource}); err != nil {
			t.Fatal(err)
		}
		before, generation := getSettings(), host.app
		blocked = apply(map[string]string{"fleet.controllerEnabled": "false"})
		if blocked.Code != 409 || !strings.Contains(blocked.Body.String(), "SYSTEM_SETTINGS_CONTROLLER_IN_USE") || !strings.Contains(blocked.Body.String(), "Offline room") {
			t.Fatalf("remote dependency was ignored: %d %s", blocked.Code, blocked.Body)
		}
		if getSettings().Revision != before.Revision || host.app != generation {
			t.Fatal("refused switch changed configuration or replaced the runtime")
		}
		remoteSource.Online, remoteSource.Stale, remoteSource.Inventory.Rooms = true, false, nil
		if err := catalogService.SyncRuntimeCatalog([]rooms.RuntimeCatalogSource{remoteSource}); err != nil {
			t.Fatal(err)
		}
		if response := apply(map[string]string{"fleet.controllerEnabled": "false"}); response.Code != 200 {
			t.Fatalf("switch after removing dependency: %d %s", response.Code, response.Body)
		}
		for world, before := range identities {
			if identity(world) != before {
				t.Fatalf("%s was replaced while checking remote dependencies", world)
			}
		}
	})
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

func TestSoftwareRestartUsesIdleJobGuardWithoutRequiringWorldsStopped(t *testing.T) {
	engine := gin.New()
	app := newApplication(engine, applicationHooks{})
	// Settings reload's guard represents a live room: software restart must
	// never call it. Running rooms outlive the management process.
	app.prepareReload = func(context.Context, []string) (func(), error) {
		t.Error("software restart attempted to stop/reload world runtime")
		return nil, systemsettings.ErrRuntimeBusy
	}
	busy, resumed := true, false
	app.prepareSoftwareRestart = func(context.Context) (func(), error) {
		if busy {
			return nil, systemsettings.ErrRuntimeBusy
		}
		return func() { resumed = true }, nil
	}
	host := &RuntimeHost{app: app, active: make(map[*requestTicket]struct{}), changed: make(chan struct{}, 1)}
	engine.GET("/ready", func(c *gin.Context) { c.Status(200) })
	engine.POST("/apply", func(c *gin.Context) {
		resume, err := host.prepareSoftwareRestart(c.Request.Context())
		if err != nil {
			c.Status(409)
			return
		}
		resume()
		c.Status(202)
	})
	response := request(host, "POST", "/apply", nil, nil, "")
	if response.Code != 409 {
		t.Fatal(response.Code)
	}
	if response = request(host, "GET", "/ready", nil, nil, ""); response.Code != 200 {
		t.Fatal("busy job left admission closed")
	}
	busy = false
	if response = request(host, "POST", "/apply", nil, nil, ""); response.Code != 202 || !resumed {
		t.Fatal("idle software restart denied", response.Code)
	}
	if response = request(host, "GET", "/ready", nil, nil, ""); response.Code != 200 {
		t.Fatal("resume left admission closed")
	}
}
