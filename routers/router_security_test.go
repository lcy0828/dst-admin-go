package routers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dont/internal/authn"
	"dont/models"
)

func TestRouterPublishesOnlyV2AndMigratesAdmin(t *testing.T) {
	configureRouterTestEnvironment(t)
	db := models.DB()
	if err := db.DropTableIfExists("dont_admin_session", "dont_auth").Error; err != nil {
		t.Fatalf("reset auth tables: %v", err)
	}
	if err := db.Table("dont_auth").CreateTable(&authn.Admin{}).Error; err != nil {
		t.Fatalf("create legacy auth table: %v", err)
	}
	legacy := authn.Admin{ID: 1, Username: "lcy", Password: "001008"}
	if err := db.Table("dont_auth").Create(&legacy).Error; err != nil {
		t.Fatalf("insert legacy admin: %v", err)
	}

	router, err := InitRouter()
	if err != nil {
		t.Fatalf("initialize router: %v", err)
	}

	response := request(router, http.MethodGet, "/api/v2/rooms", nil, nil, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated v2 status = %d, want 401", response.Code)
	}

	response = request(router, http.MethodPost, "/api/v2/auth/login", map[string]string{
		"username": "lcy", "password": "001008",
	}, nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}
	var migrated authn.Admin
	if err := db.Table("dont_auth").First(&migrated, 1).Error; err != nil {
		t.Fatalf("load migrated admin: %v", err)
	}
	if !strings.HasPrefix(migrated.Password, "$2") {
		t.Fatalf("legacy password was not migrated: %q", migrated.Password)
	}

	response = request(router, http.MethodGet, "/api/v2/system/capabilities", nil, cookies[0], "")
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated capabilities status = %d: %s", response.Code, response.Body.String())
	}
	response = request(router, http.MethodPost, "/api/v2/rooms", map[string]string{}, cookies[0], "")
	if response.Code != http.StatusForbidden {
		t.Fatalf("write without CSRF status = %d, want 403: %s", response.Code, response.Body.String())
	}

	legacyRoutes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/gamelog"},
		{http.MethodGet, "/static/gamelog.html"},
		{http.MethodGet, "/api/dashboard/"},
		{http.MethodGet, "/api/mod/local"},
		{http.MethodDelete, "/api/mod/local"},
		{http.MethodPost, "/api/tmux/raw-command"},
		{http.MethodPost, "/api/cron/tmux/test-raw-command"},
		{http.MethodPost, "/api/agent/command"},
	}
	for _, route := range legacyRoutes {
		response = request(router, route.method, route.path, nil, cookies[0], "")
		if response.Code != http.StatusNotFound {
			t.Errorf("legacy route %s %s status = %d, want 404: %s", route.method, route.path, response.Code, response.Body.String())
		}
		if response.Header().Get("X-Request-ID") == "" {
			t.Errorf("legacy route %s %s did not return X-Request-ID", route.method, route.path)
		}
	}
}

func TestMemoryAdaptersAreRejectedOutsideTestEnvironment(t *testing.T) {
	for _, adapterName := range testAdapterEnvironmentVariables {
		t.Run(adapterName, func(t *testing.T) {
			clearTestAdapterEnvironment(t)
			t.Setenv("DST_ADMIN_ENV", "production")
			t.Setenv(adapterName, "memory")
			if err := validateTestAdapters(); err == nil || !strings.Contains(err.Error(), adapterName) {
				t.Fatalf("validate %s error = %v, want adapter-specific rejection", adapterName, err)
			}
		})
	}
}

func TestMemoryAdaptersRequireTheSupportedDriver(t *testing.T) {
	clearTestAdapterEnvironment(t)
	t.Setenv("DST_ADMIN_ENV", "test")
	t.Setenv("DST_ADMIN_TEST_CONTROL", "tmux")
	if err := validateTestAdapters(); err == nil || !strings.Contains(err.Error(), "DST_ADMIN_TEST_CONTROL") {
		t.Fatalf("unsupported test driver error = %v, want rejection", err)
	}
}

func TestMemoryAdaptersAreAcceptedOnlyInExplicitTestEnvironment(t *testing.T) {
	clearTestAdapterEnvironment(t)
	t.Setenv("DST_ADMIN_ENV", "test")
	for _, adapterName := range testAdapterEnvironmentVariables {
		t.Setenv(adapterName, "memory")
	}
	if err := validateTestAdapters(); err != nil {
		t.Fatalf("validate explicit test adapters: %v", err)
	}
}

func configureRouterTestEnvironment(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	paths := map[string]string{
		"DST_ADMIN_SAVE_PATH":         filepath.Join(root, "saves"),
		"DST_ADMIN_BACKUP_PATH":       filepath.Join(root, "backups"),
		"DST_ADMIN_SERVER_PATH":       filepath.Join(root, "server", "dontstarve_dedicated_server_nullrenderer"),
		"DST_ADMIN_UGC_PATH":          filepath.Join(root, "ugc_mods"),
		"DST_ADMIN_WORKSHOP_CONTENT":  filepath.Join(root, "workshop", "content", "322330"),
		"DST_ADMIN_WORKSHOP_DOWNLOAD": filepath.Join(root, "workshop"),
		"DST_ADMIN_STEAMCMD_PATH":     filepath.Join(root, "steamcmd"),
		"DST_ADMIN_MAP_PATH":          filepath.Join(root, "maps"),
		"DST_ADMIN_MAP_RENDERER_PATH": filepath.Join(root, "render-map"),
		"DST_ADMIN_LUA_PATH":          filepath.Join(root, "lua-fallback"),
	}
	for name, path := range paths {
		t.Setenv(name, path)
	}
	for _, path := range []string{paths["DST_ADMIN_SAVE_PATH"], paths["DST_ADMIN_BACKUP_PATH"], paths["DST_ADMIN_UGC_PATH"], paths["DST_ADMIN_WORKSHOP_CONTENT"], paths["DST_ADMIN_MAP_PATH"]} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create router test path %s: %v", path, err)
		}
	}
	t.Setenv("DST_ADMIN_ENV", "test")
	for _, name := range []string{
		"DST_ADMIN_TEST_AGENTS", "DST_ADMIN_TEST_SYSTEM_STATUS", "DST_ADMIN_TEST_SYSTEM_SETTINGS",
		"DST_ADMIN_TEST_CONTAINERS", "DST_ADMIN_TEST_CONTROL", "DST_ADMIN_TEST_MODS",
		"DST_ADMIN_TEST_PLAYERS", "DST_ADMIN_TEST_WORLD_STATE", "DST_ADMIN_TEST_UPDATE", "DST_ADMIN_TEST_MAP",
	} {
		t.Setenv(name, "memory")
	}
}

func clearTestAdapterEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range testAdapterEnvironmentVariables {
		t.Setenv(name, "")
	}
}

func request(handler http.Handler, method, path string, body interface{}, cookie *http.Cookie, csrfToken string) *httptest.ResponseRecorder {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Host = "dst-admin.test"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrfToken != "" {
		req.Header.Set("X-CSRF-Token", csrfToken)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}
