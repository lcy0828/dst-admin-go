package topology

import (
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/shared"
)

func TestMachineWorldsFollowAppliedPlacementAndKeepOfflineObservations(t *testing.T) {
	now := time.Now()
	room, master := resourceRoom("room-a", "Cluster_A")
	caves := master
	caves.ID = "caves"
	caves.DirectoryName = "Caves"
	caves.Name = "Caves"
	plans := map[string]roomPlan{room.ID: {room: room, worlds: []rooms.World{master, caves}, record: record{Placements: []storedPlacement{
		{WorldID: master.ID, AppliedTargetID: "local", AppliedInstallationID: "default", DesiredTargetID: "agent:other", DesiredInstallationID: "default"},
		{WorldID: caves.ID, AppliedTargetID: "agent:node", AppliedInstallationID: "default"},
	}}}}
	local := agents.RuntimeTargetInventory{Target: agents.RuntimeTarget{ID: "local", Online: true, Config: agents.RuntimeConfig{InstallationID: "default"}}, Available: true, ObservedAt: &now,
		Inventory: shared.RuntimeInventoryReport{Rooms: []shared.RoomInventoryReport{{Directory: room.DirectoryName, Shards: []shared.ShardInventoryReport{{Directory: master.DirectoryName}}}}, Processes: []shared.ShardProcessReport{{Cluster: room.DirectoryName, Shard: master.DirectoryName}}}}
	remote := agents.RuntimeTargetInventory{Target: agents.RuntimeTarget{ID: "agent:node", Online: false, Config: agents.RuntimeConfig{InstallationID: "default"}}, Available: true, Stale: true, ObservedAt: &now,
		Inventory: shared.RuntimeInventoryReport{Rooms: []shared.RoomInventoryReport{{Directory: room.DirectoryName, Shards: []shared.ShardInventoryReport{{Directory: "Caves"}}}}, Processes: []shared.ShardProcessReport{{Cluster: room.DirectoryName, Shard: "Caves"}}}}
	values := machineObservations(plans, []agents.RuntimeTargetInventory{local, remote})
	if len(values[0].Worlds) != 1 || values[0].Worlds[0].WorldID != master.ID || !values[0].Worlds[0].Known || !values[0].Worlds[0].Running {
		t.Fatalf("wrong local placement: %#v", values[0])
	}
	if len(values[1].Worlds) != 1 || values[1].Worlds[0].WorldID != caves.ID || values[1].Worlds[0].Known || !values[1].Worlds[0].Running {
		t.Fatalf("lost offline observation: %#v", values[1])
	}
	local.Inventory.Rooms = nil
	local.Inventory.Processes = nil
	missing := machineObservations(plans, []agents.RuntimeTargetInventory{local})
	if missing[0].Worlds[0].Known {
		t.Fatal("missing world misreported as stopped")
	}
}
