package topology

import (
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
