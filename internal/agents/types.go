package agents

import (
	"context"
	"errors"
	"time"

	"dont/shared"
)

var (
	ErrUnavailable          = errors.New("agent transport is unavailable")
	ErrAgentNotFound        = errors.New("agent not found")
	ErrAgentOffline         = errors.New("agent is offline")
	ErrAgentOnline          = errors.New("online agent cannot be forgotten")
	ErrCommandNotFound      = errors.New("agent command not found")
	ErrInvalidInput         = errors.New("agent input is invalid")
	ErrUnsupportedAction    = errors.New("agent action is not supported")
	ErrConfirmationRequired = errors.New("agent key rotation confirmation is required")
	ErrRuntimeNotConfigured = errors.New("agent runtime is not configured")
	ErrInventoryNotFound    = errors.New("agent runtime inventory not found")
)

type Status string

const (
	StatusOnline  Status = "online"
	StatusOffline Status = "offline"
)

type CommandStatus string

const (
	CommandQueued    CommandStatus = "queued"
	CommandRunning   CommandStatus = "running"
	CommandSucceeded CommandStatus = "succeeded"
	CommandFailed    CommandStatus = "failed"
	CommandCanceled  CommandStatus = "canceled"
)

type Action string

const (
	ActionSystemRefresh Action = "system.refresh"
	ActionDiskInspect   Action = "disk.inspect"
)

type Metrics struct {
	CPUCount              int        `json:"cpuCount,omitempty"`
	LogicalProcessors     int        `json:"logicalProcessors,omitempty"`
	PhysicalCores         int        `json:"physicalCores,omitempty"`
	PhysicalCoreSource    string     `json:"physicalCoreSource,omitempty"`
	PhysicalCoreEstimated bool       `json:"physicalCoreEstimated"`
	RunningShardCount     int        `json:"runningShardCount"`
	MemoryUsed            int64      `json:"memoryUsed,omitempty"`
	MemoryTotal           int64      `json:"memoryTotal,omitempty"`
	MemoryAvailable       int64      `json:"memoryAvailable,omitempty"`
	UptimeSeconds         int64      `json:"uptimeSeconds,omitempty"`
	ObservedAt            *time.Time `json:"observedAt,omitempty"`
}

type CapacityState string

const (
	CapacityAvailable     CapacityState = "available"
	CapacityFull          CapacityState = "full"
	CapacityOvercommitted CapacityState = "overcommitted"
	CapacityUnknown       CapacityState = "unknown"
)

type Capacity struct {
	State                 CapacityState `json:"state"`
	LogicalProcessors     int           `json:"logicalProcessors"`
	PhysicalCores         int           `json:"physicalCores"`
	PhysicalCoreEstimated bool          `json:"physicalCoreEstimated"`
	ReservedPhysicalCores int           `json:"reservedPhysicalCores"`
	RecommendedShardLimit int           `json:"recommendedShardLimit"`
	RunningShards         int           `json:"runningShards"`
	AvailableSlots        int           `json:"availableSlots"`
	Message               string        `json:"message"`
}

type Agent struct {
	ID            string                 `json:"id"`
	Hostname      string                 `json:"hostname"`
	OS            string                 `json:"os"`
	Arch          string                 `json:"arch"`
	Version       string                 `json:"version"`
	IPAddresses   []string               `json:"ipAddresses"`
	Status        Status                 `json:"status"`
	LastHeartbeat time.Time              `json:"lastHeartbeat"`
	LastReportAt  *time.Time             `json:"lastReportAt,omitempty"`
	Capabilities  []string               `json:"capabilities"`
	Metrics       Metrics                `json:"metrics"`
	Capacity      Capacity               `json:"capacity"`
	MetricsStale  bool                   `json:"metricsStale"`
	StaleReason   string                 `json:"staleReason,omitempty"`
	Details       map[string]interface{} `json:"details"`
	CreatedAt     time.Time              `json:"createdAt"`
	UpdatedAt     time.Time              `json:"updatedAt"`
}

type RuntimeKind string

const (
	RuntimeKindLocal RuntimeKind = "local"
	RuntimeKindAgent RuntimeKind = "agent"
)

type RuntimeStatus string

const (
	RuntimeStatusReady                 RuntimeStatus = "ready"
	RuntimeStatusOffline               RuntimeStatus = "offline"
	RuntimeStatusConfigurationRequired RuntimeStatus = "configuration_required"
)

// RuntimeConfig contains paths interpreted on the target machine. Remote
// values are never merged into the controller's local app.conf settings.
type RuntimeConfig struct {
	DisplayName         string     `json:"displayName"`
	SavePath            string     `json:"savePath"`
	BackupPath          string     `json:"backupPath"`
	ServerPath          string     `json:"serverPath"`
	UGCPath             string     `json:"ugcPath"`
	SteamCMDPath        string     `json:"steamcmdPath"`
	WorkshopContentPath string     `json:"workshopContentPath"`
	LuaBinary           string     `json:"luaBinary"`
	LuaFallbackPath     string     `json:"luaFallbackPath"`
	ServerMode          string     `json:"serverMode"`
	UpdatedAt           *time.Time `json:"updatedAt,omitempty"`
}

type RuntimeTarget struct {
	ID            string        `json:"id"`
	Kind          RuntimeKind   `json:"kind"`
	AgentID       string        `json:"agentId,omitempty"`
	Name          string        `json:"name"`
	Hostname      string        `json:"hostname"`
	OS            string        `json:"os"`
	Arch          string        `json:"arch"`
	Status        RuntimeStatus `json:"status"`
	Default       bool          `json:"default"`
	Configured    bool          `json:"configured"`
	Online        bool          `json:"online"`
	Capabilities  []string      `json:"capabilities"`
	LastHeartbeat *time.Time    `json:"lastHeartbeat,omitempty"`
	Config        RuntimeConfig `json:"config"`
}

type RuntimeTargetInventory struct {
	Target      RuntimeTarget                 `json:"target"`
	Available   bool                          `json:"available"`
	Inventory   shared.RuntimeInventoryReport `json:"inventory"`
	Capacity    Capacity                      `json:"capacity"`
	ObservedAt  *time.Time                    `json:"observedAt,omitempty"`
	ReceivedAt  *time.Time                    `json:"receivedAt,omitempty"`
	Stale       bool                          `json:"stale"`
	StaleReason string                        `json:"staleReason,omitempty"`
}

type ActionDefinition struct {
	ID          Action   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Platforms   []string `json:"platforms"`
}

type CommandInput struct {
	Action         Action `json:"action"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

type Command struct {
	ID         string        `json:"id"`
	AgentID    string        `json:"agentId"`
	AgentName  string        `json:"agentName"`
	Action     Action        `json:"action"`
	Status     CommandStatus `json:"status"`
	JobID      string        `json:"jobId,omitempty"`
	RemoteID   string        `json:"remoteId,omitempty"`
	Output     string        `json:"output,omitempty"`
	Error      string        `json:"error,omitempty"`
	ExitCode   *int          `json:"exitCode,omitempty"`
	StartedAt  *time.Time    `json:"startedAt,omitempty"`
	FinishedAt *time.Time    `json:"finishedAt,omitempty"`
	DurationMs int64         `json:"durationMs"`
	CreatedAt  time.Time     `json:"createdAt"`
}

type CommandFilter struct {
	AgentID string
	Status  CommandStatus
	Query   string
	StartAt *time.Time
	EndAt   *time.Time
	Limit   int
	Offset  int
}

type CommandList struct {
	Items  []Command `json:"items"`
	Total  int       `json:"total"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}

type SecurityStatus struct {
	Available       bool       `json:"available"`
	Configured      bool       `json:"configured"`
	MaskedKey       string     `json:"maskedKey"`
	Fingerprint     string     `json:"fingerprint"`
	RotatedAt       *time.Time `json:"rotatedAt,omitempty"`
	ConnectedAgents int        `json:"connectedAgents"`
}

type RotateKeyInput struct {
	Confirmation string `json:"confirmation"`
}

type RotateKeyResult struct {
	NewKey      string    `json:"newKey"`
	Fingerprint string    `json:"fingerprint"`
	RotatedAt   time.Time `json:"rotatedAt"`
}

type TransportSnapshot struct {
	ID            string
	Status        Status
	Hostname      string
	OS            string
	Arch          string
	Version       string
	IPAddresses   []string
	LastHeartbeat time.Time
	LastReportAt  *time.Time
	Capabilities  []string
	Metrics       Metrics
	Details       map[string]interface{}
}

type ExecutionResult struct {
	RemoteID string
	Output   string
	ExitCode int
}

type InventorySnapshot struct {
	AgentID     string                        `json:"agentId"`
	Inventory   shared.RuntimeInventoryReport `json:"inventory"`
	Capacity    Capacity                      `json:"capacity"`
	ObservedAt  time.Time                     `json:"observedAt"`
	ReceivedAt  time.Time                     `json:"receivedAt"`
	Stale       bool                          `json:"stale"`
	StaleReason string                        `json:"staleReason,omitempty"`
}

type Transport interface {
	Available() bool
	Snapshots() ([]TransportSnapshot, error)
	Execute(context.Context, string, Action, int) (ExecutionResult, error)
	Inventory(context.Context, string, RuntimeConfig, int) (shared.RuntimeInventoryReport, error)
	CurrentKey() (string, error)
	RotateKey(context.Context) (string, error)
}
