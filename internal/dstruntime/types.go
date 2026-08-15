package dstruntime

import (
	"errors"
	"time"
)

const (
	ProtocolVersion = 2
	RuntimeVersion  = "2.3.1"
)

var (
	ErrRuntimeNotInstalled   = errors.New("DST Admin runtime is not installed")
	ErrManagedBlockInvalid   = errors.New("customcommands.lua managed block is invalid")
	ErrManagedFileChanged    = errors.New("DST Admin managed runtime file was modified")
	ErrUnsafeRuntimePath     = errors.New("DST Admin runtime path is unsafe")
	ErrSnapshotUnavailable   = errors.New("DST Admin telemetry snapshot is unavailable")
	ErrSnapshotStale         = errors.New("DST Admin telemetry snapshot is stale")
	ErrSnapshotInvalid       = errors.New("DST Admin telemetry snapshot is invalid")
	ErrRuntimeUnavailable    = errors.New("DST Admin runtime is unavailable")
	ErrRuntimeResultAbsent   = errors.New("DST Admin runtime result is unavailable")
	ErrRuntimeResultStale    = errors.New("DST Admin runtime result is stale")
	ErrRuntimeResultInvalid  = errors.New("DST Admin runtime result is invalid")
	ErrRuntimeRequestInvalid = errors.New("DST Admin runtime request is invalid")
	ErrRuntimeActivation     = errors.New("DST Admin runtime activation failed")
	ErrRuntimeRefresh        = errors.New("DST Admin runtime snapshot refresh failed")
)

type InstallState string

const (
	InstallStateMissing   InstallState = "missing"
	InstallStateInstalled InstallState = "installed"
	InstallStateOutdated  InstallState = "outdated"
	InstallStateInvalid   InstallState = "invalid"
)

type WorldStatus struct {
	RoomID     string       `json:"roomId"`
	WorldID    string       `json:"worldId"`
	WorldName  string       `json:"worldName"`
	State      InstallState `json:"state"`
	Version    string       `json:"version,omitempty"`
	Protocol   int          `json:"protocolVersion,omitempty"`
	Changed    bool         `json:"changed,omitempty"`
	BackupPath string       `json:"backupPath,omitempty"`
	Message    string       `json:"message,omitempty"`
}

type Backup struct {
	ID        string    `json:"id"`
	WorldID   string    `json:"worldId"`
	CreatedAt time.Time `json:"createdAt"`
	Reason    string    `json:"reason"`
}

type HealthState string

const (
	HealthStateReady       HealthState = "ready"
	HealthStateStarting    HealthState = "starting"
	HealthStateStopped     HealthState = "stopped"
	HealthStateDegraded    HealthState = "degraded"
	HealthStateUnavailable HealthState = "unavailable"
)

type WorldReport struct {
	WorldStatus
	ProcessRunning bool        `json:"processRunning"`
	HealthState    HealthState `json:"healthState"`
	Health         *Health     `json:"health,omitempty"`
	HealthMessage  string      `json:"healthMessage,omitempty"`
}

type LifecycleMode string

const (
	LifecycleModeActivate LifecycleMode = "activate"
	LifecycleModeReload   LifecycleMode = "reload"
	LifecycleModeCurrent  LifecycleMode = "current"
)

type LifecycleResult struct {
	RoomID    string        `json:"roomId"`
	WorldID   string        `json:"worldId"`
	WorldName string        `json:"worldName"`
	Mode      LifecycleMode `json:"mode"`
	Health    Health        `json:"health"`
	Message   string        `json:"message"`
}

type Snapshot struct {
	SchemaVersion      int              `json:"schemaVersion"`
	ProducerVersion    string           `json:"producerVersion"`
	ProducerInstanceID string           `json:"producerInstanceId"`
	SessionID          string           `json:"sessionId"`
	ShardID            string           `json:"shardId"`
	Sequence           int64            `json:"sequence"`
	CapturedAtUnix     int64            `json:"capturedAtUnix"`
	Complete           bool             `json:"complete"`
	Players            []SnapshotPlayer `json:"players"`
	CapturedAt         time.Time        `json:"-"`
}

type SnapshotPlayer struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Prefab        string   `json:"prefab,omitempty"`
	Admin         bool     `json:"admin"`
	Age           int      `json:"age"`
	NetID         string   `json:"netId,omitempty"`
	NetScore      *int     `json:"netScore,omitempty"`
	HealthPercent *float64 `json:"healthPercent,omitempty"`
	HungerPercent *float64 `json:"hungerPercent,omitempty"`
	SanityPercent *float64 `json:"sanityPercent,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	Moisture      *float64 `json:"moisture,omitempty"`
}

type SnapshotRefreshResult struct {
	Players    Snapshot           `json:"players"`
	WorldState WorldStateSnapshot `json:"worldState"`
	Health     Health             `json:"health"`
}

type WorldStateSnapshot struct {
	SchemaVersion         int       `json:"schemaVersion"`
	ProducerVersion       string    `json:"producerVersion"`
	ProducerInstanceID    string    `json:"producerInstanceId"`
	SessionID             string    `json:"sessionId"`
	ShardID               string    `json:"shardId"`
	Sequence              int64     `json:"sequence"`
	CapturedAtUnix        int64     `json:"capturedAtUnix"`
	Complete              bool      `json:"complete"`
	Season                string    `json:"season"`
	Phase                 string    `json:"phase"`
	Cycles                *int      `json:"cycles,omitempty"`
	ElapsedDaysInSeason   *int      `json:"elapsedDaysInSeason,omitempty"`
	RemainingDaysInSeason *int      `json:"remainingDaysInSeason,omitempty"`
	SeasonProgress        *float64  `json:"seasonProgress,omitempty"`
	DayProgress           *float64  `json:"dayProgress,omitempty"`
	PhaseProgress         *float64  `json:"phaseProgress,omitempty"`
	Precipitation         string    `json:"precipitation"`
	MoonPhase             string    `json:"moonPhase"`
	Temperature           *float64  `json:"temperature,omitempty"`
	Wetness               *float64  `json:"wetness,omitempty"`
	Moisture              *float64  `json:"moisture,omitempty"`
	MoistureCeil          *float64  `json:"moistureCeil,omitempty"`
	PrecipitationRate     *float64  `json:"precipitationRate,omitempty"`
	NightmarePhase        string    `json:"nightmarePhase"`
	NightmareProgress     *float64  `json:"nightmareProgress,omitempty"`
	CapturedAt            time.Time `json:"-"`
}

type Health struct {
	SchemaVersion            int                     `json:"schemaVersion"`
	ProducerVersion          string                  `json:"producerVersion"`
	ProducerInstanceID       string                  `json:"producerInstanceId"`
	SessionID                string                  `json:"sessionId"`
	ShardID                  string                  `json:"shardId"`
	Running                  bool                    `json:"running"`
	Ready                    bool                    `json:"ready"`
	Writing                  bool                    `json:"writing"`
	Sequence                 int64                   `json:"sequence"`
	LastCapturedAtUnix       *int64                  `json:"lastCapturedAtUnix"`
	LastWrittenAtUnix        *int64                  `json:"lastWrittenAtUnix"`
	LastDurationMilliseconds *float64                `json:"lastDurationMilliseconds"`
	LastError                *string                 `json:"lastError"`
	ConsecutiveFailures      int                     `json:"consecutiveFailures"`
	Modules                  map[string]ModuleHealth `json:"modules,omitempty"`
	ReadAt                   time.Time               `json:"-"`
}

type ModuleHealth struct {
	Running                  bool     `json:"running"`
	Ready                    bool     `json:"ready"`
	Busy                     bool     `json:"busy,omitempty"`
	Sequence                 int64    `json:"sequence,omitempty"`
	LastCapturedAtUnix       *int64   `json:"lastCapturedAtUnix,omitempty"`
	LastWrittenAtUnix        *int64   `json:"lastWrittenAtUnix,omitempty"`
	LastDurationMilliseconds *float64 `json:"lastDurationMilliseconds,omitempty"`
	ConsecutiveFailures      int      `json:"consecutiveFailures,omitempty"`
	LastError                *string  `json:"lastError,omitempty"`
}

type CommandRequest struct {
	RequestID string                 `json:"requestId"`
	Action    string                 `json:"action"`
	Arguments map[string]interface{} `json:"arguments"`
}

type CommandReceipt struct {
	SchemaVersion      int                    `json:"schemaVersion"`
	ProducerVersion    string                 `json:"producerVersion"`
	ProducerInstanceID string                 `json:"producerInstanceId"`
	SessionID          string                 `json:"sessionId"`
	ShardID            string                 `json:"shardId"`
	Sequence           int64                  `json:"sequence"`
	RequestID          string                 `json:"requestId"`
	Action             string                 `json:"action"`
	OK                 bool                   `json:"ok"`
	Code               string                 `json:"code"`
	Message            string                 `json:"message"`
	Details            map[string]interface{} `json:"details,omitempty"`
	CompletedAtUnix    int64                  `json:"completedAtUnix"`
	CompletedAt        time.Time              `json:"-"`
}

type RuntimeEvent struct {
	Sequence       int64                  `json:"sequence"`
	Kind           string                 `json:"kind"`
	OccurredAtUnix int64                  `json:"occurredAtUnix"`
	Fields         map[string]interface{} `json:"fields"`
	OccurredAt     time.Time              `json:"-"`
}

type EventBatch struct {
	SchemaVersion      int            `json:"schemaVersion"`
	ProducerVersion    string         `json:"producerVersion"`
	ProducerInstanceID string         `json:"producerInstanceId"`
	SessionID          string         `json:"sessionId"`
	ShardID            string         `json:"shardId"`
	FirstSequence      int64          `json:"firstSequence"`
	LastSequence       int64          `json:"lastSequence"`
	Events             []RuntimeEvent `json:"events"`
	ReadAt             time.Time      `json:"-"`
}

type DiagnosticRequest struct {
	RequestID       string `json:"requestId"`
	Profile         string `json:"profile"`
	Prefab          string `json:"prefab,omitempty"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	SampleLimit     int    `json:"sampleLimit,omitempty"`
}

type DiagnosticReport struct {
	SchemaVersion      int                    `json:"schemaVersion"`
	ProducerVersion    string                 `json:"producerVersion"`
	ProducerInstanceID string                 `json:"producerInstanceId"`
	SessionID          string                 `json:"sessionId"`
	ShardID            string                 `json:"shardId"`
	Sequence           int64                  `json:"sequence"`
	RequestID          string                 `json:"requestId"`
	Profile            string                 `json:"profile"`
	OK                 bool                   `json:"ok"`
	Code               string                 `json:"code"`
	Message            string                 `json:"message"`
	Result             map[string]interface{} `json:"result,omitempty"`
	CompletedAtUnix    int64                  `json:"completedAtUnix"`
	CompletedAt        time.Time              `json:"-"`
}
