package shared

import "time"

const (
	RuntimeOperationProtocolVersion    = 3
	MaximumRuntimeConsoleCommandBytes  = 1023
	MaximumRuntimeCommandDocumentBytes = 16 * 1024
	MaximumRuntimeModOverridesBytes    = 4 * 1024 * 1024
	MaxChunkBytes                      = 256 * 1024
)

type RuntimeAction string

type RuntimeLogSource string

const (
	RuntimeActionConsoleHealth           RuntimeAction = "runtime.console.health"
	RuntimeActionConsoleSend             RuntimeAction = "runtime.console.send"
	RuntimeActionObserveOperation        RuntimeAction = "runtime.operation.observe"
	RuntimeActionReadLogs                RuntimeAction = "runtime.logs.read"
	RuntimeActionChatLogsList            RuntimeAction = "runtime.chat-logs.list"
	RuntimeActionChatLogsRead            RuntimeAction = "runtime.chat-logs.read"
	RuntimeActionReadArtifacts           RuntimeAction = "runtime.artifacts.read"
	RuntimeActionWorldStateRead          RuntimeAction = "runtime.worldstate.read"
	RuntimeActionEntityArtwork           RuntimeAction = "runtime.entity-artwork.read"
	RuntimeActionMigrationExportPrepare  RuntimeAction = "runtime.migration.export.prepare"
	RuntimeActionMigrationExportRead     RuntimeAction = "runtime.migration.export.read"
	RuntimeActionMigrationExportRelease  RuntimeAction = "runtime.migration.export.release"
	RuntimeActionMigrationPeerGrant      RuntimeAction = "runtime.migration.peer.grant"
	RuntimeActionMigrationFetch          RuntimeAction = "runtime.migration.fetch.v2"
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
	RuntimeActionModPeerGrant            RuntimeAction = "runtime.mods.peer.grant"
	RuntimeActionModFetch                RuntimeAction = "runtime.mods.fetch.v2"
	RuntimeActionModDownload             RuntimeAction = "runtime.mods.download.v1"
	RuntimeActionModLink                 RuntimeAction = "runtime.mods.local-link.v1"
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
	RuntimeActionModInstallationState    RuntimeAction = "runtime.mods.installation.state"
	RuntimeActionModFilesObserve         RuntimeAction = "runtime.mods.files.observe"
	RuntimeActionModFilesInventory       RuntimeAction = "runtime.mods.files.inventory"
	RuntimeActionModSchemaRead           RuntimeAction = "runtime.mods.schema.read"
	RuntimeActionModOverridesRead        RuntimeAction = "runtime.mods.overrides.read"
	RuntimeActionLuaJITObserve           RuntimeAction = "runtime.luajit.observe"
	RuntimeActionLuaJITInstall           RuntimeAction = "runtime.luajit.install"
	RuntimeActionLuaJITDownload          RuntimeAction = "runtime.luajit.download"
	RuntimeActionGameVersionObserve      RuntimeAction = "runtime.game.version.observe"
	RuntimeActionGameVersionUpdate       RuntimeAction = "runtime.game.version.update"
	RuntimeActionNetworkEgressObserve    RuntimeAction = "runtime.network.egress.observe"
	RuntimeActionNetworkEndpointListen   RuntimeAction = "runtime.network.endpoint.listen"
	RuntimeActionNetworkEndpointProbe    RuntimeAction = "runtime.network.endpoint.probe"
	RuntimeActionCPUPrepare              RuntimeAction = "runtime.cpu.prepare"
	RuntimeActionCPUApply                RuntimeAction = "runtime.cpu.apply"
	RuntimeActionCPUObserve              RuntimeAction = "runtime.cpu.observe"
	RuntimeActionConfigurationRead       RuntimeAction = "runtime.configuration.read"
	RuntimeActionConfigurationApply      RuntimeAction = "runtime.configuration.apply"
	RuntimeActionModConfigurationWrite   RuntimeAction = "runtime.configuration.mod.write"
	RuntimeActionClusterTokenReveal      RuntimeAction = "runtime.configuration.cluster-token.reveal"
	RuntimeActionConfigurationBegin      RuntimeAction = "runtime.configuration.begin"
	RuntimeActionConfigurationWrite      RuntimeAction = "runtime.configuration.write"
	RuntimeActionConfigurationPrepare    RuntimeAction = "runtime.configuration.prepare"
	RuntimeActionConfigurationPublish    RuntimeAction = "runtime.configuration.publish"
	RuntimeActionConfigurationRollback   RuntimeAction = "runtime.configuration.rollback"
	RuntimeActionConfigurationComplete   RuntimeAction = "runtime.configuration.complete"
	RuntimeActionRoomRecoveryMove        RuntimeAction = "runtime.room.recovery.move"
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
	ArtifactRuntimeHealth        ArtifactKind = "runtime.health"
	ArtifactRuntimePlayers       ArtifactKind = "runtime.players"
	ArtifactRuntimePlayerHistory ArtifactKind = "runtime.player_history"
	ArtifactRuntimeWorldState    ArtifactKind = "runtime.worldstate"
	ArtifactRuntimeEvents        ArtifactKind = "runtime.events"
	ArtifactRuntimeCommand       ArtifactKind = "runtime.command_receipt"
	ArtifactRuntimeDiagnostics   ArtifactKind = "runtime.diagnostics"
	ArtifactRuntimeBarrier       ArtifactKind = "runtime.snapshot_barrier"
)

type RuntimeConsoleRequest struct {
	Mode            ConsoleMode             `json:"mode"`
	Command         string                  `json:"command"`
	CoalesceKey     string                  `json:"coalesce_key,omitempty"`
	CommandDocument *RuntimeCommandDocument `json:"command_document,omitempty"`
}

type RuntimeCommandDocument struct {
	RequestID string `json:"request_id"`
	SHA256    string `json:"sha256"`
	Data      []byte `json:"data"`
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

type RuntimeChatLogRequest struct {
	GenerationID string `json:"generation_id,omitempty"`
	Cursor       int64  `json:"cursor,omitempty"`
	MaxBytes     int    `json:"max_bytes,omitempty"`
	MaxLines     int    `json:"max_lines,omitempty"`
	ResolveTimes bool   `json:"resolve_times,omitempty"`
}

func IsRuntimeLogSource(value RuntimeLogSource) bool {
	return value == "" || value == RuntimeLogSourceServer || value == RuntimeLogSourceChat
}

type RuntimeEntityArtworkRequest struct {
	Prefab string `json:"prefab"`
	ModID  string `json:"mod_id,omitempty"`
}
type RuntimeEntityArtworkResult struct {
	Prefab string `json:"prefab"`
	ModID  string `json:"mod_id,omitempty"`
	Data   []byte `json:"data,omitempty"`
}

type RuntimeArtifactRequest struct {
	Kind ArtifactKind `json:"kind"`
}

type RuntimeObservationRequest struct {
	ObservedOperationID  string `json:"observed_operation_id"`
	ObservedOperationKey string `json:"observed_operation_key,omitempty"`
}

type RuntimeMigrationRequest struct {
	MigrationID        string                          `json:"migration_id"`
	PeerSubject        string                          `json:"peer_subject,omitempty"`
	Offset             int64                           `json:"offset,omitempty"`
	Size               int64                           `json:"size,omitempty"`
	SHA256             string                          `json:"sha256,omitempty"`
	Data               []byte                          `json:"data,omitempty"`
	FetchLocations     []RuntimeMigrationFetchLocation `json:"fetch_locations,omitempty"`
	ShardBindAll       bool                            `json:"shard_bind_all,omitempty"`
	ShardMasterAddress string                          `json:"shard_master_address,omitempty"`
	ShardMasterPort    int                             `json:"shard_master_port,omitempty"`
}

type RuntimeMigrationFetchLocation struct {
	DownloadURL   string    `json:"download_url"`
	DownloadPath  string    `json:"download_path"`
	DownloadToken string    `json:"download_token"`
	Size          int64     `json:"size"`
	SHA256        string    `json:"sha256"`
	ExpiresAt     time.Time `json:"expires_at"`
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

type RuntimeModFetchSource string

type RuntimeModFetchFeasibility string

type RuntimeModFetchStatus string

const (
	RuntimeModUploadCacheBundle     RuntimeModUploadKind  = "cache_bundle"
	RuntimeModUploadReleasePlan     RuntimeModUploadKind  = "release_plan"
	RuntimeModFetchSourceSteam      RuntimeModFetchSource = "steam"
	RuntimeModFetchSourcePeer       RuntimeModFetchSource = "peer"
	RuntimeModFetchSourceController RuntimeModFetchSource = "controller"
	RuntimeModFetchSourceCache      RuntimeModFetchSource = "cache"
	RuntimeModFetchSourceLegacy     RuntimeModFetchSource = "legacy_upload"

	RuntimeModFetchFeasibilityAvailable   RuntimeModFetchFeasibility = "available"
	RuntimeModFetchFeasibilityUnavailable RuntimeModFetchFeasibility = "unavailable"
	RuntimeModFetchFeasibilityUnknown     RuntimeModFetchFeasibility = "unknown"

	RuntimeModFetchStatusSucceeded   RuntimeModFetchStatus = "succeeded"
	RuntimeModFetchStatusFailed      RuntimeModFetchStatus = "failed"
	RuntimeModFetchStatusUnavailable RuntimeModFetchStatus = "unavailable"
	RuntimeModFetchStatusSkipped     RuntimeModFetchStatus = "skipped"
)

// RuntimeModFetchAttempt reports observations from a real fetch path. Bytes
// only includes data transferred during this attempt, excluding resumed data.
type RuntimeModFetchAttempt struct {
	Source         RuntimeModFetchSource      `json:"source"`
	Feasibility    RuntimeModFetchFeasibility `json:"feasibility"`
	Status         RuntimeModFetchStatus      `json:"status"`
	Selected       bool                       `json:"selected,omitempty"`
	Bytes          int64                      `json:"bytes,omitempty"`
	DurationMillis int64                      `json:"duration_ms,omitempty"`
	BytesPerSecond int64                      `json:"bytes_per_second,omitempty"`
	ErrorCode      string                     `json:"error_code,omitempty"`
	ErrorMessage   string                     `json:"error_message,omitempty"`
	ObservedAt     time.Time                  `json:"observed_at,omitempty"`
}

type RuntimeModFetchLocation struct {
	Source        RuntimeModFetchSource `json:"source"`
	DownloadURL   string                `json:"download_url,omitempty"`
	DownloadPath  string                `json:"download_path"`
	DownloadToken string                `json:"download_token"`
	Size          int64                 `json:"size"`
	SHA256        string                `json:"sha256"`
}

type RuntimeModMetadata struct {
	Title             string    `json:"title,omitempty"`
	Version           string    `json:"version,omitempty"`
	PublishedFileSize int64     `json:"published_file_size,omitempty"`
	SteamManifestID   string    `json:"steam_manifest_id,omitempty"`
	SteamUpdatedAt    time.Time `json:"steam_updated_at,omitempty"`
}

type RuntimeModVersion struct {
	WorkshopID string             `json:"workshop_id"`
	TreeSHA256 string             `json:"tree_sha256"`
	Metadata   RuntimeModMetadata `json:"metadata,omitempty"`
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
	OperationID    string                   `json:"operation_id"`
	NodeID         string                   `json:"node_id"`
	Mode           string                   `json:"mode,omitempty"`
	InstallationID string                   `json:"installation_id,omitempty"`
	Mods           []RuntimeModVersion      `json:"mods,omitempty"`
	Shards         []RuntimeModShardRelease `json:"shards,omitempty"`
}

type RuntimeModRequest struct {
	Kind               RuntimeModUploadKind      `json:"kind,omitempty"`
	UploadID           string                    `json:"upload_id,omitempty"`
	OperationID        string                    `json:"release_operation_id,omitempty"`
	WorkshopID         string                    `json:"workshop_id,omitempty"`
	ExpectedTreeSHA256 string                    `json:"expected_tree_sha256,omitempty"`
	PeerSubject        string                    `json:"peer_subject,omitempty"`
	Offset             int64                     `json:"offset,omitempty"`
	Size               int64                     `json:"size,omitempty"`
	SHA256             string                    `json:"sha256,omitempty"`
	Data               []byte                    `json:"data,omitempty"`
	Metadata           RuntimeModMetadata        `json:"metadata,omitempty"`
	FetchSources       []RuntimeModFetchSource   `json:"fetch_sources,omitempty"`
	FetchLocations     []RuntimeModFetchLocation `json:"fetch_locations,omitempty"`
	Validate           bool                      `json:"validate,omitempty"`
	RoomDirectory      string                    `json:"room_directory,omitempty"`
	WorldDirectory     string                    `json:"world_directory,omitempty"`
	WorkshopIDs        []string                  `json:"workshop_ids,omitempty"`
	Worlds             []RuntimeModObserveWorld  `json:"worlds,omitempty"`
}

type RuntimeModObserveWorld struct {
	RoomID         string `json:"room_id"`
	RoomDirectory  string `json:"room_directory"`
	WorldID        string `json:"world_id"`
	WorldDirectory string `json:"world_directory"`
}

// RuntimeGameVersionRequest contains only update intent. Executable paths,
// installation roots and the fixed DST App ID are resolved by the Agent from
// its trusted local installation configuration.
type RuntimeGameVersionRequest struct {
	ExpectedVersion string `json:"expected_version,omitempty"`
	CleanCache      bool   `json:"clean_cache,omitempty"`
}

type RuntimeNetworkRegion string

const (
	RuntimeNetworkRegionCN     RuntimeNetworkRegion = "cn"
	RuntimeNetworkRegionGlobal RuntimeNetworkRegion = "global"
)

type RuntimeNetworkRequest struct {
	Region        RuntimeNetworkRegion            `json:"region,omitempty"`
	BindAddress   string                          `json:"bind_address,omitempty"`
	Port          int                             `json:"port,omitempty"`
	Tokens        []string                        `json:"tokens,omitempty"`
	Endpoints     []RuntimeNetworkEndpointRequest `json:"endpoints,omitempty"`
	TimeoutMillis int                             `json:"timeout_millis,omitempty"`
}

type RuntimeNetworkEndpointRequest struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	Token   string `json:"token"`
}

func IsRuntimeNetworkRegion(value RuntimeNetworkRegion) bool {
	return value == RuntimeNetworkRegionCN || value == RuntimeNetworkRegionGlobal
}

type RuntimeConfigurationRequest struct {
	ExpectedFiles  map[string]string `json:"expected_files,omitempty"`
	ExpectedSHA256 string            `json:"expected_sha256,omitempty"`
	PublicationID  string            `json:"publication_id"`
	Scope          string            `json:"scope"`
	Offset         int64             `json:"offset,omitempty"`
	Size           int64             `json:"size,omitempty"`
	SHA256         string            `json:"sha256,omitempty"`
	Data           []byte            `json:"data,omitempty"`
}

type RuntimeMapRequest struct {
	TransferID string   `json:"transfer_id,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
	FileName   string   `json:"file_name,omitempty"`
	Offset     int64    `json:"offset,omitempty"`
	Layers     []string `json:"layers,omitempty"`
}

// RuntimeOperationRequest references a trusted installation and, where needed,
// a managed Shard. Only explicit game inspection/adoption accepts a host source
// path; execution destinations still come from the installation registry. It
// never accepts an executable, container specification or shell command.
type RuntimeOperationRequest struct {
	EntityArtwork    *RuntimeEntityArtworkRequest `json:"entity_artwork,omitempty"`
	GameInstallation *GameInstallationRequest     `json:"game_installation,omitempty"`
	LuaJIT           *RuntimeLuaJITRequest        `json:"luajit,omitempty"`
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
	ChatLogs         *RuntimeChatLogRequest       `json:"chat_logs,omitempty"`
	Artifacts        *RuntimeArtifactRequest      `json:"artifacts,omitempty"`
	Observation      *RuntimeObservationRequest   `json:"observation,omitempty"`
	Migration        *RuntimeMigrationRequest     `json:"migration,omitempty"`
	Backup           *RuntimeBackupRequest        `json:"backup,omitempty"`
	Mod              *RuntimeModRequest           `json:"mod,omitempty"`
	GameVersion      *RuntimeGameVersionRequest   `json:"game_version,omitempty"`
	Network          *RuntimeNetworkRequest       `json:"network,omitempty"`
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

type RuntimeChatLogGeneration struct {
	ID                 string    `json:"id"`
	FileName           string    `json:"file_name"`
	Archived           bool      `json:"archived"`
	Size               int64     `json:"size"`
	StartedAt          time.Time `json:"started_at,omitempty"`
	StartedAtEstimated bool      `json:"started_at_estimated,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
	TimeVersion        int       `json:"time_version,omitempty"`
}

// Chat timestamps are separate from generic log lines so older readers can
// retain raw lines without mistaking a wrapped clock for a wall-clock date.
const ChatTimeVersion = 1

type RuntimeChatLogTime struct {
	Cursor     int64      `json:"cursor"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
}

type RuntimeChatLogResult struct {
	Generations []RuntimeChatLogGeneration `json:"generations,omitempty"`
	Generation  *RuntimeChatLogGeneration  `json:"generation,omitempty"`
	Cursor      int64                      `json:"cursor,omitempty"`
	Lines       []RuntimeLogLine           `json:"lines,omitempty"`
	Complete    bool                       `json:"complete,omitempty"`
	TimeVersion int                        `json:"time_version,omitempty"`
	Times       []RuntimeChatLogTime       `json:"times,omitempty"`
	TimesReady  bool                       `json:"times_ready,omitempty"`
	ReadBytes   int64                      `json:"read_bytes,omitempty"`
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

type RuntimeWorldStateRead struct {
	Runtime   ShardRuntimeStatus    `json:"runtime"`
	StartedAt time.Time             `json:"started_at,omitempty"`
	SessionID string                `json:"session_id,omitempty"`
	ShardID   string                `json:"shard_id,omitempty"`
	Artifacts RuntimeArtifactBundle `json:"artifacts"`
	ReadError string                `json:"read_error,omitempty"`
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
	MigrationID   string                         `json:"migration_id"`
	Offset        int64                          `json:"offset,omitempty"`
	NextOffset    int64                          `json:"next_offset,omitempty"`
	Size          int64                          `json:"size,omitempty"`
	SHA256        string                         `json:"sha256,omitempty"`
	Data          []byte                         `json:"data,omitempty"`
	Complete      bool                           `json:"complete"`
	RecoveryRef   string                         `json:"recovery_ref,omitempty"`
	FetchLocation *RuntimeMigrationFetchLocation `json:"fetch_location,omitempty"`
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
	Version        int                   `json:"version"`
	WorkshopID     string                `json:"workshop_id"`
	TreeSHA256     string                `json:"tree_sha256"`
	ManifestSHA256 string                `json:"manifest_sha256"`
	Size           int64                 `json:"size"`
	FileCount      int                   `json:"file_count"`
	Metadata       RuntimeModMetadata    `json:"metadata"`
	CreatedAt      time.Time             `json:"created_at"`
	FetchSource    RuntimeModFetchSource `json:"fetch_source,omitempty"`
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
	InstallationID         string                          `json:"installation_id"`
	LastOperationID        string                          `json:"last_operation_id"`
	Mods                   map[string]string               `json:"mods"`
	ManagedSetupSHA256     string                          `json:"managed_setup_sha256"`
	WorkshopManifestSHA256 string                          `json:"workshop_manifest_sha256,omitempty"`
	Shards                 map[string]RuntimeModShardState `json:"shards"`
	UpdatedAt              time.Time                       `json:"updated_at"`
}

type RuntimeModFileStatus string

const (
	RuntimeModFileReady   RuntimeModFileStatus = "ready"
	RuntimeModFileMissing RuntimeModFileStatus = "missing"
	RuntimeModFileInvalid RuntimeModFileStatus = "invalid"
)

type RuntimeModFileState struct {
	Status          RuntimeModFileStatus `json:"status"`
	Reason          string               `json:"reason,omitempty"`
	Name            string               `json:"name,omitempty"`
	Version         string               `json:"version,omitempty"`
	InstalledSize   int64                `json:"installed_size,omitempty"`
	SteamManifestID string               `json:"steam_manifest_id,omitempty"`
	SteamUpdatedAt  *time.Time           `json:"steam_updated_at,omitempty"`
	MetadataReason  string               `json:"metadata_reason,omitempty"`
}

type RuntimeModWorldFileState struct {
	RoomID       string   `json:"room_id"`
	WorldID      string   `json:"world_id"`
	LoadedModIDs []string `json:"loaded_mod_ids"`
	LogObserved  bool     `json:"log_observed"`
}

type RuntimeModFilesObservation struct {
	InstallationID string                              `json:"installation_id"`
	Mods           map[string]RuntimeModFileState      `json:"mods"`
	Worlds         map[string]RuntimeModWorldFileState `json:"worlds"`
	ObservedAt     time.Time                           `json:"observed_at"`
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

type RuntimeModSchema struct {
	WorkshopID     string      `json:"workshop_id"`
	Options        interface{} `json:"options"`
	Parser         string      `json:"parser"`
	FallbackUsed   bool        `json:"fallback_used"`
	FallbackReason string      `json:"fallback_reason,omitempty"`
	Warnings       []string    `json:"warnings"`
}

type RuntimeModResult struct {
	Kind           RuntimeModUploadKind         `json:"kind,omitempty"`
	UploadID       string                       `json:"upload_id,omitempty"`
	OperationID    string                       `json:"release_operation_id,omitempty"`
	Offset         int64                        `json:"offset,omitempty"`
	NextOffset     int64                        `json:"next_offset,omitempty"`
	Size           int64                        `json:"size,omitempty"`
	SHA256         string                       `json:"sha256,omitempty"`
	Complete       bool                         `json:"complete"`
	CacheManifest  *RuntimeModCacheManifest     `json:"cache_manifest,omitempty"`
	Release        *RuntimeModReleaseState      `json:"release,omitempty"`
	Installation   *RuntimeModInstallationState `json:"installation,omitempty"`
	Files          *RuntimeModFilesObservation  `json:"files,omitempty"`
	Overrides      *RuntimeModOverridesChunk    `json:"overrides,omitempty"`
	Schema         *RuntimeModSchema            `json:"schema,omitempty"`
	AvailableBytes int64                        `json:"available_bytes,omitempty"`
	RuntimeVersion string                       `json:"runtime_version,omitempty"`
	FetchSource    RuntimeModFetchSource        `json:"fetch_source,omitempty"`
	FetchAttempts  []RuntimeModFetchAttempt     `json:"fetch_attempts,omitempty"`
	FetchLocation  *RuntimeModFetchLocation     `json:"fetch_location,omitempty"`
}

type RuntimeGameVersionResult struct {
	Installed         bool      `json:"installed"`
	AppID             string    `json:"app_id,omitempty"`
	UpdateMethod      string    `json:"update_method,omitempty"`
	GameVersion       string    `json:"game_version,omitempty"`
	SteamBuild        string    `json:"steam_build,omitempty"`
	Branch            string    `json:"branch,omitempty"`
	CurrentVersion    string    `json:"current_version,omitempty"`
	AvailableBytes    uint64    `json:"available_bytes"`
	SteamCMDAvailable bool      `json:"steamcmd_available"`
	UpdateSupported   bool      `json:"update_supported"`
	Log               string    `json:"log,omitempty"`
	ObservedAt        time.Time `json:"observed_at"`
}

type RuntimeNetworkResult struct {
	Address        string                         `json:"address,omitempty"`
	Region         RuntimeNetworkRegion           `json:"region,omitempty"`
	EndpointProbes []RuntimeNetworkEndpointResult `json:"endpoint_probes,omitempty"`
	ReceivedTokens []string                       `json:"received_tokens,omitempty"`
	ObservedAt     time.Time                      `json:"observed_at"`
}

type RuntimeNetworkEndpointResult struct {
	Address       string `json:"address"`
	Port          int    `json:"port"`
	Reachable     bool   `json:"reachable"`
	LatencyMillis int64  `json:"latency_millis,omitempty"`
	Error         string `json:"error,omitempty"`
}

type RuntimeConfigurationResult struct {
	Warnings         []string                   `json:"warnings,omitempty"`
	RevisionConflict bool                       `json:"revision_conflict,omitempty"`
	PublicationID    string                     `json:"publication_id"`
	Offset           int64                      `json:"offset,omitempty"`
	NextOffset       int64                      `json:"next_offset,omitempty"`
	Size             int64                      `json:"size,omitempty"`
	SHA256           string                     `json:"sha256,omitempty"`
	Complete         bool                       `json:"complete"`
	Files            []RuntimeConfigurationFile `json:"files,omitempty"`
	TokenStatus      *RuntimeClusterTokenStatus `json:"token_status,omitempty"`
}

// RuntimeClusterTokenStatus intentionally carries no token material. The
// digest is calculated from the normalized token and is used only for
// optimistic concurrency when the controller publishes a replacement.
type RuntimeClusterTokenStatus struct {
	Exists      bool      `json:"exists"`
	Configured  bool      `json:"configured"`
	MaskedValue string    `json:"masked_value,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	Mode        uint32    `json:"mode,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

type RuntimeClusterTokenReveal struct {
	Exists    bool      `json:"exists"`
	Token     string    `json:"token,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// RuntimeConfigurationFile is one allowlisted configuration file resolved
// inside an Agent's trusted DST installation. Missing optional files are
// represented explicitly so callers can calculate a stable revision.
type RuntimeConfigurationFile struct {
	Name      string    `json:"name"`
	Exists    bool      `json:"exists"`
	Mode      uint32    `json:"mode,omitempty"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Data      []byte    `json:"data,omitempty"`
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

type RuntimeRoomRecoveryResult struct {
	RecoveryRef string `json:"recovery_ref"`
}

type RuntimeOperationResult struct {
	EntityArtwork    *RuntimeEntityArtworkResult `json:"entity_artwork,omitempty"`
	GameInstallation *GameInstallationReport     `json:"game_installation,omitempty"`
	LuaJITReleases   []LuaJITRelease             `json:"luajit_releases,omitempty"`
	LuaJITRelease    *LuaJITRelease              `json:"luajit_release,omitempty"`
	LuaJIT           *RuntimePerformanceReport   `json:"luajit,omitempty"`
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
	ChatLogs         *RuntimeChatLogResult       `json:"chat_logs,omitempty"`
	Artifacts        *RuntimeArtifactBundle      `json:"artifacts,omitempty"`
	WorldState       *RuntimeWorldStateRead      `json:"world_state,omitempty"`
	Evidence         *RuntimeOperationEvidence   `json:"evidence,omitempty"`
	Migration        *RuntimeMigrationResult     `json:"migration,omitempty"`
	Backup           *RuntimeBackupResult        `json:"backup,omitempty"`
	Mod              *RuntimeModResult           `json:"mod,omitempty"`
	GameVersion      *RuntimeGameVersionResult   `json:"game_version,omitempty"`
	Network          *RuntimeNetworkResult       `json:"network,omitempty"`
	CPU              *RuntimeCPUResult           `json:"cpu,omitempty"`
	Configuration    *RuntimeConfigurationResult `json:"configuration,omitempty"`
	ClusterToken     *RuntimeClusterTokenReveal  `json:"cluster_token,omitempty"`
	Map              *RuntimeMapResult           `json:"map,omitempty"`
	RoomRecovery     *RuntimeRoomRecoveryResult  `json:"room_recovery,omitempty"`
}

func IsRuntimeAction(value RuntimeAction) bool {
	if IsGameInstallationAction(value) {
		return true
	}
	switch value {
	case RuntimeActionConsoleHealth, RuntimeActionConsoleSend, RuntimeActionObserveOperation, RuntimeActionReadLogs,
		RuntimeActionChatLogsList, RuntimeActionChatLogsRead, RuntimeActionReadArtifacts, RuntimeActionWorldStateRead, RuntimeActionEntityArtwork,
		RuntimeActionMigrationExportPrepare, RuntimeActionMigrationExportRead, RuntimeActionMigrationExportRelease,
		RuntimeActionMigrationPeerGrant, RuntimeActionMigrationFetch,
		RuntimeActionMigrationImportBegin, RuntimeActionMigrationImportWrite, RuntimeActionMigrationImportCommit,
		RuntimeActionMigrationTargetRollback, RuntimeActionMigrationTargetComplete,
		RuntimeActionMigrationSourceFinalize, RuntimeActionMigrationSourceRollback, RuntimeActionMigrationSourceComplete:
		return true
	case RuntimeActionBackupStage, RuntimeActionBackupRead, RuntimeActionBackupRelease,
		RuntimeActionRestoreBegin, RuntimeActionRestoreWrite, RuntimeActionRestorePrepare,
		RuntimeActionRestorePublish, RuntimeActionRestoreRollback, RuntimeActionRestoreComplete:
		return true
	case RuntimeActionModTargetObserve, RuntimeActionModCacheInspect, RuntimeActionModPeerGrant, RuntimeActionModFetch, RuntimeActionModDownload, RuntimeActionModLink, RuntimeActionModUploadBegin, RuntimeActionModUploadWrite, RuntimeActionModUploadCommit,
		RuntimeActionModReleasePlanBegin, RuntimeActionModReleasePlanWrite, RuntimeActionModReleasePlanCommit,
		RuntimeActionModReleasePrepare, RuntimeActionModReleasePublish, RuntimeActionModReleaseRollback,
		RuntimeActionModReleaseComplete, RuntimeActionModReleaseState, RuntimeActionModInstallationState, RuntimeActionModFilesObserve, RuntimeActionModFilesInventory, RuntimeActionModSchemaRead, RuntimeActionModOverridesRead:
		return true
	case RuntimeActionLuaJITObserve, RuntimeActionLuaJITInstall, RuntimeActionLuaJITDownload, RuntimeActionGameVersionObserve, RuntimeActionGameVersionUpdate:
		return true
	case RuntimeActionNetworkEgressObserve, RuntimeActionNetworkEndpointListen, RuntimeActionNetworkEndpointProbe:
		return true
	case RuntimeActionCPUPrepare, RuntimeActionCPUApply, RuntimeActionCPUObserve:
		return true
	case RuntimeActionConfigurationRead, RuntimeActionConfigurationApply, RuntimeActionModConfigurationWrite, RuntimeActionClusterTokenReveal, RuntimeActionConfigurationBegin, RuntimeActionConfigurationWrite, RuntimeActionConfigurationPrepare,
		RuntimeActionConfigurationPublish, RuntimeActionConfigurationRollback, RuntimeActionConfigurationComplete:
		return true
	case RuntimeActionRoomRecoveryMove:
		return true
	case RuntimeActionMapSessions, RuntimeActionMapStatus, RuntimeActionMapSnapshotPrepare, RuntimeActionMapRender, RuntimeActionMapRead, RuntimeActionMapRelease:
		return true
	default:
		return false
	}
}

func RuntimeActionMutates(value RuntimeAction) bool {
	if IsGameInstallationAction(value) {
		return value != RuntimeActionGameInstallationObserve
	}
	switch value {
	case RuntimeActionConsoleSend, RuntimeActionMigrationExportPrepare, RuntimeActionMigrationExportRelease,
		RuntimeActionMigrationFetch,
		RuntimeActionMigrationImportBegin, RuntimeActionMigrationImportWrite, RuntimeActionMigrationImportCommit,
		RuntimeActionMigrationTargetRollback, RuntimeActionMigrationTargetComplete,
		RuntimeActionMigrationSourceFinalize, RuntimeActionMigrationSourceRollback, RuntimeActionMigrationSourceComplete:
		return true
	case RuntimeActionBackupStage, RuntimeActionBackupRelease,
		RuntimeActionRestoreBegin, RuntimeActionRestoreWrite, RuntimeActionRestorePrepare,
		RuntimeActionRestorePublish, RuntimeActionRestoreRollback, RuntimeActionRestoreComplete:
		return true
	case RuntimeActionModFetch, RuntimeActionModDownload, RuntimeActionModLink, RuntimeActionModUploadBegin, RuntimeActionModUploadWrite, RuntimeActionModUploadCommit,
		RuntimeActionModReleasePlanBegin, RuntimeActionModReleasePlanWrite, RuntimeActionModReleasePlanCommit,
		RuntimeActionModReleasePrepare, RuntimeActionModReleasePublish, RuntimeActionModReleaseRollback,
		RuntimeActionModReleaseComplete:
		return true
	case RuntimeActionLuaJITInstall, RuntimeActionLuaJITDownload, RuntimeActionGameVersionUpdate:
		return true
	case RuntimeActionCPUPrepare, RuntimeActionCPUApply:
		return true
	case RuntimeActionConfigurationApply, RuntimeActionModConfigurationWrite, RuntimeActionConfigurationBegin, RuntimeActionConfigurationWrite, RuntimeActionConfigurationPrepare,
		RuntimeActionConfigurationPublish, RuntimeActionConfigurationRollback, RuntimeActionConfigurationComplete:
		return true
	case RuntimeActionRoomRecoveryMove:
		return true
	case RuntimeActionMapSnapshotPrepare, RuntimeActionMapRender, RuntimeActionMapRelease:
		return true
	default:
		return false
	}
}

func RuntimeOperationRequiresLease(request RuntimeOperationRequest) bool {
	if request.Action == RuntimeActionConsoleSend && request.Console != nil && request.Console.Mode == ConsoleModeProbe {
		return false
	}
	return RuntimeActionMutates(request.Action)
}
