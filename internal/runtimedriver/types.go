package runtimedriver

import (
	"context"
	"errors"
	"time"

	"dont/shared"
)

var (
	ErrInvalidTarget          = errors.New("runtime driver target is invalid")
	ErrTopologyChanged        = errors.New("runtime target topology revision changed")
	ErrCapabilityMissing      = errors.New("runtime driver capability is unavailable")
	ErrUnsupportedRuntime     = errors.New("runtime driver kind is unsupported")
	ErrOperationNotDispatched = errors.New("runtime operation was not dispatched")
)

type Kind string

const (
	KindNative     Kind = "native"
	KindContainer  Kind = "container"
	KindKubernetes Kind = "kubernetes"
)

type Capability string

const (
	CapabilityLifecycle       Capability = "lifecycle"
	CapabilityConsoleInput    Capability = "consoleInput"
	CapabilityConsoleHealth   Capability = "consoleHealth"
	CapabilityRawConsole      Capability = "rawConsole"
	CapabilityOperationProof  Capability = "operationProof"
	CapabilityLogContinuation Capability = "logContinuation"
	CapabilityArtifacts       Capability = "artifacts"
	CapabilitySnapshotBarrier Capability = "snapshotBarrier"
	CapabilityBackupStage     Capability = "backupStage"
	CapabilityBackupRestore   Capability = "backupRestore"
	CapabilityModPrepare      Capability = "modPrepare"
	CapabilityModPublish      Capability = "modPublish"
	CapabilityGameUpdate      Capability = "gameUpdate"
	CapabilityExclusiveCPU    Capability = "exclusiveCpu"
	CapabilityPublishedUDP    Capability = "publishedUdpEndpoint"
)

type Target struct {
	TargetID         string
	InstallationID   string
	RoomID           string
	WorldID          string
	Cluster          string
	Shard            string
	TopologyRevision string
}

type Operation struct {
	ID             string
	Key            string
	LeaseID        string
	FencingToken   uint64
	LeaseExpiresAt *time.Time
}

type MigrationDescriptor struct {
	MigrationID string
	Size        int64
	SHA256      string
	RecoveryRef string
}

type MigrationChunk struct {
	Offset     int64
	NextOffset int64
	Size       int64
	SHA256     string
	Data       []byte
	Complete   bool
}

type BackupDescriptor struct {
	BackupID     string
	Size         int64
	ContentSize  int64
	FileCount    int
	SHA256       string
	SharedSHA256 string
}

type BackupChunk struct {
	Offset     int64
	NextOffset int64
	Size       int64
	SHA256     string
	Data       []byte
	Complete   bool
}

type ModUploadDescriptor struct {
	UploadID           string
	Kind               shared.RuntimeModUploadKind
	OperationID        string
	WorkshopID         string
	ExpectedTreeSHA256 string
	Size               int64
	SHA256             string
	Metadata           shared.RuntimeModMetadata
}

// ModDriver is intentionally separate from Driver so existing runtime
// providers do not acquire Mod distribution methods they cannot implement.
type ModDriver interface {
	ObserveModTarget(context.Context, Target) (int64, string, error)
	InspectModCache(context.Context, Target, string, string) (shared.RuntimeModCacheManifest, error)
	BeginModUpload(context.Context, Target, Operation, ModUploadDescriptor) (int64, error)
	WriteModUpload(context.Context, Target, Operation, ModUploadDescriptor, int64, []byte) (int64, error)
	CommitModUpload(context.Context, Target, Operation, ModUploadDescriptor) (shared.RuntimeModCacheManifest, error)
	BeginModReleasePlan(context.Context, Target, Operation, ModUploadDescriptor) (int64, error)
	WriteModReleasePlan(context.Context, Target, Operation, ModUploadDescriptor, int64, []byte) (int64, error)
	CommitModReleasePlan(context.Context, Target, Operation, ModUploadDescriptor) (shared.RuntimeModReleaseState, error)
	PrepareModRelease(context.Context, Target, Operation, string) (shared.RuntimeModReleaseState, error)
	PublishModRelease(context.Context, Target, Operation, string) (shared.RuntimeModReleaseState, error)
	RollbackModRelease(context.Context, Target, Operation, string) (shared.RuntimeModReleaseState, error)
	CompleteModRelease(context.Context, Target, Operation, string) (shared.RuntimeModReleaseState, error)
	ModReleaseState(context.Context, Target, string) (shared.RuntimeModReleaseState, error)
	ReadModOverrides(context.Context, Target, string, string, int64) (shared.RuntimeModOverridesChunk, error)
}

// GameVersionDriver is installation-scoped and intentionally separate from
// Driver because lifecycle targets and game installations have different
// capability boundaries.
type GameVersionDriver interface {
	ObserveGameVersion(context.Context, Target) (shared.RuntimeGameVersionResult, error)
	UpdateGameVersion(context.Context, Target, Operation, string, bool) (shared.RuntimeGameVersionResult, error)
}

// CPUDriver is separate from Driver because CPU enforcement is optional and
// only available when a Runtime can prove the effective process/container
// resource boundary.
type CPUDriver interface {
	PrepareCPU(context.Context, Target, Operation, shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error)
	ApplyCPU(context.Context, Target, Operation, shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error)
	ObserveCPU(context.Context, Target, shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error)
}

type SnapshotBarrierReceipt struct {
	SchemaVersion      int       `json:"schemaVersion"`
	ProducerVersion    string    `json:"producerVersion"`
	ProducerInstanceID string    `json:"producerInstanceId"`
	BarrierID          string    `json:"barrierId"`
	State              string    `json:"state"`
	SessionID          string    `json:"sessionId"`
	ShardID            string    `json:"shardId"`
	SnapshotBefore     int64     `json:"snapshotBefore"`
	SnapshotAfter      int64     `json:"snapshotAfter,omitempty"`
	PreparedAtUnix     int64     `json:"preparedAtUnix"`
	CompletedAtUnix    int64     `json:"completedAtUnix,omitempty"`
	ReleasedAtUnix     int64     `json:"releasedAtUnix,omitempty"`
	Proof              string    `json:"proof,omitempty"`
	Message            string    `json:"message,omitempty"`
	ReadAt             time.Time `json:"-"`
}

// SnapshotBarrierDriver is optional because a provider may support cold
// staging without having a running DST Runtime capable of proving hot saves.
type SnapshotBarrierDriver interface {
	PrepareSnapshotBarrier(context.Context, Target, Operation, string) (SnapshotBarrierReceipt, error)
	CommitSnapshotBarrier(context.Context, Target, Operation, string) error
	SnapshotBarrier(context.Context, Target, string) (SnapshotBarrierReceipt, error)
	ReleaseSnapshotBarrier(context.Context, Target, Operation, string) error
	CancelSnapshotBarrier(context.Context, Target, Operation, string) error
}

type Driver interface {
	Kind() Kind
	Capabilities() []Capability
	Status(context.Context, Target) (shared.ShardRuntimeStatus, error)
	ExecuteShard(context.Context, Target, Operation, shared.ShardAction, time.Duration) (shared.ShardOperationResult, error)
	SendConsole(context.Context, Target, Operation, shared.RuntimeConsoleRequest, time.Duration) (shared.RuntimeOperationResult, error)
	ConsoleHealth(context.Context, Target) (shared.RuntimeConsoleHealth, error)
	ObserveOperation(context.Context, Target, string, string) (shared.RuntimeOperationEvidence, error)
	ReadLogs(context.Context, Target, shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error)
	ReadArtifacts(context.Context, Target, shared.ArtifactKind) (shared.RuntimeArtifactBundle, error)
	PrepareMigrationExport(context.Context, Target, Operation, string) (MigrationDescriptor, error)
	ReadMigrationExport(context.Context, Target, string, int64) (MigrationChunk, error)
	ReleaseMigrationExport(context.Context, Target, Operation, string) error
	BeginMigrationImport(context.Context, Target, Operation, MigrationDescriptor) error
	WriteMigrationImport(context.Context, Target, Operation, MigrationDescriptor, int64, []byte) (int64, error)
	CommitMigrationImport(context.Context, Target, Operation, string) error
	RollbackMigrationTarget(context.Context, Target, Operation, string) error
	CompleteMigrationTarget(context.Context, Target, Operation, string) error
	FinalizeMigrationSource(context.Context, Target, Operation, string) (string, error)
	RollbackMigrationSource(context.Context, Target, Operation, string) error
	CompleteMigrationSource(context.Context, Target, Operation, string) (string, error)
	StageBackup(context.Context, Target, Operation, string) (BackupDescriptor, error)
	ReadBackup(context.Context, Target, string, int64) (BackupChunk, error)
	ReleaseBackup(context.Context, Target, Operation, string) error
	BeginRestore(context.Context, Target, Operation, BackupDescriptor) error
	WriteRestore(context.Context, Target, Operation, BackupDescriptor, int64, []byte) (int64, error)
	PrepareRestore(context.Context, Target, Operation, string) error
	PublishRestore(context.Context, Target, Operation, string, bool) (string, error)
	RollbackRestore(context.Context, Target, Operation, string) error
	CompleteRestore(context.Context, Target, Operation, string) (string, error)
}

func HasCapability(driver Driver, expected Capability) bool {
	if driver == nil {
		return false
	}
	for _, capability := range driver.Capabilities() {
		if capability == expected {
			return true
		}
	}
	return false
}
