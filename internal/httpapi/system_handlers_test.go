package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"dont/internal/systemsettings"
	"dont/internal/systemstatus"

	"github.com/gin-gonic/gin"
)

func TestSystemStatusAndSettingsHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settingsService, err := systemsettings.NewService(systemsettings.NewMemoryRepository())
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewSystemStatusHandler(systemstatus.NewService(systemstatus.NewMemoryProvider())).Register(v2)
	NewSystemSettingsHandler(settingsService).Register(v2)

	response := performJSON(router, http.MethodGet, "/api/v2/system/status", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["host"].(map[string]interface{})["hostname"] != "林火主机" {
		t.Fatalf("unexpected status: %s", response.Body.String())
	}
	if application := responseData(t, response)["application"].(map[string]interface{}); application["version"] == "" || application["commit"] == "" {
		t.Fatalf("system status did not expose build identity: %s", response.Body.String())
	}
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
}
