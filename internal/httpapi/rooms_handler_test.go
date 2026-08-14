package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/internal/topology"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type roomHandlerControl struct {
	running map[string]bool
}

func (c *roomHandlerControl) IsRunning(_ context.Context, room, world string) (bool, error) {
	return c.running[room+"/"+world], nil
}

func (c *roomHandlerControl) Start(_ context.Context, room, world string) error {
	c.running[room+"/"+world] = true
	return nil
}

func (c *roomHandlerControl) Stop(_ context.Context, room, world string) error {
	c.running[room+"/"+world] = false
	return nil
}

func newRoomHandlerApp(t *testing.T) (*gin.Engine, *jobs.Service) {
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
	control := &roomHandlerControl{running: make(map[string]bool)}
	operations := shards.NewOperations(roomService, control)
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewRoomHandler(roomService, operations, jobService).Register(v2)
	NewJobHandler(jobService).Register(v2)
	return router, jobService
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
	control := &roomHandlerControl{running: make(map[string]bool)}
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

func TestRoomAndShardJobHTTPFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, jobService := newRoomHandlerApp(t)

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
	router, jobService := newRoomHandlerApp(t)
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
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := jobService.Get(jobID)
	t.Fatalf("batch job did not succeed: %#v", job)
}

func TestBatchRoomActionRequiresAtLeastOneRoom(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, _ := newRoomHandlerApp(t)
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
