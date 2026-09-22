package runtimedriver

import (
	"context"
	"errors"
	"time"

	"dont/shared"
)

var (
	ErrInvalidTarget           = errors.New("runtime driver target is invalid")
	ErrTopologyChanged         = errors.New("runtime target topology revision changed")
	ErrCapabilityMissing       = errors.New("runtime driver capability is unavailable")
	ErrUnsupportedRuntime      = errors.New("runtime driver kind is unsupported")
	ErrOperationNotDispatched  = errors.New("runtime operation was not dispatched")
	ErrMigrationPeerFallback   = errors.New("migration peer transfer may safely fall back")
	ErrRoomRecoveryMultiTarget = errors.New("room recovery requires every world to use one runtime installation")
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
	CapabilityChatHistory     Capability = "chatHistory"
	CapabilityArtifacts       Capability = "artifacts"
	CapabilityWorldStateRead  Capability = "worldStateRead"
	CapabilityEntityArtwork   Capability = "entityArtwork"
	CapabilitySnapshotBarrier Capability = "snapshotBarrier"
	CapabilityBackupStage     Capability = "backupStage"
	CapabilityBackupRestore   Capability = "backupRestore"
	CapabilityModPrepare      Capability = "modPrepare"
	CapabilityModPublish      Capability = "modPublish"
	CapabilityGameUpdate      Capability = "gameUpdate"
	CapabilityExclusiveCPU    Capability = "exclusiveCpu"
	CapabilityPublishedUDP    Capability = "publishedUdpEndpoint"
	CapabilityConfigRead      Capability = "configRead"
	CapabilityConfigSecrets   Capability = "configSecrets"
	CapabilityConfigPublish   Capability = "configPublish"
	CapabilityConfigApply     Capability = "configApply"
	CapabilityMapRender       Capability = "mapRender"
	CapabilityShardRouting    Capability = "shardRouting"
	CapabilityMigrationPeer   Capability = "migrationPeer"
	CapabilityRoomRecovery    Capability = "roomRecovery"
)

type Target struct {
	TargetID          string
	InstallationID    string
	RoomID            string
	WorldID           string
	Cluster           string
	Shard             string
	TopologyRevision  string
	Capabilities      []Capability
	CapabilitiesKnown bool
}

type Operation struct {
	ID             string
	Key            string
	LeaseID        string
	FencingToken   uint64
	LeaseExpiresAt *time.Time
	RuntimeMode    shared.RuntimePerformanceMode
	LaunchOptions  shared.RuntimeLaunchOptions
}

type RoomRecoveryDriver interface {
	MoveRoomToRecovery(context.Context, Target, Operation) (string, error)
}

type WorldStateReader interface {
	ReadWorldState(context.Context, Target) (shared.RuntimeWorldStateRead, error)
}

type RoomRecoveryLocation struct {
	TargetID       string
	InstallationID string
	RecoveryRef    string
}

type MigrationDescriptor struct {
	MigrationID        string
	Size               int64
	SHA256             string
	RecoveryRef        string
	ShardBindAll       bool
	ShardMasterAddress string
	ShardMasterPort    int
}

type MigrationChunk struct {
	Offset     int64
	NextOffset int64
	Size       int64
	SHA256     string
	Data       []byte
	Complete   bool
}

// MigrationPeerSource and MigrationPeerTarget are optional. Keeping peer
// transfer outside Driver preserves the direct in-process path and lets old
// Agents continue through the Controller relay without pretending support.
type MigrationPeerSource interface {
	GrantMigrationExport(context.Context, Target, MigrationDescriptor, string) (shared.RuntimeMigrationFetchLocation, error)
}

type MigrationPeerTarget interface {
	FetchMigrationImport(context.Context, Target, Operation, MigrationDescriptor, []shared.RuntimeMigrationFetchLocation) (int64, error)
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

type ModSchemaReader interface {
	ReadModSchema(context.Context, Target, string) (shared.RuntimeModSchema, error)
}

// ModDriver is intentionally separate from Driver so existing runtime
// providers do not acquire Mod distribution methods they cannot implement.
type ModDriver interface {
	ObserveModTarget(context.Context, Target) (int64, string, error)
	InspectModCache(context.Context, Target, string, string) (shared.RuntimeModCacheManifest, error)
	FetchModCache(context.Context, Target, Operation, string, string, shared.RuntimeModMetadata, []shared.RuntimeModFetchLocation) (ModFetchResult, error)
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
	ModInstallationState(context.Context, Target) (*shared.RuntimeModInstallationState, error)
	ObserveModFiles(context.Context, Target, []string, []shared.RuntimeModObserveWorld) (*shared.RuntimeModFilesObservation, error)
	InventoryModFiles(context.Context, Target) (*shared.RuntimeModFilesObservation, error)
	ReadModOverrides(context.Context, Target, string, string, int64) (shared.RuntimeModOverridesChunk, error)
}

// ModDownloadDriver is the direct installation-scoped Workshop download path.
// It intentionally bypasses exact-artifact cache and publication operations.
type ModDownloadDriver interface {
	DownloadMods(context.Context, Target, Operation, []string) error
}

type ModLocalLinkDriver interface {
	LinkMods(context.Context, Target, Operation, []string) error
}

type ModFetchResult struct {
	Manifest shared.RuntimeModCacheManifest
	Attempts []shared.RuntimeModFetchAttempt
}

// ModPeerDriver is optional. Remote runtimes expose it only when an Agent has
// an explicitly configured, externally reachable Mod artifact listener.
type ModPeerDriver interface {
	GrantModArtifact(context.Context, Target, string, string, string) (shared.RuntimeModFetchLocation, error)
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

type ConfigurationDescriptor struct {
	PublicationID string
	Scope         string
	Size          int64
	SHA256        string
}

type ConfigurationSnapshot struct {
	Target Target
	Result shared.RuntimeConfigurationResult
}

// ConfigurationReader exposes a fixed, path-free configuration snapshot.
// It is implemented by both the controller-local Runtime and remote Agents.
type ConfigurationReader interface {
	ReadConfiguration(context.Context, Target, string) (shared.RuntimeConfigurationResult, error)
}

type ModOverridesUpdate struct {
	Target         Target
	ExpectedSHA256 string
	Content        []byte
}

type ModOverridesWriter interface {
	WriteModOverrides(context.Context, Target, Operation, string, []byte) error
}

// ClusterTokenReader is deliberately separate from ConfigurationReader so
// secret material cannot be requested through the ordinary snapshot surface.
type ClusterTokenReader interface {
	RevealClusterToken(context.Context, Target) (shared.RuntimeClusterTokenReveal, error)
}

// ConfigurationDriver is separate because older Agents may support lifecycle
// operations without supporting transactional configuration publication.
type ConfigurationDriver interface {
	BeginConfiguration(context.Context, Target, Operation, ConfigurationDescriptor) (int64, error)
	WriteConfiguration(context.Context, Target, Operation, ConfigurationDescriptor, int64, []byte) (int64, error)
	PrepareConfiguration(context.Context, Target, Operation, string, string) error
	PublishConfiguration(context.Context, Target, Operation, string, string) error
	RollbackConfiguration(context.Context, Target, Operation, string, string) error
	CompleteConfiguration(context.Context, Target, Operation, string, string) error
}

type ConfigurationApplier interface {
	ApplyConfiguration(context.Context, Target, Operation, ConfigurationDescriptor, []byte, map[string]string) ([]string, error)
}

type MapDescriptor struct {
	TransferID   string
	Size         int64
	SHA256       string
	SourceSHA256 string
	Log          string
}

type MapChunk struct {
	MapDescriptor
	Offset     int64
	NextOffset int64
	Data       []byte
	Complete   bool
}

// MapDriver keeps Session files and official game assets on the Runtime node.
// The controller receives only bounded Session metadata or verified artifacts.
type MapDriver interface {
	ListMapSessions(context.Context, Target) ([]shared.RuntimeMapSession, error)
	MapRendererStatus(context.Context, Target) (shared.RuntimeMapRenderer, error)
	PrepareMapSnapshot(context.Context, Target, Operation, string, string, string) (MapDescriptor, error)
	RenderMap(context.Context, Target, Operation, string, string, string, []string) (MapDescriptor, error)
	ReadMapTransfer(context.Context, Target, string, int64) (MapChunk, error)
	ReleaseMapTransfer(context.Context, Target, Operation, string) error
}

// ChatLogDriver exposes only enumerated DST chat log generations. Callers
// cannot provide a host path, which keeps local and Agent access equivalent.
type ChatLogDriver interface {
	ListChatLogGenerations(context.Context, Target) ([]shared.RuntimeChatLogGeneration, error)
	ReadChatLogGeneration(context.Context, Target, shared.RuntimeChatLogRequest) (shared.RuntimeChatLogResult, error)
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

// HasTargetCapability separates code-level driver support from the capability
// snapshot advertised by one concrete Runtime target. Test adapters without
// target metadata retain the driver-level fallback.
func HasTargetCapability(driver Driver, target Target, expected Capability) bool {
	if !HasCapability(driver, expected) {
		return false
	}
	if !target.CapabilitiesKnown {
		return true
	}
	for _, capability := range target.Capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}

func runtimeCapabilities(values []string) []Capability {
	result := make([]Capability, 0, len(values))
	appendCapability := func(capability Capability) {
		for _, existing := range result {
			if existing == capability {
				return
			}
		}
		result = append(result, capability)
	}
	for _, value := range values {
		switch value {
		case "shard.control.v1":
			appendCapability(CapabilityLifecycle)
		case "runtime.console.v2":
			appendCapability(CapabilityConsoleInput)
			appendCapability(CapabilityConsoleHealth)
			appendCapability(CapabilityRawConsole)
		case "runtime.driver.v2":
			appendCapability(CapabilityOperationProof)
		case "runtime.logs.v1":
			appendCapability(CapabilityLogContinuation)
		case "runtime.chat-history.v1":
			appendCapability(CapabilityChatHistory)
		case "runtime.artifacts.v1":
			appendCapability(CapabilityArtifacts)
		case "runtime.entity-artwork.v1":
			appendCapability(CapabilityEntityArtwork)
		case "runtime.worldstate.read.v1":
			appendCapability(CapabilityWorldStateRead)
		case "runtime.backup.v1":
			appendCapability(CapabilitySnapshotBarrier)
			appendCapability(CapabilityBackupStage)
			appendCapability(CapabilityBackupRestore)
		case "runtime.mods.v1":
			appendCapability(CapabilityModPrepare)
			appendCapability(CapabilityModPublish)
		case "runtime.game-update.v1":
			appendCapability(CapabilityGameUpdate)
		case "runtime.cpu.v1":
			appendCapability(CapabilityExclusiveCPU)
		case "runtime.configuration.read.v1":
			appendCapability(CapabilityConfigRead)
		case "runtime.configuration.secrets.v1":
			appendCapability(CapabilityConfigSecrets)
		case "runtime.configuration.v1":
			appendCapability(CapabilityConfigPublish)
		case "runtime.configuration.apply.v1":
			appendCapability(CapabilityConfigApply)
		case "runtime.maps.v1":
			appendCapability(CapabilityMapRender)
		case "runtime.migration.shard-routing.v1":
			appendCapability(CapabilityShardRouting)
		case "runtime.migration.peer.v1":
			appendCapability(CapabilityMigrationPeer)
		}
	}
	return result
}
