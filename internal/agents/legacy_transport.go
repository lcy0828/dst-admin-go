package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	legacyserver "dont/server"
	"dont/shared"
)

type LegacyTransport struct{ server func() *legacyserver.Server }

func NewLegacyTransport(provider func() *legacyserver.Server) *LegacyTransport {
	return &LegacyTransport{server: provider}
}

func (t *LegacyTransport) current() *legacyserver.Server {
	if t == nil || t.server == nil {
		return nil
	}
	return t.server()
}

func (t *LegacyTransport) Available() bool { return t.current() != nil }

func (t *LegacyTransport) Snapshots() ([]TransportSnapshot, error) {
	server := t.current()
	if server == nil {
		return nil, ErrUnavailable
	}
	values := server.GetAllAgentInfo()
	items := make([]TransportSnapshot, 0, len(values))
	for id, info := range values {
		items = append(items, legacySnapshot(id, info))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (t *LegacyTransport) Execute(ctx context.Context, agentID string, action Action, timeout int) (ExecutionResult, error) {
	server := t.current()
	if server == nil {
		return ExecutionResult{ExitCode: 1}, ErrUnavailable
	}
	switch action {
	case ActionSystemRefresh:
		before := legacyReportTimestamp(server.GetAllAgentInfo()[agentID])
		if err := server.RequestPassiveReport(agentID, "system_info", map[string]interface{}{}); err != nil {
			return ExecutionResult{ExitCode: 1}, err
		}
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ExecutionResult{ExitCode: 1}, ctx.Err()
			case <-ticker.C:
				info, exists := server.GetAllAgentInfo()[agentID]
				if !exists {
					return ExecutionResult{ExitCode: 1}, ErrAgentOffline
				}
				if after := legacyReportTimestamp(info); after > before {
					encoded, _ := json.Marshal(info)
					return ExecutionResult{RemoteID: "report-" + agentID, Output: string(encoded), ExitCode: 0}, nil
				}
			}
		}
	case ActionDiskInspect:
		info, exists := server.GetAllAgentInfo()[agentID]
		if !exists {
			return ExecutionResult{ExitCode: 1}, ErrAgentOffline
		}
		program, arguments, err := diskCommand(stringValue(info["os"]))
		if err != nil {
			return ExecutionResult{ExitCode: 1}, err
		}
		remoteID, err := server.SendExec(agentID, program, arguments, timeout)
		if err != nil {
			return ExecutionResult{ExitCode: 1}, err
		}
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ExecutionResult{RemoteID: remoteID, ExitCode: 1}, ctx.Err()
			case <-ticker.C:
				result, resultErr := server.GetCommandResult(remoteID)
				if resultErr != nil {
					continue
				}
				if result.Status == "pending" {
					continue
				}
				execution := ExecutionResult{RemoteID: remoteID, Output: result.Output, ExitCode: result.ExitCode}
				if !result.Success || result.Status == "failed" {
					return execution, errors.New(nonEmpty(result.ErrorMsg, "Agent 命令执行失败"))
				}
				return execution, nil
			}
		}
	default:
		return ExecutionResult{ExitCode: 1}, ErrUnsupportedAction
	}
}

func (t *LegacyTransport) ExecuteShard(ctx context.Context, agentID string, request shared.ShardOperationRequest, timeout int) (ShardExecutionResult, error) {
	server := t.current()
	if server == nil {
		return ShardExecutionResult{}, ErrUnavailable
	}
	remoteID, err := server.SendShardOperation(agentID, request, timeout)
	if err != nil {
		return ShardExecutionResult{}, err
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ShardExecutionResult{RemoteID: remoteID}, ctx.Err()
		case <-ticker.C:
			command, resultErr := server.GetCommandResult(remoteID)
			if resultErr != nil || command.Status == "pending" || command.Status == "received" {
				continue
			}
			result := ShardExecutionResult{RemoteID: remoteID}
			if strings.TrimSpace(command.Output) != "" {
				if decodeErr := json.Unmarshal([]byte(command.Output), &result.Result); decodeErr != nil {
					return result, fmt.Errorf("解析 Agent 分片操作结果: %w", decodeErr)
				}
			}
			if !command.Success || command.Status == "failed" {
				return result, errors.New(nonEmpty(command.ErrorMsg, "Agent 分片操作失败"))
			}
			if result.Result.ProtocolVersion != shared.ShardOperationProtocolVersion || result.Result.OperationID != request.OperationID {
				return result, errors.New("Agent 返回的分片操作结果无效")
			}
			return result, nil
		}
	}
}

func (t *LegacyTransport) ExecuteRuntime(ctx context.Context, agentID string, request shared.RuntimeOperationRequest, timeout int) (RuntimeExecutionResult, error) {
	server := t.current()
	if server == nil {
		return RuntimeExecutionResult{}, ErrUnavailable
	}
	remoteID, err := server.SendRuntimeOperation(agentID, request, timeout)
	if err != nil {
		return RuntimeExecutionResult{}, err
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return RuntimeExecutionResult{RemoteID: remoteID}, ctx.Err()
		case <-ticker.C:
			command, resultErr := server.GetCommandResult(remoteID)
			if resultErr != nil || command.Status == "pending" || command.Status == "received" {
				continue
			}
			result := RuntimeExecutionResult{RemoteID: remoteID}
			if strings.TrimSpace(command.Output) != "" {
				if decodeErr := json.Unmarshal([]byte(command.Output), &result.Result); decodeErr != nil {
					return result, fmt.Errorf("解析 Agent Runtime 操作结果: %w", decodeErr)
				}
			}
			if !command.Success || command.Status == "failed" {
				return result, errors.New(nonEmpty(command.ErrorMsg, "Agent Runtime 操作失败"))
			}
			if result.Result.ProtocolVersion != shared.RuntimeOperationProtocolVersion || result.Result.OperationID != request.OperationID {
				return result, errors.New("Agent 返回的 Runtime 操作结果无效")
			}
			return result, nil
		}
	}
}

func (t *LegacyTransport) Inventory(ctx context.Context, agentID string, config RuntimeConfig, _ int) (shared.RuntimeInventoryReport, error) {
	server := t.current()
	if server == nil {
		return shared.RuntimeInventoryReport{}, ErrUnavailable
	}
	before := legacyReportTimestamp(server.GetAllAgentInfo()[agentID])
	params := map[string]interface{}{
		"installation_id": config.InstallationID, "display_name": config.DisplayName,
		"save_path": config.SavePath, "server_path": config.ServerPath, "server_mode": config.ServerMode,
	}
	if err := server.RequestPassiveReport(agentID, "dst_runtime_inventory", params); err != nil {
		return shared.RuntimeInventoryReport{}, err
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return shared.RuntimeInventoryReport{}, ctx.Err()
		case <-ticker.C:
			info, exists := server.GetAllAgentInfo()[agentID]
			if !exists {
				return shared.RuntimeInventoryReport{}, ErrAgentOffline
			}
			if legacyReportTimestamp(info) <= before || stringValue(info["_last_report_type"]) != "dst_runtime_inventory" {
				continue
			}
			if message := stringValue(info["error"]); message != "" {
				return shared.RuntimeInventoryReport{}, errors.New(message)
			}
			encoded, err := json.Marshal(info["inventory"])
			if err != nil {
				return shared.RuntimeInventoryReport{}, err
			}
			var report shared.RuntimeInventoryReport
			if err := json.Unmarshal(encoded, &report); err != nil {
				return shared.RuntimeInventoryReport{}, err
			}
			if report.ProtocolVersion < 1 || report.ObservedAt.IsZero() {
				return shared.RuntimeInventoryReport{}, errors.New("Agent 返回的 DST 运行时清单无效")
			}
			return report, nil
		}
	}
}

func (t *LegacyTransport) CurrentKey() (string, error) {
	server := t.current()
	if server == nil {
		return "", ErrUnavailable
	}
	return server.GetSecurityKey(), nil
}

func (t *LegacyTransport) RotateKey(ctx context.Context) (string, error) {
	server := t.current()
	if server == nil {
		return "", ErrUnavailable
	}
	before := server.GetSecurityKey()
	if err := server.GenerateNewSecurityKey(); err != nil {
		return "", err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		key := server.GetSecurityKey()
		if key != "" && key != before {
			return key, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func legacySnapshot(id string, info map[string]interface{}) TransportSnapshot {
	heartbeat := time.Unix(int64Value(info["last_heartbeat"]), 0).UTC()
	if heartbeat.Unix() <= 0 {
		heartbeat = time.Now().UTC()
	}
	reportAt := time.Unix(int64Value(info["timestamp"]), 0).UTC()
	var reportPointer *time.Time
	if reportAt.Unix() > 0 {
		reportPointer = &reportAt
	}
	capabilities := stringSlice(info["capabilities"])
	if len(capabilities) == 0 {
		capabilities = []string{"system.report", "command.exec"}
	}
	platform := strings.ToLower(stringValue(info["os"]))
	if (platform == "linux" || platform == "darwin" || platform == "windows") && !containsString(capabilities, "disk.inspect") {
		capabilities = append(capabilities, "disk.inspect")
	}
	memory := mapValue(info["memory"])
	cpuInfo := mapValue(info["cpu"])
	logicalProcessors := int(int64Value(cpuInfo["logical_processors"]))
	if logicalProcessors < 1 {
		logicalProcessors = int(int64Value(info["cpu_count"]))
	}
	physicalCores := int(int64Value(cpuInfo["physical_cores"]))
	memoryUsed := int64Value(memory["used"])
	if memoryUsed == 0 {
		memoryUsed = int64Value(memory["allocated"])
	}
	memoryTotal := int64Value(memory["total"])
	if memoryTotal == 0 {
		memoryTotal = int64Value(memory["system"])
	}
	var observedAt *time.Time
	if parsed := timeValue(info["runtime_observed_at"]); !parsed.IsZero() {
		observedAt = &parsed
	} else if reportPointer != nil {
		observedAt = reportPointer
	}
	details := make(map[string]interface{}, len(info))
	for key, value := range info {
		if key != "security_key" && !strings.HasPrefix(key, "_") {
			details[key] = value
		}
	}
	return TransportSnapshot{ID: id, Status: StatusOnline, Hostname: stringValue(info["hostname"]), OS: stringValue(info["os"]), Arch: stringValue(info["arch"]), Version: nonEmpty(stringValue(info["agent_version"]), "legacy"), IPAddresses: stringSlice(info["ip_addresses"]), LastHeartbeat: heartbeat, LastReportAt: reportPointer, Capabilities: capabilities, Metrics: Metrics{
		CPUCount: logicalProcessors, LogicalProcessors: logicalProcessors, PhysicalCores: physicalCores,
		PhysicalCoreSource: stringValue(cpuInfo["physical_core_source"]), PhysicalCoreEstimated: boolValue(cpuInfo["physical_core_estimated"]),
		RunningShardCount: int(int64Value(info["dst_process_count"])), MemoryUsed: memoryUsed, MemoryTotal: memoryTotal,
		MemoryAvailable: int64Value(memory["available"]), UptimeSeconds: int64Value(info["uptime_seconds"]), ObservedAt: observedAt,
	}, Details: details}
}

func diskCommand(platform string) (string, []string, error) {
	switch strings.ToLower(platform) {
	case "linux", "darwin":
		return "df", []string{"-Pk"}, nil
	case "windows":
		return "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command", "Get-PSDrive -PSProvider FileSystem | Select-Object Name,Used,Free"}, nil
	default:
		return "", nil, ErrUnsupportedAction
	}
}

func legacyReportTimestamp(info map[string]interface{}) int64 {
	if info == nil {
		return 0
	}
	if reportedAt := int64Value(info["_last_passive_report_at"]); reportedAt > 0 {
		return reportedAt
	}
	return int64Value(info["timestamp"]) * int64(time.Second)
}
func stringValue(value interface{}) string { text, _ := value.(string); return strings.TrimSpace(text) }
func int64Value(value interface{}) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	}
	return 0
}
func mapValue(value interface{}) map[string]interface{} {
	typed, _ := value.(map[string]interface{})
	if typed == nil {
		return map[string]interface{}{}
	}
	return typed
}
func stringSlice(value interface{}) []string {
	raw, ok := value.([]interface{})
	if ok {
		result := make([]string, 0, len(raw))
		for _, item := range raw {
			if text := stringValue(item); text != "" {
				result = append(result, text)
			}
		}
		return result
	}
	typed, _ := value.([]string)
	return typed
}
func boolValue(value interface{}) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}
func timeValue(value interface{}) time.Time {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC()
	case string:
		parsed, _ := time.Parse(time.RFC3339Nano, typed)
		return parsed.UTC()
	}
	return time.Time{}
}
func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
