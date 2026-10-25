package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/shared"
	"github.com/gin-gonic/gin"
)

type placedMaintenanceRuntime struct{ target string }

func (r *placedMaintenanceRuntime) LuaRuntimeScope(string, string) (string, error) {
	return r.target, nil
}

func (r *placedMaintenanceRuntime) EnsureLuaRuntime(context.Context, string, string, shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
	if r.target == "agent:node-a" {
		return shared.RuntimeLuaResult{State: "missing", Message: "Runtime disabled on node A"}, nil
	}
	return shared.RuntimeLuaResult{Deferred: true}, dstruntime.ErrRuntimeRefreshDeferred
}

type placedRuntimeHealthBridge struct {
	runtimeHandlerHealthBridge
	calls int
}

func (b *placedRuntimeHealthBridge) Health(context.Context, string, string) (dstruntime.Health, error) {
	b.calls++
	return b.health, nil
}

func TestRemoteRuntimePlacementChangeDiscardsPreviousNodeOptOut(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, manager, catalog, process := newRuntimeHandlerTestApp(t)
	manager.statuses = manager.statuses[:1]
	process.running["Cluster_1/Master"] = true
	runtime := &placedMaintenanceRuntime{target: "agent:node-a"}
	updater := dstruntime.NewAutoUpdater(observationMaintenanceCatalog{catalog}, runtime)
	worldID := rooms.EncodeID("Master")
	if _, err := updater.EnsureWorld(context.Background(), catalog.room.ID, worldID, false); err != nil {
		t.Fatal(err)
	}
	// Applied placement changes; node B is healthy but a concurrent operation
	// defers maintenance before a fresh installation observation is returned.
	runtime.target = "agent:node-b"
	if _, err := updater.EnsureWorld(context.Background(), catalog.room.ID, worldID, false); !errors.Is(err, dstruntime.ErrRuntimeRefreshDeferred) {
		t.Fatal(err)
	}
	bridge := &placedRuntimeHealthBridge{runtimeHandlerHealthBridge: runtimeHandlerHealthBridge{local: false, health: dstruntime.Health{ProducerVersion: dstruntime.RuntimeVersion, Running: true, Ready: true, ReadAt: time.Now()}}}
	handler := NewDSTRuntimeHandler(manager, catalog, process, bridge)
	handler.ConfigureUpdater(updater)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/rooms/"+catalog.room.ID+"/runtime", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	item := responseData(t, response)["items"].([]interface{})[0].(map[string]interface{})
	if bridge.calls == 0 || item["state"] == "missing" {
		t.Fatalf("node A opt-out was attributed to node B and suppressed its health read: healthCalls=%d response=%s", bridge.calls, response.Body.String())
	}
}
