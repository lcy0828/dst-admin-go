package agents

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dont/internal/jobs"
	"dont/internal/runtimeinventory"
	"dont/shared"
)

const (
	defaultReservedPhysicalCores = 1
	lowCoreThreshold             = 2
	defaultShardMemoryEstimate   = 1400 * 1024 * 1024
	memoryWarningHeadroom        = 768 * 1024 * 1024
	memoryCriticalHeadroom       = 384 * 1024 * 1024
	metricsFreshnessWindow       = shared.SystemReportFreshnessWindow
	inventoryFreshnessWindow     = 90 * time.Second
	maximumReportedRooms         = 256
	maximumReportedShardsPerRoom = 64
	maximumReportedProcesses     = 512
)

func (s *Service) Inventory(agentID string) (InventorySnapshot, error) {
	agent, err := s.Agent(agentID)
	if err != nil {
		return InventorySnapshot{}, err
	}
	config, configErr := s.runtimeConfigForAgent(agent)
	var snapshot InventorySnapshot
	if configErr == nil {
		snapshot, err = s.store.InventoryForInstallation(agentID, config.InstallationID)
	} else {
		snapshot, err = s.store.Inventory(agentID)
	}
	if err != nil {
		return InventorySnapshot{}, err
	}
	snapshot.Stale, snapshot.StaleReason = s.inventoryFreshness(agent, snapshot)
	snapshot.Capacity = capacityForResources(
		snapshot.Inventory.CPU.LogicalProcessors,
		snapshot.Inventory.CPU.PhysicalCores,
		snapshot.Inventory.CPU.PhysicalCoreEstimated,
		len(snapshot.Inventory.Processes),
		snapshot.Stale,
		snapshot.Inventory.Memory.TotalBytes,
		snapshot.Inventory.Memory.AvailableBytes,
		0,
	)
	return snapshot, nil
}

// RuntimeTargetInventories returns one normalized read-only snapshot per
// target installation. Entries on the same machine intentionally share a
// Target ID; callers use Target.Config.InstallationID as the endpoint key.
func (s *Service) RuntimeTargetInventories(ctx context.Context) ([]RuntimeTargetInventory, error) {
	return s.runtimeTargetInventories(ctx, true)
}

// CachedRuntimeTargetInventories returns the last trusted inventory without
// touching the local filesystem or requesting a passive Agent report.
func (s *Service) CachedRuntimeTargetInventories(ctx context.Context) ([]RuntimeTargetInventory, error) {
	return s.runtimeTargetInventories(ctx, false)
}

func (s *Service) runtimeTargetInventories(ctx context.Context, collectLocal bool) ([]RuntimeTargetInventory, error) {
	targets, err := s.RuntimeTargets()
	if err != nil {
		return nil, err
	}
	items := make([]RuntimeTargetInventory, 0, len(targets))
	for _, target := range targets {
		if target.ID == "local" {
			configs := s.localRuntimeConfigs()
			if len(configs) == 0 {
				items = append(items, RuntimeTargetInventory{Target: target, Capacity: Capacity{State: CapacityUnknown}, Stale: true, StaleReason: "runtime_not_configured"})
				continue
			}
			for _, config := range configs {
				installationTarget := runtimeTargetForConfig(target, config)
				item := RuntimeTargetInventory{Target: installationTarget, Capacity: Capacity{State: CapacityUnknown}, Stale: true}
				if !collectLocal {
					item.StaleReason = "inventory_missing"
					items = append(items, item)
					continue
				}
				report, collectErr := runtimeinventory.Collect(ctx, shared.RuntimeInventoryRequest{
					InstallationID: config.InstallationID, DisplayName: target.Name,
					SavePath: config.SavePath, ServerPath: config.ServerPath, ServerMode: config.ServerMode,
				})
				if collectErr != nil {
					item.StaleReason = "collection_failed"
					items = append(items, item)
					continue
				}
				if s.localProcesses != nil && config.InstallationID == target.Config.InstallationID {
					processes, processErr := s.localProcesses.ContainerProcesses(ctx)
					if processErr != nil {
						report.Warnings = append(report.Warnings, "无法读取本机容器分片状态: "+processErr.Error())
					} else {
						report.Processes = processes
					}
				}
				report, collectErr = normalizeInventory(report, config)
				if collectErr != nil {
					item.StaleReason = "invalid_report"
					items = append(items, item)
					continue
				}
				observedAt := report.ObservedAt.UTC()
				item.Available, item.Stale = true, false
				item.Inventory = report
				item.ObservedAt, item.ReceivedAt = &observedAt, &observedAt
				item.Capacity = capacityForResources(
					report.CPU.LogicalProcessors, report.CPU.PhysicalCores, report.CPU.PhysicalCoreEstimated,
					len(report.Processes), false, report.Memory.TotalBytes, report.Memory.AvailableBytes, 0,
				)
				items = append(items, item)
			}
			continue
		}
		item := RuntimeTargetInventory{Target: target, Capacity: Capacity{State: CapacityUnknown}, Stale: true}
		if !target.Configured {
			item.StaleReason = "runtime_not_configured"
			items = append(items, item)
			continue
		}
		configs := runtimeConfigsForTarget(target)
		for _, config := range configs {
			installationTarget := runtimeTargetForConfig(target, config)
			item = RuntimeTargetInventory{Target: installationTarget, Capacity: Capacity{State: CapacityUnknown}, Stale: true}
			snapshot, snapshotErr := s.store.InventoryForInstallation(target.AgentID, config.InstallationID)
			if errors.Is(snapshotErr, ErrInventoryNotFound) {
				item.StaleReason = "inventory_missing"
				items = append(items, item)
				continue
			}
			if snapshotErr != nil {
				return nil, snapshotErr
			}
			snapshot.Stale, snapshot.StaleReason = s.inventoryFreshness(targetAgent(target), snapshot)
			observedAt, receivedAt := snapshot.ObservedAt.UTC(), snapshot.ReceivedAt.UTC()
			item.Available, item.Inventory = true, snapshot.Inventory
			item.Capacity = capacityForResources(
				snapshot.Inventory.CPU.LogicalProcessors, snapshot.Inventory.CPU.PhysicalCores,
				snapshot.Inventory.CPU.PhysicalCoreEstimated, len(snapshot.Inventory.Processes), snapshot.Stale,
				snapshot.Inventory.Memory.TotalBytes, snapshot.Inventory.Memory.AvailableBytes, 0,
			)
			item.ObservedAt, item.ReceivedAt = &observedAt, &receivedAt
			item.Stale, item.StaleReason = snapshot.Stale, snapshot.StaleReason
			items = append(items, item)
		}
	}
	return items, nil
}

// CollectRuntimeTargetInventory actively observes exactly one configured DST
// installation. The result is kept in memory by the observation coordinator;
// callers choose separately when to checkpoint it to SQLite.
func (s *Service) CollectRuntimeTargetInventory(ctx context.Context, targetID, installationID string) (RuntimeTargetInventory, error) {
	targetID, installationID = strings.TrimSpace(targetID), strings.TrimSpace(installationID)
	targets, err := s.RuntimeTargets()
	if err != nil {
		return RuntimeTargetInventory{}, err
	}
	var target RuntimeTarget
	found := false
	for _, candidate := range targets {
		if candidate.ID == targetID {
			target, found = candidate, true
			break
		}
	}
	if !found {
		return RuntimeTargetInventory{}, ErrRuntimeTargetNotFound
	}
	if !target.Configured {
		return RuntimeTargetInventory{}, ErrRuntimeNotConfigured
	}
	var config RuntimeConfig
	for _, candidate := range runtimeConfigsForTarget(target) {
		if installationID == "" || candidate.InstallationID == installationID {
			config = candidate
			break
		}
	}
	if config.InstallationID == "" {
		return RuntimeTargetInventory{}, ErrRuntimeNotConfigured
	}
	target = runtimeTargetForConfig(target, config)
	item := RuntimeTargetInventory{Target: target, Capacity: Capacity{State: CapacityUnknown}, Stale: true}
	var report shared.RuntimeInventoryReport
	if target.ID == "local" {
		report, err = s.collectLocalInventory(ctx, target, config)
	} else {
		if !target.Online {
			return item, ErrAgentOffline
		}
		if !containsString(target.Capabilities, "runtime.inventory.read") {
			return item, ErrUnsupportedAction
		}
		agent, agentErr := s.Agent(target.AgentID)
		if agentErr != nil {
			return item, agentErr
		}
		report, err = s.collectRemoteInventory(ctx, agent, config)
	}
	if err != nil {
		return item, err
	}
	now := s.now().UTC()
	observedAt := report.ObservedAt.UTC()
	item.Available, item.Stale, item.Inventory = true, false, report
	item.ObservedAt, item.ReceivedAt = &observedAt, &now
	item.Capacity = capacityForResources(
		report.CPU.LogicalProcessors, report.CPU.PhysicalCores, report.CPU.PhysicalCoreEstimated,
		len(report.Processes), false, report.Memory.TotalBytes, report.Memory.AvailableBytes, 0,
	)
	return item, nil
}

func (s *Service) collectLocalInventory(ctx context.Context, target RuntimeTarget, config RuntimeConfig) (shared.RuntimeInventoryReport, error) {
	report, err := runtimeinventory.Collect(ctx, shared.RuntimeInventoryRequest{
		InstallationID: config.InstallationID, DisplayName: target.Name,
		SavePath: config.SavePath, ServerPath: config.ServerPath, ServerMode: config.ServerMode,
	})
	if err != nil {
		return shared.RuntimeInventoryReport{}, err
	}
	if s.localProcesses != nil && config.InstallationID == target.Config.InstallationID {
		processes, processErr := s.localProcesses.ContainerProcesses(ctx)
		if processErr != nil {
			report.Warnings = append(report.Warnings, "无法读取本机容器分片状态: "+processErr.Error())
		} else {
			report.Processes = processes
		}
	}
	return normalizeInventory(report, config)
}

func (s *Service) collectRemoteInventory(ctx context.Context, agent Agent, config RuntimeConfig) (shared.RuntimeInventoryReport, error) {
	report, err := s.transport.Inventory(ctx, agent.ID, config, 20)
	if err != nil {
		return shared.RuntimeInventoryReport{}, err
	}
	return normalizeInventory(report, config)
}

// CheckpointRuntimeTargetInventory persists a remote hot snapshot. Local
// snapshots are intentionally memory-only because they can be reconstructed.
func (s *Service) CheckpointRuntimeTargetInventory(targetID string, report shared.RuntimeInventoryReport) error {
	targetID = strings.TrimSpace(targetID)
	if targetID == "local" {
		return nil
	}
	if !strings.HasPrefix(targetID, "agent:") {
		return ErrRuntimeTargetNotFound
	}
	_, err := s.store.SaveInventory(strings.TrimPrefix(targetID, "agent:"), report)
	if err == nil {
		s.emitRuntimeTopologyChanged()
	}
	return err
}

func (s *Service) localRuntimeConfigs() []RuntimeConfig {
	s.localMu.RLock()
	items := make([]RuntimeConfig, 0, len(s.localInstallations))
	for _, config := range s.localInstallations {
		items = append(items, config)
	}
	s.localMu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].InstallationID < items[j].InstallationID })
	return items
}

func runtimeConfigsForTarget(target RuntimeTarget) []RuntimeConfig {
	if !target.Configured {
		return nil
	}
	if len(target.Installations) <= 1 {
		return []RuntimeConfig{target.Config}
	}
	items := make([]RuntimeConfig, 0, len(target.Installations))
	for _, installation := range target.Installations {
		config := target.Config
		config.InstallationID = installation.ID
		config.SavePath = installation.SavePath
		config.ServerPath = installation.ServerPath
		config.SteamCMDPath = installation.SteamCMDPath
		config.UGCPath = installation.UGCPath
		config.WorkshopContentPath = installation.WorkshopContentPath
		config.ServerMode = installation.ServerMode
		items = append(items, normalizeRuntimeConfig(config))
	}
	return items
}

func runtimeTargetForConfig(target RuntimeTarget, config RuntimeConfig) RuntimeTarget {
	target.Config = config
	for _, installation := range target.Installations {
		if installation.ID == config.InstallationID {
			target.Performance = cloneRuntimePerformance(installation.Performance)
			break
		}
	}
	return target
}

func targetAgent(target RuntimeTarget) Agent {
	status := StatusOffline
	if target.Online {
		status = StatusOnline
	}
	lastHeartbeat := time.Time{}
	if target.LastHeartbeat != nil {
		lastHeartbeat = target.LastHeartbeat.UTC()
	}
	return Agent{ID: target.AgentID, Status: status, LastHeartbeat: lastHeartbeat}
}

func (s *Service) RefreshInventory(agentID string) (jobs.Job, error) {
	agent, err := s.Agent(agentID)
	if err != nil {
		return jobs.Job{}, err
	}
	if agent.Status != StatusOnline {
		return jobs.Job{}, ErrAgentOffline
	}
	if !containsString(agent.Capabilities, "runtime.inventory.read") {
		return jobs.Job{}, ErrUnsupportedAction
	}
	config, err := s.runtimeConfigForAgent(agent)
	if err != nil {
		return jobs.Job{}, err
	}
	target := runtimeTargetFromAgent(s.decorateAgent(agent), config, true)
	return s.submitInventoryRefresh(agent, runtimeConfigsForTarget(target))
}

func (s *Service) submitInventoryRefresh(agent Agent, configs []RuntimeConfig) (jobs.Job, error) {
	if len(configs) == 0 {
		return jobs.Job{}, ErrRuntimeNotConfigured
	}
	return s.jobs.SubmitFactory("agent.inventory", "", "", []jobs.TargetSpec{{ID: agent.ID, Name: agent.Hostname}}, func(jobs.Job) jobs.Runner {
		return func(jobContext context.Context, reportResult func(jobs.TargetResult)) error {
			for _, config := range configs {
				requestContext, cancel := context.WithTimeout(jobContext, 30*time.Second)
				_, refreshErr := s.observeInventory(requestContext, agent, config)
				cancel()
				if refreshErr == nil {
					continue
				}
				code := "AGENT_INVENTORY_FAILED"
				if errors.Is(refreshErr, context.DeadlineExceeded) {
					code = "AGENT_INVENTORY_TIMEOUT"
				} else if errors.Is(refreshErr, ErrAgentOffline) {
					code = "AGENT_OFFLINE"
				}
				reportResult(jobs.TargetResult{TargetID: agent.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: code, Message: refreshErr.Error()}})
				return refreshErr
			}
			reportResult(jobs.TargetResult{TargetID: agent.ID, Status: jobs.StatusSucceeded, Message: fmt.Sprintf("节点 %d 个运行时清单已刷新", len(configs))})
			return nil
		}
	})
}

func (s *Service) observeInventory(ctx context.Context, agent Agent, config RuntimeConfig) (InventorySnapshot, error) {
	report, err := s.collectRemoteInventory(ctx, agent, config)
	if err != nil {
		return InventorySnapshot{}, err
	}
	snapshot, err := s.store.SaveInventory(agent.ID, report)
	if err != nil {
		return InventorySnapshot{}, err
	}
	s.emitRuntimeTopologyChanged()
	return snapshot, nil
}

func (s *Service) WatchInventories(ctx context.Context, interval time.Duration, notify func()) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	select {
	case <-s.inventoryWake:
	default:
	}
	s.refreshConfiguredInventories(ctx, notify)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.inventoryWake:
			s.refreshConfiguredInventories(ctx, notify)
		case <-ticker.C:
			s.refreshConfiguredInventories(ctx, notify)
		}
	}
}

func (s *Service) refreshConfiguredInventories(ctx context.Context, notify func()) {
	_, _ = s.Sync()
	agents, err := s.store.Agents()
	if err != nil {
		return
	}
	configs, err := s.store.RuntimeConfigs()
	if err != nil {
		return
	}
	semaphore := make(chan struct{}, 4)
	var changed atomic.Bool
	var waitGroup sync.WaitGroup
	for _, item := range agents {
		config, configured := configs[item.ID]
		if !configured || item.Status != StatusOnline || !containsString(item.Capabilities, "runtime.inventory.read") {
			continue
		}
		item = s.decorateAgent(item)
		var bindErr error
		config, bindErr = bindAdvertisedRuntimeInstallation(item, config)
		if bindErr != nil {
			continue
		}
		target := runtimeTargetFromAgent(item, config, true)
		configs := runtimeConfigsForTarget(target)
		item := item
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			for _, installationConfig := range configs {
				requestContext, cancel := context.WithTimeout(ctx, 30*time.Second)
				_, refreshErr := s.observeInventory(requestContext, item, installationConfig)
				cancel()
				if refreshErr == nil {
					changed.Store(true)
				}
			}
		}()
	}
	waitGroup.Wait()
	if changed.Load() && notify != nil {
		notify()
	}
}

func (s *Service) decorateAgent(agent Agent) Agent {
	var latest *AgentRelease
	if s.releases != nil {
		latest, _ = s.releases.Latest(agent.OS, agent.Arch)
	}
	return s.decorateAgentWithRelease(agent, latest)
}

func (s *Service) decorateAgentWithRelease(agent Agent, latest *AgentRelease) Agent {
	agent.InstallationRegistrySupported = runtimeInstallationRegistrySupported(agent.Details)
	agent.Installations = advertisedRuntimeInstallations(agent.Details)
	observedAt := agent.Metrics.ObservedAt
	if observedAt == nil {
		observedAt = agent.LastReportAt
	}
	agent.MetricsStale, agent.StaleReason = observationStaleness(s.now().UTC(), agent.Status, observedAt, metricsFreshnessWindow)
	agent.Capacity = capacityForResources(
		agent.Metrics.LogicalProcessors,
		agent.Metrics.PhysicalCores,
		agent.Metrics.PhysicalCoreEstimated,
		agent.Metrics.RunningShardCount,
		agent.MetricsStale,
		uint64(maxInt64(agent.Metrics.MemoryTotal)),
		uint64(maxInt64(agent.Metrics.MemoryAvailable)),
		0,
	)
	if snapshots, err := s.store.Inventories(agent.ID); err == nil && len(snapshots) > 0 {
		combined := combinedInventorySnapshots(snapshots)
		inventoryStale, _ := s.inventoryFreshness(agent, combined)
		agent.Capacity = capacityForResources(
			combined.Inventory.CPU.LogicalProcessors,
			combined.Inventory.CPU.PhysicalCores,
			combined.Inventory.CPU.PhysicalCoreEstimated,
			len(combined.Inventory.Processes),
			inventoryStale,
			combined.Inventory.Memory.TotalBytes,
			combined.Inventory.Memory.AvailableBytes,
			0,
		)
	}
	agent.Update = s.agentUpdateStatus(agent, latest)
	return agent
}

func (s *Service) agentUpdateStatus(agent Agent, latest *AgentRelease) AgentUpdateStatus {
	status := AgentUpdateStatus{CurrentVersion: agent.Version, Mode: AgentUpdateModeMigration, Reason: "manual_migration_required"}
	deployment := strings.ToLower(strings.TrimSpace(stringValue(agent.Details["deployment_profile"])))
	switch {
	case strings.EqualFold(agent.OS, "windows"):
		status.Mode, status.Reason = AgentUpdateModeUnsupported, "windows_service_helper_required"
	case deployment == "container":
		status.Mode, status.Reason = AgentUpdateModeContainer, "container_image_required"
	case containsString(agent.Capabilities, shared.AgentUpgradeCommand):
		status.Mode, status.Supported, status.Reason = AgentUpdateModeSelf, true, ""
	}
	if latest == nil {
		return status
	}
	status.Release = latest
	status.LatestVersion = latest.Version
	status.UpdateAvailable = compareAgentVersions(latest.Version, agent.Version) > 0
	return status
}

func (s *Service) latestAgentReleasesByPlatform() map[string]*AgentRelease {
	result := make(map[string]*AgentRelease)
	if s.releases == nil {
		return result
	}
	items, err := s.releases.List()
	if err != nil {
		return result
	}
	for index := range items {
		key := agentReleasePlatformKey(items[index].OS, items[index].Arch)
		if _, exists := result[key]; !exists {
			result[key] = &items[index]
		}
	}
	return result
}

func agentReleasePlatformKey(goos, goarch string) string {
	return strings.ToLower(strings.TrimSpace(goos)) + "/" + strings.ToLower(strings.TrimSpace(goarch))
}

func combinedInventorySnapshots(values []InventorySnapshot) InventorySnapshot {
	if len(values) == 0 {
		return InventorySnapshot{}
	}
	result := values[0]
	processes := make(map[string]shared.ShardProcessReport)
	for _, value := range values {
		if value.ReceivedAt.After(result.ReceivedAt) {
			result.ObservedAt, result.ReceivedAt = value.ObservedAt, value.ReceivedAt
			result.Inventory.CPU, result.Inventory.Memory = value.Inventory.CPU, value.Inventory.Memory
		}
		for _, process := range value.Inventory.Processes {
			key := fmt.Sprintf("%s\x00%d\x00%s", process.RuntimeKind, process.PID, process.InstanceID)
			processes[key] = process
		}
	}
	result.InstallationID = ""
	result.Inventory.Processes = make([]shared.ShardProcessReport, 0, len(processes))
	for _, process := range processes {
		result.Inventory.Processes = append(result.Inventory.Processes, process)
	}
	sort.Slice(result.Inventory.Processes, func(i, j int) bool {
		if result.Inventory.Processes[i].PID == result.Inventory.Processes[j].PID {
			return result.Inventory.Processes[i].InstanceID < result.Inventory.Processes[j].InstanceID
		}
		return result.Inventory.Processes[i].PID < result.Inventory.Processes[j].PID
	})
	return result
}

func (s *Service) inventoryFreshness(agent Agent, snapshot InventorySnapshot) (bool, string) {
	if agent.Status != StatusOnline {
		return true, "agent_offline"
	}
	if snapshot.ObservedAt.After(snapshot.ReceivedAt.Add(2*time.Minute)) || snapshot.ObservedAt.Before(snapshot.ReceivedAt.Add(-10*time.Minute)) {
		return true, "clock_skew"
	}
	if s.now().UTC().Sub(snapshot.ReceivedAt) > inventoryFreshnessWindow {
		return true, "report_expired"
	}
	return false, ""
}

func observationStaleness(now time.Time, status Status, observedAt *time.Time, window time.Duration) (bool, string) {
	if status != StatusOnline {
		return true, "agent_offline"
	}
	if observedAt == nil || observedAt.IsZero() {
		return true, "report_missing"
	}
	if observedAt.After(now.Add(2 * time.Minute)) {
		return true, "clock_skew"
	}
	if now.Sub(observedAt.UTC()) > window {
		return true, "report_expired"
	}
	return false, ""
}

func capacityFor(logicalProcessors, physicalCores int, estimated bool, runningShards int, stale bool) Capacity {
	capacity := Capacity{
		State: CapacityUnknown, LogicalProcessors: logicalProcessors, PhysicalCores: physicalCores,
		PhysicalCoreEstimated: estimated, ReservedPhysicalCores: defaultReservedPhysicalCores,
		RunningShards: runningShards,
		MemoryState:   MemoryCapacityUnknown,
	}
	if stale || logicalProcessors < 1 {
		capacity.Message = "节点容量数据已过期，无法判断可安全启动的世界数量"
		return capacity
	}
	if physicalCores < 1 {
		physicalCores = logicalProcessors / 2
		if physicalCores < 1 {
			physicalCores = 1
		}
		capacity.PhysicalCores = physicalCores
		capacity.PhysicalCoreEstimated = true
	}
	budgetCores := physicalCores
	// Small VPS plans are sold and scheduled as vCPUs. Treat up to four
	// schedulable CPUs as the practical Shard budget even when the host exposes
	// them as SMT siblings; larger hosts keep the conservative physical-core
	// budget unless their topology is estimated from an effective cgroup limit.
	if logicalProcessors <= 4 || estimated {
		budgetCores = logicalProcessors
	}
	reserved := defaultReservedPhysicalCores
	if budgetCores <= lowCoreThreshold {
		reserved = 0
	}
	capacity.ReservedPhysicalCores = reserved
	limit := budgetCores - reserved
	if limit < 1 {
		limit = 1
	}
	capacity.RecommendedShardLimit = limit
	capacity.AvailableSlots = limit - runningShards
	if capacity.AvailableSlots < 0 {
		capacity.AvailableSlots = 0
	}
	switch {
	case runningShards > limit:
		capacity.State = CapacityOvercommitted
		capacity.Message = fmt.Sprintf("当前运行 %d 个世界分片，已超过建议上限 %d；一核心承载多层世界可能造成卡顿", runningShards, limit)
	case runningShards == limit:
		capacity.State = CapacityFull
		capacity.Message = fmt.Sprintf("当前运行 %d 个世界分片，已达到建议上限 %d", runningShards, limit)
	default:
		capacity.State = CapacityAvailable
		capacity.Message = fmt.Sprintf("当前运行 %d 个世界分片，建议上限 %d", runningShards, limit)
	}
	return capacity
}

func capacityForResources(
	logicalProcessors, physicalCores int,
	estimated bool,
	runningShards int,
	stale bool,
	memoryTotal, memoryAvailable uint64,
	additionalShards int,
) Capacity {
	capacity := capacityFor(logicalProcessors, physicalCores, estimated, runningShards, stale)
	capacity.MemoryTotalBytes = memoryTotal
	capacity.MemoryAvailableBytes = memoryAvailable
	if stale || memoryTotal == 0 {
		return capacity
	}
	if additionalShards < 0 {
		additionalShards = 0
	}
	capacity.EstimatedAdditionalMemoryBytes = uint64(additionalShards) * defaultShardMemoryEstimate
	if capacity.EstimatedAdditionalMemoryBytes >= memoryAvailable {
		capacity.ProjectedMemoryAvailableBytes = 0
	} else {
		capacity.ProjectedMemoryAvailableBytes = memoryAvailable - capacity.EstimatedAdditionalMemoryBytes
	}
	switch {
	case capacity.EstimatedAdditionalMemoryBytes > memoryAvailable || capacity.ProjectedMemoryAvailableBytes < memoryCriticalHeadroom:
		capacity.MemoryState = MemoryCapacityCritical
		capacity.Message += fmt.Sprintf("；预计启动后可用内存不足 %d MiB", memoryCriticalHeadroom/(1024*1024))
	case capacity.ProjectedMemoryAvailableBytes < memoryWarningHeadroom:
		capacity.MemoryState = MemoryCapacityTight
		capacity.Message += fmt.Sprintf("；预计启动后可用内存低于 %d MiB", memoryWarningHeadroom/(1024*1024))
	default:
		capacity.MemoryState = MemoryCapacityHealthy
	}
	return capacity
}

func maxInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func CapacityFor(logicalProcessors, physicalCores int, estimated bool, runningShards int, stale bool) Capacity {
	return capacityFor(logicalProcessors, physicalCores, estimated, runningShards, stale)
}

func CapacityForResources(
	logicalProcessors, physicalCores int,
	estimated bool,
	runningShards int,
	stale bool,
	memoryTotal, memoryAvailable uint64,
	additionalShards int,
) Capacity {
	return capacityForResources(
		logicalProcessors, physicalCores, estimated, runningShards, stale,
		memoryTotal, memoryAvailable, additionalShards,
	)
}

func normalizeInventory(report shared.RuntimeInventoryReport, config RuntimeConfig) (shared.RuntimeInventoryReport, error) {
	if report.ProtocolVersion != shared.RuntimeInventoryProtocolVersion || report.ObservedAt.IsZero() ||
		report.CPU.LogicalProcessors < 1 || report.CPU.PhysicalCores < 0 || report.CPU.PhysicalCores > report.CPU.LogicalProcessors ||
		len(report.CPU.Threads) > report.CPU.LogicalProcessors ||
		len(report.Rooms) > maximumReportedRooms || len(report.Processes) > maximumReportedProcesses {
		return shared.RuntimeInventoryReport{}, errors.New("Agent 返回的 DST 运行时清单不符合协议")
	}
	seenLogicalCPUs := make(map[int]bool, len(report.CPU.Threads))
	for _, thread := range report.CPU.Threads {
		if thread.LogicalID < 0 || thread.LogicalID >= report.CPU.LogicalProcessors || seenLogicalCPUs[thread.LogicalID] ||
			strings.TrimSpace(thread.PackageID) == "" || strings.TrimSpace(thread.CoreID) == "" ||
			len(thread.PackageID) > 64 || len(thread.CoreID) > 64 {
			return shared.RuntimeInventoryReport{}, errors.New("Agent 返回的 CPU 拓扑不符合协议")
		}
		seenLogicalCPUs[thread.LogicalID] = true
	}
	if report.CPU.TopologyAvailable && len(report.CPU.Threads) != report.CPU.LogicalProcessors {
		return shared.RuntimeInventoryReport{}, errors.New("Agent 返回的 CPU 拓扑不完整")
	}
	reportedSavePath := strings.TrimSpace(report.Installation.SavePath)
	reportedServerPath := strings.TrimSpace(report.Installation.ServerPath)
	if !sameRuntimePath(reportedSavePath, config.SavePath) || !sameRuntimePath(reportedServerPath, config.ServerPath) {
		return shared.RuntimeInventoryReport{}, errors.New("Agent 返回的运行时路径与已配置安装不一致")
	}
	report.Installation.SavePath = cleanRuntimePath(reportedSavePath)
	report.Installation.ServerPath = cleanRuntimePath(reportedServerPath)
	report.Installation.ID = trimLimit(report.Installation.ID, 128)
	report.Installation.DisplayName = trimLimit(report.Installation.DisplayName, 100)
	report.Installation.SavePath = trimLimit(report.Installation.SavePath, 2048)
	report.Installation.ServerPath = trimLimit(report.Installation.ServerPath, 2048)
	report.Installation.ServerMode = trimLimit(report.Installation.ServerMode, 16)
	report.CPU.PhysicalCoreSource = trimLimit(report.CPU.PhysicalCoreSource, 64)
	report.Warnings = cleanStrings(report.Warnings, 64, 240)
	seenProcesses := make(map[string]bool)
	processes := make([]shared.ShardProcessReport, 0, len(report.Processes))
	for _, process := range report.Processes {
		process.RuntimeKind = strings.ToLower(trimLimit(process.RuntimeKind, 24))
		process.InstanceID = trimLimit(process.InstanceID, 128)
		process.Executable = trimLimit(process.Executable, 2048)
		if !reportedShardProcess(process) {
			continue
		}
		identity := fmt.Sprintf("native:%d", process.PID)
		if process.RuntimeKind == "container" {
			identity = "container:" + process.InstanceID
		}
		if process.PID <= 0 || process.RuntimeKind == "container" && process.InstanceID == "" || seenProcesses[identity] {
			continue
		}
		seenProcesses[identity] = true
		process.Cluster = trimLimit(process.Cluster, 255)
		process.Shard = trimLimit(process.Shard, 255)
		process.StorageRoot = trimLimit(process.StorageRoot, 2048)
		process.ConfigDirectory = trimLimit(process.ConfigDirectory, 2048)
		processes = append(processes, process)
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	report.Processes = processes
	for roomIndex := range report.Rooms {
		room := &report.Rooms[roomIndex]
		if len(room.Shards) > maximumReportedShardsPerRoom {
			return shared.RuntimeInventoryReport{}, errors.New("Agent 返回的房间分片数量超过协议上限")
		}
		room.Directory = trimLimit(room.Directory, 255)
		room.Name = trimLimit(room.Name, 255)
		room.ConfigPath = trimLimit(room.ConfigPath, 2048)
		for shardIndex := range room.Shards {
			shard := &room.Shards[shardIndex]
			shard.Directory = trimLimit(shard.Directory, 255)
			shard.Name = trimLimit(shard.Name, 255)
			shard.Role = trimLimit(shard.Role, 32)
			shard.ConfigPath = trimLimit(shard.ConfigPath, 2048)
		}
		sort.Slice(room.Shards, func(i, j int) bool { return room.Shards[i].Directory < room.Shards[j].Directory })
	}
	sort.Slice(report.Rooms, func(i, j int) bool { return report.Rooms[i].Directory < report.Rooms[j].Directory })
	if report.Rooms == nil {
		report.Rooms = []shared.RoomInventoryReport{}
	}
	if report.Processes == nil {
		report.Processes = []shared.ShardProcessReport{}
	}
	if report.Warnings == nil {
		report.Warnings = []string{}
	}
	return report, nil
}

func reportedShardProcess(process shared.ShardProcessReport) bool {
	if strings.EqualFold(strings.TrimSpace(process.RuntimeKind), "container") {
		return true
	}
	executable := strings.TrimSpace(process.Executable)
	return executable == "" || runtimeinventory.IsDSTExecutable(executable)
}

func reportedShardProcesses(values []shared.ShardProcessReport) []shared.ShardProcessReport {
	result := make([]shared.ShardProcessReport, 0, len(values))
	for _, process := range values {
		if reportedShardProcess(process) {
			result = append(result, process)
		}
	}
	if result == nil {
		return []shared.ShardProcessReport{}
	}
	return result
}

func sameRuntimePath(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	leftWindows, rightWindows := windowsAbsolutePathPattern.MatchString(left), windowsAbsolutePathPattern.MatchString(right)
	if leftWindows || rightWindows {
		return leftWindows && rightWindows && strings.EqualFold(cleanWindowsRuntimePath(left), cleanWindowsRuntimePath(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func cleanRuntimePath(value string) string {
	value = strings.TrimSpace(value)
	if windowsAbsolutePathPattern.MatchString(value) {
		return cleanWindowsRuntimePath(value)
	}
	return filepath.Clean(value)
}

func cleanWindowsRuntimePath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "/", `\`)
	// Keep a drive root such as C:\ intact while removing redundant trailing
	// separators from regular paths. The Agent already resolves dot segments.
	if len(value) > 3 {
		value = strings.TrimRight(value, `\`)
	}
	return value
}
