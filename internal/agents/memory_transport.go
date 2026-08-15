package agents

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"dont/shared"
)

type MemoryTransport struct {
	mu        sync.Mutex
	snapshots map[string]TransportSnapshot
	key       string
	now       func() time.Time
}

func NewMemoryTransport() *MemoryTransport {
	now := time.Now().UTC()
	return &MemoryTransport{
		key: "memory-agent-key-0123456789-abcdef",
		now: time.Now,
		snapshots: map[string]TransportSnapshot{
			"agent-primary": {
				ID: "agent-primary", Status: StatusOnline, Hostname: "林火节点", OS: "linux", Arch: "amd64", Version: "2.0.0-test",
				IPAddresses: []string{"192.168.2.12"}, LastHeartbeat: now, LastReportAt: utcTimePointer(now),
					Capabilities: []string{"system.report", "command.exec", "disk.inspect", "runtime.inventory.read", "runtime.processes.read", "runtime.capacity.read", "shard.control.v1", "runtime.driver.v1", "runtime.console.v1", "runtime.logs.v1", "runtime.artifacts.v1", "runtime.migration.v1", "runtime.backup.v1", "runtime.mods.v1", "runtime.game-update.v1", "runtime.cpu.v1"},
				Metrics:      Metrics{CPUCount: 16, LogicalProcessors: 16, PhysicalCores: 8, PhysicalCoreSource: "test", RunningShardCount: 2, MemoryUsed: 3 * 1024 * 1024 * 1024, MemoryTotal: 8 * 1024 * 1024 * 1024, UptimeSeconds: 86400, ObservedAt: utcTimePointer(now)},
				Details:      map[string]interface{}{"goVersion": "go1.25", "currentDirectory": "/opt/dst-admin-agent"},
			},
			"agent-offline": {
				ID: "agent-offline", Status: StatusOffline, Hostname: "离线节点", OS: "darwin", Arch: "arm64", Version: "1.9.0-test",
				IPAddresses: []string{"192.168.2.13"}, LastHeartbeat: now.Add(-10 * time.Minute), LastReportAt: utcTimePointer(now.Add(-10 * time.Minute)),
				Capabilities: []string{"system.report", "command.exec", "disk.inspect"}, Metrics: Metrics{CPUCount: 8, LogicalProcessors: 8, PhysicalCores: 4, PhysicalCoreSource: "test", MemoryTotal: 4 * 1024 * 1024 * 1024, ObservedAt: utcTimePointer(now.Add(-10 * time.Minute))}, Details: map[string]interface{}{},
			},
		},
	}
}

func (m *MemoryTransport) Inventory(ctx context.Context, agentID string, config RuntimeConfig, _ int) (shared.RuntimeInventoryReport, error) {
	select {
	case <-ctx.Done():
		return shared.RuntimeInventoryReport{}, ctx.Err()
	default:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, exists := m.snapshots[agentID]
	if !exists || snapshot.Status != StatusOnline {
		return shared.RuntimeInventoryReport{}, ErrAgentOffline
	}
	now := m.now().UTC()
	return shared.RuntimeInventoryReport{
		ProtocolVersion: shared.RuntimeInventoryProtocolVersion,
		ObservedAt:      now,
		CPU:             shared.CPUInventory{LogicalProcessors: 16, PhysicalCores: 8, PhysicalCoreSource: "test"},
		Memory:          shared.MemoryInventory{TotalBytes: 8 * 1024 * 1024 * 1024, UsedBytes: 3 * 1024 * 1024 * 1024, AvailableBytes: 5 * 1024 * 1024 * 1024},
		Installation: shared.RuntimeInstallationReport{
			ID: "default", DisplayName: config.DisplayName, SavePath: config.SavePath, ServerPath: config.ServerPath,
			ServerMode: config.ServerMode, SavePathOK: true, ServerPathOK: true,
		},
		Rooms: []shared.RoomInventoryReport{{
			Directory: "Cluster_1", Name: "测试房间", ConfigPath: config.SavePath + "/Cluster_1/cluster.ini", MasterPort: 10889,
			Shards: []shared.ShardInventoryReport{
				{Directory: "Master", Name: "Master", ID: 1, Role: "master", ServerPort: 10999, AuthenticationPort: 8767, MasterServerPort: 27017},
				{Directory: "Caves", Name: "Caves", ID: 2, Role: "secondary", ServerPort: 10998, AuthenticationPort: 8768, MasterServerPort: 27018},
			},
		}},
		Processes: []shared.ShardProcessReport{
			{PID: 101, Executable: "dontstarve_dedicated_server_nullrenderer", Cluster: "Cluster_1", Shard: "Master", RSSBytes: 1024},
			{PID: 102, Executable: "dontstarve_dedicated_server_nullrenderer", Cluster: "Cluster_1", Shard: "Caves", RSSBytes: 1024},
		},
		Warnings: []string{},
	}, nil
}

func (m *MemoryTransport) Available() bool { return true }

func (m *MemoryTransport) Snapshots() ([]TransportSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]TransportSnapshot, 0, len(m.snapshots))
	for _, item := range m.snapshots {
		items = append(items, cloneSnapshot(item))
	}
	return items, nil
}

func (m *MemoryTransport) ForgetSnapshot(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if snapshot, exists := m.snapshots[agentID]; exists && snapshot.Status == StatusOffline {
		delete(m.snapshots, agentID)
	}
}

func (m *MemoryTransport) Execute(ctx context.Context, agentID string, action Action, _ int) (ExecutionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, exists := m.snapshots[agentID]
	if !exists || snapshot.Status != StatusOnline {
		return ExecutionResult{ExitCode: 1}, ErrAgentOffline
	}
	select {
	case <-ctx.Done():
		return ExecutionResult{ExitCode: 1}, ctx.Err()
	default:
	}
	switch action {
	case ActionSystemRefresh:
		now := m.now().UTC()
		snapshot.LastHeartbeat = now
		snapshot.LastReportAt = utcTimePointer(now)
		snapshot.Metrics.UptimeSeconds++
		m.snapshots[agentID] = snapshot
		return ExecutionResult{RemoteID: "memory-report", Output: "系统信息已刷新", ExitCode: 0}, nil
	case ActionDiskInspect:
		return ExecutionResult{RemoteID: "memory-disk", Output: "", ExitCode: 1}, errors.New("测试节点磁盘检查失败")
	default:
		return ExecutionResult{ExitCode: 1}, ErrUnsupportedAction
	}
}

func (m *MemoryTransport) ExecuteShard(ctx context.Context, agentID string, request shared.ShardOperationRequest, _ int) (ShardExecutionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, exists := m.snapshots[agentID]
	if !exists || snapshot.Status != StatusOnline {
		return ShardExecutionResult{}, ErrAgentOffline
	}
	select {
	case <-ctx.Done():
		return ShardExecutionResult{}, ctx.Err()
	default:
	}
	if !shared.IsShardAction(request.Action) {
		return ShardExecutionResult{}, ErrUnsupportedAction
	}
	state := "running"
	if request.Action == shared.ShardActionStop {
		state = "stopped"
	}
	return ShardExecutionResult{RemoteID: "memory-" + request.OperationID, Result: shared.ShardOperationResult{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: request.OperationID, OperationKey: request.OperationKey,
		InstallationID: request.InstallationID, Action: request.Action, Cluster: request.Cluster, Shard: request.Shard,
		FencingToken: request.FencingToken, Status: shared.ShardRuntimeStatus{State: state, SessionExists: state == "running"},
		Message: "测试分片操作已完成", ObservedAt: m.now().UTC(),
	}}, nil
}

func (m *MemoryTransport) ExecuteRuntime(ctx context.Context, agentID string, request shared.RuntimeOperationRequest, _ int) (RuntimeExecutionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, exists := m.snapshots[agentID]
	if !exists || snapshot.Status != StatusOnline {
		return RuntimeExecutionResult{}, ErrAgentOffline
	}
	select {
	case <-ctx.Done():
		return RuntimeExecutionResult{}, ctx.Err()
	default:
	}
	if !shared.IsRuntimeAction(request.Action) {
		return RuntimeExecutionResult{}, ErrUnsupportedAction
	}
	result := shared.RuntimeOperationResult{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: request.OperationID, OperationKey: request.OperationKey,
		InstallationID: request.InstallationID, Action: request.Action, Cluster: request.Cluster, Shard: request.Shard,
		FencingToken: request.FencingToken, Outcome: shared.RuntimeOutcomeObserved, Message: "测试 Runtime 操作已完成", ObservedAt: m.now().UTC(),
	}
	if request.Action == shared.RuntimeActionConsoleSend {
		result.Outcome = shared.RuntimeOutcomeSent
	}
	if request.Action == shared.RuntimeActionGameVersionObserve || request.Action == shared.RuntimeActionGameVersionUpdate {
		version := "747465"
		if request.GameVersion != nil && request.GameVersion.ExpectedVersion != "" {
			version = request.GameVersion.ExpectedVersion
		}
		result.GameVersion = &shared.RuntimeGameVersionResult{
			Installed: true, CurrentVersion: version, AvailableBytes: 16 * 1024 * 1024 * 1024,
			SteamCMDAvailable: true, UpdateSupported: true, ObservedAt: m.now().UTC(),
		}
		if request.Action == shared.RuntimeActionGameVersionUpdate {
			result.Outcome = shared.RuntimeOutcomeConfirmed
		}
	}
	if request.Action == shared.RuntimeActionCPUPrepare || request.Action == shared.RuntimeActionCPUApply || request.Action == shared.RuntimeActionCPUObserve {
		if request.CPU == nil {
			return RuntimeExecutionResult{}, ErrInvalidInput
		}
		state := shared.RuntimeCPUStateApplied
		if request.Action == shared.RuntimeActionCPUPrepare {
			state = shared.RuntimeCPUStatePrepared
		}
		if request.CPU.Policy == shared.RuntimeCPUPolicyNone {
			state = shared.RuntimeCPUStateReleased
		}
		result.CPU = &shared.RuntimeCPUResult{
			Policy: request.CPU.Policy, LogicalCPUIds: append([]int(nil), request.CPU.LogicalCPUIds...),
			EffectiveCPUIds: append([]int(nil), request.CPU.LogicalCPUIds...), State: state,
			RuntimeKind: "memory", InstanceID: "memory-shard", Enforced: state != shared.RuntimeCPUStatePrepared,
			InstanceRunning: state == shared.RuntimeCPUStateApplied, ObservedAt: m.now().UTC(),
		}
		if request.Action != shared.RuntimeActionCPUObserve {
			result.Outcome = shared.RuntimeOutcomeConfirmed
		}
	}
	return RuntimeExecutionResult{RemoteID: "memory-" + request.OperationID, Result: result}, nil
}

func (m *MemoryTransport) CurrentKey() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.key, nil
}

func (m *MemoryTransport) RotateKey(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.key = base64.RawURLEncoding.EncodeToString(data)
	return m.key, nil
}

func cloneSnapshot(value TransportSnapshot) TransportSnapshot {
	value.IPAddresses = append([]string(nil), value.IPAddresses...)
	value.Capabilities = append([]string(nil), value.Capabilities...)
	details := make(map[string]interface{}, len(value.Details))
	for key, item := range value.Details {
		details[key] = item
	}
	value.Details = details
	return value
}
