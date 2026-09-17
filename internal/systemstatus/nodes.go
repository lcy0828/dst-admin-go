package systemstatus

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/agents"
)

var ErrNodeResourceNotFound = errors.New("node resource target not found")

type AgentCatalog interface {
	Agents() ([]agents.Agent, bool, error)
	Agent(string) (agents.Agent, error)
	RefreshSystemInfo(context.Context, string) (agents.Agent, error)
}

type NodeResource struct {
	TargetID     string       `json:"targetId"`
	Name         string       `json:"name"`
	Kind         string       `json:"kind"`
	Online       bool         `json:"online"`
	Stale        bool         `json:"stale"`
	StaleReason  string       `json:"staleReason,omitempty"`
	AgentVersion string       `json:"agentVersion,omitempty"`
	ObservedAt   *time.Time   `json:"observedAt,omitempty"`
	ReceivedAt   *time.Time   `json:"receivedAt,omitempty"`
	Host         HostStatus   `json:"host"`
	CPU          CPUStatus    `json:"cpu"`
	Memory       MemoryStatus `json:"memory"`
	Disk         DiskStatus   `json:"disk"`
	Warnings     []string     `json:"warnings"`
}

type NodeResourceSnapshot struct {
	Items      []NodeResource `json:"items"`
	Total      int            `json:"total"`
	ObservedAt time.Time      `json:"observedAt"`
}

type NodeResourceService struct {
	local  *Service
	agents AgentCatalog
	now    func() time.Time
}

func NewNodeResourceService(local *Service, agentCatalog AgentCatalog) (*NodeResourceService, error) {
	if local == nil || agentCatalog == nil {
		return nil, errors.New("node resource dependencies are required")
	}
	return &NodeResourceService{local: local, agents: agentCatalog, now: time.Now}, nil
}

func (s *NodeResourceService) Snapshot(targetID string) (NodeResourceSnapshot, error) {
	return s.snapshot(targetID, false)
}

func (s *NodeResourceService) snapshot(targetID string, refreshLocal bool) (NodeResourceSnapshot, error) {
	targetID = strings.TrimSpace(targetID)
	result := NodeResourceSnapshot{Items: []NodeResource{}, ObservedAt: s.now().UTC()}

	switch {
	case targetID == "local":
		result.Items = append(result.Items, localNodeResource(s.local.readStatus(refreshLocal)))
	case strings.HasPrefix(targetID, "agent:"):
		agentID := strings.TrimSpace(strings.TrimPrefix(targetID, "agent:"))
		if agentID == "" {
			return NodeResourceSnapshot{}, ErrNodeResourceNotFound
		}
		item, err := s.agents.Agent(agentID)
		if errors.Is(err, agents.ErrAgentNotFound) {
			return NodeResourceSnapshot{}, ErrNodeResourceNotFound
		}
		if err != nil {
			return NodeResourceSnapshot{}, err
		}
		result.Items = append(result.Items, agentNodeResource(item))
	case targetID == "":
		result.Items = append(result.Items, localNodeResource(s.local.readStatus(refreshLocal)))
		items, _, err := s.agents.Agents()
		if err != nil {
			return NodeResourceSnapshot{}, err
		}
		sort.SliceStable(items, func(i, j int) bool {
			left := strings.ToLower(strings.TrimSpace(items[i].DisplayName))
			right := strings.ToLower(strings.TrimSpace(items[j].DisplayName))
			if left == right {
				return items[i].ID < items[j].ID
			}
			return left < right
		})
		for _, item := range items {
			result.Items = append(result.Items, agentNodeResource(item))
		}
	default:
		return NodeResourceSnapshot{}, ErrNodeResourceNotFound
	}

	result.Total = len(result.Items)
	return result, nil
}

func (s *NodeResourceService) Refresh(ctx context.Context, targetID string) (NodeResourceSnapshot, error) {
	result, err := s.snapshot(targetID, true)
	if err != nil {
		return NodeResourceSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for index := range result.Items {
		item := &result.Items[index]
		if item.Kind != string(agents.RuntimeKindAgent) || !item.Online {
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			var agent agents.Agent
			var refreshErr error
			select {
			case slots <- struct{}{}:
				agent, refreshErr = s.agents.RefreshSystemInfo(ctx, strings.TrimPrefix(item.TargetID, "agent:"))
				<-slots
			case <-ctx.Done():
				refreshErr = ctx.Err()
			}
			if refreshErr == nil {
				*item = agentNodeResource(agent)
				return
			}
			item.Stale = true
			item.StaleReason = "refresh_failed"
			message := "实时资源采集失败: " + refreshErr.Error()
			if errors.Is(refreshErr, context.DeadlineExceeded) {
				message = "实时资源采集超时，请检查机器连接"
			} else if errors.Is(refreshErr, agents.ErrAgentOffline) {
				item.Online = false
				message = "机器已离线，无法刷新系统资源"
			}
			item.Warnings = append(item.Warnings, message)
		}()
	}
	workers.Wait()
	return result, nil
}

func localNodeResource(status Status) NodeResource {
	observedAt := status.ObservedAt.UTC()
	return NodeResource{
		TargetID: "local", Name: status.Host.Hostname, Kind: string(agents.RuntimeKindLocal),
		Online: true, ObservedAt: &observedAt, ReceivedAt: &observedAt,
		Host: status.Host, CPU: status.CPU, Memory: status.Memory, Disk: status.Disk,
		Warnings: append([]string(nil), status.Warnings...),
	}
}

func agentNodeResource(agent agents.Agent) NodeResource {
	metrics := agent.Metrics
	threads := metrics.LogicalProcessors
	if threads < 1 {
		threads = metrics.CPUCount
	}
	memory := MemoryStatus{
		Available:  metrics.MemoryTotal > 0,
		TotalBytes: nonNegativeBytes(metrics.MemoryTotal), UsedBytes: nonNegativeBytes(metrics.MemoryUsed),
		AvailableBytes: nonNegativeBytes(metrics.MemoryAvailable),
	}
	if memory.TotalBytes > 0 {
		memory.Usage = percent(float64(memory.UsedBytes) * 100 / float64(memory.TotalBytes))
	}
	disk := DiskStatus{
		Available: metrics.DiskUsageAvailable || metrics.DiskTotal > 0, Path: metrics.DiskPath,
		TotalBytes: nonNegativeBytes(metrics.DiskTotal), UsedBytes: nonNegativeBytes(metrics.DiskUsed),
		AvailableBytes: nonNegativeBytes(metrics.DiskAvailable), Usage: percent(metrics.DiskUsage),
	}
	cpuStatus := CPUStatus{
		Available:      metrics.CPUUsageAvailable || threads > 0 || strings.TrimSpace(metrics.CPUModel) != "",
		UsageAvailable: metrics.CPUUsageAvailable,
		Model:          metrics.CPUModel, Cores: metrics.PhysicalCores, Threads: threads,
		Usage: percent(metrics.CPUUsage), CoreUsage: append([]float64(nil), metrics.CPUCoreUsage...),
		Load1: metrics.Load1, Load5: metrics.Load5, Load15: metrics.Load15, LoadSupport: metrics.LoadSupported,
	}
	name := strings.TrimSpace(agent.DisplayName)
	if name == "" {
		name = strings.TrimSpace(agent.Hostname)
	}
	if name == "" {
		name = agent.ID
	}
	return NodeResource{
		TargetID: "agent:" + agent.ID, Name: name, Kind: string(agents.RuntimeKindAgent),
		Online: agent.Status == agents.StatusOnline, Stale: agent.MetricsStale, StaleReason: agent.StaleReason,
		AgentVersion: agent.Version, ObservedAt: resourceObservedAt(agent), ReceivedAt: utcTimePointer(agent.LastReportAt),
		Host: HostStatus{
			Available: agent.Hostname != "" || agent.OS != "" || agent.Arch != "", Hostname: agent.Hostname,
			Platform: agent.OS, Architecture: agent.Arch, UptimeSeconds: uint64(maxInt64(metrics.UptimeSeconds)),
		},
		CPU: cpuStatus, Memory: memory, Disk: disk, Warnings: []string{},
	}
}

func resourceObservedAt(agent agents.Agent) *time.Time {
	if agent.Metrics.ObservedAt != nil && !agent.Metrics.ObservedAt.IsZero() {
		value := agent.Metrics.ObservedAt.UTC()
		return &value
	}
	return utcTimePointer(agent.LastReportAt)
}

func utcTimePointer(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	result := value.UTC()
	return &result
}

func nonNegativeBytes(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func maxInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
