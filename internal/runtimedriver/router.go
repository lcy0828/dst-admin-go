package runtimedriver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimefiles"
	"dont/internal/topology"
	"dont/shared"

	"github.com/google/uuid"
)

var errShardStopUnconfirmed = errors.New("分片停止未得到明确状态确认")

type PlacementResolver interface {
	AppliedPlacement(string, string) (topology.ExecutionPlacement, error)
	ResolveExecution(context.Context, string, string) (topology.ExecutionPlacement, error)
}

type LeaseService interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

type cpuAllocationResolver interface {
	CPUAllocation(string, string) (topology.CPUAllocation, error)
}

type cpuObservationRecorder interface {
	RecordCPUResult(string, string, shared.RuntimeCPUResult) (topology.CPUAllocation, error)
}

type cpuFailureRecorder interface {
	RecordCPUFailure(string, string, error) (topology.CPUAllocation, error)
}

type Router struct {
	placements PlacementResolver
	leases     LeaseService
	local      Driver
	remote     Driver
	leaseTTL   time.Duration
	cleanupTTL time.Duration
}

func NewRouter(placements PlacementResolver, leases LeaseService, local, remote Driver) (*Router, error) {
	if placements == nil || leases == nil || local == nil || remote == nil {
		return nil, errors.New("runtime driver router dependencies are required")
	}
	return &Router{
		placements: placements, leases: leases, local: local, remote: remote,
		leaseTTL: 2 * time.Minute, cleanupTTL: 30 * time.Second,
	}, nil
}

func (r *Router) DriverTarget(ctx context.Context, roomID, worldID string) (Driver, Target, error) {
	applied, err := r.placements.AppliedPlacement(roomID, worldID)
	if err != nil {
		return nil, Target{}, err
	}
	placement := applied
	driver := r.local
	if applied.AppliedTargetID != "local" {
		placement, err = r.placements.ResolveExecution(ctx, roomID, worldID)
		if err != nil {
			return nil, Target{}, err
		}
		driver = r.remote
	}
	return driver, targetFromPlacement(placement, roomID, worldID), nil
}

func targetFromPlacement(placement topology.ExecutionPlacement, roomID, worldID string) Target {
	installationID := strings.TrimSpace(placement.Target.Config.InstallationID)
	if placement.AppliedTargetID == "local" && installationID == "" {
		installationID = "default"
	}
	return Target{
		TargetID: placement.AppliedTargetID, InstallationID: installationID,
		RoomID: roomID, WorldID: worldID, Cluster: placement.Room.DirectoryName, Shard: placement.World.DirectoryName,
		TopologyRevision: placement.Revision,
	}
}

func (r *Router) IsLocalPlacement(roomID, worldID string) (bool, error) {
	applied, err := r.placements.AppliedPlacement(roomID, worldID)
	if err != nil {
		return false, err
	}
	return applied.AppliedTargetID == "local", nil
}

func (r *Router) MigrationTargets(plan topology.MigrationPlacement) (Driver, Target, Driver, Target, error) {
	if plan.SourceTargetID == "" || plan.TargetTargetID == "" || plan.SourceTargetID == plan.TargetTargetID {
		return nil, Target{}, nil, Target{}, ErrInvalidTarget
	}
	sourceDriver, targetDriver := r.remote, r.remote
	if plan.SourceTargetID == "local" {
		sourceDriver = r.local
	}
	if plan.TargetTargetID == "local" {
		targetDriver = r.local
	}
	source := Target{
		TargetID: plan.SourceTargetID, InstallationID: plan.Source.Config.InstallationID,
		RoomID: plan.Room.ID, WorldID: plan.World.ID, Cluster: plan.Room.DirectoryName,
		Shard: plan.World.DirectoryName, TopologyRevision: plan.Revision,
	}
	target := Target{
		TargetID: plan.TargetTargetID, InstallationID: plan.Target.Config.InstallationID,
		RoomID: plan.Room.ID, WorldID: plan.World.ID, Cluster: plan.Room.DirectoryName,
		Shard: plan.World.DirectoryName, TopologyRevision: plan.Revision,
	}
	if err := validateTarget(source); err != nil {
		return nil, Target{}, nil, Target{}, err
	}
	if err := validateTarget(target); err != nil {
		return nil, Target{}, nil, Target{}, err
	}
	return sourceDriver, source, targetDriver, target, nil
}

// Send keeps the established console sender contract while resolving the
// current applied Placement from the stable encoded room/world identifiers.
func (r *Router) Send(ctx context.Context, roomName, worldName, command string) error {
	_, err := r.SendID(ctx, rooms.EncodeID(roomName), rooms.EncodeID(worldName), shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: command})
	return err
}

func (r *Router) SendBackground(ctx context.Context, roomName, worldName, coalesceKey, command string) error {
	_, err := r.SendID(ctx, rooms.EncodeID(roomName), rooms.EncodeID(worldName), shared.RuntimeConsoleRequest{
		Mode: shared.ConsoleModeProbe, CoalesceKey: coalesceKey, Command: command,
	})
	return err
}

func (r *Router) SendID(ctx context.Context, roomID, worldID string, request shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeOperationResult{}, err
	}
	operation := Operation{ID: newOperationID()}
	if target.TargetID != "local" {
		lease, leaseErr := r.leases.Acquire(ctx, roomID, "runtime.console.send:"+worldID, r.leaseTTL)
		if leaseErr != nil {
			return shared.RuntimeOperationResult{}, leaseErr
		}
		defer r.leases.Release(lease)
		expires := lease.ExpiresAt.UTC()
		operation.Key, operation.LeaseID, operation.FencingToken, operation.LeaseExpiresAt = operation.ID, lease.LeaseID, lease.FencingToken, &expires
	}
	result, sendErr := driver.SendConsole(ctx, target, operation, request, 30*time.Second)
	result.TargetID = target.TargetID
	result.TopologyRevision = target.TopologyRevision
	if strings.HasPrefix(target.TargetID, "agent:") {
		result.AgentID = strings.TrimPrefix(target.TargetID, "agent:")
	}
	return result, sendErr
}

func (r *Router) Status(ctx context.Context, roomID, worldID string) (shared.ShardRuntimeStatus, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.ShardRuntimeStatus{}, err
	}
	return driver.Status(ctx, target)
}

// ExecutePlacedShard executes a lifecycle action against the currently
// applied Placement. The caller owns the room operation lease and passes its
// fencing proof in request, so this method must not acquire a second lease.
func (r *Router) ExecutePlacedShard(ctx context.Context, roomID, worldID string, request shared.ShardOperationRequest, timeout time.Duration) (shared.ShardOperationResult, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.ShardOperationResult{}, err
	}
	if request.ProtocolVersion != shared.ShardOperationProtocolVersion || strings.TrimSpace(request.OperationID) == "" || !shared.IsShardAction(request.Action) {
		return shared.ShardOperationResult{}, ErrInvalidTarget
	}
	if request.TopologyRevision != "" && request.TopologyRevision != target.TopologyRevision {
		return shared.ShardOperationResult{}, ErrTopologyChanged
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	operation := Operation{
		ID: request.OperationID, Key: request.OperationKey, LeaseID: request.LeaseID,
		FencingToken: request.FencingToken, LeaseExpiresAt: request.LeaseExpiresAt,
	}
	var cpuDriver CPUDriver
	var cpuRequest shared.RuntimeCPURequest
	if request.Action == shared.ShardActionStart || request.Action == shared.ShardActionRestart {
		resolver, ok := r.placements.(cpuAllocationResolver)
		if !ok {
			return shared.ShardOperationResult{}, ErrCapabilityMissing
		}
		allocation, allocationErr := resolver.CPUAllocation(roomID, worldID)
		if allocationErr != nil {
			return shared.ShardOperationResult{}, allocationErr
		}
		var supported bool
		cpuDriver, supported = driver.(CPUDriver)
		if !supported {
			return shared.ShardOperationResult{}, ErrCapabilityMissing
		}
		cpuRequest = cpuRequestFromAllocation(allocation)
		prepared, prepareErr := cpuDriver.PrepareCPU(ctx, target, cpuPhaseOperation(operation), cpuRequest)
		if prepareErr != nil {
			r.recordCPUFailure(roomID, worldID, prepareErr)
			return shared.ShardOperationResult{}, prepareErr
		}
		if err := r.recordCPUResult(roomID, worldID, prepared); err != nil {
			return shared.ShardOperationResult{}, err
		}
	}
	result, err := driver.ExecuteShard(ctx, target, operation, request.Action, timeout)
	if request.Action == shared.ShardActionStop {
		_, allocationErr := r.cpuAllocation(roomID, worldID)
		stopped := shardStopped(result.Status)
		if !stopped && err == nil {
			err = errShardStopUnconfirmed
		}
		if candidate, ok := driver.(CPUDriver); ok && allocationErr == nil && HasCapability(driver, CapabilityExclusiveCPU) && stopped {
			cleanupErr := r.releaseCPU(context.WithoutCancel(ctx), candidate, target, operation, roomID, worldID)
			if cleanupErr != nil {
				r.recordCPUFailure(roomID, worldID, cleanupErr)
				err = errors.Join(err, cleanupErr)
			}
		}
		return result, err
	}
	if err != nil && cpuDriver != nil {
		reconcileErr := r.reconcileCPUAfterLifecycleError(context.WithoutCancel(ctx), driver, cpuDriver, target, operation, roomID, worldID, cpuRequest, result.Status, err)
		return result, errors.Join(err, reconcileErr)
	}
	if err == nil && cpuDriver != nil {
		applied, applyErr := cpuDriver.ApplyCPU(ctx, target, cpuPhaseOperation(operation), cpuRequest)
		if applyErr != nil {
			stopContext, cancel := r.cleanupContext(ctx)
			rollback := cpuPhaseOperation(operation)
			stopped, stopErr := driver.ExecuteShard(stopContext, target, rollback, shared.ShardActionStop, r.cleanupTTL)
			cancel()
			if stopErr == nil && !shardStopped(stopped.Status) {
				stopErr = errShardStopUnconfirmed
			}
			var releaseErr error
			if shardStopped(stopped.Status) {
				releaseErr = r.releaseCPU(context.WithoutCancel(ctx), cpuDriver, target, rollback, roomID, worldID)
			}
			cleanupErr := errors.Join(applyErr, stopErr, releaseErr)
			r.recordCPUFailure(roomID, worldID, cleanupErr)
			return result, cleanupErr
		}
		if recordErr := r.recordCPUResult(roomID, worldID, applied); recordErr != nil {
			return result, recordErr
		}
	}
	return result, err
}

// ApplyCPUAllocation is called after desired policy validation/persistence.
// It serializes with room lifecycle operations and applies immediately when a
// Shard is running; stopped instances retain a prepared policy for next start.
func (r *Router) ApplyCPUAllocation(ctx context.Context, allocation topology.CPUAllocation) (shared.RuntimeCPUResult, error) {
	driver, target, err := r.DriverTarget(ctx, allocation.RoomID, allocation.WorldID)
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	if target.TargetID != allocation.TargetID {
		return shared.RuntimeCPUResult{}, ErrTopologyChanged
	}
	cpu, ok := driver.(CPUDriver)
	if !ok {
		return shared.RuntimeCPUResult{}, ErrCapabilityMissing
	}
	lease, err := r.leases.Acquire(ctx, allocation.RoomID, "runtime.cpu:"+allocation.WorldID, r.leaseTTL)
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	defer r.leases.Release(lease)
	expires := lease.ExpiresAt.UTC()
	operation := Operation{ID: newOperationID(), Key: newOperationID(), LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, LeaseExpiresAt: &expires}
	request := cpuRequestFromAllocation(allocation)
	if request.Policy == shared.RuntimeCPUPolicyNone {
		return cpu.ApplyCPU(ctx, target, cpuPhaseOperation(operation), request)
	}
	prepared, err := cpu.PrepareCPU(ctx, target, operation, request)
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	status, err := driver.Status(ctx, target)
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	if status.State != "running" && status.State != "starting" {
		return prepared, nil
	}
	return cpu.ApplyCPU(ctx, target, cpuPhaseOperation(operation), request)
}

func (r *Router) ObserveCPUAllocation(ctx context.Context, allocation topology.CPUAllocation) (shared.RuntimeCPUResult, error) {
	driver, target, err := r.DriverTarget(ctx, allocation.RoomID, allocation.WorldID)
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	if target.TargetID != allocation.TargetID {
		return shared.RuntimeCPUResult{}, ErrTopologyChanged
	}
	cpu, ok := driver.(CPUDriver)
	if !ok {
		return shared.RuntimeCPUResult{}, ErrCapabilityMissing
	}
	return cpu.ObserveCPU(ctx, target, cpuRequestFromAllocation(allocation))
}

func cpuRequestFromAllocation(allocation topology.CPUAllocation) shared.RuntimeCPURequest {
	return shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicy(allocation.Policy), LogicalCPUIds: append([]int(nil), allocation.LogicalCPUIds...)}
}

func cpuPhaseOperation(source Operation) Operation {
	source.ID, source.Key = newOperationID(), newOperationID()
	return source
}

func (r *Router) recordCPUResult(roomID, worldID string, result shared.RuntimeCPUResult) error {
	recorder, ok := r.placements.(cpuObservationRecorder)
	if !ok {
		return ErrCapabilityMissing
	}
	_, err := recorder.RecordCPUResult(roomID, worldID, result)
	return err
}

func (r *Router) recordCPUFailure(roomID, worldID string, cause error) {
	if cause == nil {
		return
	}
	if recorder, ok := r.placements.(cpuFailureRecorder); ok {
		_, _ = recorder.RecordCPUFailure(roomID, worldID, cause)
	}
}

func (r *Router) releaseCPU(ctx context.Context, cpu CPUDriver, target Target, operation Operation, roomID, worldID string) error {
	cleanupContext, cancel := r.cleanupContext(ctx)
	defer cancel()
	released, err := cpu.ApplyCPU(cleanupContext, target, cpuPhaseOperation(operation), shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyNone})
	if err != nil {
		return err
	}
	if err := r.recordCPUResult(roomID, worldID, released); errors.Is(err, topology.ErrResourceNotFound) {
		return nil
	} else {
		return err
	}
}

func (r *Router) cpuAllocation(roomID, worldID string) (topology.CPUAllocation, error) {
	resolver, ok := r.placements.(cpuAllocationResolver)
	if !ok {
		return topology.CPUAllocation{}, ErrCapabilityMissing
	}
	return resolver.CPUAllocation(roomID, worldID)
}

func (r *Router) reconcileCPUAfterLifecycleError(ctx context.Context, driver Driver, cpu CPUDriver, target Target, operation Operation, roomID, worldID string, request shared.RuntimeCPURequest, fallback shared.ShardRuntimeStatus, lifecycleErr error) error {
	status := fallback
	statusErr := error(nil)
	statusContext, cancelStatus := r.cleanupContext(ctx)
	observed, observeErr := driver.Status(statusContext, target)
	cancelStatus()
	if observeErr == nil {
		status = observed
	} else {
		statusErr = fmt.Errorf("重新确认分片状态: %w", observeErr)
	}
	if status.State == "running" || status.State == "starting" {
		applyContext, cancelApply := r.cleanupContext(ctx)
		applied, err := cpu.ApplyCPU(applyContext, target, cpuPhaseOperation(operation), request)
		cancelApply()
		if err == nil {
			if recordErr := r.recordCPUResult(roomID, worldID, applied); recordErr != nil {
				r.recordCPUFailure(roomID, worldID, errors.Join(lifecycleErr, statusErr, recordErr))
				return errors.Join(statusErr, recordErr)
			}
			return nil
		}
		stopContext, cancelStop := r.cleanupContext(ctx)
		stopped, stopErr := driver.ExecuteShard(stopContext, target, cpuPhaseOperation(operation), shared.ShardActionStop, r.cleanupTTL)
		cancelStop()
		if stopErr == nil && !shardStopped(stopped.Status) {
			stopErr = errShardStopUnconfirmed
		}
		var releaseErr error
		if shardStopped(stopped.Status) {
			releaseErr = r.releaseCPU(context.WithoutCancel(ctx), cpu, target, operation, roomID, worldID)
		}
		cleanupErr := errors.Join(statusErr, err, stopErr, releaseErr)
		r.recordCPUFailure(roomID, worldID, errors.Join(lifecycleErr, cleanupErr))
		return cleanupErr
	}
	if shardStopped(status) {
		releaseErr := r.releaseCPU(context.WithoutCancel(ctx), cpu, target, operation, roomID, worldID)
		if releaseErr != nil {
			r.recordCPUFailure(roomID, worldID, errors.Join(lifecycleErr, statusErr, releaseErr))
			return errors.Join(statusErr, releaseErr)
		}
		r.recordCPUFailure(roomID, worldID, lifecycleErr)
		return nil
	}
	unconfirmedErr := errShardStopUnconfirmed
	r.recordCPUFailure(roomID, worldID, errors.Join(lifecycleErr, statusErr, unconfirmedErr))
	return errors.Join(statusErr, unconfirmedErr)
}

func (r *Router) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := r.cleanupTTL
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func shardStopped(status shared.ShardRuntimeStatus) bool {
	return !status.SessionExists && status.State == "stopped"
}

func (r *Router) ConsoleHealth(ctx context.Context, roomID, worldID string) (shared.RuntimeConsoleHealth, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeConsoleHealth{}, err
	}
	return driver.ConsoleHealth(ctx, target)
}

func (r *Router) ReadLogs(ctx context.Context, roomID, worldID string, request shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	chunk, err := driver.ReadLogs(ctx, target, request)
	if err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	if err := runtimefiles.ValidateLogChunk(request, chunk); err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	return chunk, nil
}

func (r *Router) ReadArtifacts(ctx context.Context, roomID, worldID string, kind shared.ArtifactKind) (shared.RuntimeArtifactBundle, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeArtifactBundle{}, err
	}
	bundle, err := driver.ReadArtifacts(ctx, target, kind)
	if err != nil {
		return shared.RuntimeArtifactBundle{}, err
	}
	if err := runtimefiles.ValidateArtifactBundle(kind, bundle); err != nil {
		return shared.RuntimeArtifactBundle{}, err
	}
	return bundle, nil
}

func newOperationID() string { return strings.ToLower(uuid.NewString()) }
