package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/shared"

	"github.com/gin-gonic/gin"
)

func TestRemoteRuntimePreservesInstalledVersionAgainstLegacyHealth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, manager, catalog, process := newRuntimeHandlerTestApp(t)
	manager.statuses = manager.statuses[:1]
	process.running["Cluster_1/Master"] = true
	updater := &handlerRuntimeUpdater{report: &dstruntime.UpdateReport{State: "updated", Version: dstruntime.RuntimeVersion, InstallationState: "installed", UpdatedAt: time.Now()}}
	bridge := &runtimeHandlerHealthBridge{local: false, health: dstruntime.Health{ProducerVersion: "2.4.11", Running: true, Ready: true, ReadAt: time.Now()}}
	handler := NewDSTRuntimeHandler(manager, catalog, process, bridge)
	handler.ConfigureUpdater(updater)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/rooms/"+catalog.room.ID+"/runtime", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	item := responseData(t, response)["items"].([]interface{})[0].(map[string]interface{})
	if item["version"] != dstruntime.RuntimeVersion || item["healthState"] != "degraded" || item["health"].(map[string]interface{})["producerVersion"] != "2.4.11" {
		t.Fatalf("old health overwrote installed version and hid activation mismatch: %s", response.Body.String())
	}
}

type observationMaintenanceCatalog struct{ runtimeHandlerCatalog }

func (c observationMaintenanceCatalog) List() ([]rooms.Room, error) { return []rooms.Room{c.room}, nil }
func (c observationMaintenanceCatalog) Worlds(string) ([]rooms.World, error) {
	var result []rooms.World
	for _, w := range c.worlds {
		result = append(result, w)
	}
	return result, nil
}

type observationDisabledRuntime struct{ checked bool }

func (r *observationDisabledRuntime) EnsureLuaRuntime(context.Context, string, string, shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
	if !r.checked {
		r.checked = true
		return shared.RuntimeLuaResult{State: "missing", Message: "Runtime disabled on Agent"}, nil
	}
	return shared.RuntimeLuaResult{Deferred: true}, dstruntime.ErrRuntimeRefreshDeferred
}
func TestBusyRefreshCannotReviveDisabledRemoteInstallation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, manager, catalog, process := newRuntimeHandlerTestApp(t)
	manager.statuses = manager.statuses[:1]
	process.running["Cluster_1/Master"] = true
	updater := dstruntime.NewAutoUpdater(observationMaintenanceCatalog{catalog}, &observationDisabledRuntime{})
	worldID := rooms.EncodeID("Master")
	if _, err := updater.EnsureWorld(context.Background(), catalog.room.ID, worldID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := updater.EnsureWorld(context.Background(), catalog.room.ID, worldID, false); err == nil {
		t.Fatal("expected room busy")
	}
	bridge := &runtimeHandlerHealthBridge{local: false, health: dstruntime.Health{ProducerVersion: "2.4.11", Running: true, Ready: true, ReadAt: time.Now()}}
	handler := NewDSTRuntimeHandler(manager, catalog, process, bridge)
	handler.ConfigureUpdater(updater)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/rooms/"+catalog.room.ID+"/runtime", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	item := responseData(t, response)["items"].([]interface{})[0].(map[string]interface{})
	if item["state"] != "missing" || item["healthState"] == "ready" || item["health"] != nil || item["autoUpdate"].(map[string]interface{})["state"] == "failed" {
		t.Fatalf("busy observation revived disabled Agent installation from retained health: %s", response.Body.String())
	}
}
