package runtimedriver

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"dont/internal/agents"
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
	placement     topology.ExecutionPlacement
	allocation    topology.CPUAllocation
	allocationErr error
	recorded      []shared.RuntimeCPUResult
	failures      []error
}

func (p *cpuLifecyclePlacement) AppliedPlacement(string, string) (topology.ExecutionPlacement, error) {
	return p.placement, nil
}

func (p *cpuLifecyclePlacement) ResolveExecution(context.Context, string, string) (topology.ExecutionPlacement, error) {
	return p.placement, nil
}

func (p *cpuLifecyclePlacement) CPUAllocation(string, string) (topology.CPUAllocation, error) {
	return p.allocation, p.allocationErr
}

func (p *cpuLifecyclePlacement) RecordCPUResult(_ string, _ string, result shared.RuntimeCPUResult) (topology.CPUAllocation, error) {
	p.recorded = append(p.recorded, result)
	return p.allocation, nil
}

func (p *cpuLifecyclePlacement) RecordCPUFailure(_ string, _ string, cause error) (topology.CPUAllocation, error) {
	p.failures = append(p.failures, cause)
	return p.allocation, nil
}

type cpuLifecycleLease struct {
	acquires *int
	releases *int
}

func (l cpuLifecycleLease) Acquire(_ context.Context, roomID, operationKey string, _ time.Duration) (operationlease.Lease, error) {
	if l.acquires != nil {
		*l.acquires++
	}
	return operationlease.Lease{RoomID: roomID, OperationKey: operationKey, LeaseID: "lease", FencingToken: 1, ExpiresAt: time.Now().UTC().Add(time.Minute)}, nil
}

func (l cpuLifecycleLease) Release(operationlease.Lease) error {
	if l.releases != nil {
		*l.releases++
	}
	return nil
}

type cpuLifecycleDriver struct {
	Driver
	events           []string
	applyErr         error
	releaseErr       error
	lifecycleErr     error
	lifecycleStatus  *shared.ShardRuntimeStatus
	status           shared.ShardRuntimeStatus
	statusErr        error
	stopStatus       *shared.ShardRuntimeStatus
	stopAtDeadline   bool
	releaseEntryErr  error
	cpuCap           bool
	consoleOperation Operation
	consoleRequest   shared.RuntimeConsoleRequest
}

func (d *cpuLifecycleDriver) Kind() Kind { return KindNative }
func (d *cpuLifecycleDriver) Capabilities() []Capability {
	result := []Capability{CapabilityLifecycle}
	if d.cpuCap {
		result = append(result, CapabilityExclusiveCPU)
	}
	return result
}
func (d *cpuLifecycleDriver) Status(context.Context, Target) (shared.ShardRuntimeStatus, error) {
	if d.statusErr != nil {
		return shared.ShardRuntimeStatus{}, d.statusErr
	}
	if d.status.State != "" {
		return d.status, nil
	}
	return shared.ShardRuntimeStatus{State: "running", SessionExists: true}, nil
}
func (d *cpuLifecycleDriver) ExecuteShard(ctx context.Context, _ Target, _ Operation, action shared.ShardAction, _ time.Duration) (shared.ShardOperationResult, error) {
	d.events = append(d.events, string(action))
	status := shared.ShardRuntimeStatus{State: "running", SessionExists: true}
	if d.lifecycleStatus != nil {
		status = *d.lifecycleStatus
	}
	if action == shared.ShardActionStop {
		if d.stopAtDeadline {
			<-ctx.Done()
		}
		status = shared.ShardRuntimeStatus{State: "stopped"}
		if d.stopStatus != nil {
			status = *d.stopStatus
		}
	}
	return shared.ShardOperationResult{Action: action, Status: status}, d.lifecycleErr
}
func (d *cpuLifecycleDriver) PrepareCPU(_ context.Context, _ Target, _ Operation, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	d.events = append(d.events, "cpu.prepare")
	return shared.RuntimeCPUResult{Policy: request.Policy, LogicalCPUIds: request.LogicalCPUIds, State: shared.RuntimeCPUStatePrepared}, nil
}
func (d *cpuLifecycleDriver) ApplyCPU(ctx context.Context, _ Target, _ Operation, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	event := "cpu.apply"
	if request.Policy == shared.RuntimeCPUPolicyNone {
		event = "cpu.release"
		d.releaseEntryErr = ctx.Err()
	}
	d.events = append(d.events, event)
	if request.Policy == shared.RuntimeCPUPolicyNone && d.releaseErr != nil {
		return shared.RuntimeCPUResult{}, d.releaseErr
	}
	if request.Policy != shared.RuntimeCPUPolicyNone && d.applyErr != nil {
		return shared.RuntimeCPUResult{}, d.applyErr
	}
	state := shared.RuntimeCPUStateApplied
	if request.Policy == shared.RuntimeCPUPolicyNone {
		state = shared.RuntimeCPUStateReleased
	}
	return shared.RuntimeCPUResult{Policy: request.Policy, LogicalCPUIds: request.LogicalCPUIds, EffectiveCPUIds: request.LogicalCPUIds, State: state, Enforced: true}, nil
}
func (d *cpuLifecycleDriver) ObserveCPU(context.Context, Target, shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	return shared.RuntimeCPUResult{}, nil
}

func (d *cpuLifecycleDriver) SendConsole(_ context.Context, _ Target, operation Operation, request shared.RuntimeConsoleRequest, _ time.Duration) (shared.RuntimeOperationResult, error) {
	d.consoleOperation, d.consoleRequest = operation, request
	return shared.RuntimeOperationResult{Message: "sent"}, nil
}

func TestRemoteConsoleReusesBorrowedRoomLease(t *testing.T) {
	placement := &cpuLifecyclePlacement{placement: topology.ExecutionPlacement{
		Room: rooms.Room{DirectoryName: "Cluster_1"}, World: rooms.World{DirectoryName: "Master"},
		Revision: "revision-1", AppliedTargetID: "agent:node-a",
		Target: agents.RuntimeTarget{ID: "agent:node-a", AgentID: "node-a", Config: agents.RuntimeConfig{InstallationID: "primary"}},
	}}
	acquires, releases := 0, 0
	leases := cpuLifecycleLease{acquires: &acquires, releases: &releases}
	driver := &cpuLifecycleDriver{}
	router, err := NewRouter(placement, leases, driver, driver)
	if err != nil {
		t.Fatal(err)
	}
	borrowed := operationlease.Lease{
		RoomID: "room-1", OperationKey: "mod.activate:job-1", LeaseID: "borrowed-lease",
		FencingToken: 7, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	ctx := operationlease.WithBorrowedLeases(context.Background(), borrowed)
	if _, err := router.SendID(ctx, "room-1", "world-1", shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: `c_announce("test")`}); err != nil {
		t.Fatal(err)
	}
	if acquires != 0 || releases != 0 || driver.consoleOperation.Key == "" || driver.consoleOperation.LeaseID != borrowed.LeaseID || driver.consoleOperation.FencingToken != borrowed.FencingToken {
		t.Fatalf("acquires=%d releases=%d operation=%#v", acquires, releases, driver.consoleOperation)
	}

	if _, err := router.SendID(context.Background(), "room-1", "world-1", shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: `c_announce("test")`}); err != nil {
		t.Fatal(err)
	}
	if acquires != 1 || releases != 1 {
		t.Fatalf("ordinary send acquires=%d releases=%d", acquires, releases)
	}
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
	driver := &cpuLifecycleDriver{applyErr: applyErr, cpuCap: true}
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
	if !reflect.DeepEqual(driver.events, []string{"cpu.prepare", string(shared.ShardActionStart), "cpu.apply", string(shared.ShardActionStop), "cpu.release"}) {
		t.Fatalf("events=%v", driver.events)
	}
	if len(placement.recorded) != 2 || placement.recorded[0].State != shared.RuntimeCPUStatePrepared || placement.recorded[1].State != shared.RuntimeCPUStateReleased || len(placement.failures) != 1 {
		t.Fatalf("recorded=%#v failures=%v", placement.recorded, placement.failures)
	}
}

func TestShardStopReleasesCPUAllocation(t *testing.T) {
	router, placement, driver := newCPULifecycleRouter(t, nil)
	request := shared.ShardOperationRequest{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-stop", Action: shared.ShardActionStop, TopologyRevision: "revision-1"}
	if _, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(driver.events, []string{string(shared.ShardActionStop), "cpu.release"}) {
		t.Fatalf("events=%v", driver.events)
	}
	if len(placement.recorded) != 1 || placement.recorded[0].State != shared.RuntimeCPUStateReleased {
		t.Fatalf("recorded=%#v", placement.recorded)
	}
}

func TestShardStopDoesNotReleaseCPUWithoutConfirmedStop(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    shared.ShardRuntimeStatus
		stopErr   error
		wantError error
	}{
		{name: "still running", status: shared.ShardRuntimeStatus{State: "running", SessionExists: true}, wantError: errShardStopUnconfirmed},
		{name: "unknown after error", status: shared.ShardRuntimeStatus{}, stopErr: context.DeadlineExceeded, wantError: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, placement, driver := newCPULifecycleRouter(t, nil)
			driver.stopStatus = &test.status
			driver.lifecycleErr = test.stopErr
			request := shared.ShardOperationRequest{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-stop-unconfirmed", Action: shared.ShardActionStop, TopologyRevision: "revision-1"}
			_, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("error=%v", err)
			}
			if !reflect.DeepEqual(driver.events, []string{string(shared.ShardActionStop)}) || len(placement.recorded) != 0 {
				t.Fatalf("events=%v recorded=%#v", driver.events, placement.recorded)
			}
		})
	}
}

func TestShardStopReturnsCPUReleaseFailure(t *testing.T) {
	releaseErr := errors.New("CPU release failed")
	router, placement, driver := newCPULifecycleRouter(t, nil)
	driver.releaseErr = releaseErr
	request := shared.ShardOperationRequest{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-stop-release-failure", Action: shared.ShardActionStop, TopologyRevision: "revision-1"}
	if _, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second); !errors.Is(err, releaseErr) {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(driver.events, []string{string(shared.ShardActionStop), "cpu.release"}) || len(placement.failures) != 1 {
		t.Fatalf("events=%v failures=%v", driver.events, placement.failures)
	}
}

func TestShardStopWithoutCPUAllocationRemainsSuccessful(t *testing.T) {
	router, placement, driver := newCPULifecycleRouter(t, nil)
	placement.allocationErr = topology.ErrResourceNotFound
	request := shared.ShardOperationRequest{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-stop-legacy", Action: shared.ShardActionStop, TopologyRevision: "revision-1"}
	if _, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second); err != nil {
		t.Fatalf("legacy stop failed because CPU allocation was absent: %v", err)
	}
	if !reflect.DeepEqual(driver.events, []string{string(shared.ShardActionStop)}) {
		t.Fatalf("events=%v", driver.events)
	}
}

func TestShardStopSkipsCPUDriverWithoutAdvertisedCapability(t *testing.T) {
	router, _, driver := newCPULifecycleRouter(t, nil)
	driver.cpuCap = false
	request := shared.ShardOperationRequest{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-stop-no-capability", Action: shared.ShardActionStop, TopologyRevision: "revision-1"}
	if _, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second); err != nil {
		t.Fatalf("stop failed through an unavailable CPU backend: %v", err)
	}
	if !reflect.DeepEqual(driver.events, []string{string(shared.ShardActionStop)}) {
		t.Fatalf("events=%v", driver.events)
	}
}

func TestCanceledStartReconcilesRunningInstanceCPU(t *testing.T) {
	router, placement, driver := newCPULifecycleRouter(t, nil)
	driver.lifecycleErr = context.Canceled
	request := shared.ShardOperationRequest{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-canceled", Action: shared.ShardActionStart, TopologyRevision: "revision-1"}
	if _, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(driver.events, []string{"cpu.prepare", string(shared.ShardActionStart), "cpu.apply"}) {
		t.Fatalf("events=%v", driver.events)
	}
	if len(placement.recorded) != 2 || placement.recorded[1].State != shared.RuntimeCPUStateApplied {
		t.Fatalf("recorded=%#v", placement.recorded)
	}
}

func TestCPUApplyFailureDoesNotReleaseWhenRollbackStopIsUnconfirmed(t *testing.T) {
	applyErr := errors.New("CPU enforcement failed")
	router, placement, driver := newCPULifecycleRouter(t, applyErr)
	running := shared.ShardRuntimeStatus{State: "running", SessionExists: true}
	driver.stopStatus = &running
	request := shared.ShardOperationRequest{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-rollback-unconfirmed", Action: shared.ShardActionStart, TopologyRevision: "revision-1"}
	_, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second)
	if !errors.Is(err, applyErr) || !errors.Is(err, errShardStopUnconfirmed) {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(driver.events, []string{"cpu.prepare", string(shared.ShardActionStart), "cpu.apply", string(shared.ShardActionStop)}) || len(placement.recorded) != 1 {
		t.Fatalf("events=%v recorded=%#v", driver.events, placement.recorded)
	}
}

func TestCPUApplyFailureReturnsRollbackReleaseFailure(t *testing.T) {
	applyErr := errors.New("CPU enforcement failed")
	releaseErr := errors.New("CPU release failed")
	router, placement, driver := newCPULifecycleRouter(t, applyErr)
	driver.releaseErr = releaseErr
	request := shared.ShardOperationRequest{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-rollback-release-failure", Action: shared.ShardActionStart, TopologyRevision: "revision-1"}
	_, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second)
	if !errors.Is(err, applyErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(driver.events, []string{"cpu.prepare", string(shared.ShardActionStart), "cpu.apply", string(shared.ShardActionStop), "cpu.release"}) || len(placement.failures) != 1 {
		t.Fatalf("events=%v failures=%v", driver.events, placement.failures)
	}
}

func TestCPUApplyRollbackReleaseGetsIndependentTimeout(t *testing.T) {
	applyErr := errors.New("CPU enforcement failed")
	router, _, driver := newCPULifecycleRouter(t, applyErr)
	router.cleanupTTL = 5 * time.Millisecond
	driver.stopAtDeadline = true
	request := shared.ShardOperationRequest{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-rollback-timeout-boundary", Action: shared.ShardActionStart, TopologyRevision: "revision-1"}
	_, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second)
	if !errors.Is(err, applyErr) {
		t.Fatalf("error=%v", err)
	}
	if driver.releaseEntryErr != nil || !reflect.DeepEqual(driver.events, []string{"cpu.prepare", string(shared.ShardActionStart), "cpu.apply", string(shared.ShardActionStop), "cpu.release"}) {
		t.Fatalf("release context error=%v events=%v", driver.releaseEntryErr, driver.events)
	}
}

func TestLifecycleErrorWithUnknownStatusDoesNotReleaseCPU(t *testing.T) {
	router, placement, driver := newCPULifecycleRouter(t, nil)
	driver.lifecycleErr = context.DeadlineExceeded
	driver.lifecycleStatus = &shared.ShardRuntimeStatus{}
	driver.statusErr = errors.New("status unavailable")
	request := shared.ShardOperationRequest{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-status-unknown", Action: shared.ShardActionStart, TopologyRevision: "revision-1"}
	_, err := router.ExecutePlacedShard(context.Background(), "room-1", "world-1", request, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errShardStopUnconfirmed) {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(driver.events, []string{"cpu.prepare", string(shared.ShardActionStart)}) || len(placement.recorded) != 1 {
		t.Fatalf("events=%v recorded=%#v", driver.events, placement.recorded)
	}
}

func TestNonePolicyReleasesStoppedRuntimeImmediately(t *testing.T) {
	router, _, driver := newCPULifecycleRouter(t, nil)
	driver.statusErr = errors.New("status must not be called")
	allocation := topology.CPUAllocation{TargetID: "local", RoomID: "room-1", WorldID: "world-1", Policy: topology.CPUPolicyNone}
	result, err := router.ApplyCPUAllocation(context.Background(), allocation)
	if err != nil || result.State != shared.RuntimeCPUStateReleased {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if !reflect.DeepEqual(driver.events, []string{"cpu.release"}) {
		t.Fatalf("events=%v", driver.events)
	}
}
