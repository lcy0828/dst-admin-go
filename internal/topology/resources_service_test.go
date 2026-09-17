package topology

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/shared"
)

func TestStoppedRoomPortOverlapIsAdvisory(t *testing.T) {
	now := time.Now().UTC()
	roomA, worldA := resourceRoom("room-a", "Cluster_A")
	roomB, worldB := resourceRoom("room-b", "Cluster_B")
	inventory := runtimeInventory(resourceLocalTarget(), 4, 4, []shared.RoomInventoryReport{
		resourceInventoryRoom(roomA.DirectoryName, 10889, 10999, 8767, 27017),
		resourceInventoryRoom(roomB.DirectoryName, 10890, 10999, 8768, 27018),
	}, nil, now)
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{roomA, roomB}, worlds: map[string][]rooms.World{roomA.ID: {worldA}, roomB.ID: {worldB}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{inventory}}, newTopologyTestStore(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Topology(context.Background(), roomA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Preview(context.Background(), roomA.ID, UpdateRequest{
		ExpectedRevision: snapshot.Revision,
		Placements:       []PlacementInput{{WorldID: worldA.ID, TargetID: localTargetID}},
	}); err != nil {
		t.Fatalf("stopped room overlap blocked planning: %v", err)
	}
	infrastructure, err := service.Infrastructure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !infrastructure.Preflight.Ready || len(infrastructure.Preflight.Conflicts) != 0 || !hasResourceAdvisory(infrastructure.Preflight, "UDP_PORT_CONFIGURATION_OVERLAP", 10999) {
		t.Fatalf("preflight=%#v", infrastructure.Preflight)
	}
	for _, reservation := range infrastructure.PortReservations {
		if reservation.RoomID != "" && reservation.State != ReservationConfigured {
			t.Fatalf("stopped reservation=%#v", reservation)
		}
	}
}

func TestEnrichProviderIPAddressesUsesCurrentRuntimeInventory(t *testing.T) {
	providers := []RuntimeProvider{{TargetID: "local"}, {TargetID: "agent:node"}, {TargetID: "agent:missing"}}
	inventories := []agents.RuntimeTargetInventory{
		{Target: agents.RuntimeTarget{ID: "local", IPAddresses: []string{"192.168.2.10"}}},
		{Target: agents.RuntimeTarget{ID: "agent:node", IPAddresses: []string{"10.0.0.12", "2001:db8::12"}}},
	}
	actual := enrichProviderIPAddresses(providers, inventories)
	if len(actual[0].IPAddresses) != 1 || actual[0].IPAddresses[0] != "192.168.2.10" {
		t.Fatalf("local addresses=%v", actual[0].IPAddresses)
	}
	if len(actual[1].IPAddresses) != 2 || actual[1].IPAddresses[0] != "10.0.0.12" {
		t.Fatalf("agent addresses=%v", actual[1].IPAddresses)
	}
	if actual[2].IPAddresses == nil || len(actual[2].IPAddresses) != 0 {
		t.Fatalf("missing addresses=%v", actual[2].IPAddresses)
	}
}

func TestRunningRoomPortOverlapBlocksStart(t *testing.T) {
	now := time.Now().UTC()
	roomA, worldA := resourceRoom("room-a", "Cluster_A")
	roomB, worldB := resourceRoom("room-b", "Cluster_B")
	inventory := runtimeInventory(resourceLocalTarget(), 4, 4, []shared.RoomInventoryReport{
		resourceInventoryRoom(roomA.DirectoryName, 10889, 10999, 8767, 27017),
		resourceInventoryRoom(roomB.DirectoryName, 10890, 10999, 8768, 27018),
	}, []shared.ShardProcessReport{{PID: 42, Cluster: roomB.DirectoryName, Shard: worldB.DirectoryName}}, now)
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{roomA, roomB}, worlds: map[string][]rooms.World{roomA.ID: {worldA}, roomB.ID: {worldB}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{inventory}}, newTopologyTestStore(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := service.PreflightExecution(context.Background(), roomA.ID, []string{worldA.ID})
	var conflict *ResourceConflictError
	if !errors.As(err, &conflict) || preflight.Ready || !hasResourceConflict(preflight, "UDP_PORT_CONFLICT", 10999) {
		t.Fatalf("port conflict=%#v err=%v", conflict, err)
	}
}

func TestSameUDPPortIsAllowedAcrossTargetNetworkScopes(t *testing.T) {
	now := time.Now().UTC()
	roomA, worldA := resourceRoom("room-a", "Cluster_A")
	roomB, worldB := resourceRoom("room-b", "Cluster_B")
	local := runtimeInventory(resourceLocalTarget(), 4, 4, []shared.RoomInventoryReport{
		resourceInventoryRoom(roomA.DirectoryName, 10889, 10999, 8767, 27017),
	}, nil, now)
	remoteTarget := agents.RuntimeTarget{ID: "agent:node", AgentID: "node", Name: "节点", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true}
	remote := runtimeInventory(remoteTarget, 4, 4, []shared.RoomInventoryReport{
		resourceInventoryRoom(roomB.DirectoryName, 10889, 10999, 8767, 27017),
	}, nil, now)
	store := newTopologyTestStore(t)
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{roomA, roomB}, worlds: map[string][]rooms.World{roomA.ID: {worldA}, roomB.ID: {worldB}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local, remote}}, store,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Topology(context.Background(), roomA.ID); err != nil {
		t.Fatal(err)
	}
	recordB, err := store.load(roomB.ID)
	if err != nil {
		t.Fatal(err)
	}
	recordB.Placements[0].DesiredTargetID = remoteTarget.ID
	recordB.Placements[0].AppliedTargetID = remoteTarget.ID
	if _, err := store.Save(roomB.ID, recordB.Revision, recordB.Placements); err != nil {
		t.Fatal(err)
	}
	infrastructure, err := service.Infrastructure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !infrastructure.Preflight.Ready {
		t.Fatalf("cross-target ports conflicted: %#v", infrastructure.Preflight)
	}
}

func TestManagedReservationsPersistActiveAndPlannedStates(t *testing.T) {
	now := time.Now().UTC()
	room, world := resourceRoom("room", "Cluster")
	local := runtimeInventory(resourceLocalTarget(), 4, 4, []shared.RoomInventoryReport{
		resourceInventoryRoom(room.DirectoryName, 10889, 10999, 8767, 27017),
	}, nil, now)
	remote := runtimeInventory(agents.RuntimeTarget{ID: "agent:node", AgentID: "node", Name: "节点", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true}, 4, 4, nil, nil, now)
	store := newTopologyTestStore(t)
	service, _ := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local, remote}}, store,
	)
	current, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(context.Background(), room.ID, UpdateRequest{
		ExpectedRevision: current.Revision,
		Placements:       []PlacementInput{{WorldID: world.ID, TargetID: remote.Target.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	_, _, _, reservations, allocations, err := store.RuntimeResources()
	if err != nil {
		t.Fatal(err)
	}
	states := map[ReservationState]int{}
	for _, reservation := range reservations {
		if reservation.RoomID == room.ID && reservation.WorldID == world.ID {
			states[reservation.State]++
		}
	}
	if states[ReservationConfigured] != 4 || states[ReservationPlanned] != 4 {
		t.Fatalf("reservation states=%#v reservations=%#v", states, reservations)
	}
	if len(allocations) != 1 || allocations[0].Policy != CPUPolicyNone || allocations[0].TargetID != localTargetID {
		t.Fatalf("allocations=%#v", allocations)
	}
}

func TestUnmanagedObservedShardBlocksManagedStartPort(t *testing.T) {
	now := time.Now().UTC()
	room, world := resourceRoom("room", "Cluster")
	inventory := runtimeInventory(resourceLocalTarget(), 4, 4, []shared.RoomInventoryReport{
		resourceInventoryRoom(room.DirectoryName, 10889, 10999, 8767, 27017),
		resourceInventoryRoom("Unmanaged", 10890, 10999, 8867, 28017),
	}, []shared.ShardProcessReport{{PID: 42, Cluster: "Unmanaged", Shard: "Master"}}, now)
	service, _ := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{inventory}}, newTopologyTestStore(t),
	)
	preflight, err := service.PreflightExecution(context.Background(), room.ID, []string{world.ID})
	var conflict *ResourceConflictError
	if !errors.As(err, &conflict) || preflight.Ready || !hasResourceConflict(preflight, "UDP_PORT_CONFLICT", 10999) {
		t.Fatalf("unmanaged preflight=%#v err=%v", preflight, err)
	}
}

func TestWildcardBindConflictsWithSpecificAddress(t *testing.T) {
	values := []PortReservation{
		{ID: "a", ScopeID: "host:local", Protocol: "udp", Port: 10999, BindAddress: "0.0.0.0", State: ReservationActive, RoomID: "a", WorldID: "master", Cluster: "A", Shard: "Master"},
		{ID: "b", ScopeID: "host:local", Protocol: "udp", Port: 10999, BindAddress: "127.0.0.1", State: ReservationActive, RoomID: "b", WorldID: "master", Cluster: "B", Shard: "Master"},
	}
	conflicts, advisories := detectPortConflicts(values, nil)
	if len(conflicts) != 1 || conflicts[0].Port != 10999 {
		t.Fatalf("conflicts=%#v", conflicts)
	}
	if len(advisories) != 0 {
		t.Fatalf("advisories=%#v", advisories)
	}
	values[0].BindAddress = "192.0.2.10"
	if conflicts, _ := detectPortConflicts(values, nil); len(conflicts) != 0 {
		t.Fatalf("different specific binds conflicted: %#v", conflicts)
	}
}

func TestBatchPreflightRejectsDuplicatePortsWithinSelectedStartSet(t *testing.T) {
	now := time.Now().UTC()
	roomA, worldA := resourceRoom("room-a", "Cluster_A")
	roomB, worldB := resourceRoom("room-b", "Cluster_B")
	inventory := runtimeInventory(resourceLocalTarget(), 4, 4, []shared.RoomInventoryReport{
		resourceInventoryRoom(roomA.DirectoryName, 10889, 10999, 8767, 27017),
		resourceInventoryRoom(roomB.DirectoryName, 10890, 10999, 8768, 27018),
	}, nil, now)
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{roomA, roomB}, worlds: map[string][]rooms.World{roomA.ID: {worldA}, roomB.ID: {worldB}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{inventory}}, newTopologyTestStore(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := service.PreflightBatchExecution(context.Background(), []StartCapacitySelection{
		{RoomID: roomA.ID, WorldIDs: []string{worldA.ID}},
		{RoomID: roomB.ID, WorldIDs: []string{worldB.ID}},
	})
	var conflict *ResourceConflictError
	if !errors.As(err, &conflict) || preflight.Ready || !hasResourceConflict(preflight, "UDP_PORT_CONFLICT", 10999) {
		t.Fatalf("batch preflight=%#v err=%v", preflight, err)
	}
}

func TestCrossNodeMasterEndpointPreflight(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		service, _, room, _, caves := crossNodePreflightFixture(t, "192.0.2.10")
		preflight, err := service.PreflightExecution(context.Background(), room.ID, []string{caves.ID})
		if err != nil || !preflight.Ready {
			t.Fatalf("preflight=%#v err=%v", preflight, err)
		}
	})

	for _, test := range []struct {
		name      string
		advertise string
		code      string
	}{
		{name: "missing address", advertise: "", code: "MASTER_ADVERTISE_ADDRESS_MISSING"},
		{name: "loopback address", advertise: "127.0.0.1", code: "MASTER_ADVERTISE_ADDRESS_UNROUTABLE"},
		{name: "unspecified address", advertise: "0.0.0.0", code: "MASTER_ADVERTISE_ADDRESS_UNROUTABLE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _, room, _, caves := crossNodePreflightFixture(t, test.advertise)
			preflight, err := service.PreflightExecution(context.Background(), room.ID, []string{caves.ID})
			var conflict *ResourceConflictError
			if !errors.As(err, &conflict) || preflight.Ready || !hasResourceConflictCode(preflight, test.code) {
				t.Fatalf("preflight=%#v err=%v", preflight, err)
			}
		})
	}
}

func TestCrossNodeMasterEndpointRejectsStaleAndMismatchedSecondary(t *testing.T) {
	service, inventories, room, _, caves := crossNodePreflightFixture(t, "192.0.2.10")
	inventories[1].Stale = true
	inventories[1].StaleReason = "heartbeat_expired"
	preflight, err := service.PreflightExecution(context.Background(), room.ID, []string{caves.ID})
	if err == nil || !hasResourceConflictCode(preflight, "SECONDARY_TARGET_INVENTORY_STALE") {
		t.Fatalf("stale preflight=%#v err=%v", preflight, err)
	}

	service, inventories, room, _, caves = crossNodePreflightFixture(t, "192.0.2.10")
	inventories[1].Inventory.Rooms[0].MasterIP = "192.0.2.11"
	inventories[1].Inventory.Rooms[0].MasterPort = 10890
	preflight, err = service.PreflightExecution(context.Background(), room.ID, []string{caves.ID})
	if err == nil || !hasResourceConflictCode(preflight, "SECONDARY_MASTER_ENDPOINT_MISMATCH") || !hasResourceConflictCode(preflight, "MASTER_PORT_INCONSISTENT") {
		t.Fatalf("mismatch preflight=%#v err=%v", preflight, err)
	}
}

func TestTopologyPreviewAllowsSelectedShardLinkToReplaceObservedEndpoint(t *testing.T) {
	service, inventories, room, master, caves := crossNodePreflightFixture(t, "192.0.2.10")
	inventories[1].Inventory.Rooms[0].MasterIP = "192.0.2.11"
	snapshot, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Preview(context.Background(), room.ID, UpdateRequest{
		ExpectedRevision: snapshot.Revision,
		Placements: []PlacementInput{
			{WorldID: master.ID, TargetID: localTargetID},
			{WorldID: caves.ID, TargetID: "agent:secondary"},
		},
		ShardLinks: []ShardLinkInput{{
			SourceTargetID: "agent:secondary", Address: "192.0.2.10", Port: 10889, Mode: ShardLinkLAN,
		}},
	})
	if err != nil {
		t.Fatalf("selected route should replace observed Secondary configuration after save: %v", err)
	}
}

func TestTopologyRouteUpdateKeepsAppliedRouteSeparate(t *testing.T) {
	service, _, room, master, caves := crossNodePreflightFixture(t, "192.0.2.10")
	current, err := service.Topology(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(context.Background(), room.ID, UpdateRequest{
		ExpectedRevision: current.Revision,
		Placements: []PlacementInput{
			{WorldID: master.ID, TargetID: localTargetID},
			{WorldID: caves.ID, TargetID: "agent:secondary"},
		},
		ShardLinks: []ShardLinkInput{{
			SourceTargetID: "agent:secondary", Address: "192.0.2.20", Port: 10889, Mode: ShardLinkLAN,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.ShardLinks) != 1 || updated.ShardLinks[0].Address != "192.0.2.20" {
		t.Fatalf("desired links=%#v", updated.ShardLinks)
	}
	if len(updated.AppliedShardLinks) != 0 {
		t.Fatalf("route plan changed current route=%#v", updated.AppliedShardLinks)
	}
	resolved, err := service.ResolveAppliedShardLinks(context.Background(), room.ID)
	if err != nil || len(resolved) != 0 {
		t.Fatalf("resolved applied links=%#v err=%v", resolved, err)
	}
}

func TestSameNodeRoomAllowsLocalMasterEndpoint(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room-local", DirectoryName: "Cluster_Local", Name: "本机房间", Managed: true}
	master := rooms.World{ID: "world-master", RoomID: room.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster, IsMaster: true}
	caves := rooms.World{ID: "world-caves", RoomID: room.ID, DirectoryName: "Caves", Name: "洞穴", Role: rooms.WorldRoleCaves}
	inventory := runtimeInventory(resourceLocalTarget(), 4, 4, []shared.RoomInventoryReport{{
		Directory: room.DirectoryName, BindIP: "127.0.0.1", MasterIP: "127.0.0.1", MasterPort: 10889,
		Shards: []shared.ShardInventoryReport{
			{Directory: "Master", Role: "master", ServerPort: 10999, AuthenticationPort: 8767, MasterServerPort: 27017},
			{Directory: "Caves", Role: "secondary", ServerPort: 10998, AuthenticationPort: 8768, MasterServerPort: 27018},
		},
	}}, nil, now)
	store := newTopologyTestStore(t)
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {master, caves}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{inventory}}, store,
	)
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := service.PreflightExecution(context.Background(), room.ID, nil)
	if err != nil || !preflight.Ready {
		t.Fatalf("same-node preflight=%#v err=%v", preflight, err)
	}
	for _, world := range []rooms.World{master, caves} {
		allocation, allocationErr := store.CPUAllocation(room.ID, world.ID)
		if allocationErr != nil || allocation.Policy != CPUPolicyNone || allocation.TargetID != localTargetID {
			t.Fatalf("world %s allocation=%#v err=%v", world.ID, allocation, allocationErr)
		}
	}
}

func crossNodePreflightFixture(t *testing.T, advertiseAddress string) (*Service, []agents.RuntimeTargetInventory, rooms.Room, rooms.World, rooms.World) {
	t.Helper()
	now := time.Now().UTC()
	room := rooms.Room{ID: "room-cross-node", DirectoryName: "Cluster_Cross_Node", Name: "跨节点房间", Managed: true}
	master := rooms.World{ID: "world-master", RoomID: room.ID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster, IsMaster: true}
	caves := rooms.World{ID: "world-caves", RoomID: room.ID, DirectoryName: "Caves", Name: "洞穴", Role: rooms.WorldRoleCaves}
	remoteTarget := agents.RuntimeTarget{ID: "agent:secondary", AgentID: "secondary", Name: "Secondary", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true, OS: "linux", Arch: "amd64"}
	inventories := []agents.RuntimeTargetInventory{
		runtimeInventory(resourceLocalTarget(), 4, 4, []shared.RoomInventoryReport{{
			Directory: room.DirectoryName, BindIP: "0.0.0.0", MasterIP: "127.0.0.1", MasterPort: 10889,
			Shards: []shared.ShardInventoryReport{{Directory: "Master", Role: "master", ServerPort: 10999, AuthenticationPort: 8767, MasterServerPort: 27017}},
		}}, nil, now),
		runtimeInventory(remoteTarget, 4, 4, []shared.RoomInventoryReport{{
			Directory: room.DirectoryName, BindIP: "0.0.0.0", MasterIP: advertiseAddress, MasterPort: 10889,
			Shards: []shared.ShardInventoryReport{{Directory: "Caves", Role: "secondary", ServerPort: 10998, AuthenticationPort: 8768, MasterServerPort: 27018}},
		}}, nil, now),
	}
	store := newTopologyTestStore(t)
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {master, caves}}},
		topologyTargetCatalog{items: inventories}, store,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Topology(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	record, err := store.load(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	for index := range record.Placements {
		targetID := localTargetID
		if record.Placements[index].WorldID == caves.ID {
			targetID = remoteTarget.ID
		}
		record.Placements[index].DesiredTargetID = targetID
		record.Placements[index].AppliedTargetID = targetID
	}
	if _, err := store.Save(room.ID, record.Revision, record.Placements); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateNetworkProfile(networkResourceID(localTargetID), NetworkProfileUpdate{Name: "Master 网络", BindAddress: "0.0.0.0", AdvertiseAddress: advertiseAddress}); err != nil {
		t.Fatal(err)
	}
	return service, inventories, room, master, caves
}

func TestCPUAllocationPoliciesAndSMTSiblingSafety(t *testing.T) {
	cpu := shared.CPUInventory{
		LogicalProcessors: 4, PhysicalCores: 2, TopologyAvailable: true, SMTDetected: true,
		Threads: []shared.CPUThreadInventory{
			{LogicalID: 0, PackageID: "0", CoreID: "0"}, {LogicalID: 1, PackageID: "0", CoreID: "0"},
			{LogicalID: 2, PackageID: "0", CoreID: "1"}, {LogicalID: 3, PackageID: "0", CoreID: "1"},
		},
	}
	roomA, worldA := resourceRoom("room-a", "Cluster_A")
	roomB, worldB := resourceRoom("room-b", "Cluster_B")
	base := CPUAllocationUpdate{RoomID: roomA.ID, WorldID: worldA.ID, EnvironmentID: "environment", Policy: CPUPolicyNone, LogicalCPUIds: []int{0}}
	if _, err := validateCPUAllocation(base, roomA, worldA, localTargetID, cpu, "linux", nil); err == nil {
		t.Fatal("none policy accepted logical CPU IDs")
	}
	sharedA, err := validateCPUAllocation(CPUAllocationUpdate{EnvironmentID: "environment", Policy: CPUPolicyShared, LogicalCPUIds: []int{0}}, roomA, worldA, localTargetID, cpu, "linux", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateCPUAllocation(CPUAllocationUpdate{EnvironmentID: "environment", Policy: CPUPolicyShared, LogicalCPUIds: []int{0}}, roomB, worldB, localTargetID, cpu, "linux", []CPUAllocation{sharedA}); err != nil {
		t.Fatalf("shared overlap rejected: %v", err)
	}
	if _, err := validateCPUAllocation(CPUAllocationUpdate{EnvironmentID: "environment", Policy: CPUPolicyExclusive, LogicalCPUIds: []int{0, 1}}, roomA, worldA, localTargetID, cpu, "darwin", nil); !errors.Is(err, ErrCPUNotSupported) {
		t.Fatalf("darwin exclusive error=%v", err)
	}
	if _, err := validateCPUAllocation(CPUAllocationUpdate{EnvironmentID: "environment", Policy: CPUPolicyShared, LogicalCPUIds: []int{0}}, roomA, worldA, localTargetID, cpu, "darwin", nil); !errors.Is(err, ErrCPUNotSupported) {
		t.Fatalf("darwin shared error=%v", err)
	}
	if _, err := validateCPUAllocation(CPUAllocationUpdate{EnvironmentID: "environment", Policy: CPUPolicyExclusive, LogicalCPUIds: []int{0}}, roomA, worldA, localTargetID, cpu, "linux", nil); err == nil {
		t.Fatal("partial SMT core did not require acknowledgement")
	}
	partial, err := validateCPUAllocation(CPUAllocationUpdate{EnvironmentID: "environment", Policy: CPUPolicyExclusive, LogicalCPUIds: []int{0}, AllowSMTSiblingRisk: true}, roomA, worldA, localTargetID, cpu, "linux", nil)
	if err != nil || len(partial.Warnings) < 2 {
		t.Fatalf("acknowledged partial allocation=%#v err=%v", partial, err)
	}
	exclusive, err := validateCPUAllocation(CPUAllocationUpdate{EnvironmentID: "environment", Policy: CPUPolicyExclusive, LogicalCPUIds: []int{0, 1}}, roomA, worldA, localTargetID, cpu, "linux", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateCPUAllocation(CPUAllocationUpdate{EnvironmentID: "environment", Policy: CPUPolicyExclusive, LogicalCPUIds: []int{0, 1}}, roomB, worldB, localTargetID, cpu, "linux", []CPUAllocation{exclusive}); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("exclusive collision error=%v", err)
	}
	if _, err := validateCPUAllocation(CPUAllocationUpdate{EnvironmentID: "environment", Policy: CPUPolicyExclusive, LogicalCPUIds: []int{0, 2}, AllowSMTSiblingRisk: true}, roomB, worldB, localTargetID, cpu, "linux", nil); err == nil {
		t.Fatal("exclusive allocation accepted multiple physical cores")
	}
}

type recordingCPUAllocationExecutor struct {
	result shared.RuntimeCPUResult
	err    error
	calls  []CPUAllocation
}

type recoveringCPUAllocationExecutor struct {
	result  shared.RuntimeCPUResult
	err     error
	calls   []CPUAllocation
	observe func(context.Context, CPUAllocation) (shared.RuntimeCPUResult, error)
}

func (e *recoveringCPUAllocationExecutor) ApplyCPUAllocation(_ context.Context, value CPUAllocation) (shared.RuntimeCPUResult, error) {
	return e.result, e.err
}

func (e *recoveringCPUAllocationExecutor) ObserveCPUAllocation(ctx context.Context, value CPUAllocation) (shared.RuntimeCPUResult, error) {
	e.calls = append(e.calls, value)
	if e.observe != nil {
		return e.observe(ctx, value)
	}
	return e.result, e.err
}

func (e *recordingCPUAllocationExecutor) ApplyCPUAllocation(_ context.Context, value CPUAllocation) (shared.RuntimeCPUResult, error) {
	e.calls = append(e.calls, value)
	return e.result, e.err
}

func TestCPUAllocationPersistsDesiredAndObservedState(t *testing.T) {
	now := time.Now().UTC()
	room, world := resourceRoom("room-cpu", "Cluster_CPU")
	inventory := runtimeInventory(resourceLocalTarget(), 4, 2, []shared.RoomInventoryReport{resourceInventoryRoom(room.DirectoryName, 10889, 10999, 8767, 27017)}, nil, now)
	inventory.Inventory.CPU.TopologyAvailable = true
	inventory.Inventory.CPU.Threads = []shared.CPUThreadInventory{{LogicalID: 0, PackageID: "0", CoreID: "0"}, {LogicalID: 1, PackageID: "0", CoreID: "0"}, {LogicalID: 2, PackageID: "0", CoreID: "1"}, {LogicalID: 3, PackageID: "0", CoreID: "1"}}
	store := newTopologyTestStore(t)
	service, _ := NewService(topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{inventory}}, store)
	if _, err := service.Topology(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	executor := &recordingCPUAllocationExecutor{result: shared.RuntimeCPUResult{Policy: shared.RuntimeCPUPolicyExclusive, LogicalCPUIds: []int{0, 1}, EffectiveCPUIds: []int{0, 1}, State: shared.RuntimeCPUStateApplied, RuntimeKind: "native", InstanceID: "42:100", PID: 42, Enforced: true, InstanceRunning: true, ObservedAt: now}}
	if err := service.ConfigureCPUExecutor(executor); err != nil {
		t.Fatal(err)
	}
	allocation, err := service.UpdateCPUAllocation(context.Background(), CPUAllocationUpdate{RoomID: room.ID, WorldID: world.ID, EnvironmentID: environmentResourceID(localTargetID), Policy: CPUPolicyExclusive, LogicalCPUIds: []int{0, 1}})
	if err != nil || len(executor.calls) != 1 || allocation.ExecutionState != CPUExecutionApplied || allocation.Observed == nil || !allocation.Observed.Enforced {
		t.Fatalf("allocation=%#v calls=%d err=%v", allocation, len(executor.calls), err)
	}
	stored, err := store.CPUAllocation(room.ID, world.ID)
	if err != nil || stored.ExecutionState != CPUExecutionApplied || stored.Observed == nil || stored.Observed.InstanceID != "42:100" {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestCPUAllocationPersistsExecutionFailure(t *testing.T) {
	now := time.Now().UTC()
	room, world := resourceRoom("room-cpu-fail", "Cluster_CPU_Fail")
	inventory := runtimeInventory(resourceLocalTarget(), 2, 2, []shared.RoomInventoryReport{resourceInventoryRoom(room.DirectoryName, 10889, 10999, 8767, 27017)}, nil, now)
	store := newTopologyTestStore(t)
	service, _ := NewService(topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{inventory}}, store)
	if _, err := service.Topology(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	executor := &recordingCPUAllocationExecutor{err: errors.New("cgroup unavailable")}
	_ = service.ConfigureCPUExecutor(executor)
	_, err := service.UpdateCPUAllocation(context.Background(), CPUAllocationUpdate{RoomID: room.ID, WorldID: world.ID, EnvironmentID: environmentResourceID(localTargetID), Policy: CPUPolicyShared, LogicalCPUIds: []int{0}})
	if err == nil {
		t.Fatal("expected CPU execution failure")
	}
	stored, loadErr := store.CPUAllocation(room.ID, world.ID)
	if loadErr != nil || stored.ExecutionState != CPUExecutionFailed || stored.ExecutionError != "cgroup unavailable" || stored.Policy != CPUPolicyShared {
		t.Fatalf("stored=%#v err=%v", stored, loadErr)
	}
}

func TestConfigureCPUExecutorRecoversPersistedExecutionState(t *testing.T) {
	now := time.Now().UTC()
	room, world := resourceRoom("room-cpu-recovery", "Cluster_CPU_Recovery")
	inventory := runtimeInventory(resourceLocalTarget(), 2, 2, []shared.RoomInventoryReport{resourceInventoryRoom(room.DirectoryName, 10889, 10999, 8767, 27017)}, nil, now)
	store := newTopologyTestStore(t)
	service, _ := NewService(topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}}, topologyTargetCatalog{items: []agents.RuntimeTargetInventory{inventory}}, store)
	allocation := CPUAllocation{ID: allocationResourceID(room.ID, world.ID), EnvironmentID: environmentResourceID(localTargetID), TargetID: localTargetID, RoomID: room.ID, WorldID: world.ID}
	allocation.Policy = CPUPolicyShared
	allocation.LogicalCPUIds = []int{0}
	allocation.ExecutionState = CPUExecutionApplied
	allocation.Observed = &shared.RuntimeCPUResult{Policy: shared.RuntimeCPUPolicyShared, State: shared.RuntimeCPUStateApplied, InstanceID: "old-instance", ObservedAt: now.Add(-time.Hour)}
	if _, err := store.SaveCPUAllocation(allocation); err != nil {
		t.Fatal(err)
	}
	executor := &recoveringCPUAllocationExecutor{result: shared.RuntimeCPUResult{Policy: shared.RuntimeCPUPolicyShared, LogicalCPUIds: []int{0}, State: shared.RuntimeCPUStatePrepared, RuntimeKind: "native", ObservedAt: now}}
	if err := service.ConfigureCPUExecutor(executor); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.CPUAllocation(room.ID, world.ID)
	if err != nil || len(executor.calls) != 1 || recovered.ExecutionState != CPUExecutionPrepared || recovered.Observed == nil || recovered.Observed.InstanceID != "" {
		t.Fatalf("recovered=%#v calls=%d err=%v", recovered, len(executor.calls), err)
	}
}

func TestConfigureCPUExecutorDefersRemoteRecoveryUntilInfrastructureIsAvailable(t *testing.T) {
	now := time.Now().UTC()
	room, world := resourceRoom("room-cpu-remote-recovery", "Cluster_CPU_Remote_Recovery")
	local := runtimeInventory(resourceLocalTarget(), 2, 2, nil, nil, now)
	remoteTarget := agents.RuntimeTarget{
		ID: "agent:node", AgentID: "node", Name: "远程节点", Kind: agents.RuntimeKindAgent,
		Status: agents.RuntimeStatusReady, Online: true, Configured: true, OS: "linux", Arch: "amd64",
	}
	remote := runtimeInventory(remoteTarget, 2, 2, []shared.RoomInventoryReport{
		resourceInventoryRoom(room.DirectoryName, 10889, 10999, 8767, 27017),
	}, nil, now)
	store := newTopologyTestStore(t)
	service, _ := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {world}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{local, remote}}, store,
	)
	if _, err := service.Topology(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	record, err := store.load(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Placements[0].DesiredTargetID = remoteTarget.ID
	record.Placements[0].AppliedTargetID = remoteTarget.ID
	if _, err := store.Save(room.ID, record.Revision, record.Placements); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Infrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	allocation, err := store.CPUAllocation(room.ID, world.ID)
	if err != nil {
		t.Fatal(err)
	}
	allocation.Policy = CPUPolicyShared
	allocation.LogicalCPUIds = []int{0}
	allocation.ExecutionState = CPUExecutionApplied
	if _, err := store.SaveCPUAllocation(allocation); err != nil {
		t.Fatal(err)
	}
	executor := &recoveringCPUAllocationExecutor{result: shared.RuntimeCPUResult{
		Policy: shared.RuntimeCPUPolicyShared, LogicalCPUIds: []int{0}, State: shared.RuntimeCPUStatePrepared, RuntimeKind: "native", ObservedAt: now,
	}}
	if err := service.ConfigureCPUExecutor(executor); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("remote recovery ran before inventory-triggered retry: calls=%#v", executor.calls)
	}
	before, err := store.CPUAllocation(room.ID, world.ID)
	if err != nil || before.ExecutionState != CPUExecutionApplied {
		t.Fatalf("remote allocation changed during configure: allocation=%#v err=%v", before, err)
	}
	if _, err := service.Infrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.CPUAllocation(room.ID, world.ID)
	if err != nil || len(executor.calls) != 1 || recovered.ExecutionState != CPUExecutionPrepared {
		t.Fatalf("recovered=%#v calls=%#v err=%v", recovered, executor.calls, err)
	}
}

func TestRemoteCPURecoveryUsesIndependentTimeoutPerAllocation(t *testing.T) {
	store := newTopologyTestStore(t)
	for _, roomID := range []string{"room-a", "room-b"} {
		allocation := CPUAllocation{
			ID: allocationResourceID(roomID, "world"), EnvironmentID: environmentResourceID("agent:node"),
			TargetID: "agent:node", RoomID: roomID, WorldID: "world", Policy: CPUPolicyShared,
			LogicalCPUIds: []int{0}, ExecutionState: CPUExecutionApplied,
		}
		if _, err := store.SaveCPUAllocation(allocation); err != nil {
			t.Fatal(err)
		}
	}
	executor := &recoveringCPUAllocationExecutor{}
	executor.observe = func(ctx context.Context, allocation CPUAllocation) (shared.RuntimeCPUResult, error) {
		if allocation.RoomID == "room-a" {
			<-ctx.Done()
			return shared.RuntimeCPUResult{}, ctx.Err()
		}
		return shared.RuntimeCPUResult{Policy: shared.RuntimeCPUPolicyShared, State: shared.RuntimeCPUStatePrepared}, nil
	}
	service := &Service{
		store: store, cpuRecoveryPending: make(map[string]struct{}), cpuRecoveryTimeout: 10 * time.Millisecond,
	}
	if err := service.ConfigureCPUExecutor(executor); err != nil {
		t.Fatal(err)
	}
	inventory := agents.RuntimeTargetInventory{
		Target: agents.RuntimeTarget{ID: "agent:node", Online: true}, Available: true,
	}
	if err := service.recoverPendingCPUExecutionState(context.Background(), []agents.RuntimeTargetInventory{inventory}); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 2 || executor.calls[0].RoomID != "room-a" || executor.calls[1].RoomID != "room-b" {
		t.Fatalf("recovery calls=%#v", executor.calls)
	}
	failed, err := store.CPUAllocation("room-a", "world")
	if err != nil || failed.ExecutionState != CPUExecutionFailed {
		t.Fatalf("timed-out allocation=%#v err=%v", failed, err)
	}
	recovered, err := store.CPUAllocation("room-b", "world")
	if err != nil || recovered.ExecutionState != CPUExecutionPrepared {
		t.Fatalf("second allocation=%#v err=%v", recovered, err)
	}
	service.cpuRecoveryMu.Lock()
	pending := len(service.cpuRecoveryPending)
	service.cpuRecoveryMu.Unlock()
	if pending != 1 {
		t.Fatalf("pending recoveries=%d", pending)
	}
}

func TestRemoteCPURecoveryDoesNotOverwriteConcurrentAllocationChange(t *testing.T) {
	store := newTopologyTestStore(t)
	allocation := CPUAllocation{
		ID: allocationResourceID("room", "world"), EnvironmentID: environmentResourceID("agent:node"),
		TargetID: "agent:node", RoomID: "room", WorldID: "world", Policy: CPUPolicyShared,
		LogicalCPUIds: []int{0}, ExecutionState: CPUExecutionApplied,
	}
	if _, err := store.SaveCPUAllocation(allocation); err != nil {
		t.Fatal(err)
	}
	executor := &recoveringCPUAllocationExecutor{}
	executor.observe = func(_ context.Context, stale CPUAllocation) (shared.RuntimeCPUResult, error) {
		current, err := store.CPUAllocation(stale.RoomID, stale.WorldID)
		if err != nil {
			return shared.RuntimeCPUResult{}, err
		}
		current.Policy = CPUPolicyExclusive
		current.LogicalCPUIds = []int{1}
		current.ExecutionState = CPUExecutionDesired
		current.Observed = nil
		if _, err := store.SaveCPUAllocation(current); err != nil {
			return shared.RuntimeCPUResult{}, err
		}
		return shared.RuntimeCPUResult{Policy: shared.RuntimeCPUPolicyShared, LogicalCPUIds: []int{0}, State: shared.RuntimeCPUStateApplied}, nil
	}
	service := &Service{
		store: store, cpuRecoveryPending: make(map[string]struct{}), cpuRecoveryTimeout: time.Second,
	}
	if err := service.ConfigureCPUExecutor(executor); err != nil {
		t.Fatal(err)
	}
	inventory := agents.RuntimeTargetInventory{Target: agents.RuntimeTarget{ID: "agent:node", Online: true}, Available: true}
	if err := service.recoverPendingCPUExecutionState(context.Background(), []agents.RuntimeTargetInventory{inventory}); err != nil {
		t.Fatal(err)
	}
	current, err := store.CPUAllocation("room", "world")
	if err != nil || current.Policy != CPUPolicyExclusive || len(current.LogicalCPUIds) != 1 || current.LogicalCPUIds[0] != 1 || current.ExecutionState != CPUExecutionDesired || current.Observed != nil {
		t.Fatalf("concurrent allocation overwritten: allocation=%#v err=%v", current, err)
	}
	service.cpuRecoveryMu.Lock()
	_, pending := service.cpuRecoveryPending[allocationResourceID("room", "world")]
	service.cpuRecoveryMu.Unlock()
	if !pending {
		t.Fatal("changed allocation was not retained for a fresh recovery observation")
	}
}

func TestRemoteCPURecoveryDiscardsObservationAfterAllocationDeletion(t *testing.T) {
	store := newTopologyTestStore(t)
	allocation := CPUAllocation{
		ID: allocationResourceID("room", "world"), EnvironmentID: environmentResourceID("agent:node"),
		TargetID: "agent:node", RoomID: "room", WorldID: "world", Policy: CPUPolicyShared,
		LogicalCPUIds: []int{0}, ExecutionState: CPUExecutionApplied,
	}
	if _, err := store.SaveCPUAllocation(allocation); err != nil {
		t.Fatal(err)
	}
	executor := &recoveringCPUAllocationExecutor{}
	executor.observe = func(_ context.Context, stale CPUAllocation) (shared.RuntimeCPUResult, error) {
		if err := store.db.Table(store.cpuAllocationsTable).Where("id = ?", stale.ID).Delete(&cpuAllocationRecord{}).Error; err != nil {
			return shared.RuntimeCPUResult{}, err
		}
		return shared.RuntimeCPUResult{Policy: shared.RuntimeCPUPolicyShared, LogicalCPUIds: []int{0}, State: shared.RuntimeCPUStateApplied}, nil
	}
	service := &Service{
		store: store, cpuRecoveryPending: make(map[string]struct{}), cpuRecoveryTimeout: time.Second,
	}
	if err := service.ConfigureCPUExecutor(executor); err != nil {
		t.Fatal(err)
	}
	inventory := agents.RuntimeTargetInventory{Target: agents.RuntimeTarget{ID: "agent:node", Online: true}, Available: true}
	if err := service.recoverPendingCPUExecutionState(context.Background(), []agents.RuntimeTargetInventory{inventory}); err != nil {
		t.Fatal(err)
	}
	service.cpuRecoveryMu.Lock()
	_, pending := service.cpuRecoveryPending[allocationResourceID("room", "world")]
	service.cpuRecoveryMu.Unlock()
	if pending {
		t.Fatal("deleted allocation retained a pending recovery key")
	}
}

func TestRemoteCPURecoveryKeepsUnknownObservationPending(t *testing.T) {
	store := newTopologyTestStore(t)
	allocation := CPUAllocation{
		ID: allocationResourceID("room", "world"), EnvironmentID: environmentResourceID("agent:node"),
		TargetID: "agent:node", RoomID: "room", WorldID: "world", Policy: CPUPolicyShared,
		LogicalCPUIds: []int{0}, ExecutionState: CPUExecutionApplied,
	}
	if _, err := store.SaveCPUAllocation(allocation); err != nil {
		t.Fatal(err)
	}
	executor := &recoveringCPUAllocationExecutor{result: shared.RuntimeCPUResult{Policy: shared.RuntimeCPUPolicyShared}}
	service := &Service{
		store: store, cpuRecoveryPending: make(map[string]struct{}), cpuRecoveryTimeout: time.Second,
	}
	if err := service.ConfigureCPUExecutor(executor); err != nil {
		t.Fatal(err)
	}
	inventory := agents.RuntimeTargetInventory{Target: agents.RuntimeTarget{ID: "agent:node", Online: true}, Available: true}
	if err := service.recoverPendingCPUExecutionState(context.Background(), []agents.RuntimeTargetInventory{inventory}); err != nil {
		t.Fatal(err)
	}
	current, err := store.CPUAllocation("room", "world")
	if err != nil || current.ExecutionState != CPUExecutionDesired || current.Observed == nil {
		t.Fatalf("unknown observation=%#v err=%v", current, err)
	}
	service.cpuRecoveryMu.Lock()
	_, pending := service.cpuRecoveryPending[allocationResourceID("room", "world")]
	service.cpuRecoveryMu.Unlock()
	if !pending {
		t.Fatal("unknown observation cleared pending recovery")
	}
}

func TestRecordCPUFailureArchivesErrorWithoutLosingObservation(t *testing.T) {
	store := newTopologyTestStore(t)
	service := &Service{store: store}
	observed := &shared.RuntimeCPUResult{Policy: shared.RuntimeCPUPolicyExclusive, State: shared.RuntimeCPUStateReleased, RuntimeKind: "native", ObservedAt: time.Now().UTC()}
	allocation := CPUAllocation{ID: allocationResourceID("room", "world"), EnvironmentID: "environment", TargetID: localTargetID, RoomID: "room", WorldID: "world", Policy: CPUPolicyExclusive, LogicalCPUIds: []int{0}, ExecutionState: CPUExecutionReleased, Observed: observed}
	if _, err := store.SaveCPUAllocation(allocation); err != nil {
		t.Fatal(err)
	}
	failed, err := service.RecordCPUFailure("room", "world", errors.New("cleanup failed"))
	if err != nil || failed.ExecutionState != CPUExecutionFailed || failed.ExecutionError != "cleanup failed" || failed.Observed == nil || failed.Observed.State != shared.RuntimeCPUStateReleased {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
}

func TestPlacementEnvironmentChangeResetsCPUAllocation(t *testing.T) {
	store := newTopologyTestStore(t)
	original := CPUAllocation{
		ID: allocationResourceID("room", "world"), EnvironmentID: "environment-local", TargetID: localTargetID,
		RoomID: "room", WorldID: "world", Policy: CPUPolicyShared, LogicalCPUIds: []int{1}, PhysicalCoreKeys: []string{"0:0"},
	}
	if _, err := store.SaveCPUAllocation(original); err != nil {
		t.Fatal(err)
	}
	relocated := original
	relocated.EnvironmentID = "environment-remote"
	relocated.TargetID = "agent:node"
	relocated.Policy = CPUPolicyNone
	relocated.LogicalCPUIds = []int{}
	relocated.PhysicalCoreKeys = []string{}
	if err := store.EnsureCPUAllocations([]CPUAllocation{relocated}); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, allocations, err := store.RuntimeResources()
	if err != nil {
		t.Fatal(err)
	}
	if len(allocations) != 1 || allocations[0].Policy != CPUPolicyNone || allocations[0].TargetID != "agent:node" || len(allocations[0].LogicalCPUIds) != 0 {
		t.Fatalf("relocated allocation=%#v", allocations)
	}
}

func resourceRoom(id, directory string) (rooms.Room, rooms.World) {
	room := rooms.Room{ID: id, DirectoryName: directory, Name: directory, Managed: true}
	world := rooms.World{ID: id + "-master", RoomID: id, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster, IsMaster: true}
	return room, world
}

func TestPortPreflightSharesHostScopeAcrossInstallations(t *testing.T) {
	now := time.Now().UTC()
	roomA, worldA := resourceRoom("room-a", "Cluster_A")
	roomB, worldB := resourceRoom("room-b", "Cluster_B")
	worldA.TargetIDs = []string{"agent:node"}
	worldB.TargetIDs = []string{"agent:node"}
	target := agents.RuntimeTarget{
		ID: "agent:node", AgentID: "node", Name: "Node", Kind: agents.RuntimeKindAgent,
		Status: agents.RuntimeStatusReady, Online: true, Configured: true,
		DefaultInstallationID: "primary",
		Installations:         []agents.RuntimeInstallation{{ID: "primary", Driver: "native"}, {ID: "testing", Driver: "native"}},
	}
	primaryTarget := target
	primaryTarget.Config.InstallationID = "primary"
	testingTarget := target
	testingTarget.Config.InstallationID = "testing"
	primary := runtimeInventory(primaryTarget, 4, 4,
		[]shared.RoomInventoryReport{resourceInventoryRoom(roomA.DirectoryName, 10888, 10999, 10998, 10997)}, nil, now)
	primary.Inventory.Installation.ID = "primary"
	testing := runtimeInventory(testingTarget, 4, 4,
		[]shared.RoomInventoryReport{resourceInventoryRoom(roomB.DirectoryName, 10888, 10999, 10998, 10997)}, nil, now)
	testing.Inventory.Installation.ID = "testing"
	store := newTopologyTestStore(t)
	for _, value := range []struct {
		room         rooms.Room
		world        rooms.World
		installation string
	}{{roomA, worldA, "primary"}, {roomB, worldB, "testing"}} {
		record, err := store.EnsureWorlds(value.room.ID, []rooms.World{value.world})
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Save(value.room.ID, record.Revision, []storedPlacement{{
			WorldID: value.world.ID, WorldDirectoryName: value.world.DirectoryName, WorldName: value.world.Name, WorldRole: value.world.Role,
			DesiredTargetID: "agent:node", AppliedTargetID: "agent:node",
			DesiredInstallationID: value.installation, AppliedInstallationID: value.installation,
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(
		topologyRoomCatalog{
			rooms:  []rooms.Room{roomA, roomB},
			worlds: map[string][]rooms.World{roomA.ID: {worldA}, roomB.ID: {worldB}},
		},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{primary, testing}}, store,
	)
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := service.PreflightBatchExecution(context.Background(), []StartCapacitySelection{
		{RoomID: roomA.ID}, {RoomID: roomB.ID},
	})
	if err == nil || !hasResourceConflict(preflight, "UDP_PORT_CONFLICT", 10999) {
		t.Fatalf("host-scoped conflict missing: preflight=%#v err=%v", preflight, err)
	}
}

func TestCrossNodePreflightReadsThePlacementInstallation(t *testing.T) {
	now := time.Now().UTC()
	room := rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Role: rooms.WorldRoleMaster, IsMaster: true}
	caves := rooms.World{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Role: rooms.WorldRoleCaves}
	target := func(id, installationID string) agents.RuntimeTarget {
		return agents.RuntimeTarget{
			ID: id, AgentID: id, Name: id, Kind: agents.RuntimeKindAgent,
			Status: agents.RuntimeStatusReady, Online: true, Configured: true,
			Config: agents.RuntimeConfig{InstallationID: installationID},
		}
	}
	inventory := func(runtimeTarget agents.RuntimeTarget, rooms []shared.RoomInventoryReport) agents.RuntimeTargetInventory {
		observedAt := now
		return agents.RuntimeTargetInventory{
			Target: runtimeTarget, Available: true, ObservedAt: &observedAt, ReceivedAt: &observedAt,
			Inventory: shared.RuntimeInventoryReport{
				ProtocolVersion: shared.RuntimeInventoryProtocolVersion, ObservedAt: now,
				Installation: shared.RuntimeInstallationReport{ID: runtimeTarget.Config.InstallationID}, Rooms: rooms,
			},
		}
	}
	masterRoom := resourceInventoryRoom(room.DirectoryName, 10888, 10999, 10998, 10997)
	masterRoom.BindIP = "0.0.0.0"
	secondaryRoom := resourceInventoryRoom(room.DirectoryName, 10888, 11099, 11098, 11097)
	secondaryRoom.MasterIP = "10.0.0.10"
	plans := map[string]roomPlan{room.ID: {
		room:   room,
		worlds: []rooms.World{master, caves},
		record: record{Placements: []storedPlacement{
			{WorldID: master.ID, AppliedTargetID: "agent:master", AppliedInstallationID: "primary"},
			{WorldID: caves.ID, AppliedTargetID: "agent:secondary", AppliedInstallationID: "primary"},
		}},
	}}
	inventories := []agents.RuntimeTargetInventory{
		inventory(target("agent:master", "primary"), []shared.RoomInventoryReport{masterRoom}),
		inventory(target("agent:master", "testing"), nil),
		inventory(target("agent:secondary", "primary"), []shared.RoomInventoryReport{secondaryRoom}),
	}
	profiles := map[string]NetworkProfile{
		"agent:master":    {AdvertiseAddress: "10.0.0.10"},
		"agent:secondary": {},
	}
	build := resourceBuild{preflight: ResourcePreflight{Ready: true}}
	appendCrossNodeMasterPreflight(&build, plans, inventories, profiles, nil)
	for _, warning := range build.preflight.Warnings {
		if strings.Contains(warning, "MASTER_ROOM_CONFIG_MISSING") || strings.Contains(warning, "MASTER_PORT_MISSING") {
			t.Fatalf("preflight used the wrong installation: %#v", build.preflight)
		}
	}
}

func resourceLocalTarget() agents.RuntimeTarget {
	return agents.RuntimeTarget{ID: localTargetID, Name: "本机", Kind: agents.RuntimeKindLocal, Status: agents.RuntimeStatusReady, Online: true, Configured: true, OS: "linux", Arch: "amd64"}
}

func resourceInventoryRoom(cluster string, clusterPort, serverPort, authPort, steamPort int) shared.RoomInventoryReport {
	return shared.RoomInventoryReport{
		Directory: cluster, MasterPort: clusterPort,
		Shards: []shared.ShardInventoryReport{{
			Directory: "Master", Role: "master", ServerPort: serverPort,
			AuthenticationPort: authPort, MasterServerPort: steamPort,
		}},
	}
}

func hasResourceConflict(preflight ResourcePreflight, code string, port int) bool {
	for _, conflict := range preflight.Conflicts {
		if conflict.Code == code && conflict.Port == port {
			return true
		}
	}
	return false
}

func hasResourceAdvisory(preflight ResourcePreflight, code string, port int) bool {
	for _, advisory := range preflight.Advisories {
		if advisory.Code == code && advisory.Port == port {
			return true
		}
	}
	return false
}

func hasResourceConflictCode(preflight ResourcePreflight, code string) bool {
	for _, conflict := range preflight.Conflicts {
		if conflict.Code == code {
			return true
		}
	}
	return false
}
