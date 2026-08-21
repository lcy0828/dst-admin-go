package shared

import "time"

const (
	RuntimeOperationProtocolVersion = 2
	MaxChunkBytes                   = 256 * 1024
)

type RuntimeAction string

type RuntimeLogSource string

const (
	RuntimeActionConsoleHealth           RuntimeAction = "runtime.console.health"
	RuntimeActionConsoleSend             RuntimeAction = "runtime.console.send"
	RuntimeActionObserveOperation        RuntimeAction = "runtime.operation.observe"
	RuntimeActionReadLogs                RuntimeAction = "runtime.logs.read"
	RuntimeActionReadArtifacts           RuntimeAction = "runtime.artifacts.read"
	RuntimeActionMigrationExportPrepare  RuntimeAction = "runtime.migration.export.prepare"
	RuntimeActionMigrationExportRead     RuntimeAction = "runtime.migration.export.read"
	RuntimeActionMigrationExportRelease  RuntimeAction = "runtime.migration.export.release"
	RuntimeActionMigrationImportBegin    RuntimeAction = "runtime.migration.import.begin"
	RuntimeActionMigrationImportWrite    RuntimeAction = "runtime.migration.import.write"
	RuntimeActionMigrationImportCommit   RuntimeAction = "runtime.migration.import.commit"
	RuntimeActionMigrationTargetRollback RuntimeAction = "runtime.migration.target.rollback"
	RuntimeActionMigrationTargetComplete RuntimeAction = "runtime.migration.target.complete"
	RuntimeActionMigrationSourceFinalize RuntimeAction = "runtime.migration.source.finalize"
	RuntimeActionMigrationSourceRollback RuntimeAction = "runtime.migration.source.rollback"
	RuntimeActionMigrationSourceComplete RuntimeAction = "runtime.migration.source.complete"
	RuntimeActionBackupStage             RuntimeAction = "runtime.backup.stage"
	RuntimeActionBackupRead              RuntimeAction = "runtime.backup.read"
	RuntimeActionBackupRelease           RuntimeAction = "runtime.backup.release"
	RuntimeActionRestoreBegin            RuntimeAction = "runtime.restore.begin"
	RuntimeActionRestoreWrite            RuntimeAction = "runtime.restore.write"
	RuntimeActionRestorePrepare          RuntimeAction = "runtime.restore.prepare"
	RuntimeActionRestorePublish          RuntimeAction = "runtime.restore.publish"
	RuntimeActionRestoreRollback         RuntimeAction = "runtime.restore.rollback"
	RuntimeActionRestoreComplete         RuntimeAction = "runtime.restore.complete"
	RuntimeActionModTargetObserve        RuntimeAction = "runtime.mods.target.observe"
	RuntimeActionModCacheInspect         RuntimeAction = "runtime.mods.cache.inspect"
	RuntimeActionModUploadBegin          RuntimeAction = "runtime.mods.upload.begin"
	RuntimeActionModUploadWrite          RuntimeAction = "runtime.mods.upload.write"
	RuntimeActionModUploadCommit         RuntimeAction = "runtime.mods.upload.commit"
	RuntimeActionModReleasePlanBegin     RuntimeAction = "runtime.mods.release.plan.begin"
	RuntimeActionModReleasePlanWrite     RuntimeAction = "runtime.mods.release.plan.write"
	RuntimeActionModReleasePlanCommit    RuntimeAction = "runtime.mods.release.plan.commit"
	RuntimeActionModReleasePrepare       RuntimeAction = "runtime.mods.release.prepare"
	RuntimeActionModReleasePublish       RuntimeAction = "runtime.mods.release.publish"
	RuntimeActionModReleaseRollback      RuntimeAction = "runtime.mods.release.rollback"
	RuntimeActionModReleaseComplete      RuntimeAction = "runtime.mods.release.complete"
	RuntimeActionModReleaseState         RuntimeAction = "runtime.mods.release.state"
	RuntimeActionModOverridesRead        RuntimeAction = "runtime.mods.overrides.read"
	RuntimeActionGameVersionObserve      RuntimeAction = "runtime.game.version.observe"
	RuntimeActionGameVersionUpdate       RuntimeAction = "runtime.game.version.update"
	RuntimeActionCPUPrepare              RuntimeAction = "runtime.cpu.prepare"
	RuntimeActionCPUApply                RuntimeAction = "runtime.cpu.apply"
	RuntimeActionCPUObserve              RuntimeAction = "runtime.cpu.observe"
	RuntimeActionConfigurationBegin      RuntimeAction = "runtime.configuration.begin"
	RuntimeActionConfigurationWrite      RuntimeAction = "runtime.configuration.write"
	RuntimeActionConfigurationPrepare    RuntimeAction = "runtime.configuration.prepare"
	RuntimeActionConfigurationPublish    RuntimeAction = "runtime.configuration.publish"
	RuntimeActionConfigurationRollback   RuntimeAction = "runtime.configuration.rollback"
	RuntimeActionConfigurationComplete   RuntimeAction = "runtime.configuration.complete"
	RuntimeActionMapSessions             RuntimeAction = "runtime.maps.sessions"
	RuntimeActionMapStatus               RuntimeAction = "runtime.maps.status"
	RuntimeActionMapSnapshotPrepare      RuntimeAction = "runtime.maps.snapshot.prepare"
	RuntimeActionMapRender               RuntimeAction = "runtime.maps.render"
	RuntimeActionMapRead                 RuntimeAction = "runtime.maps.read"
	RuntimeActionMapRelease              RuntimeAction = "runtime.maps.release"
)

const (
	RuntimeLogSourceServer RuntimeLogSource = "server"
	RuntimeLogSourceChat   RuntimeLogSource = "chat"
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
	ArtifactRuntimeBarrier     ArtifactKind = "runtime.snapshot_barrier"
)

type RuntimeConsoleRequest struct {
	Mode        ConsoleMode `json:"mode"`
	Command     string      `json:"command"`
	CoalesceKey string      `json:"coalesce_key,omitempty"`
}

type RuntimeLogRequest struct {
	Source   RuntimeLogSource `json:"source,omitempty"`
	FileID   string           `json:"file_id,omitempty"`
	Cursor   int64            `json:"cursor"`
	MaxBytes int              `json:"max_bytes"`
	MaxLines int              `json:"max_lines"`
	Query    string           `json:"query,omitempty"`
	Raw      bool             `json:"raw,omitempty"`
}

func IsRuntimeLogSource(value RuntimeLogSource) bool {
	return value == "" || value == RuntimeLogSourceServer || value == RuntimeLogSourceChat
}

type RuntimeArtifactRequest struct {
	Kind ArtifactKind `json:"kind"`
}

type RuntimeObservationRequest struct {
	ObservedOperationID  string `json:"observed_operation_id"`
	ObservedOperationKey string `json:"observed_operation_key,omitempty"`
}

type RuntimeMigrationRequest struct {
	MigrationID string `json:"migration_id"`
	Offset      int64  `json:"offset,omitempty"`
	Size        int64  `json:"size,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Data        []byte `json:"data,omitempty"`
}

type RuntimeBackupRequest struct {
	BackupID      string `json:"backup_id"`
	Offset        int64  `json:"offset,omitempty"`
	Size          int64  `json:"size,omitempty"`
	ContentSize   int64  `json:"content_size,omitempty"`
	FileCount     int    `json:"file_count,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	SharedSHA256  string `json:"shared_sha256,omitempty"`
	Data          []byte `json:"data,omitempty"`
	PublishShared bool   `json:"publish_shared,omitempty"`
}

type RuntimeModUploadKind string

const (
	RuntimeModUploadCacheBundle RuntimeModUploadKind = "cache_bundle"
	RuntimeModUploadReleasePlan RuntimeModUploadKind = "release_plan"
)

type RuntimeModMetadata struct {
	Title             string    `json:"title,omitempty"`
	Version           string    `json:"version,omitempty"`
	PublishedFileSize int64     `json:"published_file_size,omitempty"`
	SteamUpdatedAt    time.Time `json:"steam_updated_at,omitempty"`
}

type RuntimeModVersion struct {
	WorkshopID string `json:"workshop_id"`
	TreeSHA256 string `json:"tree_sha256"`
}

type RuntimeModShardRelease struct {
	InstallationID string              `json:"installation_id"`
	RoomID         string              `json:"room_id"`
	RoomDirectory  string              `json:"room_directory"`
	WorldID        string              `json:"world_id"`
	WorldDirectory string              `json:"world_directory"`
	Mods           []RuntimeModVersion `json:"mods"`
	ModOverrides   []byte              `json:"mod_overrides"`
}

// RuntimeModPlanInput is the JSON value uploaded by the release-plan chunk
// actions. It deliberately contains identities and directory names, never
// host paths.
type RuntimeModPlanInput struct {
	OperationID string                   `json:"operation_id"`
	NodeID      string                   `json:"node_id"`
	Shards      []RuntimeModShardRelease `json:"shards"`
}

type RuntimeModRequest struct {
	Kind               RuntimeModUploadKind `json:"kind,omitempty"`
	UploadID           string               `json:"upload_id,omitempty"`
	OperationID        string               `json:"release_operation_id,omitempty"`
	WorkshopID         string               `json:"workshop_id,omitempty"`
	ExpectedTreeSHA256 string               `json:"expected_tree_sha256,omitempty"`
	Offset             int64                `json:"offset,omitempty"`
	Size               int64                `json:"size,omitempty"`
	SHA256             string               `json:"sha256,omitempty"`
	Data               []byte               `json:"data,omitempty"`
	Metadata           RuntimeModMetadata   `json:"metadata,omitempty"`
	RoomDirectory      string               `json:"room_directory,omitempty"`
	WorldDirectory     string               `json:"world_directory,omitempty"`
}

// RuntimeGameVersionRequest contains only update intent. Executable paths,
// installation roots and the fixed DST App ID are resolved by the Agent from
// its trusted local installation configuration.
type RuntimeGameVersionRequest struct {
	ExpectedVersion string `json:"expected_version,omitempty"`
	CleanCache      bool   `json:"clean_cache,omitempty"`
}

type RuntimeConfigurationRequest struct {
	PublicationID string `json:"publication_id"`
	Scope         string `json:"scope"`
	Offset        int64  `json:"offset,omitempty"`
	Size          int64  `json:"size,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Data          []byte `json:"data,omitempty"`
}

type RuntimeMapRequest struct {
	TransferID string   `json:"transfer_id,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
	FileName   string   `json:"file_name,omitempty"`
	Offset     int64    `json:"offset,omitempty"`
	Layers     []string `json:"layers,omitempty"`
}

// RuntimeOperationRequest references a trusted installation and a managed
// Shard. It never accepts a host path, executable, container specification or
// shell command.
type RuntimeOperationRequest struct {
	ProtocolVersion  int                          `json:"protocol_version"`
	OperationID      string                       `json:"operation_id"`
	OperationKey     string                       `json:"operation_key,omitempty"`
	InstallationID   string                       `json:"installation_id"`
	Action           RuntimeAction                `json:"action"`
	Cluster          string                       `json:"cluster"`
	Shard            string                       `json:"shard"`
	TopologyRevision string                       `json:"topology_revision"`
	LeaseID          string                       `json:"lease_id,omitempty"`
	FencingToken     uint64                       `json:"fencing_token,omitempty"`
	LeaseExpiresAt   *time.Time                   `json:"lease_expires_at,omitempty"`
	Console          *RuntimeConsoleRequest       `json:"console,omitempty"`
	Logs             *RuntimeLogRequest           `json:"logs,omitempty"`
	Artifacts        *RuntimeArtifactRequest      `json:"artifacts,omitempty"`
	Observation      *RuntimeObservationRequest   `json:"observation,omitempty"`
	Migration        *RuntimeMigrationRequest     `json:"migration,omitempty"`
	Backup           *RuntimeBackupRequest        `json:"backup,omitempty"`
	Mod              *RuntimeModRequest           `json:"mod,omitempty"`
	GameVersion      *RuntimeGameVersionRequest   `json:"game_version,omitempty"`
	CPU              *RuntimeCPURequest           `json:"cpu,omitempty"`
	Configuration    *RuntimeConfigurationRequest `json:"configuration,omitempty"`
	Map              *RuntimeMapRequest           `json:"map,omitempty"`
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
	Available            bool               `json:"available"`
	Status               string             `json:"status,omitempty"`
	Accepting            bool               `json:"accepting"`
	Busy                 bool               `json:"busy"`
	Pending              int                `json:"pending"`
	PendingLimit         int                `json:"pending_limit,omitempty"`
	Class                string             `json:"class,omitempty"`
	CoalesceKey          string             `json:"coalesce_key,omitempty"`
	StartedAt            time.Time          `json:"started_at,omitempty"`
	InstanceID           string             `json:"instance_id,omitempty"`
	Maintenance          bool               `json:"maintenance"`
	MaintenanceOwner     string             `json:"maintenance_owner,omitempty"`
	MaintenanceStartedAt time.Time          `json:"maintenance_started_at,omitempty"`
	InputDirty           bool               `json:"input_dirty"`
	ExternalWriter       bool               `json:"external_writer"`
	Runtime              ShardRuntimeStatus `json:"runtime"`
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
	StartedAt time.Time        `json:"started_at,omitempty"`
	UpdatedAt time.Time        `json:"updated_at"`
	Lines     []RuntimeLogLine `json:"lines"`
	Data      []byte           `json:"data,omitempty"`
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

type RuntimeMigrationResult struct {
	MigrationID string `json:"migration_id"`
	Offset      int64  `json:"offset,omitempty"`
	NextOffset  int64  `json:"next_offset,omitempty"`
	Size        int64  `json:"size,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Data        []byte `json:"data,omitempty"`
	Complete    bool   `json:"complete"`
	RecoveryRef string `json:"recovery_ref,omitempty"`
}

type RuntimeBackupResult struct {
	BackupID     string `json:"backup_id"`
	Offset       int64  `json:"offset,omitempty"`
	NextOffset   int64  `json:"next_offset,omitempty"`
	Size         int64  `json:"size,omitempty"`
	ContentSize  int64  `json:"content_size,omitempty"`
	FileCount    int    `json:"file_count,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	SharedSHA256 string `json:"shared_sha256,omitempty"`
	Data         []byte `json:"data,omitempty"`
	Complete     bool   `json:"complete"`
	RecoveryRef  string `json:"recovery_ref,omitempty"`
}

type RuntimeModCacheManifest struct {
	Version        int                `json:"version"`
	WorkshopID     string             `json:"workshop_id"`
	TreeSHA256     string             `json:"tree_sha256"`
	ManifestSHA256 string             `json:"manifest_sha256"`
	Size           int64              `json:"size"`
	FileCount      int                `json:"file_count"`
	Metadata       RuntimeModMetadata `json:"metadata"`
	CreatedAt      time.Time          `json:"created_at"`
}

type RuntimeModShardState struct {
	RoomID         string              `json:"room_id"`
	RoomDirectory  string              `json:"room_directory"`
	WorldID        string              `json:"world_id"`
	WorldDirectory string              `json:"world_directory"`
	Mods           []RuntimeModVersion `json:"mods"`
	ConfigSHA256   string              `json:"config_sha256"`
}

type RuntimeModInstallationState struct {
	InstallationID     string                          `json:"installation_id"`
	LastOperationID    string                          `json:"last_operation_id"`
	Mods               map[string]string               `json:"mods"`
	ManagedSetupSHA256 string                          `json:"managed_setup_sha256"`
	Shards             map[string]RuntimeModShardState `json:"shards"`
	UpdatedAt          time.Time                       `json:"updated_at"`
}

type RuntimeModReleaseState struct {
	OperationID string                       `json:"operation_id"`
	Phase       string                       `json:"phase"`
	UpdatedAt   time.Time                    `json:"updated_at"`
	State       *RuntimeModInstallationState `json:"state,omitempty"`
}

type RuntimeModOverridesChunk struct {
	RoomDirectory  string `json:"room_directory"`
	WorldDirectory string `json:"world_directory"`
	Offset         int64  `json:"offset"`
	NextOffset     int64  `json:"next_offset"`
	Size           int64  `json:"size"`
	SHA256         string `json:"sha256"`
	Data           []byte `json:"data,omitempty"`
	Complete       bool   `json:"complete"`
}

type RuntimeModResult struct {
	Kind           RuntimeModUploadKind      `json:"kind,omitempty"`
	UploadID       string                    `json:"upload_id,omitempty"`
	OperationID    string                    `json:"release_operation_id,omitempty"`
	Offset         int64                     `json:"offset,omitempty"`
	NextOffset     int64                     `json:"next_offset,omitempty"`
	Size           int64                     `json:"size,omitempty"`
	SHA256         string                    `json:"sha256,omitempty"`
	Complete       bool                      `json:"complete"`
	CacheManifest  *RuntimeModCacheManifest  `json:"cache_manifest,omitempty"`
	Release        *RuntimeModReleaseState   `json:"release,omitempty"`
	Overrides      *RuntimeModOverridesChunk `json:"overrides,omitempty"`
	AvailableBytes int64                     `json:"available_bytes,omitempty"`
	RuntimeVersion string                    `json:"runtime_version,omitempty"`
}

type RuntimeGameVersionResult struct {
	Installed         bool      `json:"installed"`
	CurrentVersion    string    `json:"current_version,omitempty"`
	AvailableBytes    uint64    `json:"available_bytes"`
	SteamCMDAvailable bool      `json:"steamcmd_available"`
	UpdateSupported   bool      `json:"update_supported"`
	Log               string    `json:"log,omitempty"`
	ObservedAt        time.Time `json:"observed_at"`
}

type RuntimeConfigurationResult struct {
	PublicationID string `json:"publication_id"`
	Offset        int64  `json:"offset,omitempty"`
	NextOffset    int64  `json:"next_offset,omitempty"`
	Size          int64  `json:"size,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Complete      bool   `json:"complete"`
}

type RuntimeMapSession struct {
	SessionID   string    `json:"session_id"`
	FileName    string    `json:"file_name"`
	Size        int64     `json:"size"`
	PlayerCount int       `json:"player_count"`
	ModifiedAt  time.Time `json:"modified_at"`
}

type RuntimeMapResult struct {
	TransferID   string              `json:"transfer_id,omitempty"`
	Sessions     []RuntimeMapSession `json:"sessions,omitempty"`
	Offset       int64               `json:"offset,omitempty"`
	NextOffset   int64               `json:"next_offset,omitempty"`
	Size         int64               `json:"size,omitempty"`
	SHA256       string              `json:"sha256,omitempty"`
	SourceSHA256 string              `json:"source_sha256,omitempty"`
	Data         []byte              `json:"data,omitempty"`
	Complete     bool                `json:"complete"`
	Log          string              `json:"log,omitempty"`
	Renderer     *RuntimeMapRenderer `json:"renderer,omitempty"`
}

type RuntimeMapRenderer struct {
	Available       bool     `json:"available"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	Version         string   `json:"version,omitempty"`
	Artifacts       []string `json:"artifacts"`
	Error           string   `json:"error,omitempty"`
}

type RuntimeOperationResult struct {
	ProtocolVersion  int                         `json:"protocol_version"`
	OperationID      string                      `json:"operation_id"`
	OperationKey     string                      `json:"operation_key,omitempty"`
	InstallationID   string                      `json:"installation_id"`
	Action           RuntimeAction               `json:"action"`
	Cluster          string                      `json:"cluster"`
	Shard            string                      `json:"shard"`
	FencingToken     uint64                      `json:"fencing_token,omitempty"`
	Outcome          RuntimeOutcome              `json:"outcome"`
	Message          string                      `json:"message,omitempty"`
	Idempotent       bool                        `json:"idempotent"`
	ObservedAt       time.Time                   `json:"observed_at"`
	TargetID         string                      `json:"target_id,omitempty"`
	AgentID          string                      `json:"agent_id,omitempty"`
	TopologyRevision string                      `json:"topology_revision,omitempty"`
	ConsoleHealth    *RuntimeConsoleHealth       `json:"console_health,omitempty"`
	Logs             *RuntimeLogChunk            `json:"logs,omitempty"`
	Artifacts        *RuntimeArtifactBundle      `json:"artifacts,omitempty"`
	Evidence         *RuntimeOperationEvidence   `json:"evidence,omitempty"`
	Migration        *RuntimeMigrationResult     `json:"migration,omitempty"`
	Backup           *RuntimeBackupResult        `json:"backup,omitempty"`
	Mod              *RuntimeModResult           `json:"mod,omitempty"`
	GameVersion      *RuntimeGameVersionResult   `json:"game_version,omitempty"`
	CPU              *RuntimeCPUResult           `json:"cpu,omitempty"`
	Configuration    *RuntimeConfigurationResult `json:"configuration,omitempty"`
	Map              *RuntimeMapResult           `json:"map,omitempty"`
}

func IsRuntimeAction(value RuntimeAction) bool {
	switch value {
	case RuntimeActionConsoleHealth, RuntimeActionConsoleSend, RuntimeActionObserveOperation, RuntimeActionReadLogs, RuntimeActionReadArtifacts,
		RuntimeActionMigrationExportPrepare, RuntimeActionMigrationExportRead, RuntimeActionMigrationExportRelease,
		RuntimeActionMigrationImportBegin, RuntimeActionMigrationImportWrite, RuntimeActionMigrationImportCommit,
		RuntimeActionMigrationTargetRollback, RuntimeActionMigrationTargetComplete,
		RuntimeActionMigrationSourceFinalize, RuntimeActionMigrationSourceRollback, RuntimeActionMigrationSourceComplete:
		return true
	case RuntimeActionBackupStage, RuntimeActionBackupRead, RuntimeActionBackupRelease,
		RuntimeActionRestoreBegin, RuntimeActionRestoreWrite, RuntimeActionRestorePrepare,
		RuntimeActionRestorePublish, RuntimeActionRestoreRollback, RuntimeActionRestoreComplete:
		return true
	case RuntimeActionModTargetObserve, RuntimeActionModCacheInspect, RuntimeActionModUploadBegin, RuntimeActionModUploadWrite, RuntimeActionModUploadCommit,
		RuntimeActionModReleasePlanBegin, RuntimeActionModReleasePlanWrite, RuntimeActionModReleasePlanCommit,
		RuntimeActionModReleasePrepare, RuntimeActionModReleasePublish, RuntimeActionModReleaseRollback,
		RuntimeActionModReleaseComplete, RuntimeActionModReleaseState, RuntimeActionModOverridesRead:
		return true
	case RuntimeActionGameVersionObserve, RuntimeActionGameVersionUpdate:
		return true
	case RuntimeActionCPUPrepare, RuntimeActionCPUApply, RuntimeActionCPUObserve:
		return true
	case RuntimeActionConfigurationBegin, RuntimeActionConfigurationWrite, RuntimeActionConfigurationPrepare,
		RuntimeActionConfigurationPublish, RuntimeActionConfigurationRollback, RuntimeActionConfigurationComplete:
		return true
	case RuntimeActionMapSessions, RuntimeActionMapStatus, RuntimeActionMapSnapshotPrepare, RuntimeActionMapRender, RuntimeActionMapRead, RuntimeActionMapRelease:
		return true
	default:
		return false
	}
}

func RuntimeActionMutates(value RuntimeAction) bool {
	switch value {
	case RuntimeActionConsoleSend, RuntimeActionMigrationExportPrepare, RuntimeActionMigrationExportRelease,
		RuntimeActionMigrationImportBegin, RuntimeActionMigrationImportWrite, RuntimeActionMigrationImportCommit,
		RuntimeActionMigrationTargetRollback, RuntimeActionMigrationTargetComplete,
		RuntimeActionMigrationSourceFinalize, RuntimeActionMigrationSourceRollback, RuntimeActionMigrationSourceComplete:
		return true
	case RuntimeActionBackupStage, RuntimeActionBackupRelease,
		RuntimeActionRestoreBegin, RuntimeActionRestoreWrite, RuntimeActionRestorePrepare,
		RuntimeActionRestorePublish, RuntimeActionRestoreRollback, RuntimeActionRestoreComplete:
		return true
	case RuntimeActionModUploadBegin, RuntimeActionModUploadWrite, RuntimeActionModUploadCommit,
		RuntimeActionModReleasePlanBegin, RuntimeActionModReleasePlanWrite, RuntimeActionModReleasePlanCommit,
		RuntimeActionModReleasePrepare, RuntimeActionModReleasePublish, RuntimeActionModReleaseRollback,
		RuntimeActionModReleaseComplete:
		return true
	case RuntimeActionGameVersionUpdate:
		return true
	case RuntimeActionCPUPrepare, RuntimeActionCPUApply:
		return true
	case RuntimeActionConfigurationBegin, RuntimeActionConfigurationWrite, RuntimeActionConfigurationPrepare,
		RuntimeActionConfigurationPublish, RuntimeActionConfigurationRollback, RuntimeActionConfigurationComplete:
		return true
	case RuntimeActionMapSnapshotPrepare, RuntimeActionMapRender, RuntimeActionMapRelease:
		return true
	default:
		return false
	}
}
