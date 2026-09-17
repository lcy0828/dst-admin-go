package topology

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/shared"

	"github.com/google/uuid"
)

const (
	maximumManualShardLinkCandidates = 16
	shardLinkProbeTimeout            = 5 * time.Second
)

type endpointProber interface {
	ListenNetworkEndpoint(context.Context, string, string, int, []string, time.Duration) ([]string, error)
	ProbeNetworkEndpoints(context.Context, string, []shared.RuntimeNetworkEndpointRequest, time.Duration) ([]shared.RuntimeNetworkEndpointResult, error)
}

type shardLinkProbeCandidate struct {
	ShardLinkCandidate
	token string
}

func validateDesiredShardLinks(inputs []ShardLinkInput, placements []storedPlacement, worlds []rooms.World, inventories []agents.RuntimeTargetInventory) ([]storedShardLink, error) {
	master, _, ok := proposedMasterPlacement(placements, worlds)
	if !ok {
		return nil, &FieldError{Fields: map[string]string{"placements": "跨机器部署必须存在唯一的 Master 世界"}}
	}
	expected := make(map[string]storedPlacement)
	for _, placement := range placements {
		if placement.DesiredTargetID == master.DesiredTargetID {
			continue
		}
		key := shardLinkEndpointKey(placement.DesiredTargetID, placement.DesiredInstallationID)
		expected[key] = placement
	}
	fields := map[string]string{}
	seen := map[string]bool{}
	result := make([]storedShardLink, 0, len(inputs))
	targets := targetsByID(inventories)
	for index, input := range inputs {
		path := fmt.Sprintf("shardLinks.%d", index)
		input.SourceTargetID = strings.TrimSpace(input.SourceTargetID)
		input.SourceInstallationID = strings.TrimSpace(input.SourceInstallationID)
		input.Address = strings.TrimSpace(strings.Trim(input.Address, "[]"))
		if target, exists := targets[input.SourceTargetID]; exists {
			if installationID, valid := canonicalInstallationID(target, input.SourceInstallationID); valid {
				input.SourceInstallationID = installationID
			}
		}
		key := shardLinkEndpointKey(input.SourceTargetID, input.SourceInstallationID)
		if _, exists := expected[key]; !exists {
			fields[path+".sourceTargetId"] = "互联线路来源不是当前计划中的 Secondary 运行位置"
		} else if seen[key] {
			fields[path+".sourceTargetId"] = "同一 Secondary 运行位置只能选择一条互联线路"
		}
		seen[key] = true
		if !validEndpointAddress(input.Address) || advertiseAddressUnroutable(input.Address) {
			fields[path+".address"] = "互联地址必须是可路由的 IP 或主机名"
		}
		if input.Port < 1 || input.Port > 65535 {
			fields[path+".port"] = "互联端口必须在 1-65535 之间"
		}
		if !validShardLinkMode(input.Mode) {
			fields[path+".mode"] = "互联线路类型无效"
		}
		result = append(result, storedShardLink{
			SourceTargetID: input.SourceTargetID, SourceInstallationID: input.SourceInstallationID,
			MasterTargetID: master.DesiredTargetID, MasterInstallationID: master.DesiredInstallationID,
			Address: input.Address, Port: input.Port, Mode: input.Mode,
		})
	}
	for key := range expected {
		if !seen[key] {
			fields["shardLinks"] = "必须为每个跨机器 Secondary 选择一条已探测的 Master 互联线路"
			break
		}
	}
	if len(inputs) != len(expected) {
		fields["shardLinks"] = "互联线路数量与跨机器 Secondary 运行位置不一致"
	}
	if len(fields) > 0 {
		return nil, &FieldError{Fields: fields}
	}
	return normalizedStoredShardLinks(result), nil
}

func reconcileStoredShardLinks(values []storedShardLink, placements []storedPlacement, worlds []rooms.World) []storedShardLink {
	master, _, ok := proposedMasterPlacement(placements, worlds)
	if !ok {
		return []storedShardLink{}
	}
	expected := map[string]bool{}
	for _, placement := range placements {
		if placement.DesiredTargetID != master.DesiredTargetID {
			expected[shardLinkEndpointKey(placement.DesiredTargetID, placement.DesiredInstallationID)] = true
		}
	}
	result := make([]storedShardLink, 0, len(values))
	for _, value := range values {
		if value.MasterTargetID != master.DesiredTargetID || value.MasterInstallationID != master.DesiredInstallationID ||
			!expected[shardLinkEndpointKey(value.SourceTargetID, value.SourceInstallationID)] {
			continue
		}
		result = append(result, value)
	}
	return normalizedStoredShardLinks(result)
}

func sameStoredShardLinks(left, right []storedShardLink) bool {
	left, right = normalizedStoredShardLinks(left), normalizedStoredShardLinks(right)
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

func publicStoredShardLinks(values []storedShardLink) []ShardLink {
	values = normalizedStoredShardLinks(values)
	result := make([]ShardLink, 0, len(values))
	for _, value := range values {
		result = append(result, ShardLink{
			SourceTargetID: value.SourceTargetID, SourceInstallationID: value.SourceInstallationID,
			MasterTargetID: value.MasterTargetID, MasterInstallationID: value.MasterInstallationID,
			Address: value.Address, Port: value.Port, Mode: value.Mode,
		})
	}
	return result
}

func storedPublicShardLinks(values []ShardLink) []storedShardLink {
	result := make([]storedShardLink, 0, len(values))
	for _, value := range values {
		result = append(result, storedShardLink{
			SourceTargetID: value.SourceTargetID, SourceInstallationID: value.SourceInstallationID,
			MasterTargetID: value.MasterTargetID, MasterInstallationID: value.MasterInstallationID,
			Address: value.Address, Port: value.Port, Mode: value.Mode,
		})
	}
	return normalizedStoredShardLinks(result)
}

// nextAppliedShardLinks advances only routes that belong to the topology that
// is actually applied. During a multi-world transition, unmatched routes stay
// absent instead of borrowing an endpoint from the future desired topology.
func nextAppliedShardLinks(current, desired []storedShardLink, placements []storedPlacement) []storedShardLink {
	if placementsAligned(placements) {
		return normalizedStoredShardLinks(desired)
	}
	master, ok := appliedMasterPlacement(placements)
	if !ok {
		return []storedShardLink{}
	}
	expected := make(map[string]bool)
	for _, placement := range placements {
		if placement.AppliedTargetID != master.AppliedTargetID {
			expected[shardLinkEndpointKey(placement.AppliedTargetID, placement.AppliedInstallationID)] = true
		}
	}
	selected := make(map[string]storedShardLink, len(expected))
	appendMatching := func(values []storedShardLink, replace bool) {
		for _, value := range values {
			key := shardLinkEndpointKey(value.SourceTargetID, value.SourceInstallationID)
			if !expected[key] || value.MasterTargetID != master.AppliedTargetID ||
				value.MasterInstallationID != master.AppliedInstallationID {
				continue
			}
			if _, exists := selected[key]; !exists || replace {
				selected[key] = value
			}
		}
	}
	appendMatching(current, false)
	appendMatching(desired, true)
	result := make([]storedShardLink, 0, len(selected))
	for _, value := range selected {
		result = append(result, value)
	}
	return normalizedStoredShardLinks(result)
}

func appliedMasterPlacement(placements []storedPlacement) (storedPlacement, bool) {
	var master storedPlacement
	count := 0
	for _, placement := range placements {
		if placement.WorldRole == rooms.WorldRoleMaster {
			master, count = placement, count+1
		}
	}
	return master, count == 1
}

func validShardLinkMode(value ShardLinkMode) bool {
	switch value {
	case ShardLinkLAN, ShardLinkOverlay, ShardLinkTunnel, ShardLinkPublic, ShardLinkConfigured, ShardLinkManual:
		return true
	default:
		return false
	}
}

func shardLinkEndpointKey(targetID, installationID string) string {
	return strings.TrimSpace(targetID) + "\x00" + strings.TrimSpace(installationID)
}

func (s *Service) DiscoverShardLinks(ctx context.Context, roomID string, request ShardLinkDiscoveryRequest) (ShardLinkDiscoveryResult, error) {
	inventories, err := s.targets.RuntimeTargetInventories(ctx)
	if err != nil {
		return ShardLinkDiscoveryResult{}, err
	}
	if err := s.syncRuntimeRoomCatalog(inventories); err != nil {
		return ShardLinkDiscoveryResult{}, err
	}
	if err := s.store.SyncRuntimeCatalog(inventories); err != nil {
		return ShardLinkDiscoveryResult{}, err
	}
	plans, err := s.reconcileAll(roomID)
	if err != nil {
		return ShardLinkDiscoveryResult{}, err
	}
	plans = canonicalizePlanPlacements(plans, inventories)
	selected, exists := plans[roomID]
	if !exists {
		return ShardLinkDiscoveryResult{}, rooms.ErrRoomNotFound
	}
	if request.ExpectedRevision != selected.record.Revision {
		return ShardLinkDiscoveryResult{}, &RevisionConflictError{CurrentRevision: selected.record.Revision}
	}
	placements, err := validateRequest(UpdateRequest{
		ExpectedRevision: request.ExpectedRevision,
		Placements:       request.Placements,
	}, selected, inventories)
	if err != nil {
		return ShardLinkDiscoveryResult{}, err
	}
	manual, err := normalizeManualShardLinkCandidates(request.ManualCandidates)
	if err != nil {
		return ShardLinkDiscoveryResult{}, err
	}

	targets := targetsByID(inventories)
	proposed := desiredPlacements(selected.record.Placements, placements)
	masterPlacement, _, ok := proposedMasterPlacement(proposed, selected.worlds)
	if !ok {
		return ShardLinkDiscoveryResult{}, &FieldError{Fields: map[string]string{"placements": "跨机器部署必须存在唯一的 Master 世界"}}
	}
	masterTarget := targets[masterPlacement.DesiredTargetID]
	masterPort, err := roomMasterPort(selected.room.DirectoryName, inventories)
	if err != nil {
		return ShardLinkDiscoveryResult{}, err
	}
	if masterPort == 0 {
		return ShardLinkDiscoveryResult{}, &FieldError{Fields: map[string]string{"masterPort": "无法从运行节点清单读取 Master Shard 端口"}}
	}

	result := ShardLinkDiscoveryResult{
		RoomID: roomID, Revision: selected.record.Revision,
		MasterTargetID: masterPlacement.DesiredTargetID, MasterInstallationID: masterPlacement.DesiredInstallationID,
		MasterTargetName: masterTarget.Name, MasterPort: masterPort, Links: []ShardLinkDiscovery{}, ObservedAt: time.Now().UTC(),
	}
	baseCandidates := s.shardLinkCandidates(ctx, masterTarget, masterPort, manual)
	result.Links = proposedShardLinks(proposed, selected.worlds, targets, masterPlacement, baseCandidates)
	if len(result.Links) == 0 {
		return result, nil
	}
	if len(baseCandidates) == 0 {
		result.ActiveProbeUnavailableReason = "Master 运行机器没有可探测的候选地址"
		return result, nil
	}
	if s.endpointProbe == nil {
		markShardLinkCandidatesUnverified(result.Links, "当前控制器不支持 Agent 端点探测")
		result.ActiveProbeUnavailableReason = "当前控制器不支持 Agent 端点探测"
		return result, nil
	}
	if !masterTarget.Online {
		markShardLinkCandidatesUnverified(result.Links, "Master 运行机器当前离线")
		result.ActiveProbeUnavailableReason = "Master 运行机器当前离线"
		return result, nil
	}

	probeLinks, tokens := prepareShardLinkProbeCandidates(result.Links)
	probeContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	type listenerResult struct {
		received []string
		err      error
	}
	listenerDone := make(chan listenerResult, 1)
	go func() {
		received, listenErr := s.endpointProbe.ListenNetworkEndpoint(
			probeContext, masterTarget.ID, "0.0.0.0", masterPort, tokens, 6500*time.Millisecond,
		)
		listenerDone <- listenerResult{received: received, err: listenErr}
	}()

	select {
	case listened := <-listenerDone:
		reason := "Master 端点探测监听启动失败"
		if listened.err != nil {
			reason += ": " + listened.err.Error()
		}
		markShardLinkCandidatesUnverifiedInternal(probeLinks, reason)
		result.Links = publicShardLinkDiscoveries(probeLinks)
		result.ActiveProbeUnavailableReason = reason
		return result, nil
	case <-time.After(200 * time.Millisecond):
	}

	result.ActiveProbe = true
	type probeResult struct {
		index  int
		values []shared.RuntimeNetworkEndpointResult
		err    error
	}
	probesDone := make(chan probeResult, len(probeLinks))
	for index, link := range probeLinks {
		index, link := index, link
		go func() {
			requests := make([]shared.RuntimeNetworkEndpointRequest, 0, len(link.Candidates))
			for _, candidate := range link.Candidates {
				requests = append(requests, shared.RuntimeNetworkEndpointRequest{
					Address: candidate.Address, Port: candidate.Port, Token: candidate.token,
				})
			}
			values, probeErr := s.endpointProbe.ProbeNetworkEndpoints(probeContext, link.SourceTargetID, requests, shardLinkProbeTimeout)
			probesDone <- probeResult{index: index, values: values, err: probeErr}
		}()
	}
	for range probeLinks {
		probed := <-probesDone
		applyShardLinkProbeResults(&probeLinks[probed.index], probed.values, probed.err)
	}
	select {
	case <-listenerDone:
	case <-probeContext.Done():
	}
	result.Links = publicShardLinkDiscoveries(probeLinks)
	return result, nil
}

func (s *Service) VerifyDesiredShardLinks(ctx context.Context, roomID string) error {
	result, err := s.plan(ctx, roomID, nil)
	if err != nil {
		return err
	}
	if len(result.record.ShardLinks) == 0 {
		return nil
	}
	placements := make([]PlacementInput, 0, len(result.record.Placements))
	manual := make([]ShardLinkCandidateInput, 0, len(result.record.ShardLinks))
	for _, placement := range result.record.Placements {
		placements = append(placements, PlacementInput{
			WorldID: placement.WorldID, TargetID: placement.DesiredTargetID, InstallationID: placement.DesiredInstallationID,
		})
	}
	for _, link := range result.record.ShardLinks {
		manual = append(manual, ShardLinkCandidateInput{Address: link.Address, Port: link.Port, Name: "已选择线路"})
	}
	discovery, err := s.DiscoverShardLinks(ctx, roomID, ShardLinkDiscoveryRequest{
		ExpectedRevision: result.record.Revision, Placements: placements, ManualCandidates: manual,
	})
	if err != nil {
		return err
	}
	if !discovery.ActiveProbe {
		return executionBlocked("SHARD_LINK_PROBE_UNAVAILABLE", nonEmptyShardLinkReason(discovery.ActiveProbeUnavailableReason))
	}
	discoveredByEndpoint := make(map[string]ShardLinkDiscovery, len(discovery.Links))
	for _, link := range discovery.Links {
		discoveredByEndpoint[shardLinkEndpointKey(link.SourceTargetID, link.SourceInstallationID)] = link
	}
	for _, selected := range result.record.ShardLinks {
		discovered, exists := discoveredByEndpoint[shardLinkEndpointKey(selected.SourceTargetID, selected.SourceInstallationID)]
		if !exists {
			return executionBlocked("SHARD_LINK_PROBE_MISSING", fmt.Sprintf("运行节点 %s 缺少 Shard 互联探测结果", selected.SourceTargetID))
		}
		reachable := false
		var reason string
		for _, candidate := range discovered.Candidates {
			if !sameEndpointAddress(candidate.Address, selected.Address) || candidate.Port != selected.Port {
				continue
			}
			reachable, reason = candidate.Reachable, candidate.Error
			break
		}
		if !reachable {
			message := fmt.Sprintf("从 %s 无法连接 Master 互联端点 %s:%d", discovered.SourceTargetName, selected.Address, selected.Port)
			if strings.TrimSpace(reason) != "" {
				message += ": " + reason
			}
			return executionBlocked("SHARD_LINK_UNREACHABLE", message)
		}
	}
	return nil
}

func nonEmptyShardLinkReason(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "无法执行 Master Shard 互联端点探测"
	}
	return value
}

func normalizeManualShardLinkCandidates(values []ShardLinkCandidateInput) ([]ShardLinkCandidateInput, error) {
	if len(values) > maximumManualShardLinkCandidates {
		return nil, &FieldError{Fields: map[string]string{"manualCandidates": "手工候选地址最多 16 个"}}
	}
	result := make([]ShardLinkCandidateInput, 0, len(values))
	fields := map[string]string{}
	for index, value := range values {
		value.Address = strings.TrimSpace(strings.Trim(value.Address, "[]"))
		value.Name = strings.TrimSpace(value.Name)
		path := fmt.Sprintf("manualCandidates.%d", index)
		if !validEndpointAddress(value.Address) {
			fields[path+".address"] = "候选地址必须是有效 IP 或主机名"
		}
		if value.Port < 0 || value.Port > 65535 {
			fields[path+".port"] = "候选端口必须在 1-65535 之间"
		}
		if len([]rune(value.Name)) > 100 {
			fields[path+".name"] = "候选名称不能超过 100 个字符"
		}
		result = append(result, value)
	}
	if len(fields) > 0 {
		return nil, &FieldError{Fields: fields}
	}
	return result, nil
}

func proposedMasterPlacement(placements []storedPlacement, worlds []rooms.World) (storedPlacement, rooms.World, bool) {
	roles := make(map[string]rooms.World, len(worlds))
	for _, world := range worlds {
		roles[world.ID] = world
	}
	var placement storedPlacement
	var master rooms.World
	count := 0
	for _, candidate := range placements {
		world := roles[candidate.WorldID]
		if world.Role == rooms.WorldRoleMaster || world.IsMaster {
			placement, master, count = candidate, world, count+1
		}
	}
	return placement, master, count == 1
}

func roomMasterPort(directory string, inventories []agents.RuntimeTargetInventory) (int, error) {
	port := 0
	for _, inventory := range inventories {
		for _, room := range inventory.Inventory.Rooms {
			if !strings.EqualFold(room.Directory, directory) || room.MasterPort <= 0 {
				continue
			}
			if port != 0 && port != room.MasterPort {
				return 0, &FieldError{Fields: map[string]string{"masterPort": "不同运行节点上报的 Master Shard 端口不一致"}}
			}
			port = room.MasterPort
		}
	}
	return port, nil
}

func (s *Service) shardLinkCandidates(ctx context.Context, master agents.RuntimeTarget, masterPort int, manual []ShardLinkCandidateInput) []ShardLinkCandidate {
	values := make([]ShardLinkCandidate, 0, len(master.IPAddresses)+len(manual)+2)
	for _, address := range master.IPAddresses {
		values = append(values, ShardLinkCandidate{Address: address, Port: masterPort, Kind: shardLinkAddressKind(address), Name: "Master 网卡地址", Status: "pending"})
	}
	if profile, ok := s.networkProfileForTarget(master.ID); ok && strings.TrimSpace(profile.AdvertiseAddress) != "" {
		values = append(values, ShardLinkCandidate{Address: profile.AdvertiseAddress, Port: masterPort, Kind: "configured", Name: "已配置地址", Status: "pending"})
	}
	if s.egress != nil {
		detectContext, cancel := context.WithTimeout(ctx, 5*time.Second)
		if detected, err := s.egress.DetectEgress(detectContext, master.ID, shared.RuntimeNetworkRegionGlobal); err == nil && strings.TrimSpace(detected.Address) != "" {
			values = append(values, ShardLinkCandidate{Address: detected.Address, Port: masterPort, Kind: "public", Name: "探测到的公网出口", Status: "pending"})
		}
		cancel()
	}
	for _, candidate := range manual {
		port := candidate.Port
		if port == 0 {
			port = masterPort
		}
		name := candidate.Name
		if name == "" {
			name = "手工候选地址"
		}
		values = append(values, ShardLinkCandidate{Address: candidate.Address, Port: port, Kind: "manual", Name: name, Status: "pending"})
	}
	return uniqueSortedShardLinkCandidates(values)
}

func (s *Service) networkProfileForTarget(targetID string) (NetworkProfile, bool) {
	_, environments, profiles, _, _, err := s.store.RuntimeResources()
	if err != nil {
		return NetworkProfile{}, false
	}
	profileByID := make(map[string]NetworkProfile, len(profiles))
	for _, profile := range profiles {
		profileByID[profile.ID] = profile
	}
	for _, environment := range environments {
		if environment.TargetID == targetID {
			profile, ok := profileByID[environment.NetworkProfileID]
			return profile, ok
		}
	}
	return NetworkProfile{}, false
}

func proposedShardLinks(placements []storedPlacement, worlds []rooms.World, targets map[string]agents.RuntimeTarget, master storedPlacement, candidates []ShardLinkCandidate) []ShardLinkDiscovery {
	worldByID := make(map[string]rooms.World, len(worlds))
	for _, world := range worlds {
		worldByID[world.ID] = world
	}
	seen := map[string]bool{}
	result := []ShardLinkDiscovery{}
	for _, placement := range placements {
		if placement.WorldID == master.WorldID || placement.DesiredTargetID == master.DesiredTargetID {
			continue
		}
		key := placement.DesiredTargetID + "\x00" + placement.DesiredInstallationID
		if seen[key] {
			continue
		}
		seen[key] = true
		target := targets[placement.DesiredTargetID]
		result = append(result, ShardLinkDiscovery{
			SourceTargetID: placement.DesiredTargetID, SourceInstallationID: placement.DesiredInstallationID,
			SourceTargetName: target.Name, Candidates: append([]ShardLinkCandidate(nil), candidates...),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].SourceTargetName != result[j].SourceTargetName {
			return result[i].SourceTargetName < result[j].SourceTargetName
		}
		return result[i].SourceTargetID < result[j].SourceTargetID
	})
	return result
}

func prepareShardLinkProbeCandidates(values []ShardLinkDiscovery) ([]struct {
	SourceTargetID       string
	SourceInstallationID string
	SourceTargetName     string
	Candidates           []shardLinkProbeCandidate
}, []string) {
	result := make([]struct {
		SourceTargetID       string
		SourceInstallationID string
		SourceTargetName     string
		Candidates           []shardLinkProbeCandidate
	}, len(values))
	tokens := make([]string, 0)
	for index, link := range values {
		result[index].SourceTargetID = link.SourceTargetID
		result[index].SourceInstallationID = link.SourceInstallationID
		result[index].SourceTargetName = link.SourceTargetName
		for _, candidate := range link.Candidates {
			token := strings.ReplaceAll(uuid.NewString(), "-", "")
			result[index].Candidates = append(result[index].Candidates, shardLinkProbeCandidate{ShardLinkCandidate: candidate, token: token})
			tokens = append(tokens, token)
		}
	}
	return result, tokens
}

func applyShardLinkProbeResults(link *struct {
	SourceTargetID       string
	SourceInstallationID string
	SourceTargetName     string
	Candidates           []shardLinkProbeCandidate
}, values []shared.RuntimeNetworkEndpointResult, probeErr error) {
	if probeErr != nil {
		for index := range link.Candidates {
			link.Candidates[index].Status = "unverified"
			link.Candidates[index].Error = probeErr.Error()
		}
		return
	}
	for index := range link.Candidates {
		candidate := &link.Candidates[index]
		candidate.Status, candidate.Error = "unreachable", "端点未返回有效探测响应"
		for _, value := range values {
			if value.Address != candidate.Address || value.Port != candidate.Port {
				continue
			}
			candidate.Reachable = value.Reachable
			candidate.LatencyMillis = value.LatencyMillis
			candidate.Error = value.Error
			if value.Reachable {
				candidate.Status, candidate.Error = "reachable", ""
			}
			break
		}
	}
}

func publicShardLinkDiscoveries(values []struct {
	SourceTargetID       string
	SourceInstallationID string
	SourceTargetName     string
	Candidates           []shardLinkProbeCandidate
}) []ShardLinkDiscovery {
	result := make([]ShardLinkDiscovery, 0, len(values))
	for _, link := range values {
		item := ShardLinkDiscovery{
			SourceTargetID: link.SourceTargetID, SourceInstallationID: link.SourceInstallationID,
			SourceTargetName: link.SourceTargetName, Candidates: make([]ShardLinkCandidate, 0, len(link.Candidates)),
		}
		for _, candidate := range link.Candidates {
			item.Candidates = append(item.Candidates, candidate.ShardLinkCandidate)
		}
		reachable := make([]ShardLinkCandidate, 0)
		for _, candidate := range item.Candidates {
			if candidate.Reachable {
				reachable = append(reachable, candidate)
			}
		}
		if len(reachable) == 1 {
			selected := reachable[0]
			item.AutoSelected = &selected
		}
		result = append(result, item)
	}
	return result
}

func markShardLinkCandidatesUnverified(values []ShardLinkDiscovery, reason string) {
	for linkIndex := range values {
		for candidateIndex := range values[linkIndex].Candidates {
			values[linkIndex].Candidates[candidateIndex].Status = "unverified"
			values[linkIndex].Candidates[candidateIndex].Error = reason
		}
	}
}

func markShardLinkCandidatesUnverifiedInternal(values []struct {
	SourceTargetID       string
	SourceInstallationID string
	SourceTargetName     string
	Candidates           []shardLinkProbeCandidate
}, reason string) {
	for linkIndex := range values {
		for candidateIndex := range values[linkIndex].Candidates {
			values[linkIndex].Candidates[candidateIndex].Status = "unverified"
			values[linkIndex].Candidates[candidateIndex].Error = reason
		}
	}
}

func uniqueSortedShardLinkCandidates(values []ShardLinkCandidate) []ShardLinkCandidate {
	seen := map[string]bool{}
	result := make([]ShardLinkCandidate, 0, len(values))
	for _, value := range values {
		value.Address = strings.TrimSpace(strings.Trim(value.Address, "[]"))
		if !validEndpointAddress(value.Address) || value.Port < 1 || value.Port > 65535 {
			continue
		}
		key := strings.ToLower(value.Address) + "\x00" + fmt.Sprintf("%d", value.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, value)
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := shardLinkKindPriority(result[i].Kind), shardLinkKindPriority(result[j].Kind)
		if left != right {
			return left < right
		}
		if result[i].Address != result[j].Address {
			return result[i].Address < result[j].Address
		}
		return result[i].Port < result[j].Port
	})
	return result
}

func shardLinkAddressKind(value string) string {
	address, err := netip.ParseAddr(strings.TrimSpace(strings.Trim(value, "[]")))
	if err != nil {
		return "interface"
	}
	address = address.Unmap()
	if net.ParseIP(address.String()).IsPrivate() {
		return "lan"
	}
	if netip.MustParsePrefix("100.64.0.0/10").Contains(address) {
		return "overlay"
	}
	if address.IsGlobalUnicast() {
		return "public"
	}
	return "interface"
}

func shardLinkKindPriority(value string) int {
	switch value {
	case "lan":
		return 0
	case "overlay":
		return 1
	case "configured":
		return 2
	case "manual":
		return 3
	case "public":
		return 4
	default:
		return 5
	}
}
