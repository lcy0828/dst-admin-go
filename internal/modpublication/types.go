package modpublication

import (
	"errors"
	"time"
)

const (
	PlanVersion        = 1
	RequiredCapability = "mod.publication.v1"
)

var (
	ErrInvalidInput        = errors.New("mod publication input is invalid")
	ErrNotFound            = errors.New("mod publication not found")
	ErrConflict            = errors.New("mod publication conflicts with active state")
	ErrIdempotencyConflict = errors.New("mod publication idempotency key conflicts with another plan")
	ErrTopologyChanged     = errors.New("mod publication topology changed")
	ErrPlanChanged         = errors.New("mod publication plan changed")
	ErrVersionConflict     = errors.New("one installation requires multiple versions of the same Workshop item")
	ErrPreviewBlocked      = errors.New("mod publication preview contains blocking issues")
	ErrRecoveryRequired    = errors.New("mod publication requires recovery")
)

type Status string

const (
	StatusPreviewed        Status = "previewed"
	StatusPreparing        Status = "preparing"
	StatusPrepared         Status = "prepared"
	StatusPublishing       Status = "publishing"
	StatusCommitted        Status = "committed"
	StatusCompleting       Status = "completing"
	StatusSucceeded        Status = "succeeded"
	StatusFailed           Status = "failed"
	StatusRolledBack       Status = "rolled_back"
	StatusRecoveryRequired Status = "recovery_required"
)

type Outcome string

const (
	OutcomeFull    Outcome = "full"
	OutcomePartial Outcome = "partial"
	OutcomeNone    Outcome = "none"
)

type Blocker struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	TargetID       string `json:"targetId,omitempty"`
	InstallationID string `json:"installationId,omitempty"`
}

type ModRequirement struct {
	WorkshopID string `json:"workshopId"`
	TreeSHA256 string `json:"treeSha256,omitempty"`
}

type ContentArtifact struct {
	WorkshopID     string `json:"workshopId"`
	TreeSHA256     string `json:"treeSha256"`
	ManifestSHA256 string `json:"manifestSha256"`
	Size           int64  `json:"size"`
	FileCount      int    `json:"fileCount"`
	SourceRef      string `json:"sourceRef"`
}

type ManagedWorld struct {
	RoomID         string           `json:"roomId"`
	RoomDirectory  string           `json:"roomDirectory"`
	WorldID        string           `json:"worldId"`
	WorldDirectory string           `json:"worldDirectory"`
	Mods           []ModRequirement `json:"mods"`
	ModOverrides   []byte           `json:"modOverrides"`
}

type AppliedPlacement struct {
	RoomID         string `json:"roomId"`
	WorldID        string `json:"worldId"`
	TargetID       string `json:"targetId"`
	NodeID         string `json:"nodeId"`
	InstallationID string `json:"installationId"`
}

type PlacementSnapshot struct {
	TopologyRevision string             `json:"topologyRevision"`
	Placements       []AppliedPlacement `json:"placements"`
}

type RuntimeObservation struct {
	TargetID         string          `json:"targetId"`
	NodeID           string          `json:"nodeId"`
	InstallationID   string          `json:"installationId"`
	Online           bool            `json:"online"`
	Capabilities     []string        `json:"capabilities"`
	AvailableBytes   int64           `json:"availableBytes"`
	Version          string          `json:"version"`
	CachedTreeSHA256 map[string]bool `json:"cachedTreeSha256,omitempty"`
}

type WorldPlan struct {
	RoomID         string            `json:"roomId"`
	RoomDirectory  string            `json:"roomDirectory"`
	WorldID        string            `json:"worldId"`
	WorldDirectory string            `json:"worldDirectory"`
	Mods           []ContentArtifact `json:"mods"`
	ModOverrides   []byte            `json:"modOverrides"`
}

type TargetPlan struct {
	TargetID       string            `json:"targetId"`
	NodeID         string            `json:"nodeId"`
	InstallationID string            `json:"installationId"`
	Online         bool              `json:"online"`
	Capabilities   []string          `json:"capabilities"`
	RuntimeVersion string            `json:"runtimeVersion"`
	MinimumVersion string            `json:"minimumVersion,omitempty"`
	AvailableBytes int64             `json:"availableBytes"`
	RequiredBytes  int64             `json:"requiredBytes"`
	Mods           []ContentArtifact `json:"mods"`
	Worlds         []WorldPlan       `json:"worlds"`
	Blockers       []Blocker         `json:"blockers"`
}

type Plan struct {
	Version          int          `json:"version"`
	RoomID           string       `json:"roomId"`
	TopologyRevision string       `json:"topologyRevision"`
	PlanHash         string       `json:"planHash"`
	AffectedRoomIDs  []string     `json:"affectedRoomIds"`
	Targets          []TargetPlan `json:"targets"`
	Blockers         []Blocker    `json:"blockers"`
	Ready            bool         `json:"ready"`
	RestartRequired  bool         `json:"restartRequired"`
	CreatedAt        time.Time    `json:"createdAt"`
}

type TargetResult struct {
	TargetID       string     `json:"targetId"`
	InstallationID string     `json:"installationId"`
	Status         Status     `json:"status"`
	CacheEnsured   bool       `json:"cacheEnsured"`
	Prepared       bool       `json:"prepared"`
	Published      bool       `json:"published"`
	Completed      bool       `json:"completed"`
	RolledBack     bool       `json:"rolledBack"`
	ErrorCode      string     `json:"errorCode,omitempty"`
	ErrorMessage   string     `json:"errorMessage,omitempty"`
	PreparedAt     *time.Time `json:"preparedAt,omitempty"`
	PublishedAt    *time.Time `json:"publishedAt,omitempty"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
	RolledBackAt   *time.Time `json:"rolledBackAt,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

type Publication struct {
	ID                  string         `json:"id"`
	SourceJobID         string         `json:"sourceJobId,omitempty"`
	RoomID              string         `json:"roomId"`
	Status              Status         `json:"status"`
	Outcome             Outcome        `json:"outcome"`
	Plan                Plan           `json:"plan"`
	ProtectionBackupIDs []string       `json:"protectionBackupIds"`
	Fences              []Fence        `json:"fences"`
	CommitDecision      bool           `json:"commitDecision"`
	RestartRequired     bool           `json:"restartRequired"`
	ErrorCode           string         `json:"errorCode,omitempty"`
	ErrorMessage        string         `json:"errorMessage,omitempty"`
	Targets             []TargetResult `json:"targets"`
	CommitDecidedAt     *time.Time     `json:"commitDecidedAt,omitempty"`
	CreatedAt           time.Time      `json:"createdAt"`
	UpdatedAt           time.Time      `json:"updatedAt"`
	FinishedAt          *time.Time     `json:"finishedAt,omitempty"`
}

type PublishRequest struct {
	ID          string `json:"id"`
	SourceJobID string `json:"sourceJobId,omitempty"`
	Plan        Plan   `json:"plan"`
}

type Fence struct {
	RoomID       string    `json:"roomId"`
	LeaseID      string    `json:"leaseId"`
	FencingToken uint64    `json:"fencingToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

type RuntimeOperation struct {
	PublicationID  string  `json:"publicationId"`
	PlanHash       string  `json:"planHash"`
	Fences         []Fence `json:"fences"`
	Action         string  `json:"action"`
	IdempotencyKey string  `json:"idempotencyKey"`
}
