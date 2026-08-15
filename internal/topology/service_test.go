package topology

import (
	"context"
	"errors"
	"hash/fnv"
	"strings"
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
	if !updated.RemoteExecutionReady || updated.Mode != "applied_placement" || updated.CapacityPolicy.Enforced {
		t.Fatalf("topology execution mode=%#v", updated)
	}
}

func TestPreviewStartCapacityCountsRunningShardsAcrossRooms(t *testing.T) {
	now := time.Now().UTC()
	roomA := rooms.Room{ID: "room-a", DirectoryName: "Cluster_A", Name: "A", Managed: true}
	roomB := rooms.Room{ID: "room-b", DirectoryName: "Cluster_B", Name: "B", Managed: true}
	masterA := rooms.World{ID: "master-a", RoomID: roomA.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	cavesA := rooms.World{ID: "caves-a", RoomID: roomA.ID, DirectoryName: "Caves", Name: "洞穴", Role: rooms.WorldRoleCaves}
	masterB := rooms.World{ID: "master-b", RoomID: roomB.ID, DirectoryName: "Master", Name: "另一房间地表", Role: rooms.WorldRoleMaster}
	catalog := topologyRoomCatalog{
		rooms: []rooms.Room{roomA, roomB},
		worlds: map[string][]rooms.World{
			roomA.ID: {masterA, cavesA},
			roomB.ID: {masterB},
		},
	}
	local := runtimeInventory(
		agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		4, 4,
		[]shared.RoomInventoryReport{inventoryRoom("Cluster_A", "Master", "Caves"), inventoryRoom("Cluster_B", "Master")},
		[]shared.ShardProcessReport{{PID: 101, Cluster: "Cluster_B", Shard: "Master"}}, now,
	)
	service, err := NewService(catalog, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local}}, newTopologyTestStore(t))
	if err != nil {
		t.Fatal(err)
	}

	preview, err := service.PreviewStartCapacity(context.Background(), roomA.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Targets) != 1 {
		t.Fatalf("targets = %#v", preview.Targets)
	}
	target := preview.Targets[0]
	if target.CurrentRunningShards != 1 || target.StartingShards != 2 || target.ProjectedRunningShards != 3 {
		t.Fatalf("target = %#v", target)
	}
	if target.Capacity.State != agents.CapacityFull || preview.RequiresRiskConfirmation {
		t.Fatalf("full capacity should be allowed without risk confirmation: %#v", preview)
	}
	if preview.Policy.ShardsPerPhysicalCore != 1 || preview.Policy.ReservedPhysicalCores != 1 || preview.Policy.Enforced {
		t.Fatalf("policy = %#v", preview.Policy)
	}

	local.Inventory.Processes = append(local.Inventory.Processes, shared.ShardProcessReport{PID: 102, Cluster: "Cluster_A", Shard: "Master"})
	service, err = NewService(catalog, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local}}, newTopologyTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	preview, err = service.PreviewStartCapacity(context.Background(), roomA.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	target = preview.Targets[0]
	if target.CurrentRunningShards != 2 || target.StartingShards != 1 || target.ProjectedRunningShards != 3 {
		t.Fatalf("already-running shard was counted twice: %#v", target)
	}
}

func TestPreviewStartCapacityRequiresConfirmationForOvercommitOrUnknownCapacity(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	caves := rooms.World{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Name: "洞穴", Role: rooms.WorldRoleCaves}
	catalog := topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {master, caves}}}

	t.Run("overcommitted", func(t *testing.T) {
		local := runtimeInventory(
			agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
			3, 3, []shared.RoomInventoryReport{inventoryRoom("Cluster", "Master", "Caves")}, nil, now,
		)
		service, err := NewService(catalog, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local}}, newTopologyTestStore(t))
		if err != nil {
			t.Fatal(err)
		}
		preview, err := service.PreviewStartCapacity(context.Background(), room.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(preview.Targets) != 1 || preview.Targets[0].Capacity.State != agents.CapacityFull || preview.RequiresRiskConfirmation {
			t.Fatalf("projected-at-limit preview = %#v", preview)
		}
		local.Inventory.Processes = append(local.Inventory.Processes, shared.ShardProcessReport{PID: 1, Cluster: "Other", Shard: "Master"})
		service, err = NewService(catalog, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local}}, newTopologyTestStore(t))
		if err != nil {
			t.Fatal(err)
		}
		preview, err = service.PreviewStartCapacity(context.Background(), room.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if preview.Targets[0].Capacity.State != agents.CapacityOvercommitted || !preview.RequiresRiskConfirmation {
			t.Fatalf("overcommit preview = %#v", preview)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		local := runtimeInventory(
			agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
			4, 4, []shared.RoomInventoryReport{inventoryRoom("Cluster", "Master", "Caves")}, nil, now,
		)
		local.Stale = true
		local.StaleReason = "collection_failed"
		service, err := NewService(catalog, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local}}, newTopologyTestStore(t))
		if err != nil {
			t.Fatal(err)
		}
		preview, err := service.PreviewStartCapacity(context.Background(), room.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if preview.Targets[0].Capacity.State != agents.CapacityUnknown || !preview.RequiresRiskConfirmation {
			t.Fatalf("unknown preview = %#v", preview)
		}
	})
}

func TestPreviewBatchStartCapacityMergesConcurrentRoomsBeforeCapacityDecision(t *testing.T) {
	now := time.Now().UTC()
	roomA := rooms.Room{ID: "room-a", DirectoryName: "Cluster_A", Name: "A", Managed: true}
	roomB := rooms.Room{ID: "room-b", DirectoryName: "Cluster_B", Name: "B", Managed: true}
	masterA := rooms.World{ID: "master-a", RoomID: roomA.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	cavesA := rooms.World{ID: "caves-a", RoomID: roomA.ID, DirectoryName: "Caves", Name: "洞穴", Role: rooms.WorldRoleCaves}
	masterB := rooms.World{ID: "master-b", RoomID: roomB.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	catalog := topologyRoomCatalog{
		rooms: []rooms.Room{roomA, roomB},
		worlds: map[string][]rooms.World{
			roomA.ID: {masterA, cavesA},
			roomB.ID: {masterB},
		},
	}
	local := runtimeInventory(
		agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		3, 3,
		[]shared.RoomInventoryReport{inventoryRoom("Cluster_A", "Master", "Caves"), inventoryRoom("Cluster_B", "Master")},
		nil, now,
	)
	service, err := NewService(catalog, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local}}, newTopologyTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	roomAPreview, err := service.PreviewStartCapacity(context.Background(), roomA.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	roomBPreview, err := service.PreviewStartCapacity(context.Background(), roomB.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if roomAPreview.RequiresRiskConfirmation || roomBPreview.RequiresRiskConfirmation {
		t.Fatalf("individual previews unexpectedly required confirmation: A=%#v B=%#v", roomAPreview, roomBPreview)
	}
	batch, err := service.PreviewBatchStartCapacity(context.Background(), []StartCapacitySelection{
		{RoomID: roomA.ID},
		{RoomID: roomB.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Targets) != 1 || batch.Targets[0].CurrentRunningShards != 0 ||
		batch.Targets[0].StartingShards != 3 || batch.Targets[0].ProjectedRunningShards != 3 ||
		batch.Targets[0].Capacity.State != agents.CapacityOvercommitted || !batch.RequiresRiskConfirmation {
		t.Fatalf("batch preview = %#v", batch)
	}
	if len(batch.Rooms) != 2 || len(batch.Rooms[0].WorldIDs) != 2 || len(batch.Rooms[1].WorldIDs) != 1 {
		t.Fatalf("normalized rooms = %#v", batch.Rooms)
	}
	if _, err := service.PreviewBatchStartCapacity(context.Background(), []StartCapacitySelection{{RoomID: roomA.ID}, {RoomID: roomA.ID}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate room error = %v", err)
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
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(strings.ToLower(directory)))
	base := 10000 + int(hash.Sum32()%1000)*20
	value := shared.RoomInventoryReport{Directory: directory, MasterPort: base, Shards: make([]shared.ShardInventoryReport, 0, len(shards))}
	for _, shard := range shards {
		offset := 7
		role := "secondary"
		switch strings.ToLower(shard) {
		case "master":
			offset, role = 1, "master"
		case "caves":
			offset = 4
		}
		value.Shards = append(value.Shards, shared.ShardInventoryReport{
			Directory: shard, Role: role, ServerPort: base + offset,
			AuthenticationPort: base + offset + 1, MasterServerPort: base + offset + 2,
		})
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

func TestResolveExecutionUsesAppliedTargetWithoutDesiredFallback(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room-a", DirectoryName: "Cluster_A", Name: "A", Managed: true}
	world := rooms.World{ID: "master-a", RoomID: room.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	local := runtimeInventory(
		agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		4, 4, []shared.RoomInventoryReport{inventoryRoom("Cluster_A", "Master")}, nil, now,
	)
	remote := runtimeInventory(
		agents.RuntimeTarget{ID: "agent:node", AgentID: "node", Name: "远程", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true, Capabilities: []string{"shard.control.v1"}},
		4, 4, []shared.RoomInventoryReport{inventoryRoom("Cluster_A", "Master")}, nil, now,
	)
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local, remote}}, newTopologyTestStore(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Update(context.Background(), room.ID, UpdateRequest{
		ExpectedRevision: current.Revision, Placements: []PlacementInput{{WorldID: world.ID, TargetID: "agent:node"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ResolveExecution(context.Background(), room.ID, world.ID)
	if err != nil || resolved.AppliedTargetID != localTargetID || resolved.DesiredTargetID != "agent:node" {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
}

func TestResolveExecutionRequiresFreshAppliedAgentAndRejectsConflict(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room-a", DirectoryName: "Cluster_A", Name: "A", Managed: true}
	world := rooms.World{ID: "master-a", RoomID: room.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	remoteTarget := agents.RuntimeTarget{
		ID: "agent:node", AgentID: "node", Name: "远程", Kind: agents.RuntimeKindAgent,
		Status: agents.RuntimeStatusReady, Online: true, Configured: true, Capabilities: []string{"shard.control.v1"},
	}
	remote := runtimeInventory(remoteTarget, 4, 4, []shared.RoomInventoryReport{inventoryRoom("Cluster_A", "Master")}, nil, now)
	store := newTopologyTestStore(t)
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{remote}}, store,
	)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Ensure(room.ID, []string{world.ID})
	if err != nil {
		t.Fatal(err)
	}
	record.Placements[0].DesiredTargetID, record.Placements[0].AppliedTargetID = "agent:node", "agent:node"
	if _, err := store.Save(room.ID, record.Revision, record.Placements); err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ResolveExecution(context.Background(), room.ID, world.ID)
	if err != nil || resolved.Target.AgentID != "node" {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}

	stale := remote
	stale.Stale = true
	stale.StaleReason = "report_expired"
	staleService, _ := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{stale}}, store,
	)
	if _, err := staleService.ResolveExecution(context.Background(), room.ID, world.ID); !executionErrorCode(err, "APPLIED_INVENTORY_STALE") {
		t.Fatalf("stale error=%v", err)
	}

	conflictRemote := remote
	conflictRemote.Inventory.Processes = []shared.ShardProcessReport{{PID: 1, Cluster: "Cluster_A", Shard: "Master"}, {PID: 2, Cluster: "Cluster_A", Shard: "Master"}}
	conflictService, _ := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{conflictRemote}}, store,
	)
	if _, err := conflictService.ResolveExecution(context.Background(), room.ID, world.ID); !executionErrorCode(err, "SHARD_RUNTIME_CONFLICT") {
		t.Fatalf("conflict error=%v", err)
	}
}

func executionErrorCode(err error, code string) bool {
	var executionError *ExecutionError
	return errors.As(err, &executionError) && executionError.Code == code
}

func TestPrepareAndApplyMigrationMovesOnlyAppliedPlacement(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room-migrate", DirectoryName: "Cluster_Migrate", Name: "迁移房间", Managed: true}
	world := rooms.World{ID: "world-master", RoomID: room.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	local := runtimeInventory(
		agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		4, 4, []shared.RoomInventoryReport{inventoryRoom(room.DirectoryName, world.DirectoryName)}, nil, now,
	)
	remote := runtimeInventory(
		agents.RuntimeTarget{ID: "agent:node", AgentID: "node", Name: "节点", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true, Capabilities: []string{"runtime.migration.v1"}},
		4, 4, nil, nil, now,
	)
	store := newTopologyTestStore(t)
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local, remote}}, store,
	)
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := service.Update(context.Background(), room.ID, UpdateRequest{
		ExpectedRevision: current.Revision,
		Placements:       []PlacementInput{{WorldID: world.ID, TargetID: remote.Target.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PrepareMigration(context.Background(), room.ID, world.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceTargetID != localTargetID || plan.TargetTargetID != remote.Target.ID || plan.Revision != planned.Revision {
		t.Fatalf("migration plan=%#v", plan)
	}
	applied, err := service.ApplyMigration(room.ID, world.ID, plan.Revision, plan.TargetTargetID)
	if err != nil {
		t.Fatal(err)
	}
	if applied.AppliedTargetID != remote.Target.ID || applied.DesiredTargetID != remote.Target.ID || applied.Revision == plan.Revision {
		t.Fatalf("applied placement=%#v", applied)
	}
	if _, err := service.ApplyMigration(room.ID, world.ID, plan.Revision, plan.TargetTargetID); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale apply error=%v", err)
	}
}

func TestAppliedRemoteWorldSurvivesMissingLocalDirectory(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room-remote", DirectoryName: "Cluster_Remote", Name: "远程房间", Managed: true}
	world := rooms.World{ID: "world-master", RoomID: room.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster, IsMaster: true}
	catalog := topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}
	local := runtimeInventory(
		agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		4, 4, []shared.RoomInventoryReport{inventoryRoom(room.DirectoryName, world.DirectoryName)}, nil, now,
	)
	remoteTarget := agents.RuntimeTarget{
		ID: "agent:node", AgentID: "node", Name: "节点", Kind: agents.RuntimeKindAgent,
		Status: agents.RuntimeStatusReady, Online: true, Configured: true,
		Capabilities: []string{"runtime.migration.v1", "shard.control.v1"},
	}
	remote := runtimeInventory(remoteTarget, 4, 4, nil, nil, now)
	targets := &topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local, remote}}
	service, err := NewService(catalog, targets, newTopologyTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := service.Update(context.Background(), room.ID, UpdateRequest{
		ExpectedRevision: current.Revision,
		Placements:       []PlacementInput{{WorldID: world.ID, TargetID: remoteTarget.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PrepareMigration(context.Background(), room.ID, world.ID)
	if err != nil {
		t.Fatal(err)
	}
	delete(catalog.worlds, room.ID)
	applied, err := service.ApplyMigration(room.ID, world.ID, plan.Revision, remoteTarget.ID)
	if err != nil {
		t.Fatalf("apply after source directory removal: %v", err)
	}
	if applied.World.DirectoryName != world.DirectoryName || applied.World.Role != world.Role || applied.Revision == planned.Revision {
		t.Fatalf("applied=%#v", applied)
	}
	local.Inventory.Rooms = nil
	remote.Inventory.Rooms = []shared.RoomInventoryReport{inventoryRoom(room.DirectoryName, world.DirectoryName)}
	targets.items = []agents.RuntimeTargetInventory{local, remote}
	snapshot, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatalf("topology after source directory removal: %v", err)
	}
	placement := topologyPlacement(t, snapshot, world.ID)
	if placement.AppliedTargetID != remoteTarget.ID || placement.DesiredTargetID != remoteTarget.ID || placement.WorldRole != rooms.WorldRoleMaster {
		t.Fatalf("remote placement was not retained: %#v", placement)
	}
	resolved, err := service.ResolveExecution(context.Background(), room.ID, world.ID)
	if err != nil {
		t.Fatalf("resolve remote world without local directory: %v", err)
	}
	if resolved.AppliedTargetID != remoteTarget.ID || resolved.World.DirectoryName != world.DirectoryName || resolved.World.Role != rooms.WorldRoleMaster {
		t.Fatalf("resolved remote world=%#v", resolved)
	}
}
