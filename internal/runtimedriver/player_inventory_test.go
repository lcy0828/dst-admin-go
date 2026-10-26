package runtimedriver

import (
	"context"
	"errors"
	"testing"

	"dont/internal/agents"
	"dont/internal/playerinventory"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"
)

func TestAgentInventoryKeepsTypedFailureAndNeverMasksTransportFailure(t *testing.T) {
	offline := agents.ErrAgentOffline
	for _, tt := range []struct {
		code string
		err  error
	}{{"SAVE_INVALID", nil}, {"SOURCE_AMBIGUOUS", nil}, {"", offline}} {
		d, e := NewAgent(failedMigrationExecutor{result: agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{PlayerInventory: &shared.PlayerInventory{PlayerID: "KU_01234567", Code: tt.code}}}, err: tt.err})
		if e != nil {
			t.Fatal(e)
		}
		_, e = d.ReadPlayerInventory(context.Background(), Target{TargetID: "agent:node", InstallationID: "default"}, shared.PlayerInventoryRequest{PlayerID: "KU_01234567", Source: "save"})
		if tt.err != nil {
			if !errors.Is(e, tt.err) {
				t.Fatal(e)
			}
		} else {
			var typed *shared.InventoryError
			if !errors.As(e, &typed) || typed.Code != tt.code {
				t.Fatal(e)
			}
		}
	}
}

// Embedding the existing lifecycle fixture lets this test prove that neither
// a legacy node nor an offline node falls through to a local execution path.
type inventoryRouterDriver struct {
	*cpuLifecycleDriver
	calls   int
	targets []Target
	after   func()
	err     error
}

func (d *inventoryRouterDriver) Capabilities() []Capability {
	return []Capability{CapabilityPlayerInventory}
}
func (d *inventoryRouterDriver) ReadPlayerInventory(_ context.Context, target Target, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	d.calls++
	d.targets = append(d.targets, target)
	if d.after != nil {
		d.after()
	}
	return shared.PlayerInventory{PlayerID: q.PlayerID, Source: q.Source, Character: target.TargetID + "/" + target.InstallationID + "/" + target.Shard}, d.err
}

func TestInventoryRouterUsesAppliedNodeInstallationAndRejectsLateOrOfflineResult(t *testing.T) {
	placements := &cpuLifecyclePlacement{}
	local := &inventoryRouterDriver{cpuLifecycleDriver: &cpuLifecycleDriver{}}
	remote := &inventoryRouterDriver{cpuLifecycleDriver: &cpuLifecycleDriver{}}
	leaseCount := 0
	r, err := NewRouter(placements, cpuLifecycleLease{acquires: &leaseCount}, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	query := shared.PlayerInventoryRequest{PlayerID: "KU_01234567", Source: "save"}
	for _, target := range []struct{ node, installation, world string }{{"agent:forest", "default", "Master"}, {"agent:caves", "second", "Caves"}} {
		placements.placement = topology.ExecutionPlacement{
			AppliedTargetID: target.node, AppliedInstallationID: target.installation, Revision: "revision-1",
			Target: agents.RuntimeTarget{ID: target.node, Kind: agents.RuntimeKindAgent, Capabilities: []string{shared.PlayerInventoryCapability}},
			Room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1"}, World: rooms.World{ID: target.world, DirectoryName: target.world},
		}
		value, err := r.ReadPlayerInventory(context.Background(), "room", target.world, query)
		if err != nil || value.Character != target.node+"/"+target.installation+"/"+target.world || value.WorldID != target.world {
			t.Fatalf("wrong applied source: %+v %v", value, err)
		}
	}
	if local.calls != 0 || remote.calls != 2 || leaseCount != 0 {
		t.Fatalf("read used local fallback or mutation lease: local=%d remote=%d leases=%d", local.calls, remote.calls, leaseCount)
	}
	remote.after = func() { placements.placement.AppliedInstallationID = "migrated" }
	if _, err := r.ReadPlayerInventory(context.Background(), "room", "Caves", query); playerinventory.Code(err) != "PLAYER_LOCATION_UNCERTAIN" {
		t.Fatalf("accepted result from previous placement: %v", err)
	}
	remote.after = nil
	remote.err = agents.ErrAgentOffline
	if _, err := r.ReadPlayerInventory(context.Background(), "room", "Caves", query); !errors.Is(err, agents.ErrAgentOffline) || local.calls != 0 {
		t.Fatalf("offline target fell back locally: %v local=%d", err, local.calls)
	}
}
func TestInventoryRouterRejectsLegacyAgentBeforeReading(t *testing.T) {
	placements := &cpuLifecyclePlacement{placement: topology.ExecutionPlacement{AppliedTargetID: "agent:node", AppliedInstallationID: "default", Target: agents.RuntimeTarget{ID: "agent:node", Kind: agents.RuntimeKindAgent}}}
	local := &inventoryRouterDriver{cpuLifecycleDriver: &cpuLifecycleDriver{}}
	remote := &inventoryRouterDriver{cpuLifecycleDriver: &cpuLifecycleDriver{}}
	r, e := NewRouter(placements, cpuLifecycleLease{}, local, remote)
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.ReadPlayerInventory(context.Background(), "room", "world", shared.PlayerInventoryRequest{PlayerID: "KU_01234567", Source: "save"})
	if !errors.Is(e, ErrCapabilityMissing) || local.calls != 0 || remote.calls != 0 {
		t.Fatal(e, local.calls, remote.calls)
	}
}
