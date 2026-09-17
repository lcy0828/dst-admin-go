package runtimedriver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"dont/internal/operationlease"
	"dont/internal/requesttiming"
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

type RuntimeMutationObserver interface {
	RuntimeTargetChanged(string)
}

// NotifyRuntimeTargetsChanged emits at most one mutation signal per target.
// Coordinators call it at their transaction boundary so partial failures and
// successful rollbacks are observed just like successful writes.
func NotifyRuntimeTargetsChanged(observer RuntimeMutationObserver, targetIDs ...string) {
	if observer == nil {
		return
	}
	seen := make(map[string]bool, len(targetIDs))
	for _, targetID := range targetIDs {
		targetID = strings.TrimSpace(targetID)
		if targetID == "" || seen[targetID] {
			continue
		}
		seen[targetID] = true
		observer.RuntimeTargetChanged(targetID)
	}
}

type Router struct {
	placements PlacementResolver
	leases     LeaseService
	endpoints  *EndpointRegistry
	leaseTTL   time.Duration
	cleanupTTL time.Duration
	mutations  RuntimeMutationObserver
}

func (r *Router) ConfigureMutationObserver(observer RuntimeMutationObserver) error {
	if observer == nil {
		return errors.New("runtime mutation observer is required")
	}
	r.mutations = observer
	return nil
}

func NewRouter(placements PlacementResolver, leases LeaseService, local, remote Driver) (*Router, error) {
	endpoints, err := NewEndpointRegistry(local, remote)
	if err != nil {
		return nil, errors.New("runtime driver router dependencies are required")
	}
	return NewRouterWithEndpoints(placements, leases, endpoints)
}

func NewRouterWithEndpoints(placements PlacementResolver, leases LeaseService, endpoints *EndpointRegistry) (*Router, error) {
	if placements == nil || leases == nil || endpoints == nil {
		return nil, errors.New("runtime driver router dependencies are required")
	}
	return &Router{
		placements: placements, leases: leases, endpoints: endpoints,
		leaseTTL: 2 * time.Minute, cleanupTTL: 30 * time.Second,
	}, nil
}

func (r *Router) DriverTarget(ctx context.Context, roomID, worldID string) (Driver, Target, error) {
	endpoint, target, err := r.EndpointTarget(ctx, roomID, worldID)
	return endpoint.Driver, target, err
}

func (r *Router) EndpointTarget(ctx context.Context, roomID, worldID string) (RuntimeEndpoint, Target, error) {
	defer requesttiming.Start(ctx, "routing.total")()
	finishApplied := requesttiming.Start(ctx, "routing.applied_placement")
	applied, err := r.placements.AppliedPlacement(roomID, worldID)
	finishApplied()
	if err != nil {
		return RuntimeEndpoint{}, Target{}, err
	}
	placement := applied
	if applied.AppliedTargetID != LocalTargetID || strings.TrimSpace(applied.AppliedInstallationID) == "" {
		finishResolve := requesttiming.Start(ctx, "routing.resolve_execution")
		placement, err = r.placements.ResolveExecution(ctx, roomID, worldID)
		finishResolve()
		if err != nil {
			return RuntimeEndpoint{}, Target{}, err
		}
	}
	target := targetFromPlacement(placement, roomID, worldID)
	endpoint, err := r.endpoints.ResolveEndpoint(target.TargetID, target.InstallationID)
	if err != nil {
		return RuntimeEndpoint{}, Target{}, err
	}
	return endpoint, target, nil
}

func (r *Router) RegisterEndpoint(targetID, installationID string, endpoint RuntimeEndpoint) error {
	return r.endpoints.RegisterInstallation(targetID, installationID, endpoint)
}

func (r *Router) ProvisionTarget(placement topology.ExecutionPlacement) (Driver, Target, error) {
	if strings.TrimSpace(placement.DesiredTargetID) == "" || placement.Target.ID != placement.DesiredTargetID {
		return nil, Target{}, ErrInvalidTarget
	}
	installationID := strings.TrimSpace(placement.DesiredInstallationID)
	if installationID == "" {
		installationID = strings.TrimSpace(placement.Target.Config.InstallationID)
	}
	if placement.DesiredTargetID == LocalTargetID {
		if installationID == "" {
			installationID = "default"
		}
	}
	if installationID == "" {
		return nil, Target{}, ErrInvalidTarget
	}
	driver, err := r.endpoints.ResolveEndpoint(placement.DesiredTargetID, installationID)
	if err != nil {
		return nil, Target{}, err
	}
	return driver.Driver, Target{
		TargetID: placement.DesiredTargetID, InstallationID: installationID,
		RoomID: placement.Room.ID, WorldID: placement.World.ID,
		Cluster: placement.Room.DirectoryName, Shard: placement.World.DirectoryName,
		TopologyRevision: placement.Revision,
		Capabilities:     runtimeCapabilities(placement.Target.Capabilities), CapabilitiesKnown: placement.Target.Kind != "",
	}, nil
}

func (r *Router) TrustedTarget(target Target) (Driver, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	endpoint, err := r.endpoints.ResolveEndpoint(target.TargetID, target.InstallationID)
	return endpoint.Driver, err
}

func targetFromPlacement(placement topology.ExecutionPlacement, roomID, worldID string) Target {
	installationID := strings.TrimSpace(placement.AppliedInstallationID)
	if installationID == "" {
		installationID = strings.TrimSpace(placement.Target.Config.InstallationID)
	}
	if placement.AppliedTargetID == LocalTargetID && installationID == "" {
		installationID = "default"
	}
	return Target{
		TargetID: placement.AppliedTargetID, InstallationID: installationID,
		RoomID: roomID, WorldID: worldID, Cluster: placement.Room.DirectoryName, Shard: placement.World.DirectoryName,
		TopologyRevision: placement.Revision,
		Capabilities:     runtimeCapabilities(placement.Target.Capabilities), CapabilitiesKnown: placement.Target.Kind != "",
	}
}

func (r *Router) IsLocalPlacement(roomID, worldID string) (bool, error) {
	applied, err := r.placements.AppliedPlacement(roomID, worldID)
	if err != nil {
		return false, err
	}
	return applied.AppliedTargetID == LocalTargetID, nil
}

func (r *Router) MoveRoomToRecovery(ctx context.Context, roomID string, worldIDs []string) (RoomRecoveryLocation, error) {
	if strings.TrimSpace(roomID) == "" || len(worldIDs) == 0 {
		return RoomRecoveryLocation{}, ErrInvalidTarget
	}
	type recoveryEndpoint struct {
		driver RoomRecoveryDriver
		target Target
	}
	values := make(map[string]recoveryEndpoint)
	for _, worldID := range worldIDs {
		endpoint, target, err := r.EndpointTarget(ctx, roomID, worldID)
		if err != nil {
			return RoomRecoveryLocation{}, err
		}
		recovery, ok := endpoint.Driver.(RoomRecoveryDriver)
		if !ok {
			return RoomRecoveryLocation{}, ErrCapabilityMissing
		}
		values[target.TargetID+"\x00"+target.InstallationID] = recoveryEndpoint{driver: recovery, target: target}
	}
	if len(values) != 1 {
		return RoomRecoveryLocation{}, ErrRoomRecoveryMultiTarget
	}
	var selected recoveryEndpoint
	for _, value := range values {
		selected = value
	}
	operationKey := uuid.NewString()
	lease, err := r.leases.Acquire(ctx, roomID, operationKey, r.leaseTTL)
	if err != nil {
		return RoomRecoveryLocation{}, err
	}
	defer r.leases.Release(lease)
	expiresAt := lease.ExpiresAt.UTC()
	recoveryRef, err := selected.driver.MoveRoomToRecovery(ctx, selected.target, Operation{
		ID: uuid.NewString(), Key: operationKey, LeaseID: lease.LeaseID,
		FencingToken: lease.FencingToken, LeaseExpiresAt: &expiresAt,
	})
	if err != nil {
		return RoomRecoveryLocation{}, err
	}
	NotifyRuntimeTargetsChanged(r.mutations, selected.target.TargetID)
	return RoomRecoveryLocation{
		TargetID: selected.target.TargetID, InstallationID: selected.target.InstallationID, RecoveryRef: recoveryRef,
	}, nil
}

func (r *Router) MigrationTargets(plan topology.MigrationPlacement) (Driver, Target, Driver, Target, error) {
	sourceInstallationID := strings.TrimSpace(plan.SourceInstallationID)
	if sourceInstallationID == "" {
		sourceInstallationID = strings.TrimSpace(plan.Source.Config.InstallationID)
	}
	targetInstallationID := strings.TrimSpace(plan.TargetInstallationID)
	if targetInstallationID == "" {
		targetInstallationID = strings.TrimSpace(plan.Target.Config.InstallationID)
	}
	if plan.SourceTargetID == "" || plan.TargetTargetID == "" ||
		(plan.SourceTargetID == plan.TargetTargetID && sourceInstallationID == targetInstallationID) {
		return nil, Target{}, nil, Target{}, ErrInvalidTarget
	}
	source := Target{
		TargetID: plan.SourceTargetID, InstallationID: sourceInstallationID,
		RoomID: plan.Room.ID, WorldID: plan.World.ID, Cluster: plan.Room.DirectoryName,
		Shard: plan.World.DirectoryName, TopologyRevision: plan.Revision,
		Capabilities: runtimeCapabilities(plan.Source.Capabilities), CapabilitiesKnown: plan.Source.Kind != "",
	}
	target := Target{
		TargetID: plan.TargetTargetID, InstallationID: targetInstallationID,
		RoomID: plan.Room.ID, WorldID: plan.World.ID, Cluster: plan.Room.DirectoryName,
		Shard: plan.World.DirectoryName, TopologyRevision: plan.Revision,
		Capabilities: runtimeCapabilities(plan.Target.Capabilities), CapabilitiesKnown: plan.Target.Kind != "",
	}
	if source.TargetID == LocalTargetID && strings.TrimSpace(source.InstallationID) == "" {
		source.InstallationID = "default"
	}
	if target.TargetID == LocalTargetID && strings.TrimSpace(target.InstallationID) == "" {
		target.InstallationID = "default"
	}
	if err := validateTarget(source); err != nil {
		return nil, Target{}, nil, Target{}, err
	}
	if err := validateTarget(target); err != nil {
		return nil, Target{}, nil, Target{}, err
	}
	sourceEndpoint, err := r.endpoints.ResolveEndpoint(source.TargetID, source.InstallationID)
	if err != nil {
		return nil, Target{}, nil, Target{}, err
	}
	targetEndpoint, err := r.endpoints.ResolveEndpoint(target.TargetID, target.InstallationID)
	if err != nil {
		return nil, Target{}, nil, Target{}, err
	}
	return sourceEndpoint.Driver, source, targetEndpoint.Driver, target, nil
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
	if consoleRequiresLease(target, request) {
		lease, borrowed := operationlease.BorrowedLease(ctx, roomID)
		if !borrowed || lease.ExpiresAt.Before(time.Now().UTC()) {
			var leaseErr error
			lease, leaseErr = r.leases.Acquire(ctx, roomID, "runtime.console.send:"+worldID, r.leaseTTL)
			if leaseErr != nil {
				return shared.RuntimeOperationResult{}, leaseErr
			}
			defer r.leases.Release(lease)
		}
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

func consoleRequiresLease(_ Target, request shared.RuntimeConsoleRequest) bool {
	return request.Mode != shared.ConsoleModeProbe
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
	runtimeMode, runtimeModeValid := shared.NormalizeRuntimePerformanceMode(request.RuntimeMode)
	if request.ProtocolVersion != shared.ShardOperationProtocolVersion || strings.TrimSpace(request.OperationID) == "" || !shared.IsShardAction(request.Action) || !runtimeModeValid {
		return shared.ShardOperationResult{}, ErrInvalidTarget
	}
	if request.Action != shared.ShardActionStart && request.Action != shared.ShardActionRestart &&
		(request.RuntimeMode != "" || request.LaunchOptions.SkipUpdateServerMods) {
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
		FencingToken: request.FencingToken, LeaseExpiresAt: request.LeaseExpiresAt, RuntimeMode: runtimeMode,
		LaunchOptions: request.LaunchOptions,
	}
	var cpuDriver CPUDriver
	var cpuRequest shared.RuntimeCPURequest
	if request.Action == shared.ShardActionStart || request.Action == shared.ShardActionRestart {
		resolver, ok := r.placements.(cpuAllocationResolver)
		if !ok {
			return shared.ShardOperationResult{}, ErrCapabilityMissing
		}
		allocation, allocationErr := resolver.CPUAllocation(roomID, worldID)
		if allocationErr != nil && !errors.Is(allocationErr, topology.ErrResourceNotFound) {
			return shared.ShardOperationResult{}, allocationErr
		}
		if allocationErr == nil && allocation.Policy != topology.CPUPolicyNone {
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
	}
	result, err := driver.ExecuteShard(ctx, target, operation, request.Action, timeout)
	if shared.ShardActionMutates(request.Action) && r.mutations != nil {
		r.mutations.RuntimeTargetChanged(target.TargetID)
	}
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && shardLaunchConfirmed(request.Action, result.Status) {
		err = nil
		if result.Status.State == "running" {
			result.Message = "分片已启动"
		} else {
			result.Message = "分片进程已启动，DST 仍在加载"
		}
	}
	if request.Action == shared.ShardActionStop {
		_, allocationErr := r.cpuAllocation(roomID, worldID)
		stopped := shardStopped(result.Status)
		if !stopped && err == nil {
			err = errShardStopUnconfirmed
		}
		if candidate, ok := driver.(CPUDriver); ok && allocationErr == nil && HasTargetCapability(driver, target, CapabilityExclusiveCPU) && stopped {
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

func shardLaunchConfirmed(action shared.ShardAction, status shared.ShardRuntimeStatus) bool {
	if action != shared.ShardActionStart && action != shared.ShardActionRestart || !status.SessionExists {
		return false
	}
	return status.State == "starting" || status.State == "running"
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

func (r *Router) ListChatLogGenerations(ctx context.Context, roomID, worldID string) ([]shared.RuntimeChatLogGeneration, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return nil, err
	}
	provider, ok := driver.(ChatLogDriver)
	if !ok {
		return nil, ErrCapabilityMissing
	}
	generations, err := provider.ListChatLogGenerations(ctx, target)
	if err != nil {
		return nil, err
	}
	if err := runtimefiles.ValidateChatLogGenerations(generations); err != nil {
		return nil, err
	}
	return generations, nil
}

func (r *Router) ReadChatLogGeneration(ctx context.Context, roomID, worldID string, request shared.RuntimeChatLogRequest) (shared.RuntimeChatLogResult, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeChatLogResult{}, err
	}
	provider, ok := driver.(ChatLogDriver)
	if !ok {
		return shared.RuntimeChatLogResult{}, ErrCapabilityMissing
	}
	result, err := provider.ReadChatLogGeneration(ctx, target, request)
	if err != nil {
		return shared.RuntimeChatLogResult{}, err
	}
	if err := runtimefiles.ValidateChatLogResult(request, result); err != nil {
		return shared.RuntimeChatLogResult{}, err
	}
	return result, nil
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

func (r *Router) ReadWorldState(ctx context.Context, roomID, worldID string) (shared.RuntimeWorldStateRead, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeWorldStateRead{}, err
	}
	reader, ok := driver.(WorldStateReader)
	if !ok || !HasTargetCapability(driver, target, CapabilityWorldStateRead) {
		return shared.RuntimeWorldStateRead{}, fmt.Errorf("%w: update the target Agent to read world state files", ErrCapabilityMissing)
	}
	defer requesttiming.Start(ctx, "runtime.worldstate_request")()
	return reader.ReadWorldState(ctx, target)
}

func (r *Router) ReadConfiguration(ctx context.Context, roomID, worldID, scope string) (ConfigurationSnapshot, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	reader, ok := driver.(ConfigurationReader)
	if !ok || !HasTargetCapability(driver, target, CapabilityConfigRead) {
		return ConfigurationSnapshot{}, ErrCapabilityMissing
	}
	result, err := reader.ReadConfiguration(ctx, target, scope)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	if err := runtimefiles.ValidateConfiguration(scope, result); err != nil {
		return ConfigurationSnapshot{}, err
	}
	return ConfigurationSnapshot{Target: target, Result: result}, nil
}

func (r *Router) RevealClusterToken(ctx context.Context, roomID, worldID string) (shared.RuntimeClusterTokenReveal, error) {
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeClusterTokenReveal{}, err
	}
	reader, ok := driver.(ClusterTokenReader)
	if !ok || !HasTargetCapability(driver, target, CapabilityConfigSecrets) {
		return shared.RuntimeClusterTokenReveal{}, ErrCapabilityMissing
	}
	result, err := reader.RevealClusterToken(ctx, target)
	if err != nil {
		return shared.RuntimeClusterTokenReveal{}, err
	}
	if err := runtimefiles.ValidateClusterTokenReveal(result); err != nil {
		return shared.RuntimeClusterTokenReveal{}, err
	}
	return result, nil
}

func newOperationID() string { return strings.ToLower(uuid.NewString()) }
