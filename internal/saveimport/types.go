package saveimport

import (
	"errors"
	"time"
)

const (
	MaxUploadBytes  = int64(16 * 1024 * 1024 * 1024)
	MaxContentBytes = int64(64 * 1024 * 1024 * 1024)
	MaxEntries      = 100000
)

var (
	ErrImportNotFound    = errors.New("save import not found")
	ErrInvalidArchive    = errors.New("save import archive is invalid")
	ErrUnsafeArchive     = errors.New("save import archive contains an unsafe entry")
	ErrArchiveTooLarge   = errors.New("save import archive exceeds safety limits")
	ErrInvalidRequest    = errors.New("save import request is invalid")
	ErrImportNotReady    = errors.New("save import is not ready")
	ErrCandidateMissing  = errors.New("save import candidate does not exist")
	ErrRoomRunning       = errors.New("all room worlds must be stopped before import")
	ErrTargetExists      = errors.New("save import target already exists")
	ErrTokenRequired     = errors.New("cluster token is required")
	ErrMissingMods       = errors.New("required workshop mods are not downloaded")
	ErrTargetNotManaged  = errors.New("target room must be managed")
	ErrConfirmation      = errors.New("exact target room name confirmation is required")
	ErrPartialImport     = errors.New("partial shard import requires explicit confirmation")
	ErrImportBusy        = errors.New("save import already has an active operation")
	ErrPortConflict      = errors.New("save import network ports conflict with local rooms")
	ErrInsufficientSpace = errors.New("insufficient space for save import")
)

type Status string

const (
	StatusUploaded  Status = "uploaded"
	StatusAnalyzing Status = "analyzing"
	StatusReady     Status = "ready"
	StatusInvalid   Status = "invalid"
	StatusApplying  Status = "applying"
	StatusApplied   Status = "applied"
)

type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type Diagnostic struct {
	Code     string                 `json:"code"`
	Severity Severity               `json:"severity"`
	Message  string                 `json:"message"`
	Path     string                 `json:"path,omitempty"`
	AutoFix  bool                   `json:"autoFix"`
	Details  map[string]interface{} `json:"details,omitempty"`
}

type PortManifest struct {
	Server         int `json:"server"`
	Authentication int `json:"authentication"`
	MasterServer   int `json:"masterServer"`
}

type WorldManifest struct {
	DirectoryName string       `json:"directoryName"`
	Name          string       `json:"name"`
	Role          string       `json:"role"`
	IsMaster      bool         `json:"isMaster"`
	ShardID       int          `json:"shardId"`
	Ports         PortManifest `json:"ports"`
	SessionCount  int          `json:"sessionCount"`
	ModFile       bool         `json:"modFile"`
	ServerFile    string       `json:"serverFile"`
}

type ModReference struct {
	ID         string   `json:"id"`
	Worlds     []string `json:"worlds"`
	Downloaded bool     `json:"downloaded"`
}

type Candidate struct {
	ID                 string          `json:"id"`
	Root               string          `json:"root"`
	DirectoryName      string          `json:"directoryName"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	GameMode           string          `json:"gameMode"`
	MaxPlayers         int             `json:"maxPlayers"`
	PvP                bool            `json:"pvp"`
	TokenPresent       bool            `json:"tokenPresent"`
	Worlds             []WorldManifest `json:"worlds"`
	Mods               []ModReference  `json:"mods"`
	Compatibility      string          `json:"compatibility"`
	Diagnostics        []Diagnostic    `json:"diagnostics"`
	IgnoredSystemFiles int             `json:"ignoredSystemFiles"`
}

type Manifest struct {
	SchemaVersion      string       `json:"schemaVersion"`
	Format             string       `json:"format"`
	SourceName         string       `json:"sourceName"`
	CompressedSize     int64        `json:"compressedSize"`
	ContentSize        int64        `json:"contentSize"`
	FileCount          int          `json:"fileCount"`
	SHA256             string       `json:"sha256"`
	NormalizedPaths    int          `json:"normalizedPaths"`
	IgnoredSystemFiles int          `json:"ignoredSystemFiles"`
	Candidates         []Candidate  `json:"candidates"`
	Diagnostics        []Diagnostic `json:"diagnostics"`
	AnalyzedAt         time.Time    `json:"analyzedAt"`
}

type Session struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	SourceName   string     `json:"sourceName"`
	Status       Status     `json:"status"`
	Size         int64      `json:"size"`
	SHA256       string     `json:"sha256,omitempty"`
	ErrorCode    string     `json:"errorCode,omitempty"`
	ErrorMessage string     `json:"errorMessage,omitempty"`
	Manifest     *Manifest  `json:"manifest,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	AppliedAt    *time.Time `json:"appliedAt,omitempty"`
}

type UploadResult struct {
	Import Session     `json:"import"`
	Job    interface{} `json:"job,omitempty"`
}

type DeleteRequest struct {
	Confirmation string `json:"confirmation"`
}

type ApplyMode string

const (
	ApplyModeNew     ApplyMode = "new"
	ApplyModeReplace ApplyMode = "replace"
	ApplyModeClone   ApplyMode = "clone"
)

type TokenPolicy string

const (
	TokenSource   TokenPolicy = "source"
	TokenPreserve TokenPolicy = "preserve"
	TokenProvided TokenPolicy = "provided"
	TokenNone     TokenPolicy = "none"
)

type NetworkPolicy string

const (
	NetworkSource   NetworkPolicy = "source"
	NetworkPreserve NetworkPolicy = "preserve"
	NetworkAuto     NetworkPolicy = "auto"
)

type ModPolicy string

const (
	ModsPreserve          ModPolicy = "preserve"
	ModsRequireDownloaded ModPolicy = "require_downloaded"
	ModsInstallMissing    ModPolicy = "install_missing"
)

type ApplyRequest struct {
	CandidateID       string        `json:"candidateId"`
	Mode              ApplyMode     `json:"mode"`
	TargetRoomID      string        `json:"targetRoomId,omitempty"`
	DirectoryName     string        `json:"directoryName,omitempty"`
	RoomName          string        `json:"roomName,omitempty"`
	Confirmation      string        `json:"confirmation,omitempty"`
	TokenPolicy       TokenPolicy   `json:"tokenPolicy"`
	ClusterToken      string        `json:"clusterToken,omitempty"`
	NetworkPolicy     NetworkPolicy `json:"networkPolicy"`
	ModPolicy         ModPolicy     `json:"modPolicy"`
	AllowPartial      bool          `json:"allowPartial"`
	AllowMissingToken bool          `json:"allowMissingToken"`
}

type ApplyResult struct {
	ImportID           string       `json:"importId"`
	CandidateID        string       `json:"candidateId"`
	Mode               ApplyMode    `json:"mode"`
	RoomID             string       `json:"roomId"`
	DirectoryName      string       `json:"directoryName"`
	RoomName           string       `json:"roomName"`
	ProtectionBackupID string       `json:"protectionBackupId,omitempty"`
	InstalledMods      []string     `json:"installedMods"`
	Warnings           []Diagnostic `json:"warnings"`
	AppliedAt          time.Time    `json:"appliedAt"`
}
