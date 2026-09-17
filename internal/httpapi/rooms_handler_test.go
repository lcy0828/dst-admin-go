package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/internal/topology"
	"dont/shared"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type roomHandlerControl struct {
	mu            sync.RWMutex
	running       map[string]bool
	statuses      map[string]shards.RuntimeStatus
	startStatuses map[string]shards.RuntimeStatus
}

type roomRecoveryMoverStub struct {
	roomID   string
	worldIDs []string
}

func (m *roomRecoveryMoverStub) MoveRoomToRecovery(_ context.Context, roomID string, worldIDs []string) (runtimedriver.RoomRecoveryLocation, error) {
	m.roomID, m.worldIDs = roomID, append([]string(nil), worldIDs...)
	return runtimedriver.RoomRecoveryLocation{
		TargetID: "agent:node", InstallationID: "default", RecoveryRef: ".dst-admin-trash/remote-room",
	}, nil
}

func (c *roomHandlerControl) IsRunning(_ context.Context, room, world string) (bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running[room+"/"+world], nil
}

func (c *roomHandlerControl) Status(_ context.Context, room, world string) (shards.RuntimeStatus, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	key := room + "/" + world
	if status, exists := c.statuses[key]; exists {
		return status, nil
	}
	if c.running[key] {
		return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
	}
	return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
}

func (c *roomHandlerControl) Start(_ context.Context, room, world string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running[room+"/"+world] = true
	if status, ok := c.startStatuses[room+"/"+world]; ok {
		c.statuses[room+"/"+world] = status
	}
	return nil
}

func (c *roomHandlerControl) Stop(_ context.Context, room, world string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running[room+"/"+world] = false
	delete(c.statuses, room+"/"+world)
	return nil
}

func newRoomHandlerApp(t *testing.T) (*gin.Engine, *jobs.Service, *roomHandlerControl, *rooms.Service) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	roomStore := rooms.NewStore(db, "api_")
	if err := roomStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	catalog, err := rooms.NewCatalog(t.TempDir(), roomStore)
	if err != nil {
		t.Fatal(err)
	}
	roomService := rooms.NewService(catalog, roomStore)
	jobStore := jobs.NewStore(db, "api_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	control := &roomHandlerControl{running: make(map[string]bool), statuses: make(map[string]shards.RuntimeStatus)}
	operations := shards.NewOperations(roomService, control)
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewRoomHandler(roomService, operations, jobService).Register(v2)
	NewJobHandler(jobService).Register(v2)
	return router, jobService, control, roomService
}

func TestRoomHandlerPropagatesManagedRoomLifecycle(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	defer db.Close()
	roomStore := rooms.NewStore(db, "init_")
	if err := roomStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	catalog, err := rooms.NewCatalog(t.TempDir(), roomStore)
	if err != nil {
		t.Fatal(err)
	}
	roomService := rooms.NewService(catalog, roomStore)
	jobStore := jobs.NewStore(db, "init_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	control := &roomHandlerControl{running: make(map[string]bool), statuses: make(map[string]shards.RuntimeStatus)}
	handler := NewRoomHandler(roomService, shards.NewOperations(roomService, control), jobService)
	initialized := ""
	finalized := ""
	roomService.SetManagedRoomLifecycle(func(roomID string) {
		initialized = roomID
	}, func(roomID string) {
		finalized = roomID
	})
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodPost, "/api/v2/rooms", map[string]interface{}{
		"directoryName": "initialized_room", "name": "初始化测试", "gameMode": "survival", "maxPlayers": 6,
	}, nil, "")
	assertStatus(t, response, http.StatusCreated)
	roomID, _ := responseData(t, response)["id"].(string)
	if initialized == "" || initialized != roomID {
		t.Fatalf("managed room initializer got %q, want %q", initialized, roomID)
	}
	response = performJSON(router, http.MethodDelete, "/api/v2/rooms/"+roomID, map[string]interface{}{
		"confirmation": "初始化测试",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if finalized != roomID {
		t.Fatalf("managed room finalizer got %q, want %q", finalized, roomID)
	}
}

func TestRoomHandlerMovesRemoteOnlyRoomToRuntimeRecovery(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	roomStore := rooms.NewStore(db, "remote_delete_")
	if err := roomStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	catalog, err := rooms.NewCatalog(t.TempDir(), roomStore)
	if err != nil {
		t.Fatal(err)
	}
	roomService := rooms.NewService(catalog, roomStore)
	roomService.ConfigureLocalDiscovery(false)
	if err := roomService.SyncRuntimeCatalog([]rooms.RuntimeCatalogSource{{
		TargetID: "agent:node", Online: true, Available: true, ObservedAt: time.Now().UTC(),
		Inventory: shared.RuntimeInventoryReport{Rooms: []shared.RoomInventoryReport{{
			Directory: "remote_room", Name: "远端房间", Shards: []shared.ShardInventoryReport{{Directory: "Master", Name: "地表", Role: "master"}},
		}}},
	}}); err != nil {
		t.Fatal(err)
	}
	jobStore := jobs.NewStore(db, "remote_delete_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	control := &roomHandlerControl{running: make(map[string]bool), statuses: make(map[string]shards.RuntimeStatus)}
	handler := NewRoomHandler(roomService, shards.NewOperations(roomService, control), jobService)
	mover := &roomRecoveryMoverStub{}
	if err := handler.ConfigureRoomRecovery(mover); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	roomID := rooms.EncodeID("remote_room")
	response := performJSON(router, http.MethodDelete, "/api/v2/rooms/"+roomID, map[string]interface{}{
		"confirmation": "远端房间",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if mover.roomID != roomID || len(mover.worldIDs) != 1 || responseData(t, response)["recoveryName"] != "agent:node:.dst-admin-trash/remote-room" {
		t.Fatalf("mover=%#v response=%s", mover, response.Body.String())
	}
	if _, err := roomService.Room(roomID); err != rooms.ErrRoomNotFound {
		t.Fatalf("remote room remained registered: %v", err)
	}
}

func TestRoomHandlerRejectsLocalAndRemoteRoomBeforeMovingEitherCopy(t *testing.T) {
	router, _, _, roomService := newRoomHandlerApp(t)
	created := performJSON(router, http.MethodPost, "/api/v2/rooms", map[string]interface{}{
		"directoryName": "split_room", "name": "拆分房间", "gameMode": "survival", "maxPlayers": 6,
	}, nil, "")
	assertStatus(t, created, http.StatusCreated)
	roomID, _ := responseData(t, created)["id"].(string)
	if err := roomService.SyncRuntimeCatalog([]rooms.RuntimeCatalogSource{{
		TargetID: "agent:node", Online: true, Available: true, ObservedAt: time.Now().UTC(),
		Inventory: shared.RuntimeInventoryReport{Rooms: []shared.RoomInventoryReport{{
			Directory: "split_room", Name: "拆分房间", Shards: []shared.ShardInventoryReport{{Directory: "Caves", Name: "洞穴", Role: "caves"}},
		}}},
	}}); err != nil {
		t.Fatal(err)
	}
	room, err := roomService.Room(roomID)
	if err != nil || len(room.TargetIDs) != 2 {
		t.Fatalf("split room targets=%v err=%v", room.TargetIDs, err)
	}

	response := performJSON(router, http.MethodDelete, "/api/v2/rooms/"+roomID, map[string]interface{}{
		"confirmation": "拆分房间",
	}, nil, "")
	assertStatus(t, response, http.StatusConflict)
}

func TestRoomAndShardJobHTTPFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, jobService, control, _ := newRoomHandlerApp(t)

	response := performJSON(router, http.MethodPost, "/api/v2/rooms", map[string]interface{}{
		"directoryName": "../bad", "name": "", "gameMode": "unknown", "maxPlayers": 0,
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)

	response = performJSON(router, http.MethodPost, "/api/v2/rooms", map[string]interface{}{
		"directoryName": "room_2026", "name": "周末服", "description": "联机测试", "gameMode": "survival",
		"maxPlayers": 8, "includeCaves": true,
	}, nil, "")
	assertStatus(t, response, http.StatusCreated)
	roomID, _ := responseData(t, response)["id"].(string)
	if roomID == "" {
		t.Fatalf("create room response missing id: %s", response.Body.String())
	}

	response = performJSON(router, http.MethodGet, "/api/v2/rooms", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	items, _ := responseData(t, response)["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("room list = %s", response.Body.String())
	}

	response = performJSON(router, http.MethodGet, "/api/v2/rooms/"+roomID+"/worlds", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	var worldsEnvelope struct {
		Data struct {
			Items []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &worldsEnvelope); err != nil || len(worldsEnvelope.Data.Items) != 2 {
		t.Fatalf("decode worlds: %v, %s", err, response.Body.String())
	}

	response = performJSON(router, http.MethodGet, "/api/v2/rooms/"+roomID+"/runtime-modes?worldIds="+worldsEnvelope.Data.Items[0].ID, nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	var runtimeModesEnvelope struct {
		Data struct {
			RoomID      string                          `json:"roomId"`
			WorldIDs    []string                        `json:"worldIds"`
			DefaultMode shared.RuntimePerformanceMode   `json:"defaultMode"`
			Modes       []shared.RuntimePerformanceMode `json:"modes"`
			Packages    []shards.RuntimePackageOption   `json:"packages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &runtimeModesEnvelope); err != nil {
		t.Fatalf("decode runtime modes: %v, %s", err, response.Body.String())
	}
	if runtimeModesEnvelope.Data.RoomID != roomID || len(runtimeModesEnvelope.Data.WorldIDs) != 1 ||
		runtimeModesEnvelope.Data.WorldIDs[0] != worldsEnvelope.Data.Items[0].ID ||
		runtimeModesEnvelope.Data.DefaultMode != shared.RuntimePerformanceModeGame ||
		len(runtimeModesEnvelope.Data.Modes) != 1 || runtimeModesEnvelope.Data.Modes[0] != shared.RuntimePerformanceModeGame ||
		len(runtimeModesEnvelope.Data.Packages) != 3 {
		t.Fatalf("runtime modes = %#v", runtimeModesEnvelope.Data)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/"+roomID+"/actions/start", map[string]interface{}{
		"worldIds": []string{worldsEnvelope.Data.Items[0].ID}, "runtimeMode": "game", "runtimeVersion": "3.0.0",
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/"+roomID+"/worlds", map[string]interface{}{
		"directoryName": "Forest2", "type": "forest",
	}, nil, "")
	assertStatus(t, response, http.StatusCreated)
	worldID, _ := responseData(t, response)["id"].(string)
	if worldID == "" {
		t.Fatalf("create world response missing id: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodDelete, "/api/v2/rooms/"+roomID+"/worlds/"+worldID, map[string]interface{}{
		"confirmation": "wrong",
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = performJSON(router, http.MethodDelete, "/api/v2/rooms/"+roomID+"/worlds/"+worldID, map[string]interface{}{
		"confirmation": "周末服",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	worldRecoveryPath, _ := responseData(t, response)["recoveryName"].(string)
	worldRecoveryName := filepath.Base(worldRecoveryPath)
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/"+roomID+"/worlds/recovery", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/"+roomID+"/worlds/recovery/"+worldRecoveryName+"/actions/restore", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	for _, world := range worldsEnvelope.Data.Items {
		if world.Status != "stopped" {
			t.Fatalf("initial world status = %q", world.Status)
		}
	}
	control.mu.Lock()
	control.statuses["room_2026/Master"] = shards.RuntimeStatus{
		State: shards.RuntimeRunning, Code: "SAVE_WRITE_FAILED", Message: "保存写入失败", SessionExists: true,
	}
	control.mu.Unlock()
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/"+roomID+"/worlds", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	var saveHealthEnvelope struct {
		Data struct {
			Items []struct {
				DirectoryName string `json:"directoryName"`
				Status        string `json:"status"`
				StatusCode    string `json:"statusCode"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &saveHealthEnvelope); err != nil {
		t.Fatal(err)
	}
	foundSaveHealth := false
	for _, world := range saveHealthEnvelope.Data.Items {
		if world.DirectoryName == "Master" {
			foundSaveHealth = world.Status == "running" && world.StatusCode == "SAVE_WRITE_FAILED"
		}
	}
	if !foundSaveHealth {
		t.Fatalf("save health status missing: %s", response.Body.String())
	}
	control.mu.Lock()
	delete(control.statuses, "room_2026/Master")
	control.mu.Unlock()

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/"+roomID+"/actions/start", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID, _ := responseData(t, response)["id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := jobService.Get(jobID)
		if err == nil && job.Status == jobs.StatusSucceeded {
			if len(job.Targets) != 3 || job.Outcome != jobs.OutcomeFull {
				t.Fatalf("unexpected job: %#v", job)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	response = performJSON(router, http.MethodGet, "/api/v2/jobs/"+jobID, nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["status"] != string(jobs.StatusSucceeded) {
		t.Fatalf("job response = %s", response.Body.String())
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/"+roomID+"/actions/stop", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	stopJobID, _ := responseData(t, response)["id"].(string)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := jobService.Get(stopJobID)
		if err == nil && job.Status == jobs.StatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	response = performJSON(router, http.MethodDelete, "/api/v2/rooms/"+roomID, map[string]interface{}{
		"confirmation": "wrong",
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = performJSON(router, http.MethodDelete, "/api/v2/rooms/"+roomID, map[string]interface{}{
		"confirmation": "周末服",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["recoveryName"] == "" {
		t.Fatalf("delete room response missing recovery path: %s", response.Body.String())
	}
	roomRecoveryPath, _ := responseData(t, response)["recoveryName"].(string)
	roomRecoveryName := filepath.Base(roomRecoveryPath)
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/recovery", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodDelete, "/api/v2/rooms/recovery/"+roomRecoveryName, map[string]interface{}{
		"confirmation": "wrong",
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/recovery/"+roomRecoveryName+"/actions/restore", nil, nil, "")
	assertStatus(t, response, http.StatusOK)

	response = performJSON(router, http.MethodGet, "/api/v2/rooms/not-base64", nil, nil, "")
	assertStatus(t, response, http.StatusBadRequest)
}

func TestRoomFailureReturnsCapacityRiskPreview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/capacity-risk", func(c *gin.Context) {
		roomFailure(c, &shards.CapacityRiskError{Preview: topology.StartCapacityPreview{
			RoomID: "room", WorldIDs: []string{"master"}, RequiresRiskConfirmation: true,
			Targets: []topology.StartCapacityTarget{{
				TargetID: "local", TargetName: "本机", CurrentRunningShards: 2,
				StartingShards: 1, ProjectedRunningShards: 3, RequiresRiskConfirmation: true,
			}},
		}})
	})
	response := performJSON(router, http.MethodPost, "/capacity-risk", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	var envelope struct {
		Error struct {
			Code    string                        `json:"code"`
			Details topology.StartCapacityPreview `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "CAPACITY_RISK_CONFIRMATION_REQUIRED" ||
		!envelope.Error.Details.RequiresRiskConfirmation || len(envelope.Error.Details.Targets) != 1 {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestBatchRoomActionHTTPFlowUsesRoomScopedTargets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, jobService, _, _ := newRoomHandlerApp(t)
	createRoom := func(directory, name string) (string, string) {
		t.Helper()
		response := performJSON(router, http.MethodPost, "/api/v2/rooms", map[string]interface{}{
			"directoryName": directory, "name": name, "gameMode": "survival", "maxPlayers": 6,
		}, nil, "")
		assertStatus(t, response, http.StatusCreated)
		roomID, _ := responseData(t, response)["id"].(string)
		response = performJSON(router, http.MethodGet, "/api/v2/rooms/"+roomID+"/worlds", nil, nil, "")
		assertStatus(t, response, http.StatusOK)
		var envelope struct {
			Data struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Data.Items) != 1 {
			t.Fatalf("worlds: %v %s", err, response.Body.String())
		}
		return roomID, envelope.Data.Items[0].ID
	}
	roomAID, worldAID := createRoom("batch_a", "批量 A")
	roomBID, worldBID := createRoom("batch_b", "批量 B")
	response := performJSON(router, http.MethodPost, "/api/v2/rooms/actions/start", map[string]interface{}{
		"rooms": []map[string]interface{}{
			{"roomId": roomAID, "worldIds": []string{worldAID}},
			{"roomId": roomBID, "worldIds": []string{worldBID}},
		},
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID, _ := responseData(t, response)["id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := jobService.Get(jobID)
		if err == nil && job.Status == jobs.StatusSucceeded {
			if len(job.Targets) != 2 || job.Targets[0].TargetID == job.Targets[1].TargetID || job.Outcome != jobs.OutcomeFull {
				t.Fatalf("job = %#v", job)
			}
			if job.ProgressDetail == nil || len(job.ProgressDetail.Worlds) != 2 {
				t.Fatalf("batch progress missing: %+v", job.ProgressDetail)
			}
			for index, world := range job.ProgressDetail.Worlds {
				if world.WorldID != job.Targets[index].TargetID || world.Name != job.Targets[index].Name || world.Stage != "ready" || world.Percent != 100 {
					t.Fatalf("batch world identities collided: %+v", job.ProgressDetail.Worlds)
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := jobService.Get(jobID)
	t.Fatalf("batch job did not succeed: %#v", job)
}

func TestBatchRoomActionRequiresAtLeastOneRoom(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, _, _, _ := newRoomHandlerApp(t)
	response := performJSON(router, http.MethodPost, "/api/v2/rooms/actions/start", map[string]interface{}{"rooms": []interface{}{}}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "NO_ROOMS" {
		t.Fatalf("error = %v, response = %s", err, response.Body.String())
	}
}

func TestOrdinaryStartAndRestartPersistIndependentWorldProgress(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router, jobService, control, _ := newRoomHandlerApp(t)
			response := performJSON(router, http.MethodPost, "/api/v2/rooms", map[string]interface{}{
				"directoryName": "progress_room", "name": "进度测试", "gameMode": "survival", "maxPlayers": 6,
			}, nil, "")
			assertStatus(t, response, http.StatusCreated)
			roomID := responseData(t, response)["id"].(string)
			response = performJSON(router, http.MethodPost, "/api/v2/rooms/"+roomID+"/worlds", map[string]interface{}{
				"directoryName": "Caves", "name": "Caves", "role": "caves", "type": "cave",
			}, nil, "")
			assertStatus(t, response, http.StatusCreated)
			cavesID := responseData(t, response)["id"].(string)
			control.startStatuses = map[string]shards.RuntimeStatus{
				"progress_room/Caves": {State: shards.RuntimeStarting, SessionExists: true, StartupStage: "loading_world"},
			}
			if action == "restart" {
				control.running["progress_room/Master"], control.running["progress_room/Caves"] = true, true
			}
			response = performJSON(router, http.MethodPost, "/api/v2/rooms/"+roomID+"/actions/"+action, map[string]interface{}{}, nil, "")
			assertStatus(t, response, http.StatusAccepted)
			jobID := responseData(t, response)["id"].(string)
			defer func() {
				control.mu.Lock()
				control.statuses["progress_room/Caves"] = shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}
				control.mu.Unlock()
				for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
					job, _ := jobService.Get(jobID)
					if job.Status == jobs.StatusSucceeded {
						return
					}
				}
				t.Error("world job did not finish after readiness")
			}()
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				job, err := jobService.Get(jobID)
				if err != nil || job.ProgressDetail == nil || len(job.ProgressDetail.Worlds) != 2 {
					continue
				}
				worlds := job.ProgressDetail.Worlds
				if worlds[0].Stage != "ready" || worlds[1].Stage != "loading_world" {
					continue
				}
				if job.Status != jobs.StatusRunning || !worlds[0].IsMaster || worlds[1].WorldID != cavesID || worlds[1].Percent != 80 {
					t.Fatalf("incorrect independent progress: %+v", job)
				}
				events, err := jobService.EventsAfter(0, 100)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.JobID == jobID && event.Type == "job.progress" && event.Data.ProgressDetail != nil && len(event.Data.ProgressDetail.Worlds) == 2 && event.Data.ProgressDetail.Worlds[1].Stage == "loading_world" {
						return
					}
				}
				t.Fatal("world progress not forwarded through job events")
			}
			t.Fatal("ordinary lifecycle never showed ready Master while Caves was loading")
		})
	}
}
