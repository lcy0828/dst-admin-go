package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type httpTopologyRooms struct {
	room  rooms.Room
	world rooms.World
}

func (c httpTopologyRooms) List() ([]rooms.Room, error) { return []rooms.Room{c.room}, nil }
func (c httpTopologyRooms) Room(id string) (rooms.Room, error) {
	if id != c.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return c.room, nil
}
func (c httpTopologyRooms) Worlds(id string) ([]rooms.World, error) {
	if id != c.room.ID {
		return nil, rooms.ErrRoomNotFound
	}
	return []rooms.World{c.world}, nil
}

type httpTopologyTargets struct {
	items []agents.RuntimeTargetInventory
}

func (c httpTopologyTargets) RuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error) {
	return c.items, nil
}

func TestTopologyHTTPPreviewRevisionAndOvercommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := topology.NewStore(db, "topology_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "房间", Managed: true}
	world := rooms.World{ID: "world", RoomID: room.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	now := time.Now().UTC()
	local := httpTargetInventory(
		agents.RuntimeTarget{ID: "local", Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		4, []shared.RoomInventoryReport{{Directory: "Cluster", MasterPort: 10889, Shards: []shared.ShardInventoryReport{{Directory: "Master", Role: "master", ServerPort: 10999, AuthenticationPort: 8767, MasterServerPort: 27017}}}}, nil, now,
	)
	remote := httpTargetInventory(
		agents.RuntimeTarget{ID: "agent:node", AgentID: "node", Name: "节点", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		1, []shared.RoomInventoryReport{{Directory: "Cluster", MasterPort: 10889, Shards: []shared.ShardInventoryReport{{Directory: "Master", Role: "master", ServerPort: 10999, AuthenticationPort: 8767, MasterServerPort: 27017}}}},
		[]shared.ShardProcessReport{{PID: 42, Cluster: "Other", Shard: "Master"}}, now,
	)
	service, err := topology.NewService(httpTopologyRooms{room: room, world: world}, httpTopologyTargets{items: []agents.RuntimeTargetInventory{local, remote}}, store)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewTopologyHandler(service).Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodGet, "/api/v2/rooms/room/topology", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	revision, _ := responseData(t, response)["revision"].(string)
	if revision == "" || responseData(t, response)["remoteExecutionReady"] != true || responseData(t, response)["mode"] != "applied_placement" {
		t.Fatalf("topology=%s", response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/topology/preview", map[string]interface{}{
		"expectedRevision": revision, "placements": []interface{}{},
	}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_TOPOLOGY")

	request := map[string]interface{}{
		"expectedRevision": revision,
		"placements":       []map[string]string{{"worldId": "world", "targetId": "agent:node"}},
	}
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/topology/preview", request, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["requiresOvercommitConfirmation"] != true {
		t.Fatalf("preview=%s", response.Body.String())
	}
	response = performJSON(router, http.MethodPut, "/api/v2/rooms/room/topology", request, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "TOPOLOGY_OVERCOMMIT_CONFIRMATION_REQUIRED")
	request["allowOvercommit"] = true
	response = performJSON(router, http.MethodPut, "/api/v2/rooms/room/topology", request, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["revision"] == revision {
		t.Fatalf("revision did not change: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPut, "/api/v2/rooms/room/topology", request, nil, "")
	assertAPIError(t, response, http.StatusConflict, "TOPOLOGY_REVISION_CONFLICT")

	response = performJSON(router, http.MethodGet, "/api/v2/runtime-infrastructure", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	infrastructure := responseData(t, response)
	for _, field := range []string{"providers", "environments", "networkProfiles", "portReservations", "cpuAllocations"} {
		values, ok := infrastructure[field].([]interface{})
		if !ok || len(values) == 0 {
			t.Fatalf("runtime infrastructure %s=%#v body=%s", field, infrastructure[field], response.Body.String())
		}
	}
	profiles := infrastructure["networkProfiles"].([]interface{})
	profile := profiles[0].(map[string]interface{})
	profileID, _ := profile["id"].(string)
	response = performJSON(router, http.MethodPut, "/api/v2/runtime-infrastructure/network-profiles/"+profileID, map[string]interface{}{
		"name": "本机网络", "bindAddress": "not-an-ip", "advertiseAddress": "",
	}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_RUNTIME_RESOURCE")
	environments := infrastructure["environments"].([]interface{})
	localEnvironmentID := ""
	for _, value := range environments {
		environment := value.(map[string]interface{})
		if environment["targetId"] == "local" {
			localEnvironmentID, _ = environment["id"].(string)
		}
	}
	response = performJSON(router, http.MethodPut, "/api/v2/runtime-infrastructure/cpu-allocations", map[string]interface{}{
		"roomId": "room", "worldId": "world", "environmentId": localEnvironmentID,
		"policy": "none", "logicalCpuIds": []int{}, "allowSmtSiblingRisk": false,
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
}

func httpTargetInventory(target agents.RuntimeTarget, physical int, inventoryRooms []shared.RoomInventoryReport, processes []shared.ShardProcessReport, now time.Time) agents.RuntimeTargetInventory {
	report := shared.RuntimeInventoryReport{
		ProtocolVersion: shared.RuntimeInventoryProtocolVersion, ObservedAt: now,
		CPU:   shared.CPUInventory{LogicalProcessors: physical, PhysicalCores: physical},
		Rooms: inventoryRooms, Processes: processes,
	}
	return agents.RuntimeTargetInventory{
		Target: target, Available: true, Inventory: report,
		Capacity:   agents.CapacityFor(physical, physical, false, len(processes), false),
		ObservedAt: &now, ReceivedAt: &now,
	}
}
