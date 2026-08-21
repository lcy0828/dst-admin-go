package distributedbackup

import (
	"errors"
	"time"
)

var (
	ErrNotFound           = errors.New("distributed backup set not found")
	ErrInvalidInput       = errors.New("distributed backup input is invalid")
	ErrTopologyChanged    = errors.New("room topology does not match the backup set")
	ErrTargetUnavailable  = errors.New("backup runtime target is unavailable")
	ErrIntegrity          = errors.New("backup set integrity verification failed")
	ErrIncomplete         = errors.New("backup set is not complete")
	ErrSharedFilesDiffer  = errors.New("cluster shared files differ across runtime targets")
	ErrRecoveryIncomplete = errors.New("backup recovery requires operator attention")
	ErrHotUnavailable     = errors.New("hot-consistent backup is unavailable for this room")
	ErrBarrierFailed      = errors.New("cross-shard snapshot barrier failed")
	ErrNotRestorable      = errors.New("backup set does not contain restorable game save data")
)

type Status string

const (
	StatusCreating Status = "creating"
	StatusVerified Status = "verified"
	StatusPartial  Status = "partial"
	StatusFailed   Status = "failed"
	StatusCorrupt  Status = "corrupt"
)

type PartStatus string

const (
	PartPending  PartStatus = "pending"
	PartStaging  PartStatus = "staging"
	PartVerified PartStatus = "verified"
	PartFailed   PartStatus = "failed"
)

type Set struct {
	ID                    string     `json:"id"`
	RoomID                string     `json:"roomId"`
	RoomName              string     `json:"roomName"`
	Name                  string     `json:"name"`
	Kind                  string     `json:"kind"`
	Mode                  string     `json:"mode"`
	ManifestVersion       int        `json:"manifestVersion"`
	TopologyRevision      string     `json:"topologyRevision"`
	BarrierID             string     `json:"barrierId,omitempty"`
	Snapshot              int64      `json:"snapshot,omitempty"`
	SharedSHA256          string     `json:"sharedSha256,omitempty"`
	Status                Status     `json:"status"`
	Size                  int64      `json:"size"`
	ContentSize           int64      `json:"contentSize"`
	FileCount             int        `json:"fileCount"`
	ContentKind           string     `json:"contentKind"`
	Restorable            bool       `json:"restorable"`
	ValidationError       string     `json:"validationError,omitempty"`
	OriginalRunningWorlds []string   `json:"originalRunningWorlds"`
	ManifestSHA256        string     `json:"manifestSha256,omitempty"`
	Failure               string     `json:"failure,omitempty"`
	SourceJobID           string     `json:"sourceJobId,omitempty"`
	VerifiedAt            *time.Time `json:"verifiedAt,omitempty"`
	CreatedAt             time.Time  `json:"createdAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
	Parts                 []Part     `json:"parts"`
}

type Part struct {
	ID               string     `json:"id"`
	SetID            string     `json:"setId"`
	RoomID           string     `json:"roomId"`
	WorldID          string     `json:"worldId"`
	WorldName        string     `json:"worldName"`
	WorldRole        string     `json:"worldRole"`
	TargetID         string     `json:"targetId"`
	InstallationID   string     `json:"installationId"`
	Cluster          string     `json:"cluster"`
	Shard            string     `json:"shard"`
	TopologyRevision string     `json:"topologyRevision"`
	BarrierSessionID string     `json:"barrierSessionId,omitempty"`
	BarrierShardID   string     `json:"barrierShardId,omitempty"`
	BarrierInstance  string     `json:"barrierInstanceId,omitempty"`
	SnapshotBefore   int64      `json:"snapshotBefore,omitempty"`
	SnapshotAfter    int64      `json:"snapshotAfter,omitempty"`
	BarrierCompleted *time.Time `json:"barrierCompletedAt,omitempty"`
	FileName         string     `json:"fileName"`
	Status           PartStatus `json:"status"`
	Size             int64      `json:"size"`
	ContentSize      int64      `json:"contentSize"`
	FileCount        int        `json:"fileCount"`
	ContentKind      string     `json:"contentKind"`
	Restorable       bool       `json:"restorable"`
	SessionID        string     `json:"sessionId,omitempty"`
	LatestSnapshot   string     `json:"latestSnapshot,omitempty"`
	HasShardIndex    bool       `json:"hasShardIndex"`
	ValidationError  string     `json:"validationError,omitempty"`
	SHA256           string     `json:"sha256,omitempty"`
	SharedSHA256     string     `json:"sharedSha256,omitempty"`
	Failure          string     `json:"failure,omitempty"`
	VerifiedAt       *time.Time `json:"verifiedAt,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
}

type OperationStatus string

const (
	OperationRunning          OperationStatus = "running"
	OperationSucceeded        OperationStatus = "succeeded"
	OperationRolledBack       OperationStatus = "rolled_back"
	OperationRecoveryRequired OperationStatus = "recovery_required"
	OperationFailed           OperationStatus = "failed"
)

type Operation struct {
	ID                    string          `json:"id"`
	SetID                 string          `json:"setId"`
	ProtectionSetID       string          `json:"protectionSetId,omitempty"`
	RoomID                string          `json:"roomId"`
	Kind                  string          `json:"kind"`
	Phase                 string          `json:"phase"`
	Status                OperationStatus `json:"status"`
	TopologyRevision      string          `json:"topologyRevision"`
	LeaseID               string          `json:"leaseId,omitempty"`
	FencingToken          uint64          `json:"fencingToken"`
	OriginalRunningWorlds []string        `json:"originalRunningWorlds"`
	Failure               string          `json:"failure,omitempty"`
	CreatedAt             time.Time       `json:"createdAt"`
	UpdatedAt             time.Time       `json:"updatedAt"`
}

type CreateRequest struct {
	Name string `json:"name"`
	Mode string `json:"mode,omitempty"`
}

type RestoreRequest struct {
	Confirmation string `json:"confirmation"`
}

type RestoreResult struct {
	SetID           string   `json:"setId"`
	OperationID     string   `json:"operationId"`
	ProtectionSetID string   `json:"protectionSetId"`
	Warnings        []string `json:"warnings"`
}
