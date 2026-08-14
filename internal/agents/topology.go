package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dont/internal/jobs"
	"dont/shared"
)

const (
	defaultReservedPhysicalCores = 1
	metricsFreshnessWindow       = 90 * time.Second
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
	snapshot, err := s.store.Inventory(agentID)
	if err != nil {
		return InventorySnapshot{}, err
	}
	snapshot.Stale, snapshot.StaleReason = s.inventoryFreshness(agent, snapshot)
	snapshot.Capacity = capacityFor(
		snapshot.Inventory.CPU.LogicalProcessors,
		snapshot.Inventory.CPU.PhysicalCores,
		snapshot.Inventory.CPU.PhysicalCoreEstimated,
		len(snapshot.Inventory.Processes),
		snapshot.Stale,
	)
	return snapshot, nil
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
	config, err := s.store.RuntimeConfig(agentID)
	if err != nil {
		return jobs.Job{}, err
	}
	return s.jobs.SubmitFactory("agent.inventory", "", "", []jobs.TargetSpec{{ID: agent.ID, Name: agent.Hostname}}, func(jobs.Job) jobs.Runner {
		return func(jobContext context.Context, reportResult func(jobs.TargetResult)) error {
			requestContext, cancel := context.WithTimeout(jobContext, 30*time.Second)
			defer cancel()
			_, refreshErr := s.observeInventory(requestContext, agent, config)
			if refreshErr != nil {
				code := "AGENT_INVENTORY_FAILED"
				if errors.Is(refreshErr, context.DeadlineExceeded) {
					code = "AGENT_INVENTORY_TIMEOUT"
				} else if errors.Is(refreshErr, ErrAgentOffline) {
					code = "AGENT_OFFLINE"
				}
				reportResult(jobs.TargetResult{TargetID: agent.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: code, Message: refreshErr.Error()}})
				return refreshErr
			}
			reportResult(jobs.TargetResult{TargetID: agent.ID, Status: jobs.StatusSucceeded, Message: "节点运行时清单已刷新"})
			return nil
		}
	})
}

func (s *Service) observeInventory(ctx context.Context, agent Agent, config RuntimeConfig) (InventorySnapshot, error) {
	report, err := s.transport.Inventory(ctx, agent.ID, config, 30)
	if err != nil {
		return InventorySnapshot{}, err
	}
	report, err = normalizeInventory(report, config)
	if err != nil {
		return InventorySnapshot{}, err
	}
	return s.store.SaveInventory(agent.ID, report)
}

func (s *Service) WatchInventories(ctx context.Context, interval time.Duration, notify func()) {
	if interval < 30*time.Second {
		interval = 60 * time.Second
	}
	s.refreshConfiguredInventories(ctx, notify)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
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
			requestContext, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if _, refreshErr := s.observeInventory(requestContext, item, config); refreshErr == nil {
				changed.Store(true)
			}
		}()
	}
	waitGroup.Wait()
	if changed.Load() && notify != nil {
		notify()
	}
}

func (s *Service) decorateAgent(agent Agent) Agent {
	observedAt := agent.Metrics.ObservedAt
	if observedAt == nil {
		observedAt = agent.LastReportAt
	}
	agent.MetricsStale, agent.StaleReason = observationStaleness(s.now().UTC(), agent.Status, observedAt, metricsFreshnessWindow)
	agent.Capacity = capacityFor(
		agent.Metrics.LogicalProcessors,
		agent.Metrics.PhysicalCores,
		agent.Metrics.PhysicalCoreEstimated,
		agent.Metrics.RunningShardCount,
		agent.MetricsStale,
	)
	if snapshot, err := s.store.Inventory(agent.ID); err == nil {
		inventoryStale, _ := s.inventoryFreshness(agent, snapshot)
		agent.Capacity = capacityFor(
			snapshot.Inventory.CPU.LogicalProcessors,
			snapshot.Inventory.CPU.PhysicalCores,
			snapshot.Inventory.CPU.PhysicalCoreEstimated,
			len(snapshot.Inventory.Processes),
			inventoryStale,
		)
	}
	return agent
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
	limit := physicalCores - defaultReservedPhysicalCores
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

func normalizeInventory(report shared.RuntimeInventoryReport, config RuntimeConfig) (shared.RuntimeInventoryReport, error) {
	if report.ProtocolVersion != shared.RuntimeInventoryProtocolVersion || report.ObservedAt.IsZero() ||
		report.CPU.LogicalProcessors < 1 || report.CPU.PhysicalCores < 0 || report.CPU.PhysicalCores > report.CPU.LogicalProcessors ||
		len(report.Rooms) > maximumReportedRooms || len(report.Processes) > maximumReportedProcesses {
		return shared.RuntimeInventoryReport{}, errors.New("Agent 返回的 DST 运行时清单不符合协议")
	}
	if strings.TrimSpace(report.Installation.SavePath) != config.SavePath || strings.TrimSpace(report.Installation.ServerPath) != config.ServerPath {
		return shared.RuntimeInventoryReport{}, errors.New("Agent 返回的运行时路径与已配置安装不一致")
	}
	report.Installation.ID = trimLimit(report.Installation.ID, 128)
	report.Installation.DisplayName = trimLimit(report.Installation.DisplayName, 100)
	report.Installation.SavePath = trimLimit(report.Installation.SavePath, 2048)
	report.Installation.ServerPath = trimLimit(report.Installation.ServerPath, 2048)
	report.Installation.ServerMode = trimLimit(report.Installation.ServerMode, 16)
	report.CPU.PhysicalCoreSource = trimLimit(report.CPU.PhysicalCoreSource, 64)
	report.Warnings = cleanStrings(report.Warnings, 64, 240)
	seenProcesses := make(map[int32]bool)
	processes := make([]shared.ShardProcessReport, 0, len(report.Processes))
	for _, process := range report.Processes {
		if process.PID <= 0 || seenProcesses[process.PID] {
			continue
		}
		seenProcesses[process.PID] = true
		process.Executable = trimLimit(process.Executable, 2048)
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
