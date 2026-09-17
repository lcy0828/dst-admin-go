package topology

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/shared"
)

const localTargetID = "local"

type roomCatalog interface {
	List() ([]rooms.Room, error)
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}

type targetCatalog interface {
	RuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error)
}

type runtimeTargetFreshener interface {
	EnsureRuntimeTargetsFresh(context.Context, []string) error
}

type runtimeRoomCatalogSynchronizer interface {
	SyncRuntimeCatalog([]rooms.RuntimeCatalogSource) error
}

type egressDetector interface {
	DetectEgress(context.Context, string, shared.RuntimeNetworkRegion) (shared.RuntimeNetworkResult, error)
}

// NetworkDiscovery provides demand-driven network observations independently
// from the cached Runtime inventory source.
type NetworkDiscovery interface {
	DetectEgress(context.Context, string, shared.RuntimeNetworkRegion) (shared.RuntimeNetworkResult, error)
	ListenNetworkEndpoint(context.Context, string, string, int, []string, time.Duration) ([]string, error)
	ProbeNetworkEndpoints(context.Context, string, []shared.RuntimeNetworkEndpointRequest, time.Duration) ([]shared.RuntimeNetworkEndpointResult, error)
}

type Service struct {
	rooms              roomCatalog
	targets            targetCatalog
	store              *Store
	cpu                CPUAllocationExecutor
	egress             egressDetector
	endpointProbe      endpointProber
	cpuRecoveryMu      sync.Mutex
	cpuRecoveryPending map[string]struct{}
	cpuRecoveryTimeout time.Duration
}

type CPUAllocationExecutor interface {
	ApplyCPUAllocation(context.Context, CPUAllocation) (shared.RuntimeCPUResult, error)
}

type cpuAllocationObserver interface {
	ObserveCPUAllocation(context.Context, CPUAllocation) (shared.RuntimeCPUResult, error)
}

type roomPlan struct {
	room   rooms.Room
	worlds []rooms.World
	record record
}

func (s *Service) ConfigureCPUExecutor(executor CPUAllocationExecutor) error {
	if executor == nil {
		return errors.New("CPU allocation executor is required")
	}
	s.cpu = executor
	observer, ok := executor.(cpuAllocationObserver)
	if !ok {
		return nil
	}
	_, _, _, _, allocations, err := s.store.RuntimeResources()
	if err != nil {
		return err
	}
	s.resetCPURecoveryPending()
	for _, allocation := range allocations {
		if allocation.TargetID != localTargetID {
			s.addCPURecoveryPending(allocation)
			continue
		}
		if _, err := s.observeCPUExecutionState(context.Background(), observer, allocation); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) observeCPUExecutionState(ctx context.Context, observer cpuAllocationObserver, allocation CPUAllocation) (bool, error) {
	timeout := s.cpuRecoveryTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	observeContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	observed, observeErr := observer.ObserveCPUAllocation(observeContext, allocation)
	if observeErr != nil {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		return s.recordRecoveredCPUState(allocation, nil, fmt.Errorf("恢复 CPU 执行状态: %w", observeErr))
	}
	return s.recordRecoveredCPUState(allocation, &observed, nil)
}

func (s *Service) recordRecoveredCPUState(expected CPUAllocation, observed *shared.RuntimeCPUResult, cause error) (bool, error) {
	updated := expected
	confirmed := false
	if cause != nil {
		updated.ExecutionState = CPUExecutionFailed
		updated.ExecutionError = truncateResourceError(cause.Error())
	} else if observed != nil {
		updated.Observed, updated.ExecutionError = observed, ""
		switch observed.State {
		case shared.RuntimeCPUStateApplied:
			updated.ExecutionState = CPUExecutionApplied
			confirmed = true
		case shared.RuntimeCPUStatePrepared:
			updated.ExecutionState = CPUExecutionPrepared
			confirmed = true
		case shared.RuntimeCPUStateReleased:
			updated.ExecutionState = CPUExecutionReleased
			confirmed = true
		default:
			updated.ExecutionState = CPUExecutionDesired
		}
	}
	current, applied, err := s.store.SaveCPUAllocationIfCurrent(expected, updated)
	if err != nil {
		return true, err
	}
	if !applied {
		if current.ID == "" {
			s.clearCPURecoveryPending(expected.RoomID, expected.WorldID)
			return false, nil
		}
		return true, nil
	}
	if cause != nil || !confirmed {
		return true, nil
	}
	s.clearCPURecoveryPending(expected.RoomID, expected.WorldID)
	return false, nil
}

func (s *Service) recoverPendingCPUExecutionState(ctx context.Context, inventories []agents.RuntimeTargetInventory) error {
	observer, ok := s.cpu.(cpuAllocationObserver)
	if !ok {
		return nil
	}
	_, _, _, _, allocations, err := s.store.RuntimeResources()
	if err != nil {
		return err
	}
	available := make(map[string]bool, len(inventories))
	for targetID, inventory := range machineInventories(inventories) {
		available[targetID] = inventory.Target.Online && inventory.Available && !inventory.Stale
	}
	sort.Slice(allocations, func(i, j int) bool {
		if allocations[i].RoomID == allocations[j].RoomID {
			return allocations[i].WorldID < allocations[j].WorldID
		}
		return allocations[i].RoomID < allocations[j].RoomID
	})
	for _, allocation := range allocations {
		if allocation.TargetID == localTargetID || !available[allocation.TargetID] || !s.takeCPURecoveryPending(allocation) {
			continue
		}
		retry, err := s.observeCPUExecutionState(ctx, observer, allocation)
		if err != nil {
			s.addCPURecoveryPending(allocation)
			return err
		}
		if retry {
			s.addCPURecoveryPending(allocation)
		}
	}
	return nil
}

func (s *Service) resetCPURecoveryPending() {
	s.cpuRecoveryMu.Lock()
	s.cpuRecoveryPending = make(map[string]struct{})
	s.cpuRecoveryMu.Unlock()
}

func (s *Service) addCPURecoveryPending(allocation CPUAllocation) {
	s.cpuRecoveryMu.Lock()
	if s.cpuRecoveryPending == nil {
		s.cpuRecoveryPending = make(map[string]struct{})
	}
	s.cpuRecoveryPending[allocationResourceID(allocation.RoomID, allocation.WorldID)] = struct{}{}
	s.cpuRecoveryMu.Unlock()
}

func (s *Service) takeCPURecoveryPending(allocation CPUAllocation) bool {
	key := allocationResourceID(allocation.RoomID, allocation.WorldID)
	s.cpuRecoveryMu.Lock()
	defer s.cpuRecoveryMu.Unlock()
	if _, exists := s.cpuRecoveryPending[key]; !exists {
		return false
	}
	delete(s.cpuRecoveryPending, key)
	return true
}

func (s *Service) clearCPURecoveryPending(roomID, worldID string) {
	s.cpuRecoveryMu.Lock()
	delete(s.cpuRecoveryPending, allocationResourceID(roomID, worldID))
	s.cpuRecoveryMu.Unlock()
}

type planResult struct {
	snapshot    Snapshot
	record      record
	plans       map[string]roomPlan
	inventories []agents.RuntimeTargetInventory
	resources   resourceBuild
	resourcesOK bool
	changed     bool
}

func NewService(roomService roomCatalog, targetService targetCatalog, store *Store, networkDiscovery ...NetworkDiscovery) (*Service, error) {
	if roomService == nil || targetService == nil || store == nil {
		return nil, errors.New("topology dependencies are required")
	}
	if len(networkDiscovery) > 1 {
		return nil, errors.New("only one topology network discovery source is supported")
	}
	service := &Service{
		rooms: roomService, targets: targetService, store: store,
		cpuRecoveryPending: make(map[string]struct{}), cpuRecoveryTimeout: 5 * time.Second,
	}
	service.egress, _ = targetService.(egressDetector)
	service.endpointProbe, _ = targetService.(endpointProber)
	if len(networkDiscovery) == 1 {
		if networkDiscovery[0] == nil {
			return nil, errors.New("topology network discovery source is required")
		}
		service.egress = networkDiscovery[0]
		service.endpointProbe = networkDiscovery[0]
	}
	return service, nil
}

func (s *Service) Topology(ctx context.Context, roomID string) (Snapshot, error) {
	result, err := s.plan(ctx, roomID, nil)
	if err != nil {
		return Snapshot{}, err
	}
	return result.snapshot, nil
}

func (s *Service) ResolveDesiredShardLinks(ctx context.Context, roomID string) ([]ShardLink, error) {
	result, err := s.plan(ctx, roomID, nil)
	if err != nil {
		return nil, err
	}
	return publicStoredShardLinks(result.record.ShardLinks), nil
}

func (s *Service) ResolveAppliedShardLinks(ctx context.Context, roomID string) ([]ShardLink, error) {
	result, err := s.plan(ctx, roomID, nil)
	if err != nil {
		return nil, err
	}
	return publicStoredShardLinks(result.record.AppliedShardLinks), nil
}

func (s *Service) ShardLinkPlan(ctx context.Context, roomID string) (ShardLinkPlan, error) {
	result, err := s.plan(ctx, roomID, nil)
	if err != nil {
		return ShardLinkPlan{}, err
	}
	return ShardLinkPlan{
		Revision: result.record.Revision, Desired: publicStoredShardLinks(result.record.ShardLinks),
		Applied:          publicStoredShardLinks(result.record.AppliedShardLinks),
		PlacementPending: !placementsAligned(result.record.Placements),
	}, nil
}

func (s *Service) CommitDesiredShardLinks(roomID, expectedRevision string) (string, error) {
	selected, err := s.store.load(roomID)
	if err != nil {
		return "", err
	}
	if selected.Revision != expectedRevision {
		return "", &RevisionConflictError{CurrentRevision: selected.Revision}
	}
	if !placementsAligned(selected.Placements) {
		return "", executionBlocked("PLACEMENT_PENDING", "世界运行位置尚未全部生效，不能提前应用计划中的互联线路")
	}
	if sameStoredShardLinks(selected.ShardLinks, selected.AppliedShardLinks) {
		return selected.Revision, nil
	}
	saved, err := s.store.SavePlan(
		roomID, expectedRevision, selected.Placements, selected.ShardLinks, selected.ShardLinks,
	)
	if err != nil {
		return "", err
	}
	return saved.Revision, nil
}

// FleetTopology builds every managed room topology from a single inventory
// collection. It is the read boundary for machine-scoped control-plane views.
func (s *Service) FleetTopology(ctx context.Context) (FleetSnapshot, error) {
	inventories, err := s.targets.RuntimeTargetInventories(ctx)
	if err != nil {
		return FleetSnapshot{}, err
	}
	if err := s.syncRuntimeRoomCatalog(inventories); err != nil {
		return FleetSnapshot{}, err
	}
	plans, err := s.reconcileAll("")
	if err != nil {
		return FleetSnapshot{}, err
	}
	plans = canonicalizePlanPlacements(plans, inventories)
	if err := s.store.SyncRuntimeCatalog(inventories); err != nil {
		return FleetSnapshot{}, err
	}
	roomIDs := make([]string, 0, len(plans))
	for roomID := range plans {
		roomIDs = append(roomIDs, roomID)
	}
	sort.Slice(roomIDs, func(i, j int) bool {
		left, right := plans[roomIDs[i]].room, plans[roomIDs[j]].room
		if !strings.EqualFold(left.Name, right.Name) {
			return strings.ToLower(left.Name) < strings.ToLower(right.Name)
		}
		return left.ID < right.ID
	})
	base := buildSnapshot("", plans, inventories)
	result := FleetSnapshot{
		Targets: append([]TargetSummary(nil), base.Targets...),
		Rooms:   make([]Snapshot, 0, len(roomIDs)), ObservedAt: time.Now().UTC(),
	}
	for _, roomID := range roomIDs {
		result.Rooms = append(result.Rooms, buildSnapshot(roomID, plans, inventories))
	}
	return result, nil
}

func (s *Service) Preview(ctx context.Context, roomID string, request UpdateRequest) (Snapshot, error) {
	result, err := s.plan(ctx, roomID, &request)
	if err != nil {
		return Snapshot{}, err
	}
	return result.snapshot, nil
}

func (s *Service) Update(ctx context.Context, roomID string, request UpdateRequest) (Snapshot, error) {
	result, err := s.plan(ctx, roomID, &request)
	if err != nil {
		return Snapshot{}, err
	}
	if result.snapshot.RequiresOvercommitConfirmation && !request.AllowOvercommit {
		return Snapshot{}, &OvercommitError{Preview: result.snapshot}
	}
	if !result.changed {
		if result.resourcesOK {
			if err := s.persistInfrastructureState(result.resources); err != nil {
				return Snapshot{}, err
			}
		}
		return result.snapshot, nil
	}
	saved, err := s.store.SavePlan(
		roomID, request.ExpectedRevision, result.record.Placements, result.record.ShardLinks, result.record.AppliedShardLinks,
	)
	if err != nil {
		return Snapshot{}, err
	}
	result.snapshot.Revision = saved.Revision
	result.snapshot.UpdatedAt = saved.UpdatedAt
	if result.resourcesOK {
		if err := s.persistInfrastructureState(result.resources); err != nil {
			return Snapshot{}, err
		}
	}
	return result.snapshot, nil
}

// ResolveExecution returns the currently applied runtime target. Desired
// placement is never used as an execution fallback while migration is pending.
func (s *Service) ResolveExecution(ctx context.Context, roomID, worldID string) (ExecutionPlacement, error) {
	result, err := s.executionPlan(ctx, roomID)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	resolved, err := resolveExecution(result, roomID, worldID)
	if err == nil || !runtimeInventoryRefreshableError(err) {
		return resolved, err
	}
	freshener, ok := s.targets.(runtimeTargetFreshener)
	if !ok {
		return ExecutionPlacement{}, err
	}
	applied, appliedErr := s.AppliedPlacement(roomID, worldID)
	if appliedErr != nil || strings.TrimSpace(applied.AppliedTargetID) == "" {
		return ExecutionPlacement{}, err
	}
	if refreshErr := freshener.EnsureRuntimeTargetsFresh(ctx, []string{applied.AppliedTargetID}); refreshErr != nil {
		return ExecutionPlacement{}, refreshErr
	}
	result, planErr := s.executionPlan(ctx, roomID)
	if planErr != nil {
		return ExecutionPlacement{}, planErr
	}
	return resolveExecution(result, roomID, worldID)
}

func runtimeInventoryRefreshableError(err error) bool {
	var execution *ExecutionError
	if !errors.As(err, &execution) {
		return false
	}
	return execution.Code == "APPLIED_INVENTORY_STALE" || execution.Code == "APPLIED_INVENTORY_MISSING"
}

// ResolveRoomExecutions resolves every applied world in a room from one
// topology snapshot. Callers that need a room-wide consistent view should use
// this method instead of resolving each world independently.
func (s *Service) ResolveRoomExecutions(ctx context.Context, roomID string) ([]ExecutionPlacement, error) {
	result, err := s.executionPlan(ctx, roomID)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveRoomExecutions(result, roomID)
	if err == nil || !runtimeInventoryRefreshableError(err) {
		return resolved, err
	}
	freshener, ok := s.targets.(runtimeTargetFreshener)
	if !ok {
		return nil, err
	}
	selected, exists := result.plans[roomID]
	if !exists {
		return nil, rooms.ErrRoomNotFound
	}
	inventories := endpointInventories(result.inventories)
	targetIDs := make([]string, 0, len(selected.record.Placements))
	seenTargets := make(map[string]bool, len(selected.record.Placements))
	for _, placement := range selected.record.Placements {
		targetID := strings.TrimSpace(placement.AppliedTargetID)
		inventory, exists := inventories[endpointFor(targetID, placement.AppliedInstallationID)]
		if targetID == "" || seenTargets[targetID] || (exists && inventory.Available && !inventory.Stale) {
			continue
		}
		seenTargets[targetID] = true
		targetIDs = append(targetIDs, targetID)
	}
	if refreshErr := freshener.EnsureRuntimeTargetsFresh(ctx, targetIDs); refreshErr != nil {
		return nil, refreshErr
	}
	result, err = s.executionPlan(ctx, roomID)
	if err != nil {
		return nil, err
	}
	return resolveRoomExecutions(result, roomID)
}

// ResolveCachedRoomExecutions returns cached applied placements for read-only
// views. It accepts stale inventory so rendering never waits for an Agent
// refresh; control operations must continue using ResolveRoomExecutions.
func (s *Service) ResolveCachedRoomExecutions(ctx context.Context, roomID string) ([]ExecutionPlacement, error) {
	result, err := s.executionPlan(ctx, roomID)
	if err != nil {
		return nil, err
	}
	return resolveRoomExecutionsWithPolicy(result, roomID, true)
}

// ResolveCachedExecution leaves unrelated worlds out of a single-world read.
func (s *Service) ResolveCachedExecution(ctx context.Context, roomID, worldID string) (ExecutionPlacement, error) {
	result, err := s.executionPlan(ctx, roomID)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	return resolveExecutionWithPolicy(result, roomID, worldID, true)
}

func resolveRoomExecutions(result planResult, roomID string) ([]ExecutionPlacement, error) {
	return resolveRoomExecutionsWithPolicy(result, roomID, false)
}

func resolveRoomExecutionsWithPolicy(result planResult, roomID string, allowStaleInventory bool) ([]ExecutionPlacement, error) {
	selected, exists := result.plans[roomID]
	if !exists {
		return nil, rooms.ErrRoomNotFound
	}
	resolved := make([]ExecutionPlacement, 0, len(selected.worlds))
	for _, world := range selected.worlds {
		placement, err := resolveExecutionWithPolicy(result, roomID, world.ID, allowStaleInventory)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, placement)
	}
	return resolved, nil
}

// ResolveDesiredRoomExecutions resolves planned targets without requiring the
// Shard files to exist there yet. It is used only by trusted provisioning
// coordinators; normal lifecycle operations must continue using applied
// placements through ResolveRoomExecutions.
func (s *Service) ResolveDesiredRoomExecutions(ctx context.Context, roomID string) ([]ExecutionPlacement, error) {
	result, err := s.plan(ctx, roomID, nil)
	if err != nil {
		return nil, err
	}
	selected, exists := result.plans[roomID]
	if !exists {
		return nil, rooms.ErrRoomNotFound
	}
	placements := placementsByWorld(selected.record.Placements)
	inventories := endpointInventories(result.inventories)
	resolved := make([]ExecutionPlacement, 0, len(selected.worlds))
	for _, world := range selected.worlds {
		placement, ok := placements[world.ID]
		if !ok || strings.TrimSpace(placement.DesiredTargetID) == "" {
			return nil, executionBlocked("DESIRED_TARGET_MISSING", "世界没有计划运行目标")
		}
		inventory, ok := inventories[endpointFor(placement.DesiredTargetID, placement.DesiredInstallationID)]
		if !ok || !inventory.Target.Configured {
			return nil, executionBlocked("PROVISION_TARGET_MISSING", "计划运行节点不存在或尚未配置")
		}
		if !inventory.Target.Online {
			return nil, executionBlocked("PROVISION_TARGET_OFFLINE", "计划运行节点当前离线")
		}
		if !inventory.Available || inventory.Stale {
			return nil, executionBlocked("PROVISION_INVENTORY_STALE", "计划运行节点的清单不可用或已过期")
		}
		if inventory.Target.Kind == agents.RuntimeKindAgent && !containsCapability(inventory.Target.Capabilities, "runtime.migration.v1") {
			return nil, executionBlocked("AGENT_CAPABILITY_MISSING", "Agent 版本不支持受管配置投放")
		}
		resolved = append(resolved, ExecutionPlacement{
			Room: selected.room, World: world, Revision: selected.record.Revision,
			DesiredTargetID: placement.DesiredTargetID, AppliedTargetID: placement.AppliedTargetID,
			DesiredInstallationID: placement.DesiredInstallationID, AppliedInstallationID: placement.AppliedInstallationID,
			Target: inventory.Target, Inventory: inventory,
		})
	}
	return resolved, nil
}

// ApplyProvision atomically advances every provisioned world from its old
// applied target to the already-planned desired target.
func (s *Service) ApplyProvision(roomID, expectedRevision string, values []PlacementInput) (string, error) {
	selected, err := s.store.load(roomID)
	if err != nil {
		return "", err
	}
	if selected.Revision != expectedRevision {
		return "", &RevisionConflictError{CurrentRevision: selected.Revision}
	}
	requested := make(map[string]PlacementInput, len(values))
	for _, value := range values {
		if value.WorldID == "" || value.TargetID == "" {
			return "", ErrInvalidInput
		}
		if _, exists := requested[value.WorldID]; exists {
			return "", ErrInvalidInput
		}
		requested[value.WorldID] = value
	}
	placements := normalizedPlacements(selected.Placements)
	if len(requested) != len(placements) {
		return "", ErrInvalidInput
	}
	for index := range placements {
		requestedPlacement, ok := requested[placements[index].WorldID]
		if !ok {
			return "", &RevisionConflictError{CurrentRevision: selected.Revision}
		}
		expectedInstallationID := placements[index].DesiredInstallationID
		if expectedInstallationID == "" && placements[index].DesiredTargetID == requestedPlacement.TargetID {
			expectedInstallationID = requestedPlacement.InstallationID
		}
		if !sameEndpoint(
			placements[index].DesiredTargetID, expectedInstallationID,
			requestedPlacement.TargetID, requestedPlacement.InstallationID,
		) {
			return "", &RevisionConflictError{CurrentRevision: selected.Revision}
		}
		placements[index].AppliedTargetID = requestedPlacement.TargetID
		placements[index].AppliedInstallationID = requestedPlacement.InstallationID
	}
	saved, err := s.store.SavePlan(roomID, expectedRevision, placements, selected.ShardLinks, selected.ShardLinks)
	if err != nil {
		return "", err
	}
	return saved.Revision, nil
}

func resolveExecution(result planResult, roomID, worldID string) (ExecutionPlacement, error) {
	return resolveExecutionWithPolicy(result, roomID, worldID, false)
}

func resolveExecutionWithPolicy(result planResult, roomID, worldID string, allowStaleInventory bool) (ExecutionPlacement, error) {
	selected := result.plans[roomID]
	var world rooms.World
	worldFound := false
	for _, item := range selected.worlds {
		if item.ID == worldID {
			world, worldFound = item, true
			break
		}
	}
	if !worldFound {
		return ExecutionPlacement{}, rooms.ErrWorldNotFound
	}
	stored, placementFound := placementsByWorld(selected.record.Placements)[worldID]
	if !placementFound || strings.TrimSpace(stored.AppliedTargetID) == "" {
		return ExecutionPlacement{}, executionBlocked("APPLIED_TARGET_MISSING", "世界没有已生效的运行目标")
	}
	for _, placement := range result.snapshot.Placements {
		if placement.WorldID == worldID && placement.State == PlacementConflict {
			return ExecutionPlacement{}, executionBlocked("SHARD_RUNTIME_CONFLICT", "世界在多个位置或非生效目标上运行，已阻止控制操作")
		}
	}
	inventories := endpointInventories(result.inventories)
	inventory, exists := inventories[endpointFor(stored.AppliedTargetID, stored.AppliedInstallationID)]
	if !exists || !inventory.Target.Configured {
		return ExecutionPlacement{}, executionBlocked("APPLIED_TARGET_MISSING", "已生效运行目标不存在或尚未配置")
	}
	resolved := ExecutionPlacement{
		Room: selected.room, World: world, Revision: selected.record.Revision,
		DesiredTargetID: stored.DesiredTargetID, AppliedTargetID: stored.AppliedTargetID,
		DesiredInstallationID: stored.DesiredInstallationID, AppliedInstallationID: stored.AppliedInstallationID,
		Target: inventory.Target, Inventory: inventory,
	}
	if stored.AppliedTargetID == localTargetID {
		return resolved, nil
	}
	if inventory.Target.Kind != agents.RuntimeKindAgent || strings.TrimSpace(inventory.Target.AgentID) == "" {
		return ExecutionPlacement{}, executionBlocked("APPLIED_TARGET_INVALID", "已生效运行目标不是可控制的 Agent 节点")
	}
	if !inventory.Target.Online {
		return ExecutionPlacement{}, executionBlocked("APPLIED_TARGET_OFFLINE", "已生效运行目标当前离线，操作不会回落到本机")
	}
	if !containsCapability(inventory.Target.Capabilities, "shard.control.v1") {
		return ExecutionPlacement{}, executionBlocked("AGENT_CAPABILITY_MISSING", "Agent 版本不支持类型化分片控制")
	}
	if !inventory.Available && !allowStaleInventory {
		return ExecutionPlacement{}, executionBlocked("APPLIED_INVENTORY_MISSING", "已生效运行目标尚无运行时清单")
	}
	if inventory.Stale && !allowStaleInventory {
		return ExecutionPlacement{}, executionBlocked("APPLIED_INVENTORY_STALE", "已生效运行目标的运行时清单已过期")
	}
	if !inventoryHasShard(inventory.Inventory, identityFor(selected.room.DirectoryName, world.DirectoryName)) {
		return ExecutionPlacement{}, executionBlocked("APPLIED_SHARD_MISSING", "Agent 未在受信安装中发现该世界文件")
	}
	return resolved, nil
}

// AppliedPlacement is a lightweight lookup used to keep the established local
// runtime path fast. Remote targets must still pass ResolveExecution before use.
func (s *Service) AppliedPlacement(roomID, worldID string) (ExecutionPlacement, error) {
	selected, err := s.reconcileRoom(roomID)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	var world rooms.World
	found := false
	for _, item := range selected.worlds {
		if item.ID == worldID {
			world, found = item, true
			break
		}
	}
	if !found {
		return ExecutionPlacement{}, rooms.ErrWorldNotFound
	}
	stored, exists := placementsByWorld(selected.record.Placements)[worldID]
	if !exists || strings.TrimSpace(stored.AppliedTargetID) == "" {
		return ExecutionPlacement{}, executionBlocked("APPLIED_TARGET_MISSING", "世界没有已生效的运行目标")
	}
	return ExecutionPlacement{
		Room: selected.room, World: world, Revision: selected.record.Revision,
		DesiredTargetID: stored.DesiredTargetID, AppliedTargetID: stored.AppliedTargetID,
		DesiredInstallationID: stored.DesiredInstallationID, AppliedInstallationID: stored.AppliedInstallationID,
	}, nil
}

func (s *Service) PrepareMigration(ctx context.Context, roomID, worldID string) (MigrationPlacement, error) {
	result, err := s.plan(ctx, roomID, nil)
	if err != nil {
		return MigrationPlacement{}, err
	}
	selected := result.plans[roomID]
	worlds := worldsByID(selected.worlds)
	world, exists := worlds[worldID]
	if !exists {
		return MigrationPlacement{}, rooms.ErrWorldNotFound
	}
	placement, exists := placementsByWorld(selected.record.Placements)[worldID]
	if !exists {
		return MigrationPlacement{}, executionBlocked("PLACEMENT_MISSING", "世界没有可迁移的 Placement")
	}
	if sameEndpoint(placement.DesiredTargetID, placement.DesiredInstallationID, placement.AppliedTargetID, placement.AppliedInstallationID) {
		return MigrationPlacement{}, ErrMigrationNotRequired
	}
	inventories := endpointInventories(result.inventories)
	source, sourceOK := inventories[endpointFor(placement.AppliedTargetID, placement.AppliedInstallationID)]
	target, targetOK := inventories[endpointFor(placement.DesiredTargetID, placement.DesiredInstallationID)]
	if !sourceOK || !targetOK || !source.Target.Configured || !target.Target.Configured {
		return MigrationPlacement{}, executionBlocked("MIGRATION_TARGET_MISSING", "迁移源或目标运行节点不存在或尚未配置")
	}
	for _, candidate := range []agents.RuntimeTargetInventory{source, target} {
		if !candidate.Target.Online {
			return MigrationPlacement{}, executionBlocked("MIGRATION_TARGET_OFFLINE", "迁移源和目标节点必须同时在线")
		}
		if !candidate.Available || candidate.Stale {
			return MigrationPlacement{}, executionBlocked("MIGRATION_INVENTORY_STALE", "迁移源和目标节点必须具有最新运行时清单")
		}
		if candidate.Target.Kind == agents.RuntimeKindAgent && !containsCapability(candidate.Target.Capabilities, "runtime.migration.v1") {
			return MigrationPlacement{}, executionBlocked("AGENT_CAPABILITY_MISSING", "远程节点 Agent 版本不支持分片迁移")
		}
	}
	for _, link := range selected.record.ShardLinks {
		if link.SourceTargetID == placement.DesiredTargetID && link.SourceInstallationID == placement.DesiredInstallationID &&
			target.Target.Kind == agents.RuntimeKindAgent && !containsCapability(target.Target.Capabilities, "runtime.migration.shard-routing.v1") {
			return MigrationPlacement{}, executionBlocked("AGENT_SHARD_ROUTING_CAPABILITY_MISSING", "目标节点 Agent 版本不支持迁移时写入跨机器 Shard 互联线路，请先升级 Agent")
		}
	}
	identity := identityFor(selected.room.DirectoryName, world.DirectoryName)
	if !inventoryHasShard(source.Inventory, identity) {
		return MigrationPlacement{}, executionBlocked("MIGRATION_SOURCE_MISSING", "迁移源节点未发现分片文件")
	}
	if inventoryHasShard(target.Inventory, identity) {
		return MigrationPlacement{}, executionBlocked("MIGRATION_TARGET_EXISTS", "迁移目标已经存在同名分片，不能覆盖")
	}
	build, err := s.syncInfrastructureState(result.plans, result.inventories, &resourcePreflightScope{
		owners: map[string]bool{resourceOwnerKey(roomID, worldID): true},
		states: map[ReservationState]bool{
			ReservationActive: true, ReservationPlanned: true, ReservationObserved: true,
		},
	})
	if err != nil {
		return MigrationPlacement{}, err
	}
	if !build.preflight.Ready {
		return MigrationPlacement{}, &ResourceConflictError{Preflight: build.preflight}
	}
	return MigrationPlacement{
		Room: selected.room, World: world, Revision: selected.record.Revision,
		SourceTargetID: placement.AppliedTargetID, TargetTargetID: placement.DesiredTargetID,
		SourceInstallationID: placement.AppliedInstallationID, TargetInstallationID: placement.DesiredInstallationID,
		Source: source.Target, Target: target.Target, SourceInventory: source, TargetInventory: target,
		AppliedShardLinks: publicStoredShardLinks(selected.record.AppliedShardLinks),
	}, nil
}

func (s *Service) ApplyMigration(roomID, worldID, expectedRevision, targetID, installationID string) (ExecutionPlacement, error) {
	installationID = strings.TrimSpace(installationID)
	selected, err := s.store.load(roomID)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	if selected.Revision != expectedRevision {
		return ExecutionPlacement{}, &RevisionConflictError{CurrentRevision: selected.Revision}
	}
	placements := normalizedPlacements(selected.Placements)
	changed := false
	for index := range placements {
		if placements[index].WorldID != worldID {
			continue
		}
		if installationID == "" {
			installationID = placements[index].DesiredInstallationID
		}
		if !sameEndpoint(placements[index].DesiredTargetID, placements[index].DesiredInstallationID, targetID, installationID) {
			return ExecutionPlacement{}, &RevisionConflictError{CurrentRevision: selected.Revision}
		}
		if sameEndpoint(placements[index].AppliedTargetID, placements[index].AppliedInstallationID, targetID, installationID) {
			return ExecutionPlacement{}, ErrMigrationNotRequired
		}
		placements[index].AppliedTargetID = targetID
		placements[index].AppliedInstallationID = installationID
		changed = true
		break
	}
	if !changed {
		return ExecutionPlacement{}, rooms.ErrWorldNotFound
	}
	appliedShardLinks := nextAppliedShardLinks(selected.AppliedShardLinks, selected.ShardLinks, placements)
	saved, err := s.store.SavePlan(roomID, expectedRevision, placements, selected.ShardLinks, appliedShardLinks)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	placement, exists := placementsByWorld(saved.Placements)[worldID]
	if !exists {
		return ExecutionPlacement{}, rooms.ErrWorldNotFound
	}
	world := worldFromStoredPlacement(roomID, placement)
	return ExecutionPlacement{
		Room: room, World: world, Revision: saved.Revision,
		DesiredTargetID: targetID, AppliedTargetID: targetID,
		DesiredInstallationID: installationID, AppliedInstallationID: installationID,
	}, nil
}

func (s *Service) RollbackMigration(
	roomID, worldID, expectedRevision, sourceTargetID, sourceInstallationID, targetTargetID, targetInstallationID string,
	previousAppliedShardLinks []ShardLink,
) (ExecutionPlacement, error) {
	sourceTargetID = strings.TrimSpace(sourceTargetID)
	sourceInstallationID = strings.TrimSpace(sourceInstallationID)
	targetTargetID = strings.TrimSpace(targetTargetID)
	targetInstallationID = strings.TrimSpace(targetInstallationID)
	selected, err := s.store.load(roomID)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	if selected.Revision != expectedRevision {
		return ExecutionPlacement{}, &RevisionConflictError{CurrentRevision: selected.Revision}
	}
	placements := normalizedPlacements(selected.Placements)
	changed := false
	for index := range placements {
		if placements[index].WorldID != worldID {
			continue
		}
		if !sameEndpoint(placements[index].DesiredTargetID, placements[index].DesiredInstallationID, targetTargetID, targetInstallationID) ||
			!sameEndpoint(placements[index].AppliedTargetID, placements[index].AppliedInstallationID, targetTargetID, targetInstallationID) {
			return ExecutionPlacement{}, &RevisionConflictError{CurrentRevision: selected.Revision}
		}
		placements[index].AppliedTargetID = sourceTargetID
		placements[index].AppliedInstallationID = sourceInstallationID
		changed = true
		break
	}
	if !changed {
		return ExecutionPlacement{}, rooms.ErrWorldNotFound
	}
	saved, err := s.store.SavePlan(
		roomID, expectedRevision, placements, selected.ShardLinks, storedPublicShardLinks(previousAppliedShardLinks),
	)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	placement, exists := placementsByWorld(saved.Placements)[worldID]
	if !exists {
		return ExecutionPlacement{}, rooms.ErrWorldNotFound
	}
	return ExecutionPlacement{
		Room: room, World: worldFromStoredPlacement(roomID, placement), Revision: saved.Revision,
		DesiredTargetID: targetTargetID, AppliedTargetID: sourceTargetID,
		DesiredInstallationID: targetInstallationID, AppliedInstallationID: sourceInstallationID,
	}, nil
}

func (s *Service) PreviewStartCapacity(ctx context.Context, roomID string, selectedWorldIDs []string) (StartCapacityPreview, error) {
	batch, err := s.PreviewBatchStartCapacity(ctx, []StartCapacitySelection{{RoomID: roomID, WorldIDs: selectedWorldIDs}})
	if err != nil {
		return StartCapacityPreview{}, err
	}
	selection := batch.Rooms[0]
	return StartCapacityPreview{
		RoomID: roomID, WorldIDs: selection.WorldIDs, Targets: batch.Targets,
		RequiresRiskConfirmation: batch.RequiresRiskConfirmation, Policy: batch.Policy,
	}, nil
}

func (s *Service) PreviewBatchStartCapacity(ctx context.Context, selections []StartCapacitySelection) (BatchStartCapacityPreview, error) {
	if len(selections) == 0 {
		return BatchStartCapacityPreview{}, ErrInvalidInput
	}
	firstRoomID := strings.TrimSpace(selections[0].RoomID)
	if firstRoomID == "" {
		return BatchStartCapacityPreview{}, ErrInvalidInput
	}
	result, err := s.plan(ctx, firstRoomID, nil)
	if err != nil {
		return BatchStartCapacityPreview{}, err
	}
	endpointInventory := endpointInventories(result.inventories)
	inventories := machineInventories(result.inventories)
	starting := make(map[string]int)
	normalizedSelections := make([]StartCapacitySelection, 0, len(selections))
	seenRooms := make(map[string]bool, len(selections))
	for _, selection := range selections {
		roomID := strings.TrimSpace(selection.RoomID)
		if roomID == "" || seenRooms[roomID] {
			return BatchStartCapacityPreview{}, ErrInvalidInput
		}
		seenRooms[roomID] = true
		selected, exists := result.plans[roomID]
		if !exists {
			if _, roomErr := s.rooms.Room(roomID); roomErr != nil {
				return BatchStartCapacityPreview{}, roomErr
			}
			return BatchStartCapacityPreview{}, ErrRoomNotManaged
		}
		worlds, worldErr := selectCapacityWorlds(selected.worlds, selection.WorldIDs)
		if worldErr != nil {
			return BatchStartCapacityPreview{}, worldErr
		}
		placements := placementsByWorld(selected.record.Placements)
		worldIDs := make([]string, 0, len(worlds))
		for _, world := range worlds {
			worldIDs = append(worldIDs, world.ID)
			placement, placementExists := placements[world.ID]
			if !placementExists {
				return BatchStartCapacityPreview{}, executionBlocked("APPLIED_TARGET_MISSING", "世界没有已生效的运行目标")
			}
			starting[placement.AppliedTargetID] += 0
			inventory, available := endpointInventory[endpointFor(placement.AppliedTargetID, placement.AppliedInstallationID)]
			if !available || !currentlyRunning(inventory, selected.room.DirectoryName, world.DirectoryName) {
				starting[placement.AppliedTargetID]++
			}
		}
		sort.Strings(worldIDs)
		normalizedSelections = append(normalizedSelections, StartCapacitySelection{RoomID: roomID, WorldIDs: worldIDs})
	}
	targets := make([]StartCapacityTarget, 0, len(starting))
	requiresConfirmation := false
	for targetID, startingShards := range starting {
		inventory, exists := inventories[targetID]
		current := 0
		stale := true
		name := targetID
		if exists {
			name = inventory.Target.Name
			current = len(inventory.Inventory.Processes)
			stale = !inventory.Available || inventory.Stale || !inventory.Target.Online
		}
		projected := current + startingShards
		capacity := agents.CapacityForResources(
			inventory.Inventory.CPU.LogicalProcessors, inventory.Inventory.CPU.PhysicalCores,
			inventory.Inventory.CPU.PhysicalCoreEstimated, projected, stale,
			inventory.Inventory.Memory.TotalBytes, inventory.Inventory.Memory.AvailableBytes, startingShards,
		)
		requiresRisk := capacity.State == agents.CapacityOvercommitted || capacity.State == agents.CapacityUnknown || capacity.MemoryState == agents.MemoryCapacityCritical
		requiresConfirmation = requiresConfirmation || requiresRisk
		targets = append(targets, StartCapacityTarget{
			TargetID: targetID, TargetName: name, CurrentRunningShards: current, StartingShards: startingShards,
			ProjectedRunningShards: projected, Capacity: capacity, RequiresRiskConfirmation: requiresRisk,
		})
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].TargetID == localTargetID {
			return true
		}
		if targets[j].TargetID == localTargetID {
			return false
		}
		return strings.ToLower(targets[i].TargetName) < strings.ToLower(targets[j].TargetName)
	})
	sort.Slice(normalizedSelections, func(i, j int) bool { return normalizedSelections[i].RoomID < normalizedSelections[j].RoomID })
	return BatchStartCapacityPreview{
		Rooms: normalizedSelections, Targets: targets, RequiresRiskConfirmation: requiresConfirmation,
		Policy: defaultCapacityPolicy(),
	}, nil
}

func selectCapacityWorlds(worlds []rooms.World, selected []string) ([]rooms.World, error) {
	if len(selected) == 0 {
		return append([]rooms.World(nil), worlds...), nil
	}
	wanted := make(map[string]bool, len(selected))
	for _, worldID := range selected {
		worldID = strings.TrimSpace(worldID)
		if worldID == "" || wanted[worldID] {
			return nil, ErrInvalidInput
		}
		wanted[worldID] = true
	}
	result := make([]rooms.World, 0, len(wanted))
	for _, world := range worlds {
		if wanted[world.ID] {
			result = append(result, world)
			delete(wanted, world.ID)
		}
	}
	if len(wanted) > 0 {
		return nil, rooms.ErrWorldNotFound
	}
	return result, nil
}

func currentlyRunning(inventory agents.RuntimeTargetInventory, cluster, shard string) bool {
	if !inventory.Available || inventory.Stale || !inventory.Target.Online {
		return false
	}
	identity := identityFor(cluster, shard)
	for _, process := range inventory.Inventory.Processes {
		if identityFor(process.Cluster, process.Shard) == identity {
			return true
		}
	}
	return false
}

func executionBlocked(code, message string) error {
	return &ExecutionError{Code: code, Message: message}
}

func containsCapability(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

// Ordinary Runtime calls resolve only their room. Directory discovery and
// infrastructure synchronization belong to explicit topology operations.
func (s *Service) executionPlan(ctx context.Context, roomID string) (planResult, error) {
	if err := ctx.Err(); err != nil {
		return planResult{}, err
	}
	selected, err := s.reconcileRoom(roomID)
	if err != nil {
		return planResult{}, err
	}
	inventories, err := s.targets.RuntimeTargetInventories(ctx)
	if err != nil {
		return planResult{}, err
	}
	plans := canonicalizePlanPlacements(map[string]roomPlan{roomID: selected}, inventories)
	return planResult{
		snapshot: buildSnapshot(roomID, plans, inventories), record: plans[roomID].record,
		plans: plans, inventories: inventories,
	}, nil
}

func (s *Service) plan(ctx context.Context, roomID string, request *UpdateRequest) (planResult, error) {
	inventories, err := s.targets.RuntimeTargetInventories(ctx)
	if err != nil {
		return planResult{}, err
	}
	if err := s.syncRuntimeRoomCatalog(inventories); err != nil {
		return planResult{}, err
	}
	plans, err := s.reconcileAll(roomID)
	if err != nil {
		return planResult{}, err
	}
	plans = canonicalizePlanPlacements(plans, inventories)
	selected, exists := plans[roomID]
	if !exists {
		return planResult{}, rooms.ErrRoomNotFound
	}
	if err := s.store.SyncRuntimeCatalog(inventories); err != nil {
		return planResult{}, err
	}
	candidate := selected.record
	changed := false
	var resources resourceBuild
	resourcesOK := false
	if request != nil {
		placements, err := validateRequest(*request, selected, inventories)
		if err != nil {
			return planResult{}, err
		}
		if request.ExpectedRevision != selected.record.Revision {
			return planResult{}, &RevisionConflictError{CurrentRevision: selected.record.Revision}
		}
		candidate.Placements = desiredPlacements(selected.record.Placements, placements)
		if request.ShardLinks != nil {
			candidate.ShardLinks, err = validateDesiredShardLinks(request.ShardLinks, candidate.Placements, selected.worlds, inventories)
			if err != nil {
				return planResult{}, err
			}
		} else {
			candidate.ShardLinks = reconcileStoredShardLinks(candidate.ShardLinks, candidate.Placements, selected.worlds)
		}
		changed = !samePlacements(candidate.Placements, selected.record.Placements) || !sameStoredShardLinks(candidate.ShardLinks, selected.record.ShardLinks)
		selected.record = candidate
		plans[roomID] = selected
		owners := make(map[string]bool, len(selected.worlds))
		for _, world := range selected.worlds {
			owners[resourceOwnerKey(roomID, world.ID)] = true
		}
		resources, err = s.buildRuntimeResources(plans, inventories, &resourcePreflightScope{
			owners: owners, allowShardLinkConfigurationDrift: true,
			states: map[ReservationState]bool{
				ReservationActive: true, ReservationPlanned: true, ReservationObserved: true,
			},
		})
		if err != nil {
			return planResult{}, err
		}
		resourcesOK = true
		if !resources.preflight.Ready {
			return planResult{}, &ResourceConflictError{Preflight: resources.preflight}
		}
	}
	snapshot := buildSnapshot(roomID, plans, inventories)
	return planResult{
		snapshot: snapshot, record: candidate, plans: plans, inventories: inventories,
		resources: resources, resourcesOK: resourcesOK, changed: changed,
	}, nil
}

func (s *Service) syncRuntimeRoomCatalog(inventories []agents.RuntimeTargetInventory) error {
	catalog, ok := s.rooms.(runtimeRoomCatalogSynchronizer)
	if !ok {
		return nil
	}
	sources := make([]rooms.RuntimeCatalogSource, 0, len(inventories))
	for _, inventory := range inventories {
		observedAt := time.Time{}
		if inventory.ObservedAt != nil {
			observedAt = inventory.ObservedAt.UTC()
		}
		sources = append(sources, rooms.RuntimeCatalogSource{
			TargetID: inventory.Target.ID, Online: inventory.Target.Online, Available: inventory.Available,
			Stale: inventory.Stale, ObservedAt: observedAt, Inventory: inventory.Inventory,
		})
	}
	return catalog.SyncRuntimeCatalog(sources)
}

func (s *Service) reconcileAll(selectedRoomID string) (map[string]roomPlan, error) {
	var selectedRoom rooms.Room
	if strings.TrimSpace(selectedRoomID) != "" {
		var err error
		selectedRoom, err = s.rooms.Room(selectedRoomID)
		if err != nil {
			return nil, err
		}
		if !selectedRoom.Managed {
			return nil, ErrRoomNotManaged
		}
	}
	items, err := s.rooms.List()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(items)+1)
	if selectedRoom.ID != "" {
		items = append(items, selectedRoom)
	}
	plans := make(map[string]roomPlan, len(items))
	for _, room := range items {
		if seen[room.ID] || !room.Managed {
			continue
		}
		seen[room.ID] = true
		selected, err := s.reconcileRoom(room.ID)
		if err != nil {
			return nil, err
		}
		plans[room.ID] = selected
	}
	return plans, nil
}

func (s *Service) reconcileRoom(roomID string) (roomPlan, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return roomPlan{}, err
	}
	if !room.Managed {
		return roomPlan{}, ErrRoomNotManaged
	}
	worlds, err := s.rooms.Worlds(roomID)
	if err != nil {
		return roomPlan{}, err
	}
	stored, err := s.store.EnsureWorlds(roomID, worlds)
	if err != nil {
		return roomPlan{}, err
	}
	return roomPlan{room: room, worlds: mergeStoredWorlds(roomID, worlds, stored.Placements), record: stored}, nil
}

func validateRequest(request UpdateRequest, selected roomPlan, inventories []agents.RuntimeTargetInventory) ([]PlacementInput, error) {
	fields := make(map[string]string)
	if strings.TrimSpace(request.ExpectedRevision) == "" {
		fields["expectedRevision"] = "必须提供当前拓扑 revision"
	}
	targets := targetsByID(inventories)
	worlds := make(map[string]bool, len(selected.worlds))
	for _, world := range selected.worlds {
		worlds[world.ID] = true
	}
	seen := make(map[string]bool, len(request.Placements))
	placements := append([]PlacementInput(nil), request.Placements...)
	for index := range placements {
		placement := &placements[index]
		path := fmt.Sprintf("placements.%d", index)
		placement.WorldID = strings.TrimSpace(placement.WorldID)
		placement.TargetID = strings.TrimSpace(placement.TargetID)
		placement.InstallationID = strings.TrimSpace(placement.InstallationID)
		if !worlds[placement.WorldID] {
			fields[path+".worldId"] = "世界不存在或不属于当前房间"
		} else if seen[placement.WorldID] {
			fields[path+".worldId"] = "同一世界只能配置一次"
		}
		seen[placement.WorldID] = true
		target, exists := targets[placement.TargetID]
		if !exists {
			fields[path+".targetId"] = "运行目标不存在"
		} else if !target.Configured {
			fields[path+".targetId"] = "运行目标尚未完成路径配置"
		} else if installationID, valid := canonicalInstallationID(target, placement.InstallationID); !valid {
			fields[path+".installationId"] = "DST 安装实例不存在或未登记"
		} else {
			placement.InstallationID = installationID
		}
	}
	if len(request.Placements) != len(selected.worlds) || len(seen) != len(selected.worlds) {
		fields["placements"] = "必须为当前房间的每个世界指定一个运行目标"
	}
	if len(fields) > 0 {
		return nil, &FieldError{Fields: fields}
	}
	return placements, nil
}

func desiredPlacements(current []storedPlacement, input []PlacementInput) []storedPlacement {
	targets := make(map[string]PlacementInput, len(input))
	for _, placement := range input {
		placement.WorldID = strings.TrimSpace(placement.WorldID)
		placement.TargetID = strings.TrimSpace(placement.TargetID)
		placement.InstallationID = strings.TrimSpace(placement.InstallationID)
		targets[placement.WorldID] = placement
	}
	next := append([]storedPlacement(nil), current...)
	for index := range next {
		placement := targets[next[index].WorldID]
		next[index].DesiredTargetID = placement.TargetID
		next[index].DesiredInstallationID = placement.InstallationID
	}
	return normalizedPlacements(next)
}

func canonicalizePlanPlacements(plans map[string]roomPlan, inventories []agents.RuntimeTargetInventory) map[string]roomPlan {
	targets := targetsByID(inventories)
	for roomID, plan := range plans {
		placements := append([]storedPlacement(nil), plan.record.Placements...)
		for index := range placements {
			placements[index] = canonicalStoredPlacement(placements[index], targets)
		}
		plan.record.Placements = normalizedPlacements(placements)
		plans[roomID] = plan
	}
	return plans
}

func samePlacements(left, right []storedPlacement) bool {
	left, right = normalizedPlacements(left), normalizedPlacements(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type shardIdentity struct {
	cluster string
	shard   string
}

type desiredShard struct {
	endpoint placementEndpoint
	roomID   string
	worldID  string
}

func buildSnapshot(roomID string, plans map[string]roomPlan, inventories []agents.RuntimeTargetInventory) Snapshot {
	selected := plans[roomID]
	targetInventories := machineInventories(inventories)
	installationInventories := endpointInventories(inventories)
	desired := make(map[shardIdentity]desiredShard)
	plannedCounts := make(map[string]int)
	for currentRoomID, plan := range plans {
		worlds := worldsByID(plan.worlds)
		for _, placement := range plan.record.Placements {
			world, exists := worlds[placement.WorldID]
			if !exists {
				continue
			}
			identity := identityFor(plan.room.DirectoryName, world.DirectoryName)
			desired[identity] = desiredShard{
				endpoint: endpointFor(placement.DesiredTargetID, placement.DesiredInstallationID),
				roomID:   currentRoomID, worldID: world.ID,
			}
			plannedCounts[placement.DesiredTargetID]++
		}
	}

	runtimes := make(map[shardIdentity]map[placementEndpoint]int)
	matched := make(map[string]map[shardIdentity]bool)
	for _, inventory := range inventories {
		// A retained snapshot is useful for diagnostics, but it is not current
		// evidence that a Shard is still running and must not create conflicts.
		if !inventory.Available || inventory.Stale || !inventory.Target.Online {
			continue
		}
		for _, process := range inventory.Inventory.Processes {
			identity := identityFor(process.Cluster, process.Shard)
			if identity.cluster == "" || identity.shard == "" {
				continue
			}
			if runtimes[identity] == nil {
				runtimes[identity] = make(map[placementEndpoint]int)
			}
			endpoint := endpointFor(inventory.Target.ID, installationIDForInventory(inventory))
			runtimes[identity][endpoint]++
			if expected, exists := desired[identity]; exists && expected.endpoint == endpoint {
				if matched[inventory.Target.ID] == nil {
					matched[inventory.Target.ID] = make(map[shardIdentity]bool)
				}
				matched[inventory.Target.ID][identity] = true
			}
		}
	}

	allTargetIDs := make(map[string]bool, len(targetInventories))
	for targetID := range targetInventories {
		allTargetIDs[targetID] = true
	}
	for _, plan := range plans {
		for _, placement := range plan.record.Placements {
			allTargetIDs[placement.DesiredTargetID] = true
			allTargetIDs[placement.AppliedTargetID] = true
		}
	}
	targets := make([]TargetSummary, 0, len(allTargetIDs))
	issues := make([]Issue, 0)
	requiresConfirmation := false
	for targetID := range allTargetIDs {
		inventory, exists := targetInventories[targetID]
		if !exists {
			kind := agents.RuntimeKindAgent
			if targetID == localTargetID {
				kind = agents.RuntimeKindLocal
			}
			inventory = agents.RuntimeTargetInventory{
				Target: agents.RuntimeTarget{ID: targetID, Name: targetID, Kind: kind, Status: agents.RuntimeStatusOffline},
				Stale:  true, StaleReason: "target_missing", Capacity: agents.Capacity{State: agents.CapacityUnknown},
			}
			targetInventories[targetID] = inventory
		}
		observed := len(inventory.Inventory.Processes)
		managedRunning := len(matched[targetID])
		unmanaged := observed - managedRunning
		if unmanaged < 0 {
			unmanaged = 0
		}
		projected := plannedCounts[targetID] + unmanaged
		stale := inventory.Stale || !inventory.Available || !inventory.Target.Online
		additionalShards := projected - observed
		if additionalShards < 0 {
			additionalShards = 0
		}
		projectedCapacity := agents.CapacityForResources(
			inventory.Inventory.CPU.LogicalProcessors,
			inventory.Inventory.CPU.PhysicalCores,
			inventory.Inventory.CPU.PhysicalCoreEstimated,
			projected,
			stale,
			inventory.Inventory.Memory.TotalBytes,
			inventory.Inventory.Memory.AvailableBytes,
			additionalShards,
		)
		overcommitted := projectedCapacity.State == agents.CapacityOvercommitted
		memoryCritical := projectedCapacity.MemoryState == agents.MemoryCapacityCritical
		if overcommitted || memoryCritical {
			requiresConfirmation = true
		}
		if overcommitted {
			issues = append(issues, Issue{
				Code: "TARGET_OVERCOMMITTED", Severity: SeverityWarning, TargetID: targetID,
				Message: fmt.Sprintf("节点 %s 计划承载 %d 个世界分片，超过建议上限 %d；同一核心运行多层世界可能造成卡顿", inventory.Target.Name, projected, projectedCapacity.RecommendedShardLimit),
			})
		} else if projectedCapacity.State == agents.CapacityFull {
			issues = append(issues, Issue{
				Code: "TARGET_CAPACITY_FULL", Severity: SeverityWarning, TargetID: targetID,
				Message: fmt.Sprintf("节点 %s 计划承载 %d 个世界分片，已达到建议上限", inventory.Target.Name, projected),
			})
		} else if projected > 0 && projectedCapacity.State == agents.CapacityUnknown {
			issues = append(issues, Issue{
				Code: "TARGET_CAPACITY_UNKNOWN", Severity: SeverityWarning, TargetID: targetID,
				Message: fmt.Sprintf("节点 %s 的容量数据不可用，无法判断 %d 个计划分片是否会造成卡顿", inventory.Target.Name, projected),
			})
		}
		if memoryCritical {
			issues = append(issues, Issue{
				Code: "TARGET_MEMORY_CRITICAL", Severity: SeverityWarning, TargetID: targetID,
				Message: fmt.Sprintf("节点 %s 启动计划世界后预计可用内存不足 384 MiB，可能触发 OOM", inventory.Target.Name),
			})
		} else if projectedCapacity.MemoryState == agents.MemoryCapacityTight {
			issues = append(issues, Issue{
				Code: "TARGET_MEMORY_TIGHT", Severity: SeverityWarning, TargetID: targetID,
				Message: fmt.Sprintf("节点 %s 启动计划世界后预计可用内存低于 768 MiB，建议避免同时执行更新、地图或压缩任务", inventory.Target.Name),
			})
		}
		targets = append(targets, TargetSummary{
			ID: targetID, Name: inventory.Target.Name, Kind: inventory.Target.Kind, Status: inventory.Target.Status,
			Online: inventory.Target.Online, Configured: inventory.Target.Configured,
			InventoryAvailable: inventory.Available, InventoryStale: inventory.Stale, StaleReason: inventory.StaleReason,
			ObservationState: inventory.ObservationState, ObservationError: inventory.ObservationError,
			RefreshStartedAt: inventory.RefreshStartedAt,
			ObservedAt:       inventory.ObservedAt, ObservedRunningShards: observed, UnmanagedRunningShards: unmanaged,
			PlannedShards: plannedCounts[targetID], ProjectedShards: projected,
			CurrentCapacity: inventory.Capacity, ProjectedCapacity: projectedCapacity,
			RequiresOvercommitConfirmation: overcommitted || memoryCritical,
			DefaultInstallationID:          defaultInstallationID(inventory.Target),
			Installations:                  installationSummaries(inventory.Target, inventories),
		})
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].ID == localTargetID {
			return true
		}
		if targets[j].ID == localTargetID {
			return false
		}
		return strings.ToLower(targets[i].Name) < strings.ToLower(targets[j].Name)
	})

	placements := make([]Placement, 0, len(selected.worlds))
	stored := placementsByWorld(selected.record.Placements)
	for _, world := range selected.worlds {
		value := stored[world.ID]
		identity := identityFor(selected.room.DirectoryName, world.DirectoryName)
		observedTargets, observedLocations, processCount := observedRuntimeLocations(runtimes[identity])
		state, issue := placementStatus(world, value, identity, targetInventories, installationInventories, observedLocations, processCount)
		if issue != nil {
			issues = append(issues, *issue)
		}
		placements = append(placements, Placement{
			WorldID: world.ID, WorldName: world.Name, WorldRole: world.Role,
			DesiredTargetID: value.DesiredTargetID, AppliedTargetID: value.AppliedTargetID,
			DesiredInstallationID: value.DesiredInstallationID, AppliedInstallationID: value.AppliedInstallationID,
			State: state, Running: processCount > 0, ObservedTargetIDs: observedTargets, ObservedLocations: observedLocations,
		})
	}

	return Snapshot{
		RoomID: roomID, Revision: selected.record.Revision, Mode: "applied_placement", RemoteExecutionReady: true,
		Placements: placements, ShardLinks: publicStoredShardLinks(selected.record.ShardLinks),
		AppliedShardLinks: publicStoredShardLinks(selected.record.AppliedShardLinks), Targets: targets, Issues: issues,
		RequiresOvercommitConfirmation: requiresConfirmation,
		CapacityPolicy:                 defaultCapacityPolicy(),
		UpdatedAt:                      selected.record.UpdatedAt,
	}
}

func placementStatus(
	world rooms.World,
	placement storedPlacement,
	identity shardIdentity,
	targets map[string]agents.RuntimeTargetInventory,
	inventories map[placementEndpoint]agents.RuntimeTargetInventory,
	observed []PlacementLocation,
	processCount int,
) (PlacementState, *Issue) {
	if processCount > 1 || unexpectedRuntime(observed, endpointFor(placement.AppliedTargetID, placement.AppliedInstallationID)) {
		return PlacementConflict, &Issue{
			Code: "SHARD_RUNTIME_CONFLICT", Severity: SeverityError, WorldID: world.ID,
			Message: fmt.Sprintf("世界 %s 在多个位置或非生效目标上被发现，继续启动可能造成双写存档", world.Name),
		}
	}
	target, targetExists := targets[placement.DesiredTargetID]
	if !targetExists || target.StaleReason == "target_missing" {
		return PlacementInventoryMissing, &Issue{Code: "TARGET_MISSING", Severity: SeverityWarning, WorldID: world.ID, TargetID: placement.DesiredTargetID, Message: "计划运行目标已不存在，请重新选择节点"}
	}
	inventory, exists := inventories[endpointFor(placement.DesiredTargetID, placement.DesiredInstallationID)]
	if !exists {
		return PlacementInventoryMissing, &Issue{Code: "INSTALLATION_MISSING", Severity: SeverityWarning, WorldID: world.ID, TargetID: placement.DesiredTargetID, Message: fmt.Sprintf("节点 %s 未登记安装实例 %s", target.Target.Name, placement.DesiredInstallationID)}
	}
	if !inventory.Target.Online {
		return PlacementTargetOffline, &Issue{Code: "TARGET_OFFLINE", Severity: SeverityWarning, WorldID: world.ID, TargetID: placement.DesiredTargetID, Message: fmt.Sprintf("节点 %s 当前离线，计划可以保存但不能执行", inventory.Target.Name)}
	}
	if !inventory.Available {
		return PlacementInventoryMissing, &Issue{Code: "INVENTORY_MISSING", Severity: SeverityWarning, WorldID: world.ID, TargetID: placement.DesiredTargetID, Message: fmt.Sprintf("节点 %s 尚无运行时清单", inventory.Target.Name)}
	}
	if inventory.Stale {
		return PlacementInventoryStale, &Issue{Code: "INVENTORY_STALE", Severity: SeverityWarning, WorldID: world.ID, TargetID: placement.DesiredTargetID, Message: fmt.Sprintf("节点 %s 的运行时清单已过期", inventory.Target.Name)}
	}
	if !inventoryHasShard(inventory.Inventory, identity) {
		return PlacementShardMissing, &Issue{Code: "SHARD_MISSING", Severity: SeverityInfo, WorldID: world.ID, TargetID: placement.DesiredTargetID, Message: fmt.Sprintf("节点 %s 尚未准备世界 %s 的文件；后续迁移阶段需要先复制并校验", inventory.Target.Name, world.Name)}
	}
	if !sameEndpoint(placement.DesiredTargetID, placement.DesiredInstallationID, placement.AppliedTargetID, placement.AppliedInstallationID) {
		return PlacementPlanned, nil
	}
	return PlacementAligned, nil
}

func inventoryHasShard(report shared.RuntimeInventoryReport, identity shardIdentity) bool {
	for _, room := range report.Rooms {
		if identityFor(room.Directory, "").cluster != identity.cluster {
			continue
		}
		for _, shard := range room.Shards {
			if identityFor(room.Directory, shard.Directory) == identity {
				return true
			}
		}
	}
	return false
}

func observedRuntimeLocations(values map[placementEndpoint]int) ([]string, []PlacementLocation, int) {
	targetSet := make(map[string]bool, len(values))
	locations := make([]PlacementLocation, 0, len(values))
	total := 0
	for endpoint, count := range values {
		targetSet[endpoint.targetID] = true
		locations = append(locations, PlacementLocation{TargetID: endpoint.targetID, InstallationID: endpoint.installationID})
		total += count
	}
	targets := make([]string, 0, len(targetSet))
	for targetID := range targetSet {
		targets = append(targets, targetID)
	}
	sort.Strings(targets)
	sort.Slice(locations, func(i, j int) bool {
		if locations[i].TargetID == locations[j].TargetID {
			return locations[i].InstallationID < locations[j].InstallationID
		}
		return locations[i].TargetID < locations[j].TargetID
	})
	return targets, locations, total
}

func unexpectedRuntime(observed []PlacementLocation, applied placementEndpoint) bool {
	for _, location := range observed {
		if endpointFor(location.TargetID, location.InstallationID) != applied {
			return true
		}
	}
	return false
}

func identityFor(cluster, shard string) shardIdentity {
	return shardIdentity{cluster: strings.ToLower(strings.TrimSpace(cluster)), shard: strings.ToLower(strings.TrimSpace(shard))}
}

func worldsByID(values []rooms.World) map[string]rooms.World {
	result := make(map[string]rooms.World, len(values))
	for _, value := range values {
		result[value.ID] = value
	}
	return result
}

func mergeStoredWorlds(roomID string, values []rooms.World, placements []storedPlacement) []rooms.World {
	result := append([]rooms.World(nil), values...)
	seen := make(map[string]bool, len(result))
	for _, world := range result {
		seen[world.ID] = true
	}
	for _, placement := range placements {
		if !seen[placement.WorldID] {
			result = append(result, worldFromStoredPlacement(roomID, placement))
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Role != result[j].Role {
			return result[i].Role < result[j].Role
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

func worldFromStoredPlacement(roomID string, placement storedPlacement) rooms.World {
	directory := strings.TrimSpace(placement.WorldDirectoryName)
	if directory == "" {
		if decoded, err := rooms.DecodeID(placement.WorldID); err == nil {
			directory = decoded
		}
	}
	name := strings.TrimSpace(placement.WorldName)
	if name == "" {
		name = directory
	}
	role := placement.WorldRole
	if role == "" {
		switch strings.ToLower(directory) {
		case "master":
			role = rooms.WorldRoleMaster
		case "caves":
			role = rooms.WorldRoleCaves
		default:
			role = rooms.WorldRoleCustom
		}
	}
	return rooms.World{
		ID: placement.WorldID, RoomID: roomID, DirectoryName: directory, Name: name,
		Role: role, IsMaster: role == rooms.WorldRoleMaster,
	}
}

func placementsByWorld(values []storedPlacement) map[string]storedPlacement {
	result := make(map[string]storedPlacement, len(values))
	for _, value := range values {
		result[value.WorldID] = value
	}
	return result
}
