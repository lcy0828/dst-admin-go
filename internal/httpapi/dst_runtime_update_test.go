package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/shared"
	"github.com/gin-gonic/gin"
)

type handlerRuntimeUpdater struct {
	worlds      []string
	report      *dstruntime.UpdateReport
	reportReads int
}

func (u *handlerRuntimeUpdater) EnsureWorld(_ context.Context, _, world string, _ bool) (shared.RuntimeLuaResult, error) {
	u.worlds = append(u.worlds, world)
	return shared.RuntimeLuaResult{Version: dstruntime.RuntimeVersion, State: "installed", Changed: true, Message: "node runtime updated"}, nil
}
func (u *handlerRuntimeUpdater) Report(string, string) (dstruntime.UpdateReport, bool) {
	u.reportReads++
	if u.report != nil {
		return *u.report, true
	}
	return dstruntime.UpdateReport{State: "updated", Version: dstruntime.RuntimeVersion, InstallationState: "installed", UpdatedAt: time.Now()}, true
}

func TestRuntimeStatusUsesOneUpdateObservationPerWorld(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, manager, catalog, process := newRuntimeHandlerTestApp(t)
	manager.statuses = manager.statuses[:1]
	updater := &handlerRuntimeUpdater{}
	handler := NewDSTRuntimeHandler(manager, catalog, process, &runtimeHandlerHealthBridge{local: false})
	handler.ConfigureUpdater(updater)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/rooms/"+catalog.room.ID+"/runtime", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if updater.reportReads != 1 {
		t.Fatalf("status mixed %d update observations for one world", updater.reportReads)
	}
}

func TestRuntimeInstallationUsesNodeUpdaterForRemoteWorld(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, manager, catalog, process := newRuntimeHandlerTestApp(t)
	updater := &handlerRuntimeUpdater{}
	bridge := &runtimeHandlerHealthBridge{local: false}
	handler := NewDSTRuntimeHandler(manager, catalog, process, bridge)
	handler.ConfigureUpdater(updater)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	worldID := rooms.EncodeID("Master")
	response := performJSON(router, http.MethodPost, "/api/v2/rooms/"+catalog.room.ID+"/worlds/"+worldID+"/runtime/actions/install", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if manager.installCalls != 0 || len(updater.worlds) != 1 || updater.worlds[0] != worldID {
		t.Fatalf("remote install used controller files: calls=%d worlds=%v", manager.installCalls, updater.worlds)
	}
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/"+catalog.room.ID+"/runtime", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	items := responseData(t, response)["items"].([]interface{})
	for _, item := range items {
		report := item.(map[string]interface{})
		if report["state"] != "installed" || report["healthState"] != "stopped" || report["autoUpdate"] == nil {
			t.Fatalf("stopped Agent installation was reported missing: %s", response.Body.String())
		}
	}
}

func TestRemoteRuntimeStatusPreservesNodeInstallationState(t *testing.T) {
	for _, state := range []dstruntime.InstallState{dstruntime.InstallStateMissing, dstruntime.InstallStateOutdated, dstruntime.InstallStateInvalid} {
		t.Run(string(state), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			_, manager, catalog, process := newRuntimeHandlerTestApp(t)
			manager.statuses = manager.statuses[:1]
			process.running["Cluster_1/Master"] = true
			updater := &handlerRuntimeUpdater{report: &dstruntime.UpdateReport{
				State: "disabled", InstallationState: string(state), Message: "node installation state", UpdatedAt: time.Now(),
			}}
			// A retained health file (or same-named local installation) must not
			// override the installation state observed on the actual Agent.
			bridge := &runtimeHandlerHealthBridge{local: false, health: dstruntime.Health{
				ProducerVersion: dstruntime.RuntimeVersion, Running: true, Ready: true, ReadAt: time.Now(),
			}}
			handler := NewDSTRuntimeHandler(manager, catalog, process, bridge)
			handler.ConfigureUpdater(updater)
			router := gin.New()
			handler.Register(router.Group("/api/v2"))
			response := performJSON(router, http.MethodGet, "/api/v2/rooms/"+catalog.room.ID+"/runtime", nil, nil, "")
			assertStatus(t, response, http.StatusOK)
			items := responseData(t, response)["items"].([]interface{})
			report := items[0].(map[string]interface{})
			if report["state"] != string(state) || report["healthState"] == "ready" || report["health"] != nil {
				t.Fatalf("retained health overrode node installation state: %s", response.Body.String())
			}
		})
	}
}

func TestRuntimeDestructiveActionsRejectUnresolvedProcesses(t *testing.T) {
	for _, action := range []string{"uninstall", "rollback"} {
		for _, test := range []struct {
			name, code string
			status     shards.RuntimeStatus
		}{
			{"unmanaged process", "RUNTIME_PROCESS_CONFLICT", shards.RuntimeStatus{State: shards.RuntimeFailed, Code: "UNMANAGED_DST_PROCESS_CONFLICT", SessionExists: true}},
			{"legacy session", "RUNTIME_PROCESS_CONFLICT", shards.RuntimeStatus{State: shards.RuntimeFailed, Code: "LEGACY_TMUX_SOCKET_CONFLICT", SessionExists: true}},
			{"stopped session remains", "RUNTIME_PROCESS_CONFLICT", shards.RuntimeStatus{State: shards.RuntimeStopped, SessionExists: true}},
			{"starting", "RUNTIME_UNAVAILABLE", shards.RuntimeStatus{State: shards.RuntimeStarting, SessionExists: true}},
			{"unknown", "RUNTIME_UNAVAILABLE", shards.RuntimeStatus{State: shards.RuntimeUnknown}},
		} {
			t.Run(action+"/"+test.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				_, manager, catalog, process := newRuntimeHandlerTestApp(t)
				handler := NewDSTRuntimeHandler(manager, catalog, runtimeHandlerIdentifiedProcess{runtimeHandlerProcess: process, status: test.status})
				router := gin.New()
				handler.Register(router.Group("/api/v2"))
				method, path := http.MethodDelete, "/api/v2/rooms/"+catalog.room.ID+"/worlds/"+rooms.EncodeID("Master")+"/runtime"
				if action == "rollback" {
					method, path = http.MethodPost, path+"/actions/rollback"
				}
				response := performJSON(router, method, path, map[string]string{"confirmation": catalog.room.Name, "backupId": "backup"}, nil, "")
				assertAPIError(t, response, http.StatusConflict, test.code)
				if manager.uninstallCalls != 0 || manager.rollbackCalls != 0 {
					t.Fatalf("unsafe Runtime mutation reached the manager: uninstall=%d rollback=%d", manager.uninstallCalls, manager.rollbackCalls)
				}
			})
		}
	}
}
