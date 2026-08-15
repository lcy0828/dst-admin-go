package gameupdate

import (
	"context"
	"errors"
	"time"

	"dont/internal/agents"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/shared"
)

const (
	ReleasePlanVersion       = 1
	RequiredUpdateCapability = "runtime.game-update.v1"
	DefaultUpdateHeadroom    = uint64(2 * 1024 * 1024 * 1024)
)

var (
	ErrReleaseInvalid         = errors.New("game release input is invalid")
	ErrReleaseNotFound        = errors.New("game release not found")
	ErrReleaseConflict        = errors.New("game release conflicts with active state")
	ErrReleasePlanChanged     = errors.New("game release plan changed")
	ErrReleasePreviewBlocked  = errors.New("game release preview contains blockers")
	ErrDesiredVersionChanged  = errors.New("desired Steam build changed")
	ErrReleaseRecoveryNeeded  = errors.New("game release requires recovery")
	ErrReleaseTopologyChanged = errors.New("game release topology changed")
	ErrReleaseConfirmation    = errors.New("game release confirmation is invalid")
)

type ReleaseStage string

const (
	ReleaseStagePreviewed        ReleaseStage = "previewed"
	ReleaseStageProtecting       ReleaseStage = "protecting"
	ReleaseStageStopping         ReleaseStage = "stopping"
	ReleaseStageStaged           ReleaseStage = "staged"
	ReleaseStageUpdating         ReleaseStage = "updating"
	ReleaseStageVerified         ReleaseStage = "verified"
	ReleaseStageRestarting       ReleaseStage = "restarting"
	ReleaseStageConfirming       ReleaseStage = "confirming"
	ReleaseStageSucceeded        ReleaseStage = "succeeded"
	ReleaseStageFailed           ReleaseStage = "failed"
	ReleaseStageRecoveryRequired ReleaseStage = "recovery_required"
)

type ReleaseLoadConfirmation string

const (
	ReleaseLoadConfirmationLogs ReleaseLoadConfirmation = "logs"
	ReleaseLoadConfirmationNone ReleaseLoadConfirmation = "none"
)

type ReleasePolicy struct {
	CleanCache       bool                    `json:"cleanCache"`
	RestartRunning   bool                    `json:"restartRunning"`
	LoadConfirmation ReleaseLoadConfirmation `json:"loadConfirmation"`
	TimeoutSeconds   int                     `json:"timeoutSeconds"`
}

type ReleasePolicyInput struct {
	CleanCache       bool                    `json:"cleanCache"`
	RestartRunning   *bool                   `json:"restartRunning,omitempty"`
	LoadConfirmation ReleaseLoadConfirmation `json:"loadConfirmation,omitempty"`
	TimeoutSeconds   int                     `json:"timeoutSeconds,omitempty"`
}

type ReleasePreviewRequest struct {
	DesiredVersion string             `json:"desiredVersion,omitempty"`
	Policy         ReleasePolicyInput `json:"policy"`
}

type ReleaseCreateRequest struct {
	DesiredVersion string             `json:"desiredVersion,omitempty"`
	Policy         ReleasePolicyInput `json:"policy"`
	PlanHash       string             `json:"planHash"`
	Confirmation   string             `json:"confirmation"`
}

type ReleaseBlocker struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	TargetID       string `json:"targetId,omitempty"`
	InstallationID string `json:"installationId,omitempty"`
	RoomID         string `json:"roomId,omitempty"`
	WorldID        string `json:"worldId,omitempty"`
}

type ReleaseShardPlan struct {
	RoomID           string `json:"roomId"`
	RoomName         string `json:"roomName"`
	RoomDirectory    string `json:"roomDirectory"`
	WorldID          string `json:"worldId"`
	WorldName        string `json:"worldName"`
	WorldDirectory   string `json:"worldDirectory"`
	IsMaster         bool   `json:"isMaster"`
	TargetID         string `json:"targetId"`
	InstallationID   string `json:"installationId"`
	TopologyRevision string `json:"topologyRevision"`
	RuntimeState     string `json:"runtimeState,omitempty"`
	WasRunning       bool   `json:"wasRunning"`
}

type ReleaseInstallationPlan struct {
	TargetID          string             `json:"targetId"`
	TargetName        string             `json:"targetName"`
	InstallationID    string             `json:"installationId"`
	Online            bool               `json:"online"`
	InventoryFresh    bool               `json:"inventoryFresh"`
	Capabilities      []string           `json:"capabilities"`
	Installed         bool               `json:"installed"`
	CurrentVersion    string             `json:"currentVersion,omitempty"`
	DesiredVersion    string             `json:"desiredVersion"`
	UpToDate          bool               `json:"upToDate"`
	AvailableBytes    uint64             `json:"availableBytes"`
	RequiredBytes     uint64             `json:"requiredBytes"`
	SteamCMDAvailable bool               `json:"steamcmdAvailable"`
	UpdateSupported   bool               `json:"updateSupported"`
	RunningShards     int                `json:"runningShards"`
	Shards            []ReleaseShardPlan `json:"shards"`
	Blockers          []ReleaseBlocker   `json:"blockers"`
}

type ReleasePlan struct {
	Version          int                       `json:"version"`
	DesiredVersion   string                    `json:"desiredVersion"`
	TopologyRevision string                    `json:"topologyRevision"`
	PlanHash         string                    `json:"planHash"`
	Policy           ReleasePolicy             `json:"policy"`
	AffectedRoomIDs  []string                  `json:"affectedRoomIds"`
	Installations    []ReleaseInstallationPlan `json:"installations"`
	Blockers         []ReleaseBlocker          `json:"blockers"`
	Ready            bool                      `json:"ready"`
	UpdateRequired   bool                      `json:"updateRequired"`
	CreatedAt        time.Time                 `json:"createdAt"`
}

type ReleaseInstallationResult struct {
	TargetID       string       `json:"targetId"`
	InstallationID string       `json:"installationId"`
	Stage          ReleaseStage `json:"stage"`
	BeforeVersion  string       `json:"beforeVersion,omitempty"`
	AfterVersion   string       `json:"afterVersion,omitempty"`
	Log            string       `json:"log,omitempty"`
	ErrorCode      string       `json:"errorCode,omitempty"`
	ErrorMessage   string       `json:"errorMessage,omitempty"`
	StartedAt      *time.Time   `json:"startedAt,omitempty"`
	FinishedAt     *time.Time   `json:"finishedAt,omitempty"`
	UpdatedAt      time.Time    `json:"updatedAt"`
}

type ReleaseShardResult struct {
	RoomID          string       `json:"roomId"`
	WorldID         string       `json:"worldId"`
	TargetID        string       `json:"targetId"`
	InstallationID  string       `json:"installationId"`
	IsMaster        bool         `json:"isMaster"`
	WasRunning      bool         `json:"wasRunning"`
	Stage           ReleaseStage `json:"stage"`
	RuntimeState    string       `json:"runtimeState,omitempty"`
	LoadMarker      string       `json:"loadMarker,omitempty"`
	ErrorCode       string       `json:"errorCode,omitempty"`
	ErrorMessage    string       `json:"errorMessage,omitempty"`
	StoppedAt       *time.Time   `json:"stoppedAt,omitempty"`
	StartedAt       *time.Time   `json:"startedAt,omitempty"`
	LoadConfirmedAt *time.Time   `json:"loadConfirmedAt,omitempty"`
	UpdatedAt       time.Time    `json:"updatedAt"`
}

type Release struct {
	ID                  string                      `json:"id"`
	SourceJobID         string                      `json:"sourceJobId,omitempty"`
	Stage               ReleaseStage                `json:"stage"`
	Plan                ReleasePlan                 `json:"plan"`
	ProtectionBackupIDs []string                    `json:"protectionBackupIds"`
	ErrorCode           string                      `json:"errorCode,omitempty"`
	ErrorMessage        string                      `json:"errorMessage,omitempty"`
	Installations       []ReleaseInstallationResult `json:"installations"`
	Shards              []ReleaseShardResult        `json:"shards"`
	CreatedAt           time.Time                   `json:"createdAt"`
	UpdatedAt           time.Time                   `json:"updatedAt"`
	FinishedAt          *time.Time                  `json:"finishedAt,omitempty"`
}

type ReleasePublishRequest struct {
	ID          string      `json:"id"`
	SourceJobID string      `json:"sourceJobId,omitempty"`
	Plan        ReleasePlan `json:"plan"`
}

type ReleasePlacementSnapshot struct {
	TopologyRevision string
	Shards           []ReleaseShardSnapshot
}

type ReleaseShardSnapshot struct {
	Room               rooms.Room
	World              rooms.World
	Target             agents.RuntimeTarget
	InventoryAvailable bool
	InventoryStale     bool
	InventoryHasShard  bool
	TopologyRevision   string
}

type ReleaseSnapshotSource interface {
	Snapshot(context.Context) (ReleasePlacementSnapshot, error)
}

type ReleaseRuntime interface {
	ObserveInstallation(context.Context, ReleaseInstallationPlan) (shared.RuntimeGameVersionResult, error)
	UpdateInstallation(context.Context, ReleaseInstallationPlan, runtimedriver.Operation, string, bool) (shared.RuntimeGameVersionResult, error)
	Status(context.Context, ReleaseShardPlan) (shared.ShardRuntimeStatus, error)
	Stop(context.Context, ReleaseShardPlan, runtimedriver.Operation) error
	Start(context.Context, ReleaseShardPlan, runtimedriver.Operation) error
	CaptureLogCursor(context.Context, ReleaseShardPlan) (string, int64, error)
	ReadLogs(context.Context, ReleaseShardPlan, string, int64) (shared.RuntimeLogChunk, error)
}

type ReleaseLeaseService interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Renew(context.Context, operationlease.Lease, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

type ReleaseProtectionService interface {
	CreateProtection(context.Context, string, string, string, *operationlease.Lease) (string, error)
}
