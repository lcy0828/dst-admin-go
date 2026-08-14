package topology

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/shared"
)

type topologyRoomCatalog struct {
	rooms  []rooms.Room
	worlds map[string][]rooms.World
}

func (c topologyRoomCatalog) List() ([]rooms.Room, error) {
	return append([]rooms.Room(nil), c.rooms...), nil
}
func (c topologyRoomCatalog) Room(id string) (rooms.Room, error) {
	for _, room := range c.rooms {
		if room.ID == id {
			return room, nil
		}
	}
	return rooms.Room{}, rooms.ErrRoomNotFound
}
func (c topologyRoomCatalog) Worlds(id string) ([]rooms.World, error) {
	return append([]rooms.World(nil), c.worlds[id]...), nil
}

type topologyTargetCatalog struct {
	items []agents.RuntimeTargetInventory
}

func (c topologyTargetCatalog) RuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error) {
	return append([]agents.RuntimeTargetInventory(nil), c.items...), nil
}

func TestTopologyAggregatesRoomsAndRequiresExplicitOvercommit(t *testing.T) {
	now := time.Now().UTC()
	roomA := rooms.Room{ID: "room-a", DirectoryName: "Cluster_A", Name: "A", Managed: true}
	roomB := rooms.Room{ID: "room-b", DirectoryName: "Cluster_B", Name: "B", Managed: true}
	masterA := rooms.World{ID: "master-a", RoomID: roomA.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	cavesA := rooms.World{ID: "caves-a", RoomID: roomA.ID, DirectoryName: "Caves", Name: "洞穴", Role: rooms.WorldRoleCaves}
	masterB := rooms.World{ID: "master-b", RoomID: roomB.ID, DirectoryName: "Master", Name: "商店服地表", Role: rooms.WorldRoleMaster}
	catalog := topologyRoomCatalog{
		rooms:  []rooms.Room{roomA, roomB},
		worlds: map[string][]rooms.World{roomA.ID: {masterA, cavesA}, roomB.ID: {masterB}},
	}
	localInventory := runtimeInventory(
		agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		4, 4, []shared.RoomInventoryReport{
			inventoryRoom("Cluster_A", "Master", "Caves"), inventoryRoom("Cluster_B", "Master"),
		}, nil, now,
	)
	remoteInventory := runtimeInventory(
		agents.RuntimeTarget{ID: "agent:node-a", AgentID: "node-a", Name: "节点 A", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		2, 2, []shared.RoomInventoryReport{inventoryRoom("Cluster_A", "Caves")},
		[]shared.ShardProcessReport{{PID: 900, Cluster: "Unmanaged", Shard: "Master"}}, now,
	)
	service, err := NewService(catalog, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{localInventory, remoteInventory}}, newTopologyTestStore(t))
	if err != nil {
		t.Fatal(err)
	}

	current, err := service.Topology(context.Background(), roomA.ID)
	if err != nil {
		t.Fatal(err)
	}
	local := topologyTarget(t, current, localTargetID)
	if local.PlannedShards != 3 || local.ProjectedShards != 3 || local.ProjectedCapacity.State != agents.CapacityFull {
		t.Fatalf("local target=%#v", local)
	}
	request := UpdateRequest{
		ExpectedRevision: current.Revision,
		Placements: []PlacementInput{
			{WorldID: masterA.ID, TargetID: localTargetID},
			{WorldID: cavesA.ID, TargetID: "agent:node-a"},
		},
	}
	preview, err := service.Preview(context.Background(), roomA.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	remote := topologyTarget(t, preview, "agent:node-a")
	if remote.PlannedShards != 1 || remote.UnmanagedRunningShards != 1 || remote.ProjectedShards != 2 || remote.ProjectedCapacity.State != agents.CapacityOvercommitted || !preview.RequiresOvercommitConfirmation {
		t.Fatalf("remote target=%#v preview=%#v", remote, preview)
	}
	if _, err := service.Update(context.Background(), roomA.ID, request); !errors.Is(err, ErrOvercommit) {
		t.Fatalf("update without confirmation error=%v", err)
	}
	request.AllowOvercommit = true
	updated, err := service.Update(context.Background(), roomA.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	placement := topologyPlacement(t, updated, cavesA.ID)
	if placement.DesiredTargetID != "agent:node-a" || placement.AppliedTargetID != localTargetID || placement.State != PlacementPlanned {
		t.Fatalf("placement=%#v", placement)
	}
	if updated.RemoteExecutionReady || updated.Mode != "planning_only" || updated.CapacityPolicy.Enforced {
		t.Fatalf("unsafe topology mode=%#v", updated)
	}
}

func TestTopologyReportsDuplicateRuntimeAndValidatesCompletePlacement(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Room", Managed: true}
	world := rooms.World{ID: "world", RoomID: room.ID, DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}
	catalog := topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}
	local := runtimeInventory(
		agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		4, 4, []shared.RoomInventoryReport{inventoryRoom("Cluster", "Master")},
		[]shared.ShardProcessReport{{PID: 1, Cluster: "Cluster", Shard: "Master"}}, now,
	)
	remote := runtimeInventory(
		agents.RuntimeTarget{ID: "agent:node", AgentID: "node", Name: "远程", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		4, 4, []shared.RoomInventoryReport{inventoryRoom("Cluster", "Master")},
		[]shared.ShardProcessReport{{PID: 2, Cluster: "Cluster", Shard: "Master"}}, now,
	)
	service, _ := NewService(catalog, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local, remote}}, newTopologyTestStore(t))
	snapshot, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if placement := topologyPlacement(t, snapshot, world.ID); placement.State != PlacementConflict || len(placement.ObservedTargetIDs) != 2 {
		t.Fatalf("placement=%#v", placement)
	}
	_, err = service.Preview(context.Background(), room.ID, UpdateRequest{ExpectedRevision: snapshot.Revision, Placements: []PlacementInput{}})
	var fields *FieldError
	if !errors.As(err, &fields) || fields.Fields["placements"] == "" {
		t.Fatalf("incomplete placement error=%v", err)
	}
}

func TestTopologyIgnoresStaleRuntimeAsConflictEvidence(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Room", Managed: true}
	world := rooms.World{ID: "world", RoomID: room.ID, DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}
	catalog := topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}
	local := runtimeInventory(
		agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		4, 4, []shared.RoomInventoryReport{inventoryRoom("Cluster", "Master")},
		[]shared.ShardProcessReport{{PID: 1, Cluster: "Cluster", Shard: "Master"}}, now,
	)
	remote := runtimeInventory(
		agents.RuntimeTarget{ID: "agent:node", AgentID: "node", Name: "远程", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusOffline, Online: false, Configured: true},
		4, 4, []shared.RoomInventoryReport{inventoryRoom("Cluster", "Master")},
		[]shared.ShardProcessReport{{PID: 2, Cluster: "Cluster", Shard: "Master"}}, now.Add(-time.Hour),
	)
	remote.Stale = true
	remote.StaleReason = "agent_offline"
	remote.Capacity = agents.CapacityFor(4, 4, false, 1, true)

	service, _ := NewService(catalog, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local, remote}}, newTopologyTestStore(t))
	snapshot, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	placement := topologyPlacement(t, snapshot, world.ID)
	if placement.State != PlacementAligned || len(placement.ObservedTargetIDs) != 1 || placement.ObservedTargetIDs[0] != localTargetID {
		t.Fatalf("stale runtime created false conflict: %#v", placement)
	}
}

func runtimeInventory(target agents.RuntimeTarget, logical, physical int, inventoryRooms []shared.RoomInventoryReport, processes []shared.ShardProcessReport, observedAt time.Time) agents.RuntimeTargetInventory {
	report := shared.RuntimeInventoryReport{
		ProtocolVersion: shared.RuntimeInventoryProtocolVersion, ObservedAt: observedAt,
		CPU:   shared.CPUInventory{LogicalProcessors: logical, PhysicalCores: physical},
		Rooms: inventoryRooms, Processes: processes,
	}
	return agents.RuntimeTargetInventory{
		Target: target, Available: true, Inventory: report,
		Capacity:   agents.CapacityFor(logical, physical, false, len(processes), false),
		ObservedAt: &observedAt, ReceivedAt: &observedAt,
	}
}

func inventoryRoom(directory string, shards ...string) shared.RoomInventoryReport {
	value := shared.RoomInventoryReport{Directory: directory, Shards: make([]shared.ShardInventoryReport, 0, len(shards))}
	for _, shard := range shards {
		value.Shards = append(value.Shards, shared.ShardInventoryReport{Directory: shard})
	}
	return value
}

func topologyTarget(t *testing.T, snapshot Snapshot, targetID string) TargetSummary {
	t.Helper()
	for _, target := range snapshot.Targets {
		if target.ID == targetID {
			return target
		}
	}
	t.Fatalf("target %s not found in %#v", targetID, snapshot.Targets)
	return TargetSummary{}
}

func topologyPlacement(t *testing.T, snapshot Snapshot, worldID string) Placement {
	t.Helper()
	for _, placement := range snapshot.Placements {
		if placement.WorldID == worldID {
			return placement
		}
	}
	t.Fatalf("placement %s not found in %#v", worldID, snapshot.Placements)
	return Placement{}
}
