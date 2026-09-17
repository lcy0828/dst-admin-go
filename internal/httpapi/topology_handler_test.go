package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	items  []agents.RuntimeTargetInventory
	detect func(context.Context, string, shared.RuntimeNetworkRegion) (shared.RuntimeNetworkResult, error)
}

type httpShardLinkApplierFunc func(context.Context, string, string) error

func (f httpShardLinkApplierFunc) ApplyDesiredConfiguration(ctx context.Context, roomID, expectedRevision string) error {
	return f(ctx, roomID, expectedRevision)
}

func (c httpTopologyTargets) RuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error) {
	return c.items, nil
}

func (c httpTopologyTargets) DetectEgress(ctx context.Context, targetID string, region shared.RuntimeNetworkRegion) (shared.RuntimeNetworkResult, error) {
	if c.detect == nil {
		return shared.RuntimeNetworkResult{}, topology.ErrEgressDetectionUnavailable
	}
	return c.detect(ctx, targetID, region)
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
	detectedRegion := shared.RuntimeNetworkRegion("")
	service, err := topology.NewService(httpTopologyRooms{room: room, world: world}, httpTopologyTargets{
		items: []agents.RuntimeTargetInventory{local, remote},
		detect: func(_ context.Context, targetID string, region shared.RuntimeNetworkRegion) (shared.RuntimeNetworkResult, error) {
			detectedRegion = region
			return shared.RuntimeNetworkResult{Address: "203.0.113.42", Region: region, ObservedAt: now}, nil
		},
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler := NewTopologyHandler(service)
	var appliedRoomID, appliedRevision string
	if err := handler.ConfigureShardLinkApplier(httpShardLinkApplierFunc(func(_ context.Context, roomID, expectedRevision string) error {
		appliedRoomID, appliedRevision = roomID, expectedRevision
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodGet, "/api/v2/rooms/room/topology", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	revision, _ := responseData(t, response)["revision"].(string)
	if revision == "" || responseData(t, response)["remoteExecutionReady"] != true || responseData(t, response)["mode"] != "applied_placement" {
		t.Fatalf("topology=%s", response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/topology/shard-links/actions/apply", map[string]string{
		"expectedRevision": revision,
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if appliedRoomID != room.ID || appliedRevision != revision {
		t.Fatalf("applied room=%q revision=%q", appliedRoomID, appliedRevision)
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
	response = performJSON(router, http.MethodPost, "/api/v2/runtime-infrastructure/network-profiles/"+profileID+"/actions/detect-egress", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["address"] != "203.0.113.42" || responseData(t, response)["profileId"] != profileID {
		t.Fatalf("egress detection=%s", response.Body.String())
	}
	if detectedRegion != shared.RuntimeNetworkRegionGlobal {
		t.Fatalf("default region=%q", detectedRegion)
	}
	response = performJSON(router, http.MethodPost, "/api/v2/runtime-infrastructure/network-profiles/"+profileID+"/actions/detect-egress", map[string]string{"region": "cn"}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if detectedRegion != shared.RuntimeNetworkRegionCN {
		t.Fatalf("requested region=%q", detectedRegion)
	}
	response = performJSON(router, http.MethodPost, "/api/v2/runtime-infrastructure/network-profiles/"+profileID+"/actions/detect-egress", map[string]string{"region": "invalid"}, nil, "")
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

func TestTopologyHTTPSerializesRouteApplyAndTopologyUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := topology.NewStore(db, "topology_http_serial_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "serial-room", DirectoryName: "SerialCluster", Name: "串行房间", Managed: true}
	world := rooms.World{ID: "serial-world", RoomID: room.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	now := time.Now().UTC()
	local := httpTargetInventory(
		agents.RuntimeTarget{ID: "local", Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		4, []shared.RoomInventoryReport{{Directory: room.DirectoryName, MasterPort: 10889, Shards: []shared.ShardInventoryReport{{Directory: world.DirectoryName, Role: "master", ServerPort: 10999, AuthenticationPort: 8767, MasterServerPort: 27017}}}}, nil, now,
	)
	service, err := topology.NewService(httpTopologyRooms{room: room, world: world}, httpTopologyTargets{items: []agents.RuntimeTargetInventory{local}}, store)
	if err != nil {
		t.Fatal(err)
	}
	applyEntered := make(chan struct{})
	releaseApply := make(chan struct{})
	handler := NewTopologyHandler(service)
	if err := handler.ConfigureShardLinkApplier(httpShardLinkApplierFunc(func(ctx context.Context, _, _ string) error {
		close(applyEntered)
		select {
		case <-releaseApply:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodGet, "/api/v2/rooms/serial-room/topology", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	revision, _ := responseData(t, response)["revision"].(string)
	applyResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		applyResult <- performJSON(router, http.MethodPost, "/api/v2/rooms/serial-room/topology/shard-links/actions/apply", map[string]string{
			"expectedRevision": revision,
		}, nil, "")
	}()
	select {
	case <-applyEntered:
	case <-time.After(time.Second):
		t.Fatal("route apply did not begin")
	}

	updateResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		updateResult <- performJSON(router, http.MethodPut, "/api/v2/rooms/serial-room/topology", map[string]interface{}{
			"expectedRevision": revision,
			"placements":       []map[string]string{{"worldId": world.ID, "targetId": "local"}},
		}, nil, "")
	}()
	select {
	case result := <-updateResult:
		t.Fatalf("topology update completed before route apply released the room lock: %s", result.Body.String())
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseApply)
	select {
	case result := <-applyResult:
		assertStatus(t, result, http.StatusOK)
	case <-time.After(time.Second):
		t.Fatal("route apply did not complete")
	}
	select {
	case result := <-updateResult:
		assertStatus(t, result, http.StatusOK)
	case <-time.After(time.Second):
		t.Fatal("topology update did not continue after route apply")
	}
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
