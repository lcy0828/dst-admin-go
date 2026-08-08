package agents

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	legacyserver "dont/server"
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
	capabilities := []string{"system.report", "command.exec"}
	platform := strings.ToLower(stringValue(info["os"]))
	if platform == "linux" || platform == "darwin" || platform == "windows" {
		capabilities = append(capabilities, "disk.inspect")
	}
	memory := mapValue(info["memory"])
	details := make(map[string]interface{}, len(info))
	for key, value := range info {
		if key != "security_key" && !strings.HasPrefix(key, "_") {
			details[key] = value
		}
	}
	return TransportSnapshot{ID: id, Status: StatusOnline, Hostname: stringValue(info["hostname"]), OS: stringValue(info["os"]), Arch: stringValue(info["arch"]), Version: nonEmpty(stringValue(info["agent_version"]), "legacy"), IPAddresses: stringSlice(info["ip_addresses"]), LastHeartbeat: heartbeat, LastReportAt: reportPointer, Capabilities: capabilities, Metrics: Metrics{CPUCount: int(int64Value(info["cpu_count"])), MemoryUsed: int64Value(memory["allocated"]), MemoryTotal: int64Value(memory["system"]), UptimeSeconds: int64Value(info["uptime_seconds"])}, Details: details}
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
func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
