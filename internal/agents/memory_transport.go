package agents

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
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
				Capabilities: []string{"system.report", "command.exec", "disk.inspect"},
				Metrics:      Metrics{CPUCount: 8, MemoryUsed: 3 * 1024 * 1024 * 1024, MemoryTotal: 8 * 1024 * 1024 * 1024, UptimeSeconds: 86400},
				Details:      map[string]interface{}{"goVersion": "go1.25", "currentDirectory": "/opt/dst-admin-agent"},
			},
			"agent-offline": {
				ID: "agent-offline", Status: StatusOffline, Hostname: "离线节点", OS: "darwin", Arch: "arm64", Version: "1.9.0-test",
				IPAddresses: []string{"192.168.2.13"}, LastHeartbeat: now.Add(-10 * time.Minute), LastReportAt: utcTimePointer(now.Add(-10 * time.Minute)),
				Capabilities: []string{"system.report", "command.exec", "disk.inspect"}, Metrics: Metrics{CPUCount: 4, MemoryTotal: 4 * 1024 * 1024 * 1024}, Details: map[string]interface{}{},
			},
		},
	}
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
