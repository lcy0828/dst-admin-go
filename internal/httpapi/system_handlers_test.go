package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"dont/internal/agents"
	"dont/internal/systemsettings"
	"dont/internal/systemstatus"

	"github.com/gin-gonic/gin"
)

type emptySystemAgentCatalog struct{}

func (emptySystemAgentCatalog) Agents() ([]agents.Agent, bool, error) {
	return []agents.Agent{}, true, nil
}
func (emptySystemAgentCatalog) Agent(string) (agents.Agent, error) {
	return agents.Agent{}, agents.ErrAgentNotFound
}
func (emptySystemAgentCatalog) RefreshSystemInfo(context.Context, string) (agents.Agent, error) {
	return agents.Agent{}, agents.ErrAgentNotFound
}

func TestSystemStatusAndSettingsHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settingsService, err := systemsettings.NewService(systemsettings.NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	v2 := router.Group("/api/v2")
	statusService := systemstatus.NewService(systemstatus.NewMemoryProvider())
	nodeResources, err := systemstatus.NewNodeResourceService(statusService, emptySystemAgentCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	NewSystemStatusHandler(statusService, nodeResources).Register(v2)
	NewSystemSettingsHandler(settingsService).Register(v2)

	response := performJSON(router, http.MethodGet, "/api/v2/system/status", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["host"].(map[string]interface{})["hostname"] != "林火主机" {
		t.Fatalf("unexpected status: %s", response.Body.String())
	}
	if application := responseData(t, response)["application"].(map[string]interface{}); application["version"] == "" || application["commit"] == "" {
		t.Fatalf("system status did not expose build identity: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodGet, "/api/v2/system/resources?targetId=local", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	resources := responseData(t, response)
	if resources["total"] != float64(1) || resources["items"].([]interface{})[0].(map[string]interface{})["targetId"] != "local" {
		t.Fatalf("unexpected node resources: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodGet, "/api/v2/system/resources?targetId=local&refresh=true", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("live resources must not be cached by the browser")
	}
	response = performJSON(router, http.MethodGet, "/api/v2/system/resources?refresh=invalid", nil, nil, "")
	assertAPIError(t, response, http.StatusBadRequest, "INVALID_REFRESH")
	response = performJSON(router, http.MethodGet, "/api/v2/system/resources?targetId=agent:missing", nil, nil, "")
	assertAPIError(t, response, http.StatusNotFound, "NODE_RESOURCE_NOT_FOUND")
	response = performJSON(router, http.MethodGet, "/api/v2/system/settings", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "test-steam-api-key") {
		t.Fatalf("settings leaked secret: %s", response.Body.String())
	}
	settings := responseData(t, response)
	revision := settings["revision"].(string)
	response = performJSON(router, http.MethodPost, "/api/v2/system/settings/preview", map[string]interface{}{"revision": revision, "values": map[string]string{"misc.logLevel": "debug"}, "clearSecrets": []string{}}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["valid"] != true {
		t.Fatalf("unexpected preview: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/system/settings/actions/apply", map[string]interface{}{"revision": revision, "values": map[string]string{"misc.logLevel": "debug"}, "clearSecrets": []string{}, "confirmation": "wrong"}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = performJSON(router, http.MethodPost, "/api/v2/system/settings/actions/apply", map[string]interface{}{"revision": revision, "values": map[string]string{"misc.logLevel": "debug"}, "clearSecrets": []string{}, "confirmation": systemsettings.ApplyConfirmation}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), `"restartRequired":true`) || strings.Contains(response.Body.String(), "test-steam-api-key") {
		t.Fatalf("unexpected apply: %s", response.Body.String())
	}

	settings = responseData(t, response)["settings"].(map[string]interface{})
	revision = settings["revision"].(string)
	response = performJSON(router, http.MethodPost, "/api/v2/system/settings/actions/apply", map[string]interface{}{
		"revision": revision, "values": map[string]string{"security.ipWhitelist": "127.0.0.1"},
		"clearSecrets": []string{}, "confirmation": systemsettings.ApplyConfirmation,
	}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "IP_WHITELIST_LOCKOUT")

	response = performJSON(router, http.MethodPost, "/api/v2/system/settings/actions/test-email", map[string]interface{}{
		"server": "127.0.0.1", "port": 1, "username": "admin",
	}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "SMTP_TEST_FAILED")
	if !strings.Contains(response.Body.String(), `"name":"connect"`) || !strings.Contains(response.Body.String(), `"status":"failed"`) {
		t.Fatalf("SMTP failure did not expose its completed stages: %s", response.Body.String())
	}
}
