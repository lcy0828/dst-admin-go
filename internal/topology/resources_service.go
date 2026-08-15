package topology

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/shared"
)

type shardPortSet struct {
	clusterMaster int
	server        int
	steamAuth     int
	steamMaster   int
}

type resourceBuild struct {
	reservations []PortReservation
	allocations  []CPUAllocation
	preflight    ResourcePreflight
}

type resourcePreflightScope struct {
	owners map[string]bool
	states map[ReservationState]bool
}

func (s *Service) Infrastructure(ctx context.Context) (InfrastructureSnapshot, error) {
	plans, inventories, err := s.resourceContext(ctx)
	if err != nil {
		return InfrastructureSnapshot{}, err
	}
	build, err := s.syncInfrastructureState(plans, inventories, nil)
	if err != nil {
		return InfrastructureSnapshot{}, err
	}
	providers, environments, profiles, reservations, allocations, err := s.store.RuntimeResources()
	if err != nil {
		return InfrastructureSnapshot{}, err
	}
	return InfrastructureSnapshot{
		Providers: providers, Environments: environments, NetworkProfiles: profiles,
		PortReservations: reservations, CPUAllocations: allocations, Preflight: build.preflight,
		CapacityPolicy: defaultCapacityPolicy(), ObservedAt: time.Now().UTC(),
	}, nil
}

func (s *Service) UpdateNetworkProfile(ctx context.Context, profileID string, input NetworkProfileUpdate) (NetworkProfile, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.BindAddress = strings.TrimSpace(input.BindAddress)
	input.AdvertiseAddress = strings.TrimSpace(input.AdvertiseAddress)
	fields := map[string]string{}
	if utf8.RuneCountInString(input.Name) < 1 || utf8.RuneCountInString(input.Name) > 100 {
		fields["name"] = "名称需要 1-100 个字符"
	}
	if input.BindAddress == "" {
		input.BindAddress = "0.0.0.0"
	}
	if net.ParseIP(input.BindAddress) == nil {
		fields["bindAddress"] = "监听地址必须是有效 IPv4 或 IPv6 地址"
	}
	if len(input.AdvertiseAddress) > 255 || strings.ContainsAny(input.AdvertiseAddress, "\x00\r\n \t/\\") {
		fields["advertiseAddress"] = "公布地址必须是有效的单个 IP 或主机名"
	}
	if len(fields) > 0 {
		return NetworkProfile{}, &ResourceFieldError{Fields: fields}
	}
	if _, _, err := s.resourceContext(ctx); err != nil {
		return NetworkProfile{}, err
	}
	profile, err := s.store.UpdateNetworkProfile(strings.TrimSpace(profileID), input)
	if err != nil {
		return NetworkProfile{}, err
	}
	// Rebuild derived reservations so their bind identity follows the profile.
	plans, inventories, err := s.resourceContext(ctx)
	if err != nil {
		return NetworkProfile{}, err
	}
	if _, err := s.syncInfrastructureState(plans, inventories, nil); err != nil {
		return NetworkProfile{}, err
	}
	return profile, nil
}

func (s *Service) UpdateCPUAllocation(ctx context.Context, input CPUAllocationUpdate) (CPUAllocation, error) {
	input.RoomID = strings.TrimSpace(input.RoomID)
	input.WorldID = strings.TrimSpace(input.WorldID)
	input.EnvironmentID = strings.TrimSpace(input.EnvironmentID)
	input.LogicalCPUIds = sortedUniqueInts(input.LogicalCPUIds)
	if input.Policy != CPUPolicyNone && input.Policy != CPUPolicyShared && input.Policy != CPUPolicyExclusive {
		return CPUAllocation{}, &ResourceFieldError{Fields: map[string]string{"policy": "CPU 策略必须为 none、shared 或 exclusive"}}
	}
	plans, inventories, err := s.resourceContext(ctx)
	if err != nil {
		return CPUAllocation{}, err
	}
	build, err := s.syncInfrastructureState(plans, inventories, nil)
	if err != nil {
		return CPUAllocation{}, err
	}
	_ = build
	plan, exists := plans[input.RoomID]
	if !exists {
		return CPUAllocation{}, rooms.ErrRoomNotFound
	}
	world, exists := worldsByID(plan.worlds)[input.WorldID]
	if !exists {
		return CPUAllocation{}, rooms.ErrWorldNotFound
	}
	placement, exists := placementsByWorld(plan.record.Placements)[world.ID]
	if !exists {
		return CPUAllocation{}, executionBlocked("APPLIED_TARGET_MISSING", "世界没有已生效的运行目标")
	}
	expectedEnvironmentID := environmentResourceID(placement.AppliedTargetID)
	if input.EnvironmentID != expectedEnvironmentID {
		return CPUAllocation{}, &ResourceFieldError{Fields: map[string]string{"environmentId": "执行环境不是该世界当前生效 Placement 的环境"}}
	}
	inventory, exists := inventoryByTarget(inventories)[placement.AppliedTargetID]
	if !exists || !inventory.Available || inventory.Stale {
		return CPUAllocation{}, executionBlocked("CPU_INVENTORY_STALE", "CPU 分配需要目标节点的最新清单")
	}
	providers, _, _, _, allocations, err := s.store.RuntimeResources()
	if err != nil {
		return CPUAllocation{}, err
	}
	providerOS := ""
	for _, provider := range providers {
		if provider.TargetID == placement.AppliedTargetID {
			providerOS = provider.OS
			break
		}
	}
	allocation, err := validateCPUAllocation(input, plan.room, world, placement.AppliedTargetID, inventory.Inventory.CPU, providerOS, allocations)
	if err != nil {
		return CPUAllocation{}, err
	}
	return s.store.SaveCPUAllocation(allocation)
}

// PreflightExecution is called immediately before starting one or more Shards.
// It validates the applied topology, not a pending desired Placement.
func (s *Service) PreflightExecution(ctx context.Context, roomID string, worldIDs []string) (ResourcePreflight, error) {
	plans, inventories, err := s.resourceContext(ctx)
	if err != nil {
		return ResourcePreflight{}, err
	}
	selected, exists := plans[roomID]
	if !exists {
		return ResourcePreflight{}, rooms.ErrRoomNotFound
	}
	worlds, err := selectCapacityWorlds(selected.worlds, worldIDs)
	if err != nil {
		return ResourcePreflight{}, err
	}
	owners := make(map[string]bool, len(worlds))
	for _, world := range worlds {
		owners[resourceOwnerKey(roomID, world.ID)] = true
	}
	build, err := s.syncInfrastructureState(plans, inventories, &resourcePreflightScope{
		owners: owners,
		states: map[ReservationState]bool{ReservationActive: true, ReservationObserved: true},
	})
	if err != nil {
		return ResourcePreflight{}, err
	}
	if !build.preflight.Ready {
		return build.preflight, &ResourceConflictError{Preflight: build.preflight}
	}
	return build.preflight, nil
}

func (s *Service) resourceContext(ctx context.Context) (map[string]roomPlan, []agents.RuntimeTargetInventory, error) {
	inventories, err := s.targets.RuntimeTargetInventories(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := s.store.SyncRuntimeCatalog(inventories); err != nil {
		return nil, nil, err
	}
	items, err := s.rooms.List()
	if err != nil {
		return nil, nil, err
	}
	plans := make(map[string]roomPlan)
	for _, room := range items {
		if !room.Managed {
			continue
		}
		worlds, worldsErr := s.rooms.Worlds(room.ID)
		if worldsErr != nil {
			return nil, nil, worldsErr
		}
		record, ensureErr := s.store.EnsureWorlds(room.ID, worlds)
		if ensureErr != nil {
			return nil, nil, ensureErr
		}
		worlds = mergeStoredWorlds(room.ID, worlds, record.Placements)
		plans[room.ID] = roomPlan{room: room, worlds: worlds, record: record}
	}
	return plans, inventories, nil
}

func (s *Service) syncInfrastructureState(plans map[string]roomPlan, inventories []agents.RuntimeTargetInventory, scope *resourcePreflightScope) (resourceBuild, error) {
	build, err := s.buildRuntimeResources(plans, inventories, scope)
	if err != nil {
		return resourceBuild{}, err
	}
	if err := s.persistInfrastructureState(build); err != nil {
		return resourceBuild{}, err
	}
	return build, nil
}

func (s *Service) persistInfrastructureState(build resourceBuild) error {
	if err := s.store.ReplacePortReservations(build.reservations); err != nil {
		return err
	}
	if err := s.store.EnsureCPUAllocations(build.allocations); err != nil {
		return err
	}
	return nil
}

func (s *Service) buildRuntimeResources(plans map[string]roomPlan, inventories []agents.RuntimeTargetInventory, scope *resourcePreflightScope) (resourceBuild, error) {
	_, environments, profiles, _, _, err := s.store.RuntimeResources()
	if err != nil {
		return resourceBuild{}, err
	}
	environmentByTarget := make(map[string]ExecutionEnvironment, len(environments))
	profileByTarget := make(map[string]NetworkProfile, len(profiles))
	for _, environment := range environments {
		environmentByTarget[environment.TargetID] = environment
	}
	for _, profile := range profiles {
		for _, environment := range environments {
			if environment.ID == profile.EnvironmentID {
				profileByTarget[environment.TargetID] = profile
				break
			}
		}
	}
	build := resourceBuild{preflight: ResourcePreflight{Ready: true, Warnings: []string{}, Conflicts: []ResourceConflict{}}}
	managedTargets := make(map[string]bool)
	for roomID, plan := range plans {
		placements := placementsByWorld(plan.record.Placements)
		for _, world := range plan.worlds {
			placement, ok := placements[world.ID]
			if !ok {
				continue
			}
			ports, conflicts := resolveShardPorts(inventories, plan.room.DirectoryName, world.DirectoryName)
			ownerStrict := scope != nil && scope.owners[resourceOwnerKey(roomID, world.ID)]
			if scope == nil || ownerStrict {
				for index := range conflicts {
					conflicts[index].RoomID = roomID
					conflicts[index].WorldID = world.ID
				}
				build.preflight.Conflicts = append(build.preflight.Conflicts, conflicts...)
			}
			expected := []struct {
				purpose PortPurpose
				port    int
			}{
				{PortDSTServer, ports.server}, {PortSteamAuth, ports.steamAuth}, {PortSteamMasterServer, ports.steamMaster},
			}
			if world.Role == rooms.WorldRoleMaster && len(plan.worlds) > 1 {
				expected = append(expected, struct {
					purpose PortPurpose
					port    int
				}{PortClusterMaster, ports.clusterMaster})
			}
			for _, value := range expected {
				if value.port < 1 || value.port > 65535 {
					message := fmt.Sprintf("%s / %s 缺少有效的 %s UDP 端口", plan.room.Name, world.Name, value.purpose)
					if ownerStrict {
						build.preflight.Conflicts = append(build.preflight.Conflicts, ResourceConflict{Code: "PORT_CONFIGURATION_MISSING", Message: message, RoomID: roomID, WorldID: world.ID})
					} else {
						build.preflight.Warnings = append(build.preflight.Warnings, message)
					}
				}
			}
			targets := []struct {
				id    string
				state ReservationState
			}{{placement.AppliedTargetID, ReservationActive}}
			if placement.DesiredTargetID != placement.AppliedTargetID {
				targets = append(targets, struct {
					id    string
					state ReservationState
				}{placement.DesiredTargetID, ReservationPlanned})
			}
			for _, target := range targets {
				managedTargets[target.id+"\x00"+identityKey(plan.room.DirectoryName, world.DirectoryName)] = true
				build.reservations = appendManagedReservations(build.reservations, environmentByTarget[target.id], profileByTarget[target.id], target.id, roomID, world, plan.room.DirectoryName, ports, target.state)
			}
			environment := environmentByTarget[placement.AppliedTargetID]
			build.allocations = append(build.allocations, CPUAllocation{
				ID: allocationResourceID(roomID, world.ID), EnvironmentID: environment.ID, TargetID: placement.AppliedTargetID,
				RoomID: roomID, WorldID: world.ID, Policy: CPUPolicyNone, LogicalCPUIds: []int{}, PhysicalCoreKeys: []string{}, Warnings: []string{},
			})
		}
	}
	for _, inventory := range inventories {
		if !inventory.Available || inventory.Stale || !inventory.Target.Online {
			continue
		}
		targetID := inventory.Target.ID
		for _, room := range inventory.Inventory.Rooms {
			for _, shard := range room.Shards {
				if managedTargets[targetID+"\x00"+identityKey(room.Directory, shard.Directory)] {
					continue
				}
				ports := shardPortSet{clusterMaster: room.MasterPort, server: shard.ServerPort, steamAuth: shard.AuthenticationPort, steamMaster: shard.MasterServerPort}
				build.reservations = appendObservedReservations(build.reservations, environmentByTarget[targetID], profileByTarget[targetID], targetID, room.Directory, shard, ports)
			}
		}
	}
	build.preflight.Conflicts = append(build.preflight.Conflicts, detectPortConflicts(build.reservations, scope)...)
	build.preflight.Warnings = uniqueSortedStrings(build.preflight.Warnings)
	build.preflight.Conflicts = uniqueResourceConflicts(build.preflight.Conflicts)
	build.preflight.Ready = len(build.preflight.Conflicts) == 0
	sort.Slice(build.reservations, func(i, j int) bool {
		left, right := build.reservations[i], build.reservations[j]
		if left.ScopeID != right.ScopeID {
			return left.ScopeID < right.ScopeID
		}
		if left.Port != right.Port {
			return left.Port < right.Port
		}
		return left.ID < right.ID
	})
	return build, nil
}

func resolveShardPorts(inventories []agents.RuntimeTargetInventory, cluster, shard string) (shardPortSet, []ResourceConflict) {
	result := shardPortSet{}
	conflicts := []ResourceConflict{}
	merge := func(label string, current *int, candidate int) {
		if candidate <= 0 {
			return
		}
		if *current == 0 {
			*current = candidate
			return
		}
		if *current != candidate {
			conflicts = append(conflicts, ResourceConflict{Code: "PORT_CONFIGURATION_INCONSISTENT", Message: fmt.Sprintf("%s/%s 的 %s 端口在节点清单中不一致", cluster, shard, label), Port: candidate})
		}
	}
	for _, inventory := range inventories {
		for _, room := range inventory.Inventory.Rooms {
			if !strings.EqualFold(room.Directory, cluster) {
				continue
			}
			merge("cluster master", &result.clusterMaster, room.MasterPort)
			for _, world := range room.Shards {
				if !strings.EqualFold(world.Directory, shard) {
					continue
				}
				merge("DST server", &result.server, world.ServerPort)
				merge("Steam authentication", &result.steamAuth, world.AuthenticationPort)
				merge("Steam master server", &result.steamMaster, world.MasterServerPort)
			}
		}
	}
	return result, conflicts
}

func appendManagedReservations(values []PortReservation, environment ExecutionEnvironment, profile NetworkProfile, targetID, roomID string, world rooms.World, cluster string, ports shardPortSet, state ReservationState) []PortReservation {
	items := []struct {
		purpose PortPurpose
		port    int
	}{{PortDSTServer, ports.server}, {PortSteamAuth, ports.steamAuth}, {PortSteamMasterServer, ports.steamMaster}}
	if world.Role == rooms.WorldRoleMaster && ports.clusterMaster > 0 {
		items = append(items, struct {
			purpose PortPurpose
			port    int
		}{PortClusterMaster, ports.clusterMaster})
	}
	for _, item := range items {
		if item.port < 1 || item.port > 65535 {
			continue
		}
		identity := targetID + "\x00" + roomID + "\x00" + world.ID + "\x00" + string(item.purpose) + "\x00" + string(state)
		values = append(values, PortReservation{ID: stableResourceID("port", identity), EnvironmentID: environment.ID, NetworkProfileID: profile.ID, ScopeID: profile.ScopeID, TargetID: targetID, RoomID: roomID, WorldID: world.ID, Cluster: cluster, Shard: world.DirectoryName, Purpose: item.purpose, Protocol: "udp", BindAddress: profile.BindAddress, Port: item.port, State: state, Managed: true})
	}
	return values
}

func appendObservedReservations(values []PortReservation, environment ExecutionEnvironment, profile NetworkProfile, targetID, cluster string, shard shared.ShardInventoryReport, ports shardPortSet) []PortReservation {
	items := []struct {
		purpose PortPurpose
		port    int
	}{{PortDSTServer, ports.server}, {PortSteamAuth, ports.steamAuth}, {PortSteamMasterServer, ports.steamMaster}}
	if strings.EqualFold(shard.Role, "master") && ports.clusterMaster > 0 {
		items = append(items, struct {
			purpose PortPurpose
			port    int
		}{PortClusterMaster, ports.clusterMaster})
	}
	for _, item := range items {
		if item.port < 1 || item.port > 65535 {
			continue
		}
		identity := targetID + "\x00" + cluster + "\x00" + shard.Directory + "\x00" + string(item.purpose)
		values = append(values, PortReservation{ID: stableResourceID("port-observed", identity), EnvironmentID: environment.ID, NetworkProfileID: profile.ID, ScopeID: profile.ScopeID, TargetID: targetID, Cluster: cluster, Shard: shard.Directory, Purpose: item.purpose, Protocol: "udp", BindAddress: profile.BindAddress, Port: item.port, State: ReservationObserved, Managed: false})
	}
	return values
}

func detectPortConflicts(values []PortReservation, scope *resourcePreflightScope) []ResourceConflict {
	conflicts := []ResourceConflict{}
	for leftIndex := range values {
		left := values[leftIndex]
		if scope != nil && !scope.states[left.State] {
			continue
		}
		for rightIndex := leftIndex + 1; rightIndex < len(values); rightIndex++ {
			right := values[rightIndex]
			if scope != nil && !scope.states[right.State] {
				continue
			}
			if left.ScopeID == "" || left.ScopeID != right.ScopeID || left.Protocol != right.Protocol || left.Port != right.Port || !bindAddressesConflict(left.BindAddress, right.BindAddress) {
				continue
			}
			if left.RoomID != "" && left.RoomID == right.RoomID && left.WorldID == right.WorldID && left.Purpose == right.Purpose {
				continue
			}
			if scope != nil && !scope.owners[resourceOwnerKey(left.RoomID, left.WorldID)] && !scope.owners[resourceOwnerKey(right.RoomID, right.WorldID)] {
				continue
			}
			conflicts = append(conflicts, ResourceConflict{Code: "UDP_PORT_CONFLICT", ScopeID: left.ScopeID, Port: left.Port, TargetID: left.TargetID, RoomID: left.RoomID, WorldID: left.WorldID, Message: fmt.Sprintf("网络作用域 %s 的 UDP %d 同时被 %s/%s(%s) 与 %s/%s(%s) 占用", left.ScopeID, left.Port, left.Cluster, left.Shard, left.Purpose, right.Cluster, right.Shard, right.Purpose)})
		}
	}
	return conflicts
}

func bindAddressesConflict(left, right string) bool {
	normalize := func(value string) string {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" || value == "0.0.0.0" || value == "::" || value == "[::]" {
			return "*"
		}
		return value
	}
	left, right = normalize(left), normalize(right)
	return left == "*" || right == "*" || left == right
}

func validateCPUAllocation(input CPUAllocationUpdate, room rooms.Room, world rooms.World, targetID string, cpu shared.CPUInventory, platform string, existing []CPUAllocation) (CPUAllocation, error) {
	fields := map[string]string{}
	warnings := []string{"容量建议仍按一颗物理核心最多运行一个 Shard，并额外为系统、Agent、SteamCMD 和备份预留一核"}
	if input.Policy == CPUPolicyNone {
		if len(input.LogicalCPUIds) > 0 {
			fields["logicalCpuIds"] = "none 策略不能指定逻辑 CPU"
		}
		if len(fields) > 0 {
			return CPUAllocation{}, &ResourceFieldError{Fields: fields}
		}
		return CPUAllocation{ID: allocationResourceID(room.ID, world.ID), EnvironmentID: input.EnvironmentID, TargetID: targetID, RoomID: room.ID, WorldID: world.ID, Policy: CPUPolicyNone, LogicalCPUIds: []int{}, PhysicalCoreKeys: []string{}, Warnings: warnings}, nil
	}
	if len(input.LogicalCPUIds) == 0 {
		fields["logicalCpuIds"] = "shared/exclusive 策略至少需要一个逻辑 CPU"
	}
	for _, logicalID := range input.LogicalCPUIds {
		if logicalID < 0 || logicalID >= cpu.LogicalProcessors {
			fields["logicalCpuIds"] = "包含目标节点不存在的逻辑 CPU"
			break
		}
	}
	if len(fields) > 0 {
		return CPUAllocation{}, &ResourceFieldError{Fields: fields}
	}
	if input.Policy == CPUPolicyExclusive && (strings.EqualFold(platform, "darwin") || !cpu.TopologyAvailable) {
		return CPUAllocation{}, ErrCPUNotSupported
	}
	logicalToCore, siblings := cpuCoreTopology(cpu)
	coreSet := map[string]bool{}
	for _, logicalID := range input.LogicalCPUIds {
		if core := logicalToCore[logicalID]; core != "" {
			coreSet[core] = true
		}
	}
	coreKeys := make([]string, 0, len(coreSet))
	for key := range coreSet {
		coreKeys = append(coreKeys, key)
	}
	sort.Strings(coreKeys)
	if input.Policy == CPUPolicyExclusive {
		if len(coreKeys) != 1 {
			return CPUAllocation{}, &ResourceFieldError{Fields: map[string]string{"logicalCpuIds": "exclusive 策略每个 Shard 必须选择一个完整物理核心"}}
		}
		selected := map[int]bool{}
		for _, id := range input.LogicalCPUIds {
			selected[id] = true
		}
		partial := false
		for _, sibling := range siblings[coreKeys[0]] {
			if !selected[sibling] {
				partial = true
			}
		}
		if partial && !input.AllowSMTSiblingRisk {
			return CPUAllocation{}, &ResourceFieldError{Fields: map[string]string{"allowSmtSiblingRisk": "所选逻辑 CPU 未覆盖该物理核心的全部 SMT sibling；确认风险后才能继续"}}
		}
		if partial {
			warnings = append(warnings, "该独占分配未覆盖完整 SMT sibling，其他线程仍可能争用同一物理核心")
		}
	}
	requestedLogical := map[int]bool{}
	for _, id := range input.LogicalCPUIds {
		requestedLogical[id] = true
	}
	for _, allocation := range existing {
		if allocation.ID == allocationResourceID(room.ID, world.ID) || allocation.EnvironmentID != input.EnvironmentID || allocation.Policy == CPUPolicyNone {
			continue
		}
		otherLogical := map[int]bool{}
		for _, id := range allocation.LogicalCPUIds {
			otherLogical[id] = true
		}
		logicalOverlap := false
		for id := range requestedLogical {
			if otherLogical[id] {
				logicalOverlap = true
			}
		}
		coreOverlap := intersectsStrings(coreKeys, allocation.PhysicalCoreKeys)
		if (input.Policy == CPUPolicyExclusive || allocation.Policy == CPUPolicyExclusive) && (logicalOverlap || coreOverlap) {
			return CPUAllocation{}, &ResourceConflictError{Preflight: ResourcePreflight{Ready: false, Warnings: warnings, Conflicts: []ResourceConflict{{Code: "CPU_ALLOCATION_CONFLICT", TargetID: targetID, RoomID: room.ID, WorldID: world.ID, Message: "CPU 分配与同一执行环境中的另一 Shard 冲突"}}}}
		}
	}
	if input.Policy == CPUPolicyShared && !cpu.TopologyAvailable {
		warnings = append(warnings, "节点未提供完整物理核心拓扑，shared 策略无法判断 SMT sibling 争用")
	}
	return CPUAllocation{ID: allocationResourceID(room.ID, world.ID), EnvironmentID: input.EnvironmentID, TargetID: targetID, RoomID: room.ID, WorldID: world.ID, Policy: input.Policy, LogicalCPUIds: input.LogicalCPUIds, PhysicalCoreKeys: coreKeys, AllowSMTSiblingRisk: input.AllowSMTSiblingRisk, Warnings: uniqueSortedStrings(warnings)}, nil
}

func cpuCoreTopology(cpu shared.CPUInventory) (map[int]string, map[string][]int) {
	logicalToCore := map[int]string{}
	siblings := map[string][]int{}
	for _, thread := range cpu.Threads {
		key := thread.PackageID + ":" + thread.CoreID
		logicalToCore[thread.LogicalID] = key
		siblings[key] = append(siblings[key], thread.LogicalID)
	}
	return logicalToCore, siblings
}

func inventoryByTarget(values []agents.RuntimeTargetInventory) map[string]agents.RuntimeTargetInventory {
	result := make(map[string]agents.RuntimeTargetInventory, len(values))
	for _, value := range values {
		result[value.Target.ID] = value
	}
	return result
}

func resourceOwnerKey(roomID, worldID string) string { return roomID + "\x00" + worldID }
func identityKey(cluster, shard string) string {
	return strings.ToLower(cluster) + "\x00" + strings.ToLower(shard)
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func uniqueResourceConflicts(values []ResourceConflict) []ResourceConflict {
	seen := map[string]bool{}
	result := []ResourceConflict{}
	for _, value := range values {
		key := value.Code + "\x00" + value.ScopeID + "\x00" + strconv.Itoa(value.Port) + "\x00" + value.Message
		if !seen[key] {
			seen[key] = true
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Code+result[i].Message < result[j].Code+result[j].Message })
	return result
}

func intersectsStrings(left, right []string) bool {
	seen := map[string]bool{}
	for _, value := range left {
		seen[value] = true
	}
	for _, value := range right {
		if seen[value] {
			return true
		}
	}
	return false
}

func defaultCapacityPolicy() CapacityPolicy {
	return CapacityPolicy{Basis: "physical_cores", ShardsPerPhysicalCore: 1, ReservedPhysicalCores: 1, Enforced: false, Message: "保守建议一颗物理核心最多运行一层世界，并额外为系统和运维任务预留 1 核；超出只告警并要求确认。"}
}
