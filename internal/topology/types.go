package topology

import (
	"errors"
	"time"

	"dont/internal/agents"
	"dont/internal/rooms"
)

var (
	ErrInvalidInput         = errors.New("topology input is invalid")
	ErrRevisionConflict     = errors.New("topology revision has changed")
	ErrRoomNotManaged       = errors.New("room must be managed before topology can be changed")
	ErrOvercommit           = errors.New("topology exceeds the recommended shard capacity")
	ErrExecutionBlocked     = errors.New("topology does not allow shard execution")
	ErrMigrationNotRequired = errors.New("shard placement is already applied")
)

type ExecutionError struct {
	Code    string
	Message string
}

func (e *ExecutionError) Error() string { return e.Message }
func (e *ExecutionError) Unwrap() error { return ErrExecutionBlocked }

type FieldError struct {
	Fields map[string]string
}

func (e *FieldError) Error() string { return ErrInvalidInput.Error() }
func (e *FieldError) Unwrap() error { return ErrInvalidInput }

type RevisionConflictError struct {
	CurrentRevision string
}

func (e *RevisionConflictError) Error() string { return ErrRevisionConflict.Error() }
func (e *RevisionConflictError) Unwrap() error { return ErrRevisionConflict }

type OvercommitError struct {
	Preview Snapshot
}

func (e *OvercommitError) Error() string { return ErrOvercommit.Error() }
func (e *OvercommitError) Unwrap() error { return ErrOvercommit }

type PlacementState string

const (
	PlacementAligned          PlacementState = "aligned"
	PlacementPlanned          PlacementState = "planned"
	PlacementTargetOffline    PlacementState = "target_offline"
	PlacementInventoryStale   PlacementState = "inventory_stale"
	PlacementInventoryMissing PlacementState = "inventory_missing"
	PlacementShardMissing     PlacementState = "shard_missing"
	PlacementConflict         PlacementState = "conflict"
)

type IssueSeverity string

const (
	SeverityInfo    IssueSeverity = "info"
	SeverityWarning IssueSeverity = "warning"
	SeverityError   IssueSeverity = "error"
)

type PlacementInput struct {
	WorldID  string `json:"worldId"`
	TargetID string `json:"targetId"`
}

type UpdateRequest struct {
	ExpectedRevision string           `json:"expectedRevision"`
	AllowOvercommit  bool             `json:"allowOvercommit"`
	Placements       []PlacementInput `json:"placements"`
}

type Placement struct {
	WorldID           string          `json:"worldId"`
	WorldName         string          `json:"worldName"`
	WorldRole         rooms.WorldRole `json:"worldRole"`
	DesiredTargetID   string          `json:"desiredTargetId"`
	AppliedTargetID   string          `json:"appliedTargetId"`
	State             PlacementState  `json:"state"`
	ObservedTargetIDs []string        `json:"observedTargetIds"`
}

type TargetSummary struct {
	ID                             string               `json:"id"`
	Name                           string               `json:"name"`
	Kind                           agents.RuntimeKind   `json:"kind"`
	Status                         agents.RuntimeStatus `json:"status"`
	Online                         bool                 `json:"online"`
	Configured                     bool                 `json:"configured"`
	InventoryAvailable             bool                 `json:"inventoryAvailable"`
	InventoryStale                 bool                 `json:"inventoryStale"`
	StaleReason                    string               `json:"staleReason,omitempty"`
	ObservedAt                     *time.Time           `json:"observedAt,omitempty"`
	ObservedRunningShards          int                  `json:"observedRunningShards"`
	UnmanagedRunningShards         int                  `json:"unmanagedRunningShards"`
	PlannedShards                  int                  `json:"plannedShards"`
	ProjectedShards                int                  `json:"projectedShards"`
	CurrentCapacity                agents.Capacity      `json:"currentCapacity"`
	ProjectedCapacity              agents.Capacity      `json:"projectedCapacity"`
	RequiresOvercommitConfirmation bool                 `json:"requiresOvercommitConfirmation"`
}

type Issue struct {
	Code     string        `json:"code"`
	Severity IssueSeverity `json:"severity"`
	Message  string        `json:"message"`
	TargetID string        `json:"targetId,omitempty"`
	WorldID  string        `json:"worldId,omitempty"`
}

type CapacityPolicy struct {
	Basis                 string `json:"basis"`
	ShardsPerPhysicalCore int    `json:"shardsPerPhysicalCore"`
	ReservedPhysicalCores int    `json:"reservedPhysicalCores"`
	Enforced              bool   `json:"enforced"`
	Message               string `json:"message"`
}

type StartCapacityTarget struct {
	TargetID                 string          `json:"targetId"`
	TargetName               string          `json:"targetName"`
	CurrentRunningShards     int             `json:"currentRunningShards"`
	StartingShards           int             `json:"startingShards"`
	ProjectedRunningShards   int             `json:"projectedRunningShards"`
	Capacity                 agents.Capacity `json:"capacity"`
	RequiresRiskConfirmation bool            `json:"requiresRiskConfirmation"`
}

type StartCapacityPreview struct {
	RoomID                   string                `json:"roomId"`
	WorldIDs                 []string              `json:"worldIds"`
	Targets                  []StartCapacityTarget `json:"targets"`
	RequiresRiskConfirmation bool                  `json:"requiresRiskConfirmation"`
	Policy                   CapacityPolicy        `json:"policy"`
}

type StartCapacitySelection struct {
	RoomID   string   `json:"roomId"`
	WorldIDs []string `json:"worldIds"`
}

type BatchStartCapacityPreview struct {
	Rooms                    []StartCapacitySelection `json:"rooms"`
	Targets                  []StartCapacityTarget    `json:"targets"`
	RequiresRiskConfirmation bool                     `json:"requiresRiskConfirmation"`
	Policy                   CapacityPolicy           `json:"policy"`
}

type Snapshot struct {
	RoomID                         string          `json:"roomId"`
	Revision                       string          `json:"revision"`
	Mode                           string          `json:"mode"`
	RemoteExecutionReady           bool            `json:"remoteExecutionReady"`
	Placements                     []Placement     `json:"placements"`
	Targets                        []TargetSummary `json:"targets"`
	Issues                         []Issue         `json:"issues"`
	RequiresOvercommitConfirmation bool            `json:"requiresOvercommitConfirmation"`
	CapacityPolicy                 CapacityPolicy  `json:"capacityPolicy"`
	UpdatedAt                      time.Time       `json:"updatedAt"`
}

type ExecutionPlacement struct {
	Room            rooms.Room
	World           rooms.World
	Revision        string
	DesiredTargetID string
	AppliedTargetID string
	Target          agents.RuntimeTarget
	Inventory       agents.RuntimeTargetInventory
}

type MigrationPlacement struct {
	Room            rooms.Room
	World           rooms.World
	Revision        string
	SourceTargetID  string
	TargetTargetID  string
	Source          agents.RuntimeTarget
	Target          agents.RuntimeTarget
	SourceInventory agents.RuntimeTargetInventory
	TargetInventory agents.RuntimeTargetInventory
}

type storedPlacement struct {
	WorldID            string          `json:"worldId"`
	WorldDirectoryName string          `json:"worldDirectoryName,omitempty"`
	WorldName          string          `json:"worldName,omitempty"`
	WorldRole          rooms.WorldRole `json:"worldRole,omitempty"`
	DesiredTargetID    string          `json:"desiredTargetId"`
	AppliedTargetID    string          `json:"appliedTargetId"`
}

type record struct {
	RoomID     string
	Revision   string
	Placements []storedPlacement
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
