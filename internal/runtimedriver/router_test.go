package runtimedriver

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"
)

func TestTargetFromLocalPlacementUsesDefaultInstallation(t *testing.T) {
	target := targetFromPlacement(topology.ExecutionPlacement{
		AppliedTargetID: "local",
		Revision:        "revision-1",
		Room:            rooms.Room{DirectoryName: "Cluster_1"},
		World:           rooms.World{DirectoryName: "Master"},
	}, "room-1", "world-1")

	if target.InstallationID != "default" || target.TargetID != "local" || target.Cluster != "Cluster_1" || target.Shard != "Master" {
		t.Fatalf("unexpected local target: %#v", target)
	}
}

type cpuLifecyclePlacement struct {
	placement  topology.ExecutionPlacement
	allocation topology.CPUAllocation
	recorded   []shared.RuntimeCPUResult
}

func (p *cpuLifecyclePlacement) AppliedPlacement(string, string) (topology.ExecutionPlacement, error) {
	return p.placement, nil
}

func (p *cpuLifecyclePlacement) ResolveExecution(context.Context, string, string) (topology.ExecutionPlacement, error) {
	return p.placement, nil
}

func (p *cpuLifecyclePlacement) CPUAllocation(string, string) (topology.CPUAllocation, error) {
	return p.allocation, nil
}

func (p *cpuLifecyclePlacement) RecordCPUResult(_ string, _ string, result shared.RuntimeCPUResult) (topology.CPUAllocation, error) {
	p.recorded = append(p.recorded, result)
	return p.allocation, nil
}

type cpuLifecycleLease struct{}

func (cpuLifecycleLease) Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error) {
	return operationlease.Lease{LeaseID: "lease", FencingToken: 1, ExpiresAt: time.Now().UTC().Add(time.Minute)}, nil
}

func (cpuLifecycleLease) Release(operationlease.Lease) error { return nil }

type cpuLifecycleDriver struct {
	Driver
	events   []string
	applyErr error
}

func (d *cpuLifecycleDriver) Kind() Kind { return KindNative }
func (d *cpuLifecycleDriver) Capabilities() []Capability {
	return []Capability{CapabilityLifecycle, CapabilityExclusiveCPU}
}
func (d *cpuLifecycleDriver) Status(context.Context, Target) (shared.ShardRuntimeStatus, error) {
	return shared.ShardRuntimeStatus{State: "running", SessionExists: true}, nil
}
func (d *cpuLifecycleDriver) ExecuteShard(_ context.Context, _ Target, _ Operation, action shared.ShardAction, _ time.Duration) (shared.ShardOperationResult, error) {
	d.events = append(d.events, string(action))
	return shared.ShardOperationResult{Action: action, Status: shared.ShardRuntimeStatus{State: "running", SessionExists: true}}, nil
}
func (d *cpuLifecycleDriver) PrepareCPU(_ context.Context, _ Target, _ Operation, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	d.events = append(d.events, "cpu.prepare")
	return shared.RuntimeCPUResult{Policy: request.Policy, LogicalCPUIds: request.LogicalCPUIds, State: shared.RuntimeCPUStatePrepared}, nil
}
func (d *cpuLifecycleDriver) ApplyCPU(_ context.Context, _ Target, _ Operation, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	d.events = append(d.events, "cpu.apply")
	if d.applyErr != nil {
		return shared.RuntimeCPUResult{}, d.applyErr
	}
	return shared.RuntimeCPUResult{Policy: request.Policy, LogicalCPUIds: request.LogicalCPUIds, EffectiveCPUIds: request.LogicalCPUIds, State: shared.RuntimeCPUStateApplied, Enforced: true}, nil
}
func (d *cpuLifecycleDriver) ObserveCPU(context.Context, Target, shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	return shared.RuntimeCPUResult{}, nil
}

func newCPULifecycleRouter(t *testing.T, applyErr error) (*Router, *cpuLifecyclePlacement, *cpuLifecycleDriver) {
	t.Helper()
	placement := &cpuLifecyclePlacement{
		placement: topology.ExecutionPlacement{
			Room: rooms.Room{DirectoryName: "Cluster_1"}, World: rooms.World{DirectoryName: "Master"},
			Revision: "revision-1", AppliedTargetID: "local",
		},
		allocation: topology.CPUAllocation{
			TargetID: "local", RoomID: "room-1", WorldID: "world-1",
			Policy: topology.CPUPolicyExclusive, LogicalCPUIds: []int{0, 1},
		},
	}
	driver := &cpuLifecycleDriver{applyErr: applyErr}
	router, err := NewRouter(placement, cpuLifecycleLease{}, driver, driver)
	if err != nil {
		t.Fatal(err)
	}
	return router, placement, driver
}

func TestShardStartPreparesAndAppliesCPUAllocation(t *testing.T) {
	router, placement, driver := newCPULifecycleRouter(t, nil)
	request := shared.ShardOperationRequest{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-1",
		Action: shared.ShardActionStart, TopologyRevision: "revision-1",
	}
	if _, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(driver.events, []string{"cpu.prepare", string(shared.ShardActionStart), "cpu.apply"}) {
		t.Fatalf("events=%v", driver.events)
	}
	if len(placement.recorded) != 2 || placement.recorded[0].State != shared.RuntimeCPUStatePrepared || placement.recorded[1].State != shared.RuntimeCPUStateApplied {
		t.Fatalf("recorded=%#v", placement.recorded)
	}
}

func TestShardStartStopsNewInstanceWhenCPUApplyFails(t *testing.T) {
	applyErr := errors.New("CPU enforcement failed")
	router, placement, driver := newCPULifecycleRouter(t, applyErr)
	request := shared.ShardOperationRequest{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-1",
		Action: shared.ShardActionStart, TopologyRevision: "revision-1",
	}
	if _, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second); !errors.Is(err, applyErr) {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(driver.events, []string{"cpu.prepare", string(shared.ShardActionStart), "cpu.apply", string(shared.ShardActionStop)}) {
		t.Fatalf("events=%v", driver.events)
	}
	if len(placement.recorded) != 1 || placement.recorded[0].State != shared.RuntimeCPUStatePrepared {
		t.Fatalf("recorded=%#v", placement.recorded)
	}
}
