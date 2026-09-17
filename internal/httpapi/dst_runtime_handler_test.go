package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/shards"

	"github.com/gin-gonic/gin"
)

type runtimeHandlerManager struct {
	statuses       []dstruntime.WorldStatus
	health         map[string]dstruntime.Health
	healthErrors   map[string]error
	installCalls   int
	rollbackCalls  int
	uninstallCalls int
}

func (f *runtimeHandlerManager) StatusRoom(string) ([]dstruntime.WorldStatus, error) {
	return append([]dstruntime.WorldStatus(nil), f.statuses...), nil
}

func (f *runtimeHandlerManager) Health(_, worldID string) (dstruntime.Health, error) {
	if err := f.healthErrors[worldID]; err != nil {
		return dstruntime.Health{}, err
	}
	return f.health[worldID], nil
}

func (f *runtimeHandlerManager) InstallRoom(context.Context, string) ([]dstruntime.WorldStatus, error) {
	f.installCalls++
	return append([]dstruntime.WorldStatus(nil), f.statuses...), nil
}

func (f *runtimeHandlerManager) InstallWorld(context.Context, string, string) (dstruntime.WorldStatus, error) {
	return dstruntime.WorldStatus{}, nil
}

func (f *runtimeHandlerManager) UninstallWorld(context.Context, string, string) (dstruntime.WorldStatus, error) {
	f.uninstallCalls++
	return dstruntime.WorldStatus{State: dstruntime.InstallStateMissing}, nil
}

func (f *runtimeHandlerManager) Backups(string, string) ([]dstruntime.Backup, error) {
	return []dstruntime.Backup{}, nil
}

func (f *runtimeHandlerManager) RollbackWorld(_ context.Context, _, _, backupID string) (dstruntime.WorldStatus, error) {
	f.rollbackCalls++
	if backupID == "invalid" {
		return dstruntime.WorldStatus{}, dstruntime.ErrUnsafeRuntimePath
	}
	return dstruntime.WorldStatus{State: dstruntime.InstallStateInstalled}, nil
}

type runtimeHandlerCatalog struct {
	room   rooms.Room
	worlds map[string]rooms.World
}

func (f runtimeHandlerCatalog) Room(roomID string) (rooms.Room, error) {
	if roomID != f.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return f.room, nil
}

func (f runtimeHandlerCatalog) World(roomID, worldID string) (rooms.World, error) {
	if roomID != f.room.ID {
		return rooms.World{}, rooms.ErrRoomNotFound
	}
	world, ok := f.worlds[worldID]
	if !ok {
		return rooms.World{}, rooms.ErrWorldNotFound
	}
	return world, nil
}

type runtimeHandlerProcess struct {
	running map[string]bool
	errors  map[string]error
}

type runtimeHandlerIdentifiedProcess struct {
	*runtimeHandlerProcess
	status shards.RuntimeStatus
	err    error
}

func (f runtimeHandlerIdentifiedProcess) StatusFor(context.Context, string, string) (shards.RuntimeStatus, error) {
	return f.status, f.err
}

type runtimeHandlerBridge struct {
	events          dstruntime.EventBatch
	diagnostic      dstruntime.DiagnosticReport
	lifecycle       dstruntime.LifecycleResult
	eventsErr       error
	diagnosticErr   error
	lifecycleErr    error
	activateCalls   int
	reloadCalls     int
	captureRequests []dstruntime.DiagnosticRequest
}

func (b *runtimeHandlerBridge) Activate(context.Context, string, string) (dstruntime.LifecycleResult, error) {
	b.activateCalls++
	return b.lifecycle, b.lifecycleErr
}

func (b *runtimeHandlerBridge) Reload(context.Context, string, string) (dstruntime.LifecycleResult, error) {
	b.reloadCalls++
	return b.lifecycle, b.lifecycleErr
}

func (b *runtimeHandlerBridge) ReadEvents(context.Context, string, string) (dstruntime.EventBatch, error) {
	return b.events, b.eventsErr
}

func (b *runtimeHandlerBridge) LatestDiagnostic(context.Context, string, string) (dstruntime.DiagnosticReport, error) {
	return b.diagnostic, b.diagnosticErr
}

func (b *runtimeHandlerBridge) CaptureDiagnostic(_ context.Context, _, _ string, request dstruntime.DiagnosticRequest) (dstruntime.DiagnosticReport, error) {
	b.captureRequests = append(b.captureRequests, request)
	return b.diagnostic, b.diagnosticErr
}

func (f runtimeHandlerProcess) IsRunning(_ context.Context, roomName, worldName string) (bool, error) {
	key := roomName + "/" + worldName
	return f.running[key], f.errors[key]
}

func newRuntimeHandlerTestApp(t *testing.T) (*gin.Engine, *runtimeHandlerManager, runtimeHandlerCatalog, *runtimeHandlerProcess) {
	t.Helper()
	roomID := rooms.EncodeID("Cluster_1")
	masterID, cavesID := rooms.EncodeID("Master"), rooms.EncodeID("Caves")
	catalog := runtimeHandlerCatalog{
		room: rooms.Room{ID: roomID, DirectoryName: "Cluster_1", Name: "周末服", Managed: true},
		worlds: map[string]rooms.World{
			masterID: {ID: masterID, RoomID: roomID, DirectoryName: "Master", Name: "地面"},
			cavesID:  {ID: cavesID, RoomID: roomID, DirectoryName: "Caves", Name: "洞穴"},
		},
	}
	manager := &runtimeHandlerManager{
		statuses: []dstruntime.WorldStatus{
			{RoomID: roomID, WorldID: masterID, WorldName: "地面", State: dstruntime.InstallStateInstalled},
			{RoomID: roomID, WorldID: cavesID, WorldName: "洞穴", State: dstruntime.InstallStateInstalled},
		},
		health:       make(map[string]dstruntime.Health),
		healthErrors: make(map[string]error),
	}
	process := &runtimeHandlerProcess{running: make(map[string]bool), errors: make(map[string]error)}
	router := gin.New()
	NewDSTRuntimeHandler(manager, catalog, process).Register(router.Group("/api/v2"))
	return router, manager, catalog, process
}

func TestDSTRuntimeStatusKeepsPartialHealthFailuresAtHTTP200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, manager, catalog, process := newRuntimeHandlerTestApp(t)
	masterID, cavesID := rooms.EncodeID("Master"), rooms.EncodeID("Caves")
	process.running["Cluster_1/Master"] = true
	process.running["Cluster_1/Caves"] = true
	manager.health[masterID] = dstruntime.Health{
		SchemaVersion: 1, ProducerVersion: dstruntime.RuntimeVersion, ProducerInstanceID: "runtime",
		Running: true, Ready: true, ReadAt: time.Now(),
	}
	manager.healthErrors[cavesID] = errors.New("health.json is missing")

	response := performJSON(router, http.MethodGet, "/api/v2/rooms/"+catalog.room.ID+"/runtime", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	items, ok := responseData(t, response)["items"].([]interface{})
	if !ok || len(items) != 2 {
		t.Fatalf("runtime reports = %s", response.Body.String())
	}
	first, _ := items[0].(map[string]interface{})
	second, _ := items[1].(map[string]interface{})
	if first["healthState"] != string(dstruntime.HealthStateReady) || second["healthState"] != string(dstruntime.HealthStateUnavailable) {
		t.Fatalf("runtime health states = %s", response.Body.String())
	}
}

func TestRuntimeHealthStateAccountsForPausedSimulation(t *testing.T) {
	paused, unpaused := true, false
	lastError := "runtime failed"
	stale := dstruntime.Health{Running: true, Ready: true, ReadAt: time.Now().Add(-time.Minute)}
	fresh := dstruntime.Health{Running: true, Ready: true, ReadAt: time.Now()}

	tests := []struct {
		name        string
		health      dstruntime.Health
		paused      *bool
		currentBoot bool
		want        dstruntime.HealthState
	}{
		{name: "stale while paused in current boot", health: stale, paused: &paused, currentBoot: true, want: dstruntime.HealthStateReady},
		{name: "paused with unverified boot", health: stale, paused: &paused, want: dstruntime.HealthStateDegraded},
		{name: "stale while unpaused", health: stale, paused: &unpaused, want: dstruntime.HealthStateDegraded},
		{name: "stale with unknown pause state", health: stale, want: dstruntime.HealthStateDegraded},
		{name: "runtime error while paused", health: dstruntime.Health{Running: true, Ready: true, ReadAt: stale.ReadAt, LastError: &lastError}, paused: &paused, want: dstruntime.HealthStateDegraded},
		{name: "runtime failures while paused", health: dstruntime.Health{Running: true, Ready: true, ReadAt: stale.ReadAt, ConsecutiveFailures: 1}, paused: &paused, want: dstruntime.HealthStateDegraded},
		{name: "fresh and ready", health: fresh, want: dstruntime.HealthStateReady},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := runtimeHealthState(test.health, true, test.paused, test.currentBoot); got != test.want {
				t.Fatalf("runtimeHealthState() = %q, want %q", got, test.want)
			}
		})
	}
}

type runtimeHandlerHealthBridge struct {
	runtimeHandlerBridge
	currentBoot bool
	bootErr     error
	bootCalls   int
	local       bool
	health      dstruntime.Health
}

func (b *runtimeHandlerHealthBridge) HealthMatchesCurrentProcess(context.Context, string, string, dstruntime.Health) (bool, error) {
	b.bootCalls++
	return b.currentBoot, b.bootErr
}

func (b *runtimeHandlerHealthBridge) IsLocalPlacement(string, string) (bool, error) {
	return b.local, nil
}

func (b *runtimeHandlerHealthBridge) Health(context.Context, string, string) (dstruntime.Health, error) {
	return b.health, nil
}

func TestDSTRuntimeStatusUsesPauseStateForStaleHealth(t *testing.T) {
	for _, local := range []bool{true, false} {
		for _, test := range []struct {
			name          string
			paused        bool
			currentBoot   bool
			bootErr       error
			failures      int
			fresh         bool
			wantBootCalls int
			wantState     dstruntime.HealthState
			wantMessage   string
		}{
			{name: "paused simulation remains ready", paused: true, currentBoot: true, wantBootCalls: 1, wantState: dstruntime.HealthStateReady},
			{name: "previous boot health is not ready", paused: true, wantBootCalls: 1, wantState: dstruntime.HealthStateUnavailable, wantMessage: "分片已暂停，但尚未确认本次启动的 Runtime 健康数据"},
			{name: "boot read failure is visible", paused: true, bootErr: errors.New("agent offline"), wantBootCalls: 1, wantState: dstruntime.HealthStateUnavailable, wantMessage: "无法确认 Runtime 健康数据是否来自本次启动：agent offline"},
			{name: "real failure during pause is not blamed on staleness", paused: true, failures: 1, wantState: dstruntime.HealthStateDegraded, wantMessage: "Runtime 采集连续失败"},
			{name: "fresh health needs no extra read", paused: true, fresh: true, wantState: dstruntime.HealthStateReady},
			{name: "unpaused stale health is explained", paused: false, wantState: dstruntime.HealthStateDegraded, wantMessage: "Runtime 健康数据已超过 20 秒未更新"},
		} {
			t.Run(test.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				_, manager, catalog, process := newRuntimeHandlerTestApp(t)
				masterID := rooms.EncodeID("Master")
				manager.statuses = manager.statuses[:1]
				manager.health[masterID] = dstruntime.Health{
					SchemaVersion: 1, ProducerVersion: dstruntime.RuntimeVersion,
					Running: true, Ready: true, ReadAt: time.Now().Add(-time.Minute),
					ConsecutiveFailures: test.failures,
				}
				if test.fresh {
					health := manager.health[masterID]
					health.ReadAt = time.Now()
					manager.health[masterID] = health
				}
				bridge := &runtimeHandlerHealthBridge{local: local, health: manager.health[masterID], currentBoot: test.currentBoot, bootErr: test.bootErr}
				identified := runtimeHandlerIdentifiedProcess{
					runtimeHandlerProcess: process,
					status:                shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true, Paused: &test.paused},
				}
				router := gin.New()
				NewDSTRuntimeHandler(manager, catalog, identified, bridge).Register(router.Group("/api/v2"))

				response := performJSON(router, http.MethodGet, "/api/v2/rooms/"+catalog.room.ID+"/runtime", nil, nil, "")
				assertStatus(t, response, http.StatusOK)
				items, ok := responseData(t, response)["items"].([]interface{})
				if !ok || len(items) != 1 {
					t.Fatalf("runtime reports = %s", response.Body.String())
				}
				report, _ := items[0].(map[string]interface{})
				message, _ := report["healthMessage"].(string)
				if report["healthState"] != string(test.wantState) || message != test.wantMessage {
					t.Fatalf("runtime report = %s", response.Body.String())
				}
				if bridge.bootCalls != test.wantBootCalls {
					t.Fatalf("boot reads = %d, want %d", bridge.bootCalls, test.wantBootCalls)
				}
			})
		}
	}
}

func TestDSTRuntimeInstallAndMutationGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, manager, catalog, process := newRuntimeHandlerTestApp(t)
	worldID := rooms.EncodeID("Master")
	base := "/api/v2/rooms/" + catalog.room.ID

	response := performJSON(router, http.MethodPost, base+"/runtime/actions/install", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if manager.installCalls != 1 {
		t.Fatalf("install calls = %d", manager.installCalls)
	}

	response = performJSON(router, http.MethodDelete, base+"/worlds/"+worldID+"/runtime", map[string]string{"confirmation": "错误名称"}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	if manager.uninstallCalls != 0 {
		t.Fatal("uninstall ran despite mismatched confirmation")
	}

	process.running["Cluster_1/Master"] = true
	response = performJSON(router, http.MethodDelete, base+"/worlds/"+worldID+"/runtime", map[string]string{"confirmation": "周末服"}, nil, "")
	assertStatus(t, response, http.StatusConflict)
	response = performJSON(router, http.MethodPost, base+"/worlds/"+worldID+"/runtime/actions/rollback", map[string]string{
		"confirmation": "周末服", "backupId": "valid",
	}, nil, "")
	assertStatus(t, response, http.StatusConflict)
	if manager.uninstallCalls != 0 || manager.rollbackCalls != 0 {
		t.Fatal("destructive runtime mutation ran while the shard was active")
	}

	process.running["Cluster_1/Master"] = false
	response = performJSON(router, http.MethodPost, base+"/worlds/"+worldID+"/runtime/actions/rollback", map[string]string{
		"confirmation": "周末服", "backupId": "invalid",
	}, nil, "")
	assertStatus(t, response, http.StatusBadRequest)
	if manager.rollbackCalls != 1 {
		t.Fatalf("rollback calls = %d", manager.rollbackCalls)
	}
}

func TestDSTRuntimeEventAndDiagnosticEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, manager, catalog, process := newRuntimeHandlerTestApp(t)
	worldID := rooms.EncodeID("Master")
	bridge := &runtimeHandlerBridge{
		events: dstruntime.EventBatch{
			SchemaVersion: 1, ProducerVersion: dstruntime.RuntimeVersion, ProducerInstanceID: "runtime", SessionID: "session", ShardID: "1",
			FirstSequence: 1, LastSequence: 1, Events: []dstruntime.RuntimeEvent{{Sequence: 1, Kind: "world.phase", OccurredAtUnix: time.Now().Unix(), Fields: map[string]interface{}{"phase": "day"}}},
		},
		diagnostic: dstruntime.DiagnosticReport{
			SchemaVersion: 1, ProducerVersion: dstruntime.RuntimeVersion, ProducerInstanceID: "runtime", SessionID: "session", ShardID: "1", Sequence: 1,
			RequestID: "diagnostic-123456", Profile: "summary", OK: true, Code: "DIAGNOSTIC_COMPLETE", Result: map[string]interface{}{"entityCount": 10}, CompletedAtUnix: time.Now().Unix(),
		},
	}
	router := gin.New()
	NewDSTRuntimeHandler(manager, catalog, process, bridge).Register(router.Group("/api/v2"))
	base := "/api/v2/rooms/" + catalog.room.ID + "/worlds/" + worldID + "/runtime"

	response := performJSON(router, http.MethodGet, base+"/events", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["lastSequence"] != float64(1) {
		t.Fatalf("event response = %s", response.Body.String())
	}

	response = performJSON(router, http.MethodGet, base+"/diagnostics/latest", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["profile"] != "summary" {
		t.Fatalf("diagnostic response = %s", response.Body.String())
	}

	response = performJSON(router, http.MethodPost, base+"/diagnostics", map[string]interface{}{"profile": "prefab", "prefab": "pigking", "sampleLimit": 10}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if len(bridge.captureRequests) != 1 || bridge.captureRequests[0].RequestID == "" || bridge.captureRequests[0].Prefab != "pigking" {
		t.Fatalf("capture requests = %#v", bridge.captureRequests)
	}
}

func TestDSTRuntimeLifecycleEndpointsUseTheManagedBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, manager, catalog, process := newRuntimeHandlerTestApp(t)
	worldID := rooms.EncodeID("Master")
	bridge := &runtimeHandlerBridge{lifecycle: dstruntime.LifecycleResult{
		RoomID: catalog.room.ID, WorldID: worldID, WorldName: "地面", Mode: dstruntime.LifecycleModeActivate,
		Health:  dstruntime.Health{SchemaVersion: 1, ProducerVersion: dstruntime.RuntimeVersion, Running: true, Ready: true},
		Message: "activated",
	}}
	router := gin.New()
	NewDSTRuntimeHandler(manager, catalog, process, bridge).Register(router.Group("/api/v2"))
	base := "/api/v2/rooms/" + catalog.room.ID + "/worlds/" + worldID + "/runtime/actions/"

	response := performJSON(router, http.MethodPost, base+"activate", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodPost, base+"reload", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if bridge.activateCalls != 1 || bridge.reloadCalls != 1 {
		t.Fatalf("lifecycle calls activate=%d reload=%d", bridge.activateCalls, bridge.reloadCalls)
	}

	bridge.lifecycleErr = dstruntime.ErrRuntimeActivation
	response = performJSON(router, http.MethodPost, base+"reload", nil, nil, "")
	assertStatus(t, response, http.StatusConflict)
}

func TestDSTRuntimeResultErrorsAreActionable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, manager, catalog, process := newRuntimeHandlerTestApp(t)
	worldID := rooms.EncodeID("Master")
	bridge := &runtimeHandlerBridge{eventsErr: dstruntime.ErrRuntimeResultAbsent, diagnosticErr: dstruntime.ErrRuntimeUnavailable}
	router := gin.New()
	NewDSTRuntimeHandler(manager, catalog, process, bridge).Register(router.Group("/api/v2"))
	base := "/api/v2/rooms/" + catalog.room.ID + "/worlds/" + worldID + "/runtime"

	response := performJSON(router, http.MethodGet, base+"/events", nil, nil, "")
	assertStatus(t, response, http.StatusNotFound)
	response = performJSON(router, http.MethodGet, base+"/diagnostics/latest", nil, nil, "")
	assertStatus(t, response, http.StatusConflict)

	bridge.diagnosticErr = dstruntime.ErrRuntimeRequestInvalid
	response = performJSON(router, http.MethodPost, base+"/diagnostics", map[string]interface{}{"profile": "performance", "durationSeconds": 8}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	if len(bridge.captureRequests) != 1 || bridge.captureRequests[0].DurationSeconds != 8 {
		t.Fatalf("capture requests = %#v", bridge.captureRequests)
	}
}
