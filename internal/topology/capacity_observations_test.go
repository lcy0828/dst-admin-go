package topology

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/internal/runtimeobservation"
	"dont/shared"
)

type capacityObservationSource struct {
	mu       sync.Mutex
	base     []agents.RuntimeTargetInventory
	fresh    map[placementEndpoint]agents.RuntimeTargetInventory
	failures map[placementEndpoint]error
	calls    map[placementEndpoint]int
}

func (s *capacityObservationSource) CachedRuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error) {
	return append([]agents.RuntimeTargetInventory(nil), s.base...), nil
}

func (s *capacityObservationSource) CollectRuntimeTargetInventory(_ context.Context, targetID, installationID string) (agents.RuntimeTargetInventory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := endpointFor(targetID, installationID)
	s.calls[key]++
	if err := s.failures[key]; err != nil {
		return agents.RuntimeTargetInventory{}, err
	}
	return s.fresh[key], nil
}

func (*capacityObservationSource) CheckpointRuntimeTargetInventory(string, shared.RuntimeInventoryReport) error {
	return nil
}

func capacityObservationFixture(t *testing.T) (topologyRoomCatalog, *capacityObservationSource) {
	t.Helper()
	room := rooms.Room{ID: "room", DirectoryName: "Cluster", Managed: true}
	catalog := topologyRoomCatalog{rooms: []rooms.Room{room}, worlds: map[string][]rooms.World{room.ID: {
		{ID: "master", RoomID: room.ID, Name: "Master", DirectoryName: "Master", Role: rooms.WorldRoleMaster},
		{ID: "caves", RoomID: room.ID, Name: "Caves", DirectoryName: "Caves", Role: rooms.WorldRoleCaves},
	}}}
	target := agents.RuntimeTarget{ID: "local", Name: "本机", Kind: agents.RuntimeKindLocal, Online: true, Configured: true,
		DefaultInstallationID: "default", Config: agents.RuntimeConfig{InstallationID: "default"}}
	fresh := runtimeInventory(target, 2, 2, []shared.RoomInventoryReport{inventoryRoom("Cluster", "Master", "Caves")}, nil, time.Now().UTC())
	fresh.Inventory.Memory = shared.MemoryInventory{TotalBytes: 4 << 30, AvailableBytes: 3500 << 20}
	missing := agents.RuntimeTargetInventory{Target: target, Stale: true, StaleReason: "inventory_missing"}
	key := endpointFor("local", "default")
	return catalog, &capacityObservationSource{
		base: []agents.RuntimeTargetInventory{missing}, fresh: map[placementEndpoint]agents.RuntimeTargetInventory{key: fresh},
		failures: map[placementEndpoint]error{}, calls: map[placementEndpoint]int{},
	}
}

func TestCapacityPreflightRefreshesLocalInventoryWithoutPageOrAgent(t *testing.T) {
	catalog, source := capacityObservationFixture(t)
	store := newTopologyTestStore(t)
	ctx := context.Background()
	// Recreate the coordinator to model a Controller restart: local snapshots
	// are memory-only, so a page subscription or Agent must not be required.
	for restart := 0; restart < 2; restart++ {
		coordinator, err := runtimeobservation.New(source)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(coordinator.Close)
		service, err := NewService(catalog, coordinator, store)
		if err != nil {
			t.Fatal(err)
		}
		for repeat := 0; repeat < 2; repeat++ {
			preview, err := service.PreviewStartCapacity(ctx, "room", nil)
			if err != nil || len(preview.Targets) != 1 {
				t.Fatalf("preview=%+v error=%v", preview, err)
			}
			capacity := preview.Targets[0].Capacity
			if preview.RequiresRiskConfirmation || capacity.State != agents.CapacityFull || capacity.LogicalProcessors != 2 || capacity.ReservedPhysicalCores != 0 {
				t.Fatalf("healthy 2C4G start was blocked: %+v", preview)
			}
		}
		snapshot, err := service.Topology(ctx, "room")
		if err != nil {
			t.Fatal(err)
		}
		target := topologyTarget(t, snapshot, "local")
		if !target.InventoryAvailable || target.InventoryStale || target.CurrentCapacity.State == agents.CapacityUnknown {
			t.Fatalf("freshness and capacity disagree: %+v", target)
		}
		for _, issue := range snapshot.Issues {
			if issue.Code == "INVENTORY_MISSING" || issue.Code == "TARGET_CAPACITY_UNKNOWN" {
				t.Fatalf("fresh local inventory retained an obsolete issue: %+v", issue)
			}
		}
		if calls := source.calls[endpointFor("local", "default")]; calls != restart+1 {
			t.Fatalf("fresh cache was not reused: %d samples after restart %d", calls, restart)
		}
		coordinator.Close()
	}
}

func TestCapacityPreflightRefreshesOnlySelectedTargetsAndRetainsFailures(t *testing.T) {
	for _, online := range []bool{true, false} {
		t.Run(map[bool]string{true: "collection failure", false: "offline"}[online], func(t *testing.T) {
			catalog, source := capacityObservationFixture(t)
			remote := source.base[0]
			remote.Target.ID, remote.Target.AgentID, remote.Target.Kind = "agent:node", "node", agents.RuntimeKindAgent
			remote.Target.Online = online
			remoteKey := endpointFor("agent:node", "default")
			source.base = append(source.base, remote)
			source.failures[remoteKey] = errors.New("inventory probe failed")
			unrelated := remote
			unrelated.Target.ID = "agent:unrelated"
			source.base = append(source.base, unrelated)
			catalog.worlds["room"][1].TargetIDs = []string{"agent:node"}
			coordinator, err := runtimeobservation.New(source)
			if err != nil {
				t.Fatal(err)
			}
			defer coordinator.Close()
			service, err := NewService(catalog, coordinator, newTopologyTestStore(t))
			if err != nil {
				t.Fatal(err)
			}
			preview, err := service.PreviewStartCapacity(context.Background(), "room", nil)
			if err != nil || len(preview.Targets) != 2 || !preview.RequiresRiskConfirmation {
				t.Fatalf("partial failure hidden: %+v %v", preview, err)
			}
			for _, target := range preview.Targets {
				if target.TargetID == "local" && target.Capacity.State == agents.CapacityUnknown {
					t.Fatal("remote failure prevented local observation")
				}
				if target.TargetID == "agent:node" && target.Capacity.State != agents.CapacityUnknown {
					t.Fatal("remote failure used local capacity")
				}
			}
			wantRemoteCalls := 0
			if online {
				wantRemoteCalls = 1
			}
			if source.calls[remoteKey] != wantRemoteCalls || source.calls[endpointFor("local", "default")] != 1 || source.calls[endpointFor("agent:unrelated", "default")] != 0 {
				t.Fatalf("unexpected sampling scope: %+v", source.calls)
			}
		})
	}
}

func TestCapacityPreflightRefreshesMissingSiblingInstallation(t *testing.T) {
	catalog, source := capacityObservationFixture(t)
	primary := source.fresh[endpointFor("local", "default")]
	secondary := source.base[0]
	secondary.Target.Config.InstallationID = "secondary"
	secondaryFresh := primary
	secondaryFresh.Target = secondary.Target
	secondaryFresh.Inventory.Processes = []shared.ShardProcessReport{{PID: 987, Cluster: "Other", Shard: "Master"}}
	source.base = []agents.RuntimeTargetInventory{primary, secondary}
	source.fresh[endpointFor("local", "secondary")] = secondaryFresh
	combined := machineInventories(source.base)["local"]
	if !combined.Stale || combined.Capacity.State != agents.CapacityUnknown {
		t.Fatalf("fresh default hid a missing installation: %+v", combined)
	}
	coordinator, err := runtimeobservation.New(source)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	service, err := NewService(catalog, coordinator, newTopologyTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewStartCapacity(context.Background(), "room", nil)
	if err != nil || len(preview.Targets) != 1 {
		t.Fatalf("preview=%+v error=%v", preview, err)
	}
	if source.calls[endpointFor("local", "default")] != 0 || source.calls[endpointFor("local", "secondary")] != 1 || preview.Targets[0].CurrentRunningShards != 1 {
		t.Fatalf("sibling process missing or fresh default resampled: calls=%+v preview=%+v", source.calls, preview)
	}
	if preview.Targets[0].Capacity.State != agents.CapacityOvercommitted || !preview.RequiresRiskConfirmation {
		t.Fatalf("combined machine capacity ignored sibling: %+v", preview)
	}
}

func TestCapacityPreflightReusesExplicitRefreshAndRejectsInvalidScopeBeforeSampling(t *testing.T) {
	catalog, source := capacityObservationFixture(t)
	coordinator, err := runtimeobservation.New(source)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	service, err := NewService(catalog, coordinator, newTopologyTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]StartCapacitySelection{
		{{RoomID: "room"}, {RoomID: "room"}},
		{{RoomID: "room", WorldIDs: []string{"missing"}}},
		{{RoomID: "room", WorldIDs: []string{"master", "master"}}},
	} {
		if _, err := service.PreviewBatchStartCapacity(context.Background(), invalid); err == nil {
			t.Fatalf("invalid scope accepted: %+v", invalid)
		}
	}
	if len(source.calls) != 0 {
		t.Fatalf("invalid request triggered a sample: %+v", source.calls)
	}
	results, err := coordinator.Refresh(context.Background(), runtimeobservation.Scope{TargetID: "local"})
	if err != nil || len(results) != 1 || results[0].State != runtimeobservation.StateFresh {
		t.Fatalf("explicit refresh=%+v error=%v", results, err)
	}
	preview, err := service.PreviewStartCapacity(context.Background(), "room", nil)
	if err != nil || len(preview.Targets) != 1 || preview.RequiresRiskConfirmation || preview.Targets[0].Capacity.State == agents.CapacityUnknown {
		t.Fatalf("explicit fresh observation ignored: %+v error=%v", preview, err)
	}
	if calls := source.calls[endpointFor("local", "default")]; calls != 1 {
		t.Fatalf("explicit observation was sampled again: %d", calls)
	}
}
