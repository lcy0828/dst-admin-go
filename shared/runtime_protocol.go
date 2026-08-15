package shared

import "time"

const RuntimeOperationProtocolVersion = 1

type RuntimeAction string

const (
	RuntimeActionConsoleHealth    RuntimeAction = "runtime.console.health"
	RuntimeActionConsoleSend      RuntimeAction = "runtime.console.send"
	RuntimeActionObserveOperation RuntimeAction = "runtime.operation.observe"
	RuntimeActionReadLogs         RuntimeAction = "runtime.logs.read"
	RuntimeActionReadArtifacts    RuntimeAction = "runtime.artifacts.read"
)

type ConsoleMode string

const (
	ConsoleModeManaged ConsoleMode = "managed"
	ConsoleModeProbe   ConsoleMode = "probe"
	ConsoleModeRaw     ConsoleMode = "raw"
)

type ArtifactKind string

const (
	ArtifactRuntimeHealth      ArtifactKind = "runtime.health"
	ArtifactRuntimePlayers     ArtifactKind = "runtime.players"
	ArtifactRuntimeWorldState  ArtifactKind = "runtime.worldstate"
	ArtifactRuntimeEvents      ArtifactKind = "runtime.events"
	ArtifactRuntimeCommand     ArtifactKind = "runtime.command_receipt"
	ArtifactRuntimeDiagnostics ArtifactKind = "runtime.diagnostics"
)

type RuntimeConsoleRequest struct {
	Mode        ConsoleMode `json:"mode"`
	Command     string      `json:"command"`
	CoalesceKey string      `json:"coalesce_key,omitempty"`
}

type RuntimeLogRequest struct {
	FileID   string `json:"file_id,omitempty"`
	Cursor   int64  `json:"cursor"`
	MaxBytes int    `json:"max_bytes"`
	MaxLines int    `json:"max_lines"`
	Query    string `json:"query,omitempty"`
}

type RuntimeArtifactRequest struct {
	Kind ArtifactKind `json:"kind"`
}

type RuntimeObservationRequest struct {
	ObservedOperationID  string `json:"observed_operation_id"`
	ObservedOperationKey string `json:"observed_operation_key,omitempty"`
}

// RuntimeOperationRequest references a trusted installation and a managed
// Shard. It never accepts a host path, executable, container specification or
// shell command.
type RuntimeOperationRequest struct {
	ProtocolVersion  int                        `json:"protocol_version"`
	OperationID      string                     `json:"operation_id"`
	OperationKey     string                     `json:"operation_key,omitempty"`
	InstallationID   string                     `json:"installation_id"`
	Action           RuntimeAction              `json:"action"`
	Cluster          string                     `json:"cluster"`
	Shard            string                     `json:"shard"`
	TopologyRevision string                     `json:"topology_revision"`
	LeaseID          string                     `json:"lease_id,omitempty"`
	FencingToken     uint64                     `json:"fencing_token,omitempty"`
	LeaseExpiresAt   *time.Time                 `json:"lease_expires_at,omitempty"`
	Console          *RuntimeConsoleRequest     `json:"console,omitempty"`
	Logs             *RuntimeLogRequest         `json:"logs,omitempty"`
	Artifacts        *RuntimeArtifactRequest    `json:"artifacts,omitempty"`
	Observation      *RuntimeObservationRequest `json:"observation,omitempty"`
}

type RuntimeOutcome string

const (
	RuntimeOutcomeObserved  RuntimeOutcome = "observed"
	RuntimeOutcomeSent      RuntimeOutcome = "sent"
	RuntimeOutcomeConfirmed RuntimeOutcome = "confirmed"
	RuntimeOutcomeFailed    RuntimeOutcome = "failed"
	RuntimeOutcomeUnknown   RuntimeOutcome = "unknown"
)

type RuntimeConsoleHealth struct {
	Available   bool               `json:"available"`
	Accepting   bool               `json:"accepting"`
	Busy        bool               `json:"busy"`
	Pending     int                `json:"pending"`
	Class       string             `json:"class,omitempty"`
	CoalesceKey string             `json:"coalesce_key,omitempty"`
	StartedAt   time.Time          `json:"started_at,omitempty"`
	Runtime     ShardRuntimeStatus `json:"runtime"`
}

type RuntimeLogLine struct {
	Cursor int64  `json:"cursor"`
	Text   string `json:"text"`
}

type RuntimeLogChunk struct {
	FileName  string           `json:"file_name"`
	FileID    string           `json:"file_id"`
	Size      int64            `json:"size"`
	Cursor    int64            `json:"cursor"`
	Reset     bool             `json:"reset"`
	Truncated bool             `json:"truncated"`
	UpdatedAt time.Time        `json:"updated_at"`
	Lines     []RuntimeLogLine `json:"lines"`
}

type RuntimeArtifact struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	UpdatedAt time.Time `json:"updated_at"`
	Data      []byte    `json:"data"`
}

type RuntimeArtifactBundle struct {
	Kind      ArtifactKind      `json:"kind"`
	Artifacts []RuntimeArtifact `json:"artifacts"`
}

type RuntimeOperationEvidence struct {
	OperationID string         `json:"operation_id"`
	Action      string         `json:"action"`
	Completed   bool           `json:"completed"`
	Outcome     RuntimeOutcome `json:"outcome"`
	Message     string         `json:"message,omitempty"`
	ObservedAt  time.Time      `json:"observed_at"`
}

type RuntimeOperationResult struct {
	ProtocolVersion int                       `json:"protocol_version"`
	OperationID     string                    `json:"operation_id"`
	OperationKey    string                    `json:"operation_key,omitempty"`
	InstallationID  string                    `json:"installation_id"`
	Action          RuntimeAction             `json:"action"`
	Cluster         string                    `json:"cluster"`
	Shard           string                    `json:"shard"`
	FencingToken    uint64                    `json:"fencing_token,omitempty"`
	Outcome         RuntimeOutcome            `json:"outcome"`
	Message         string                    `json:"message,omitempty"`
	Idempotent      bool                      `json:"idempotent"`
	ObservedAt      time.Time                 `json:"observed_at"`
	ConsoleHealth   *RuntimeConsoleHealth     `json:"console_health,omitempty"`
	Logs            *RuntimeLogChunk          `json:"logs,omitempty"`
	Artifacts       *RuntimeArtifactBundle    `json:"artifacts,omitempty"`
	Evidence        *RuntimeOperationEvidence `json:"evidence,omitempty"`
}

func IsRuntimeAction(value RuntimeAction) bool {
	switch value {
	case RuntimeActionConsoleHealth, RuntimeActionConsoleSend, RuntimeActionObserveOperation, RuntimeActionReadLogs, RuntimeActionReadArtifacts:
		return true
	default:
		return false
	}
}

func RuntimeActionMutates(value RuntimeAction) bool {
	return value == RuntimeActionConsoleSend
}
