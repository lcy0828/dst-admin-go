package agents

import (
	"context"
	"errors"
	"time"
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
	CPUCount      int   `json:"cpuCount,omitempty"`
	MemoryUsed    int64 `json:"memoryUsed,omitempty"`
	MemoryTotal   int64 `json:"memoryTotal,omitempty"`
	UptimeSeconds int64 `json:"uptimeSeconds,omitempty"`
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
	Details       map[string]interface{} `json:"details"`
	CreatedAt     time.Time              `json:"createdAt"`
	UpdatedAt     time.Time              `json:"updatedAt"`
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

type Transport interface {
	Available() bool
	Snapshots() ([]TransportSnapshot, error)
	Execute(context.Context, string, Action, int) (ExecutionResult, error)
	CurrentKey() (string, error)
	RotateKey(context.Context) (string, error)
}
