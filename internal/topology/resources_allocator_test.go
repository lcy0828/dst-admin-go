package topology

import (
	"errors"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
)

func TestPortAllocatorSerializesConcurrentReservations(t *testing.T) {
	store := newTopologyTestStore(t)
	now := time.Now().UTC()
	inventory := runtimeInventory(resourceLocalTarget(), 4, 4, nil, nil, now)
	if err := store.SyncRuntimeCatalog([]agents.RuntimeTargetInventory{inventory}); err != nil {
		t.Fatal(err)
	}
	requests := []PortAllocationRequest{
		{OwnerID: "import-a", TargetID: localTargetID, RoomID: "room-a", Cluster: "A", Requests: []PortRequest{{WorldID: "master-a", Shard: "Master", Purpose: PortDSTServer, Preferred: 10999}}},
		{OwnerID: "import-b", TargetID: localTargetID, RoomID: "room-b", Cluster: "B", Requests: []PortRequest{{WorldID: "master-b", Shard: "Master", Purpose: PortDSTServer, Preferred: 10999}}},
	}
	results := make([]PortAllocation, len(requests))
	errs := make([]error, len(requests))
	var wait sync.WaitGroup
	for index := range requests {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = store.ReservePorts(requests[index])
		}(index)
	}
	wait.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	left, right := results[0].Reservations[0].Port, results[1].Reservations[0].Port
	if left == right || left < 10999 || right < 10999 {
		t.Fatalf("concurrent allocations=%d,%d", left, right)
	}
}

func TestPortReservationLeaseActivationAndReleaseLifecycle(t *testing.T) {
	store := newTopologyTestStore(t)
	now := time.Now().UTC()
	if err := store.SyncRuntimeCatalog([]agents.RuntimeTargetInventory{runtimeInventory(resourceLocalTarget(), 2, 2, nil, nil, now)}); err != nil {
		t.Fatal(err)
	}
	allocation, err := store.ReservePorts(PortAllocationRequest{
		OwnerID: "import", TargetID: localTargetID, RoomID: "room", Cluster: "Cluster",
		Requests: []PortRequest{{WorldID: "master", Shard: "Master", Purpose: PortClusterMaster, Preferred: 10889, Strict: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocation.Reservations[0].State != ReservationPlanned || allocation.Reservations[0].ExpiresAt == nil {
		t.Fatalf("planned allocation=%#v", allocation)
	}
	if err := store.ActivatePorts(allocation.LeaseID); err != nil {
		t.Fatal(err)
	}
	assertLeaseState(t, store, allocation.LeaseID, ReservationActive)
	if err := store.BeginReleasePorts(allocation.LeaseID); err != nil {
		t.Fatal(err)
	}
	assertLeaseState(t, store, allocation.LeaseID, ReservationReleasing)
	if err := store.CompleteReleasePorts(allocation.LeaseID); err != nil {
		t.Fatal(err)
	}
	assertLeaseState(t, store, allocation.LeaseID, ReservationReleased)
}

func TestPortAllocatorRejectsStrictConflictAndAllowsReplacementReuse(t *testing.T) {
	store := newTopologyTestStore(t)
	now := time.Now().UTC()
	if err := store.SyncRuntimeCatalog([]agents.RuntimeTargetInventory{runtimeInventory(resourceLocalTarget(), 2, 2, nil, nil, now)}); err != nil {
		t.Fatal(err)
	}
	request := PortAllocationRequest{
		OwnerID: "import-source", TargetID: localTargetID, RoomID: "room-source", Cluster: "Source",
		Requests: []PortRequest{{WorldID: "master", Shard: "Master", Purpose: PortDSTServer, Preferred: 10999, Strict: true}},
	}
	first, err := store.ReservePorts(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ActivatePorts(first.LeaseID); err != nil {
		t.Fatal(err)
	}
	conflicting := request
	conflicting.OwnerID = "import-other"
	conflicting.RoomID = "room-other"
	conflicting.Cluster = "Other"
	if _, err := store.ReservePorts(conflicting); err == nil {
		t.Fatal("strict source allocation unexpectedly reused another room's port")
	} else {
		var conflict *ResourceConflictError
		if !errors.As(err, &conflict) || len(conflict.Preflight.Conflicts) != 1 || conflict.Preflight.Conflicts[0].Code != "UDP_PORT_CONFLICT" {
			t.Fatalf("strict conflict=%#v err=%v", conflict, err)
		}
	}

	replacement := request
	replacement.OwnerID = "save-import:replace"
	second, err := store.ReservePorts(replacement)
	if err != nil {
		t.Fatalf("same-room replacement failed to reuse original port: %v", err)
	}
	if first.Reservations[0].Port != second.Reservations[0].Port {
		t.Fatalf("replacement port=%d want=%d", second.Reservations[0].Port, first.Reservations[0].Port)
	}
}

func TestPortAllocatorAllowsSamePortAcrossTargetScopes(t *testing.T) {
	store := newTopologyTestStore(t)
	now := time.Now().UTC()
	remote := agents.RuntimeTarget{ID: "agent:node", AgentID: "node", Name: "节点", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true, OS: "linux", Arch: "amd64"}
	if err := store.SyncRuntimeCatalog([]agents.RuntimeTargetInventory{
		runtimeInventory(resourceLocalTarget(), 2, 2, nil, nil, now),
		runtimeInventory(remote, 2, 2, nil, nil, now),
	}); err != nil {
		t.Fatal(err)
	}
	for _, request := range []PortAllocationRequest{
		{OwnerID: "local", TargetID: localTargetID, RoomID: "room-local", Cluster: "Local", Requests: []PortRequest{{WorldID: "master-local", Shard: "Master", Purpose: PortDSTServer, Preferred: 10999, Strict: true}}},
		{OwnerID: "remote", TargetID: remote.ID, RoomID: "room-remote", Cluster: "Remote", Requests: []PortRequest{{WorldID: "master-remote", Shard: "Master", Purpose: PortDSTServer, Preferred: 10999, Strict: true}}},
	} {
		allocation, err := store.ReservePorts(request)
		if err != nil {
			t.Fatalf("target %s allocation failed: %v", request.TargetID, err)
		}
		if allocation.Reservations[0].Port != 10999 {
			t.Fatalf("target %s port=%d", request.TargetID, allocation.Reservations[0].Port)
		}
	}
}

func TestPortAllocatorReclaimsExpiredPlannedLease(t *testing.T) {
	store := newTopologyTestStore(t)
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	if err := store.SyncRuntimeCatalog([]agents.RuntimeTargetInventory{runtimeInventory(resourceLocalTarget(), 2, 2, nil, nil, now)}); err != nil {
		t.Fatal(err)
	}
	first, err := store.ReservePorts(PortAllocationRequest{
		OwnerID: "expired", TargetID: localTargetID, RoomID: "room-old", Cluster: "Old", TTL: time.Minute,
		Requests: []PortRequest{{WorldID: "master-old", Shard: "Master", Purpose: PortDSTServer, Preferred: 10999, Strict: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	second, err := store.ReservePorts(PortAllocationRequest{
		OwnerID: "new", TargetID: localTargetID, RoomID: "room-new", Cluster: "New",
		Requests: []PortRequest{{WorldID: "master-new", Shard: "Master", Purpose: PortDSTServer, Preferred: 10999, Strict: true}},
	})
	if err != nil {
		t.Fatalf("expired planned lease still blocked allocation: %v", err)
	}
	if second.Reservations[0].Port != 10999 {
		t.Fatalf("reallocated port=%d", second.Reservations[0].Port)
	}
	assertLeaseState(t, store, first.LeaseID, ReservationReleased)
}

func TestPortAllocatorSeparatesBridgeScopeButSharesHostScopeAcrossRuntimes(t *testing.T) {
	store := newTopologyTestStore(t)
	now := time.Now().UTC()
	targets := []agents.RuntimeTarget{
		{ID: "native:node", Name: "Native", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		{ID: "container:bridge", Name: "Bridge", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
		{ID: "container:host", Name: "Host", Kind: agents.RuntimeKindAgent, Status: agents.RuntimeStatusReady, Online: true, Configured: true},
	}
	inventories := make([]agents.RuntimeTargetInventory, 0, len(targets))
	for _, target := range targets {
		inventories = append(inventories, runtimeInventory(target, 2, 2, nil, nil, now))
	}
	if err := store.SyncRuntimeCatalog(inventories); err != nil {
		t.Fatal(err)
	}
	configure := func(targetID string, kind EnvironmentKind, mode NetworkMode, scope string) {
		t.Helper()
		if err := store.db.Table(store.environmentsTable).Where("target_id = ?", targetID).Updates(map[string]interface{}{"kind": string(kind), "driver": map[EnvironmentKind]string{EnvironmentNative: "native", EnvironmentContainer: "docker"}[kind]}).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.db.Table(store.networkProfilesTable).Where("environment_id = ?", environmentResourceID(targetID)).Updates(map[string]interface{}{"mode": string(mode), "scope_id": scope, "bind_address": "0.0.0.0"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	configure("native:node", EnvironmentNative, NetworkHost, "host:physical-node")
	configure("container:bridge", EnvironmentContainer, NetworkBridge, "bridge:isolated-network")
	configure("container:host", EnvironmentContainer, NetworkHost, "host:physical-node")

	reserve := func(targetID, roomID string) error {
		_, err := store.ReservePorts(PortAllocationRequest{
			OwnerID: roomID, TargetID: targetID, RoomID: roomID, Cluster: roomID,
			Requests: []PortRequest{{WorldID: roomID + "-master", Shard: "Master", Purpose: PortDSTServer, Preferred: 10999, Strict: true}},
		})
		return err
	}
	if err := reserve("native:node", "native-room"); err != nil {
		t.Fatal(err)
	}
	if err := reserve("container:bridge", "bridge-room"); err != nil {
		t.Fatalf("isolated bridge scope conflicted with host scope: %v", err)
	}
	if err := reserve("container:host", "host-room"); err == nil {
		t.Fatal("host-network container did not conflict with native process on the same host scope")
	} else if !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("host-scope conflict=%v", err)
	}
}

func TestReservationReconciliationDoesNotDeleteLifecycleHistory(t *testing.T) {
	store := newTopologyTestStore(t)
	value := PortReservation{ID: "reservation", EnvironmentID: "environment", NetworkProfileID: "network", ScopeID: "host:local", TargetID: localTargetID, RoomID: "room", WorldID: "master", Cluster: "Cluster", Shard: "Master", Purpose: PortDSTServer, Protocol: "udp", BindAddress: "0.0.0.0", Port: 10999, State: ReservationActive, Managed: true}
	if err := store.ReplacePortReservations([]PortReservation{value}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplacePortReservations(nil); err != nil {
		t.Fatal(err)
	}
	assertReservationState(t, store, value.ID, ReservationReleasing)
	if err := store.ReplacePortReservations(nil); err != nil {
		t.Fatal(err)
	}
	assertReservationState(t, store, value.ID, ReservationReleased)
}

func assertLeaseState(t *testing.T, store *Store, leaseID string, want ReservationState) {
	t.Helper()
	_, _, _, reservations, _, err := store.RuntimeResources()
	if err != nil {
		t.Fatal(err)
	}
	for _, reservation := range reservations {
		if reservation.LeaseID == leaseID {
			if reservation.State != want {
				t.Fatalf("lease state=%s want=%s", reservation.State, want)
			}
			return
		}
	}
	t.Fatalf("lease %s missing", leaseID)
}

func assertReservationState(t *testing.T, store *Store, id string, want ReservationState) {
	t.Helper()
	_, _, _, reservations, _, err := store.RuntimeResources()
	if err != nil {
		t.Fatal(err)
	}
	for _, reservation := range reservations {
		if reservation.ID == id {
			if reservation.State != want {
				t.Fatalf("reservation state=%s want=%s", reservation.State, want)
			}
			return
		}
	}
	t.Fatalf("reservation %s missing", id)
}
