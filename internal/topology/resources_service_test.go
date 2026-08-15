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

func TestPlacementPreflightRejectsDuplicateUDPPortInOneHostScope(t *testing.T) {
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
	_, err = service.Preview(context.Background(), roomA.ID, UpdateRequest{
		ExpectedRevision: snapshot.Revision,
		Placements:       []PlacementInput{{WorldID: worldA.ID, TargetID: localTargetID}},
	})
	var conflict *ResourceConflictError
	if !errors.As(err, &conflict) || conflict.Preflight.Ready || !hasResourceConflict(conflict.Preflight, "UDP_PORT_CONFLICT", 10999) {
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
	if states[ReservationActive] != 4 || states[ReservationPlanned] != 4 {
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
	}, nil, now)
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
	conflicts := detectPortConflicts(values, nil)
	if len(conflicts) != 1 || conflicts[0].Port != 10999 {
		t.Fatalf("conflicts=%#v", conflicts)
	}
	values[0].BindAddress = "192.0.2.10"
	if conflicts := detectPortConflicts(values, nil); len(conflicts) != 0 {
		t.Fatalf("different specific binds conflicted: %#v", conflicts)
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
	service, err := NewService(
		topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {master, caves}}},
		topologyTargetCatalog{items: []agents.RuntimeTargetInventory{inventory}}, newTopologyTestStore(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := service.PreflightExecution(context.Background(), room.ID, nil)
	if err != nil || !preflight.Ready {
		t.Fatalf("same-node preflight=%#v err=%v", preflight, err)
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

func hasResourceConflictCode(preflight ResourcePreflight, code string) bool {
	for _, conflict := range preflight.Conflicts {
		if conflict.Code == code {
			return true
		}
	}
	return false
}
