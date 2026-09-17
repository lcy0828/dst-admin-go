package gameupdate

import (
	"context"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"
)

type releaseSnapshotRooms struct {
	room   rooms.Room
	worlds []rooms.World
}

func (c releaseSnapshotRooms) List() ([]rooms.Room, error) { return []rooms.Room{c.room}, nil }
func (c releaseSnapshotRooms) Worlds(string) ([]rooms.World, error) {
	return append([]rooms.World(nil), c.worlds...), nil
}

type releaseSnapshotPlacements map[string]topology.ExecutionPlacement

func (p releaseSnapshotPlacements) AppliedPlacement(_, worldID string) (topology.ExecutionPlacement, error) {
	return p[worldID], nil
}

type releaseSnapshotTargets []agents.RuntimeTargetInventory

func (t releaseSnapshotTargets) RuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error) {
	return append([]agents.RuntimeTargetInventory(nil), t...), nil
}

func TestReleaseSnapshotSelectsTheAppliedInstallationInventory(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true}
	caves := rooms.World{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Name: "Caves"}
	target := agents.RuntimeTarget{
		ID: "agent:node", AgentID: "node", Name: "Node", Kind: agents.RuntimeKindAgent,
		Status: agents.RuntimeStatusReady, Online: true, Configured: true,
		DefaultInstallationID: "primary",
		Installations:         []agents.RuntimeInstallation{{ID: "primary"}, {ID: "testing"}},
	}
	inventory := func(installationID, shard string) agents.RuntimeTargetInventory {
		current := target
		current.Config.InstallationID = installationID
		observedAt := now
		return agents.RuntimeTargetInventory{
			Target: current, Available: true, ObservedAt: &observedAt, ReceivedAt: &observedAt,
			Inventory: shared.RuntimeInventoryReport{
				ProtocolVersion: shared.RuntimeInventoryProtocolVersion, ObservedAt: now,
				Installation: shared.RuntimeInstallationReport{ID: installationID},
				Rooms: []shared.RoomInventoryReport{{
					Directory: room.DirectoryName,
					Shards:    []shared.ShardInventoryReport{{Directory: shard}},
				}},
			},
		}
	}
	service, err := NewTopologyReleaseSnapshot(
		releaseSnapshotRooms{room: room, worlds: []rooms.World{master, caves}},
		releaseSnapshotPlacements{
			master.ID: {Revision: "revision", AppliedTargetID: target.ID, AppliedInstallationID: "primary"},
			caves.ID:  {Revision: "revision", AppliedTargetID: target.ID, AppliedInstallationID: "testing"},
		},
		releaseSnapshotTargets{inventory("primary", master.DirectoryName), inventory("testing", caves.DirectoryName)},
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Shards) != 2 {
		t.Fatalf("shards=%#v", snapshot.Shards)
	}
	for _, shard := range snapshot.Shards {
		expected := "primary"
		if shard.World.ID == caves.ID {
			expected = "testing"
		}
		if shard.Target.Config.InstallationID != expected || !shard.InventoryHasShard {
			t.Fatalf("shard=%#v", shard)
		}
	}
}

func TestReleaseSnapshotResolvesImplicitDefaultInstallations(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true}
	caves := rooms.World{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Name: "Caves"}
	inventory := func(target agents.RuntimeTarget, installationID, shard string) agents.RuntimeTargetInventory {
		target.Config.InstallationID = installationID
		return agents.RuntimeTargetInventory{
			Target: target, Available: true, ObservedAt: &now, ReceivedAt: &now,
			Inventory: shared.RuntimeInventoryReport{
				ProtocolVersion: shared.RuntimeInventoryProtocolVersion, ObservedAt: now,
				Installation: shared.RuntimeInstallationReport{ID: installationID},
				Rooms: []shared.RoomInventoryReport{{
					Directory: room.DirectoryName,
					Shards:    []shared.ShardInventoryReport{{Directory: shard}},
				}},
			},
		}
	}
	local := agents.RuntimeTarget{
		ID: "local", Name: "Local", Kind: agents.RuntimeKindLocal, Online: true, Configured: true,
		DefaultInstallationID: "steam-client", Installations: []agents.RuntimeInstallation{{ID: "steam-client"}},
	}
	remote := agents.RuntimeTarget{
		ID: "agent:node", AgentID: "node", Name: "Debian", Kind: agents.RuntimeKindAgent, Online: true, Configured: true,
		DefaultInstallationID: "native", Installations: []agents.RuntimeInstallation{{ID: "native"}},
	}
	service, err := NewTopologyReleaseSnapshot(
		releaseSnapshotRooms{room: room, worlds: []rooms.World{master, caves}},
		releaseSnapshotPlacements{
			master.ID: {Revision: "revision", AppliedTargetID: local.ID},
			caves.ID:  {Revision: "revision", AppliedTargetID: remote.ID, AppliedInstallationID: "default"},
		},
		releaseSnapshotTargets{
			inventory(local, "steam-client", master.DirectoryName),
			inventory(remote, "native", caves.DirectoryName),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Shards) != 2 {
		t.Fatalf("shards=%#v", snapshot.Shards)
	}
	for _, shard := range snapshot.Shards {
		expectedID, expectedName := "steam-client", "Local"
		if shard.World.ID == caves.ID {
			expectedID, expectedName = "native", "Debian"
		}
		if shard.Target.Config.InstallationID != expectedID || shard.Target.Name != expectedName || !shard.Target.Online || !shard.InventoryAvailable || shard.InventoryStale || !shard.InventoryHasShard {
			t.Fatalf("shard=%#v", shard)
		}
	}
}
