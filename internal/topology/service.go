package topology

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

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

type Service struct {
	rooms   roomCatalog
	targets targetCatalog
	store   *Store
}

type roomPlan struct {
	room   rooms.Room
	worlds []rooms.World
	record record
}

type planResult struct {
	snapshot    Snapshot
	record      record
	plans       map[string]roomPlan
	inventories []agents.RuntimeTargetInventory
	changed     bool
}

func NewService(roomService roomCatalog, targetService targetCatalog, store *Store) (*Service, error) {
	if roomService == nil || targetService == nil || store == nil {
		return nil, errors.New("topology dependencies are required")
	}
	return &Service{rooms: roomService, targets: targetService, store: store}, nil
}

func (s *Service) Topology(ctx context.Context, roomID string) (Snapshot, error) {
	result, err := s.plan(ctx, roomID, nil)
	if err != nil {
		return Snapshot{}, err
	}
	return result.snapshot, nil
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
		return result.snapshot, nil
	}
	saved, err := s.store.Save(roomID, request.ExpectedRevision, result.record.Placements)
	if err != nil {
		return Snapshot{}, err
	}
	result.snapshot.Revision = saved.Revision
	result.snapshot.UpdatedAt = saved.UpdatedAt
	return result.snapshot, nil
}

// ResolveExecution returns the currently applied runtime target. Desired
// placement is never used as an execution fallback while migration is pending.
func (s *Service) ResolveExecution(ctx context.Context, roomID, worldID string) (ExecutionPlacement, error) {
	result, err := s.plan(ctx, roomID, nil)
	if err != nil {
		return ExecutionPlacement{}, err
	}
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
	inventories := make(map[string]agents.RuntimeTargetInventory, len(result.inventories))
	for _, inventory := range result.inventories {
		inventories[inventory.Target.ID] = inventory
	}
	inventory, exists := inventories[stored.AppliedTargetID]
	if !exists || !inventory.Target.Configured {
		return ExecutionPlacement{}, executionBlocked("APPLIED_TARGET_MISSING", "已生效运行目标不存在或尚未配置")
	}
	resolved := ExecutionPlacement{
		Room: selected.room, World: world, Revision: selected.record.Revision,
		DesiredTargetID: stored.DesiredTargetID, AppliedTargetID: stored.AppliedTargetID,
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
	if !inventory.Available {
		return ExecutionPlacement{}, executionBlocked("APPLIED_INVENTORY_MISSING", "已生效运行目标尚无运行时清单")
	}
	if inventory.Stale {
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
	plans, err := s.reconcileAll(roomID)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	selected := plans[roomID]
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
	}, nil
}

func (s *Service) PreviewStartCapacity(ctx context.Context, roomID string, selectedWorldIDs []string) (StartCapacityPreview, error) {
	result, err := s.plan(ctx, roomID, nil)
	if err != nil {
		return StartCapacityPreview{}, err
	}
	selected := result.plans[roomID]
	worlds, err := selectCapacityWorlds(selected.worlds, selectedWorldIDs)
	if err != nil {
		return StartCapacityPreview{}, err
	}
	inventories := make(map[string]agents.RuntimeTargetInventory, len(result.inventories))
	for _, inventory := range result.inventories {
		inventories[inventory.Target.ID] = inventory
	}
	placements := placementsByWorld(selected.record.Placements)
	starting := make(map[string]int)
	worldIDs := make([]string, 0, len(worlds))
	for _, world := range worlds {
		worldIDs = append(worldIDs, world.ID)
		placement, exists := placements[world.ID]
		if !exists {
			return StartCapacityPreview{}, executionBlocked("APPLIED_TARGET_MISSING", "世界没有已生效的运行目标")
		}
		starting[placement.AppliedTargetID] += 0
		inventory, available := inventories[placement.AppliedTargetID]
		if !available || !currentlyRunning(inventory, selected.room.DirectoryName, world.DirectoryName) {
			starting[placement.AppliedTargetID]++
		}
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
		capacity := agents.CapacityFor(
			inventory.Inventory.CPU.LogicalProcessors, inventory.Inventory.CPU.PhysicalCores,
			inventory.Inventory.CPU.PhysicalCoreEstimated, projected, stale,
		)
		requiresRisk := capacity.State == agents.CapacityOvercommitted || capacity.State == agents.CapacityUnknown
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
	sort.Strings(worldIDs)
	return StartCapacityPreview{
		RoomID: roomID, WorldIDs: worldIDs, Targets: targets, RequiresRiskConfirmation: requiresConfirmation,
		Policy: CapacityPolicy{
			Basis: "physical_cores", ShardsPerPhysicalCore: 1, ReservedPhysicalCores: 1, Enforced: false,
			Message: "保守建议一颗物理核心最多运行一层世界，并额外为系统和运维任务预留 1 核；超出只告警并要求确认。",
		},
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

func (s *Service) plan(ctx context.Context, roomID string, request *UpdateRequest) (planResult, error) {
	plans, err := s.reconcileAll(roomID)
	if err != nil {
		return planResult{}, err
	}
	selected, exists := plans[roomID]
	if !exists {
		return planResult{}, rooms.ErrRoomNotFound
	}
	inventories, err := s.targets.RuntimeTargetInventories(ctx)
	if err != nil {
		return planResult{}, err
	}
	candidate := selected.record
	changed := false
	if request != nil {
		if err := validateRequest(*request, selected, inventories); err != nil {
			return planResult{}, err
		}
		if request.ExpectedRevision != selected.record.Revision {
			return planResult{}, &RevisionConflictError{CurrentRevision: selected.record.Revision}
		}
		candidate.Placements = desiredPlacements(selected.record.Placements, request.Placements)
		changed = !samePlacements(candidate.Placements, selected.record.Placements)
		selected.record = candidate
		plans[roomID] = selected
	}
	snapshot := buildSnapshot(roomID, plans, inventories)
	return planResult{snapshot: snapshot, record: candidate, plans: plans, inventories: inventories, changed: changed}, nil
}

func (s *Service) reconcileAll(selectedRoomID string) (map[string]roomPlan, error) {
	selectedRoom, err := s.rooms.Room(selectedRoomID)
	if err != nil {
		return nil, err
	}
	if !selectedRoom.Managed {
		return nil, ErrRoomNotManaged
	}
	items, err := s.rooms.List()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(items)+1)
	items = append(items, selectedRoom)
	plans := make(map[string]roomPlan, len(items))
	for _, room := range items {
		if seen[room.ID] || !room.Managed {
			continue
		}
		seen[room.ID] = true
		worlds, worldsErr := s.rooms.Worlds(room.ID)
		if worldsErr != nil {
			return nil, worldsErr
		}
		worldIDs := make([]string, 0, len(worlds))
		for _, world := range worlds {
			worldIDs = append(worldIDs, world.ID)
		}
		stored, ensureErr := s.store.Ensure(room.ID, worldIDs)
		if ensureErr != nil {
			return nil, ensureErr
		}
		plans[room.ID] = roomPlan{room: room, worlds: worlds, record: stored}
	}
	return plans, nil
}

func validateRequest(request UpdateRequest, selected roomPlan, inventories []agents.RuntimeTargetInventory) error {
	fields := make(map[string]string)
	if strings.TrimSpace(request.ExpectedRevision) == "" {
		fields["expectedRevision"] = "必须提供当前拓扑 revision"
	}
	targets := make(map[string]agents.RuntimeTarget, len(inventories))
	for _, inventory := range inventories {
		targets[inventory.Target.ID] = inventory.Target
	}
	worlds := make(map[string]bool, len(selected.worlds))
	for _, world := range selected.worlds {
		worlds[world.ID] = true
	}
	seen := make(map[string]bool, len(request.Placements))
	for index, placement := range request.Placements {
		path := fmt.Sprintf("placements.%d", index)
		placement.WorldID = strings.TrimSpace(placement.WorldID)
		placement.TargetID = strings.TrimSpace(placement.TargetID)
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
		}
	}
	if len(request.Placements) != len(selected.worlds) || len(seen) != len(selected.worlds) {
		fields["placements"] = "必须为当前房间的每个世界指定一个运行目标"
	}
	if len(fields) > 0 {
		return &FieldError{Fields: fields}
	}
	return nil
}

func desiredPlacements(current []storedPlacement, input []PlacementInput) []storedPlacement {
	targets := make(map[string]string, len(input))
	for _, placement := range input {
		targets[strings.TrimSpace(placement.WorldID)] = strings.TrimSpace(placement.TargetID)
	}
	next := append([]storedPlacement(nil), current...)
	for index := range next {
		next[index].DesiredTargetID = targets[next[index].WorldID]
	}
	return normalizedPlacements(next)
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
	targetID string
	roomID   string
	worldID  string
}

func buildSnapshot(roomID string, plans map[string]roomPlan, inventories []agents.RuntimeTargetInventory) Snapshot {
	selected := plans[roomID]
	targetInventories := make(map[string]agents.RuntimeTargetInventory, len(inventories))
	for _, inventory := range inventories {
		targetInventories[inventory.Target.ID] = inventory
	}
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
			desired[identity] = desiredShard{targetID: placement.DesiredTargetID, roomID: currentRoomID, worldID: world.ID}
			plannedCounts[placement.DesiredTargetID]++
		}
	}

	runtimes := make(map[shardIdentity]map[string]int)
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
				runtimes[identity] = make(map[string]int)
			}
			runtimes[identity][inventory.Target.ID]++
			if expected, exists := desired[identity]; exists && expected.targetID == inventory.Target.ID {
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
		projectedCapacity := agents.CapacityFor(
			inventory.Inventory.CPU.LogicalProcessors,
			inventory.Inventory.CPU.PhysicalCores,
			inventory.Inventory.CPU.PhysicalCoreEstimated,
			projected,
			stale,
		)
		overcommitted := projectedCapacity.State == agents.CapacityOvercommitted
		if overcommitted {
			requiresConfirmation = true
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
		targets = append(targets, TargetSummary{
			ID: targetID, Name: inventory.Target.Name, Kind: inventory.Target.Kind, Status: inventory.Target.Status,
			Online: inventory.Target.Online, Configured: inventory.Target.Configured,
			InventoryAvailable: inventory.Available, InventoryStale: inventory.Stale, StaleReason: inventory.StaleReason,
			ObservedAt: inventory.ObservedAt, ObservedRunningShards: observed, UnmanagedRunningShards: unmanaged,
			PlannedShards: plannedCounts[targetID], ProjectedShards: projected,
			CurrentCapacity: inventory.Capacity, ProjectedCapacity: projectedCapacity,
			RequiresOvercommitConfirmation: overcommitted,
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
		observedTargets, processCount := observedTargets(runtimes[identity])
		state, issue := placementStatus(world, value, identity, targetInventories, observedTargets, processCount)
		if issue != nil {
			issues = append(issues, *issue)
		}
		placements = append(placements, Placement{
			WorldID: world.ID, WorldName: world.Name, WorldRole: world.Role,
			DesiredTargetID: value.DesiredTargetID, AppliedTargetID: value.AppliedTargetID,
			State: state, ObservedTargetIDs: observedTargets,
		})
	}

	return Snapshot{
		RoomID: roomID, Revision: selected.record.Revision, Mode: "planning_only", RemoteExecutionReady: false,
		Placements: placements, Targets: targets, Issues: issues,
		RequiresOvercommitConfirmation: requiresConfirmation,
		CapacityPolicy: CapacityPolicy{
			Basis: "physical_cores", ShardsPerPhysicalCore: 1, ReservedPhysicalCores: 1, Enforced: false,
			Message: "同一服务器可以运行多个房间和多层世界；保守建议每个运行中的世界分片预留 1 个物理核心，并为系统、Agent、SteamCMD 和备份至少保留 1 核。该规则仅用于预警，不是性能保证。",
		},
		UpdatedAt: selected.record.UpdatedAt,
	}
}

func placementStatus(world rooms.World, placement storedPlacement, identity shardIdentity, inventories map[string]agents.RuntimeTargetInventory, observed []string, processCount int) (PlacementState, *Issue) {
	if processCount > 1 || unexpectedRuntime(observed, placement.AppliedTargetID) {
		return PlacementConflict, &Issue{
			Code: "SHARD_RUNTIME_CONFLICT", Severity: SeverityError, WorldID: world.ID,
			Message: fmt.Sprintf("世界 %s 在多个位置或非生效目标上被发现，继续启动可能造成双写存档", world.Name),
		}
	}
	inventory, exists := inventories[placement.DesiredTargetID]
	if !exists || inventory.StaleReason == "target_missing" {
		return PlacementInventoryMissing, &Issue{Code: "TARGET_MISSING", Severity: SeverityWarning, WorldID: world.ID, TargetID: placement.DesiredTargetID, Message: "计划运行目标已不存在，请重新选择节点"}
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
	if placement.DesiredTargetID != placement.AppliedTargetID {
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

func observedTargets(values map[string]int) ([]string, int) {
	items := make([]string, 0, len(values))
	total := 0
	for targetID, count := range values {
		items = append(items, targetID)
		total += count
	}
	sort.Strings(items)
	return items, total
}

func unexpectedRuntime(observed []string, appliedTargetID string) bool {
	for _, targetID := range observed {
		if targetID != appliedTargetID {
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

func placementsByWorld(values []storedPlacement) map[string]storedPlacement {
	result := make(map[string]storedPlacement, len(values))
	for _, value := range values {
		result[value.WorldID] = value
	}
	return result
}
