package mods

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrInvalidModID        = errors.New("workshop mod id is invalid")
	ErrInvalidRequest      = errors.New("mod request is invalid")
	ErrRoomNotManaged      = errors.New("room must be managed before mods can be changed")
	ErrModNotConfigured    = errors.New("mod is not configured for the selected world")
	ErrModNotDownloaded    = errors.New("mod is not downloaded on the selected runtime target")
	ErrModInfoUnavailable  = errors.New("modinfo.lua is unavailable")
	ErrRevisionConflict    = errors.New("mod configuration revision has changed")
	ErrNoChanges           = errors.New("mod configuration has no changes")
	ErrConfirmationNeeded  = errors.New("exact room name confirmation is required")
	ErrSteamKeyRequired    = errors.New("steam web api key is required for text search")
	ErrSteamCMDUnavailable = errors.New("steamcmd is unavailable")
	ErrSteamCMDDownload    = errors.New("steamcmd workshop download failed")
	ErrWorkshopItemMissing = errors.New("workshop download is missing")
	ErrUnsupportedLuaValue = errors.New("Lua value is not supported")
)

type FieldError struct {
	Fields map[string]string
}

func (e *FieldError) Error() string { return ErrInvalidRequest.Error() }
func (e *FieldError) Unwrap() error { return ErrInvalidRequest }

type RevisionConflictError struct {
	CurrentRevision string
}

func (e *RevisionConflictError) Error() string { return ErrRevisionConflict.Error() }
func (e *RevisionConflictError) Unwrap() error { return ErrRevisionConflict }

type SteamMod struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	AuthorID        string    `json:"authorId,omitempty"`
	Author          string    `json:"author,omitempty"`
	MetadataWarning string    `json:"metadataWarning,omitempty"`
	Version         string    `json:"version,omitempty"`
	Description     string    `json:"description,omitempty"`
	PreviewURL      string    `json:"previewUrl,omitempty"`
	Subscriptions   int64     `json:"subscriptions"`
	Score           float64   `json:"score"`
	RatingCount     int64     `json:"ratingCount"`
	Favorites       int64     `json:"favorites"`
	Views           int64     `json:"views"`
	FileSize        int64     `json:"fileSize"`
	SteamManifestID string    `json:"steamManifestId,omitempty"`
	CreatedAt       time.Time `json:"createdAt,omitempty"`
	UpdatedAt       time.Time `json:"updatedAt,omitempty"`
	Dependencies    []string  `json:"dependencies"`
	Tags            []string  `json:"tags"`
}

type SearchSort string

const (
	SearchSortRelevance      SearchSort = "relevance"
	SearchSortTrend          SearchSort = "trend"
	SearchSortMostRecent     SearchSort = "most_recent"
	SearchSortLastUpdated    SearchSort = "last_updated"
	SearchSortMostSubscribed SearchSort = "most_subscribed"
	SearchSortTopRated       SearchSort = "top_rated"
)

type SearchOptions struct {
	Query    string
	Sort     SearchSort
	Days     int
	Tags     []string
	Page     int
	PageSize int
}

type SearchResult struct {
	Warning  string     `json:"warning,omitempty"`
	Items    []SteamMod `json:"items"`
	Total    int        `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"pageSize"`
}

type HealthState string

const (
	HealthHealthy         HealthState = "healthy"
	HealthDisabled        HealthState = "disabled"
	HealthNotDownloaded   HealthState = "not_downloaded"
	HealthNotInstalled    HealthState = "not_installed"
	HealthNotLoaded       HealthState = "not_loaded"
	HealthUpdateAvailable HealthState = "update_available"
	HealthCorrupt         HealthState = "corrupt"
	HealthParseWarning    HealthState = "parse_warning"
)

type ModState struct {
	SteamMod
	LatestVersion             string                    `json:"latestVersion,omitempty"`
	RuntimeVersion            string                    `json:"runtimeVersion,omitempty"`
	RuntimeVersionStatus      string                    `json:"runtimeVersionStatus,omitempty"`
	RuntimeCurrentTargets     int                       `json:"runtimeCurrentTargets"`
	RuntimeOutdatedTargets    int                       `json:"runtimeOutdatedTargets"`
	RuntimeUnknownTargets     int                       `json:"runtimeUnknownVersionTargets"`
	RuntimeVersions           []RuntimeModVersionTarget `json:"runtimeVersions"`
	RuntimeObserved           bool                      `json:"runtimeObserved"`
	RuntimeFileStatus         string                    `json:"runtimeFileStatus,omitempty"`
	RuntimeReadyTargets       int                       `json:"runtimeReadyTargets"`
	RuntimePendingTargets     int                       `json:"runtimePendingTargets"`
	RuntimeUnavailableTargets int                       `json:"runtimeUnavailableTargets"`
	RuntimeTotalTargets       int                       `json:"runtimeTotalTargets"`
	Configured                bool                      `json:"configured"`
	Downloaded                bool                      `json:"downloaded"`
	Installed                 bool                      `json:"installed"`
	Loaded                    bool                      `json:"loaded"`
	Enabled                   bool                      `json:"enabled"`
	ConfiguredWorlds          []string                  `json:"configuredWorlds"`
	EnabledWorlds             []string                  `json:"enabledWorlds"`
	InstalledWorlds           []string                  `json:"installedWorlds"`
	LoadedWorlds              []string                  `json:"loadedWorlds"`
	LocalUpdatedAt            *time.Time                `json:"localUpdatedAt,omitempty"`
	WorkshopManifest          bool                      `json:"workshopManifest"`
	ManifestUpdatedAt         *time.Time                `json:"manifestUpdatedAt,omitempty"`
	Health                    HealthState               `json:"health"`
	HealthMessage             string                    `json:"healthMessage"`
	RepairAction              string                    `json:"repairAction,omitempty"`
	Parser                    string                    `json:"parser,omitempty"`
	FallbackUsed              bool                      `json:"fallbackUsed"`
	FallbackReason            string                    `json:"fallbackReason,omitempty"`
	Warnings                  []string                  `json:"warnings"`
}

type RuntimeModVersionTarget struct {
	TargetID        string     `json:"targetId"`
	InstallationID  string     `json:"installationId"`
	Version         string     `json:"version,omitempty"`
	SteamManifestID string     `json:"steamManifestId,omitempty"`
	SteamUpdatedAt  *time.Time `json:"steamUpdatedAt,omitempty"`
	Status          string     `json:"status"`
	MetadataReason  string     `json:"metadataReason,omitempty"`
}

type RuntimeModRoomReference struct {
	RoomID    string `json:"roomId"`
	RoomName  string `json:"roomName"`
	WorldID   string `json:"worldId"`
	WorldName string `json:"worldName"`
}

type RuntimeInstallationMod struct {
	SteamMod
	CurrentVersion        string                    `json:"currentVersion,omitempty"`
	LatestVersion         string                    `json:"latestVersion,omitempty"`
	LatestSteamManifestID string                    `json:"latestSteamManifestId,omitempty"`
	VersionStatus         string                    `json:"versionStatus"`
	FileStatus            string                    `json:"fileStatus"`
	FileReason            string                    `json:"fileReason,omitempty"`
	InstalledSize         int64                     `json:"installedSize"`
	SteamManifestID       string                    `json:"steamManifestId,omitempty"`
	SteamUpdatedAt        *time.Time                `json:"steamUpdatedAt,omitempty"`
	MetadataReason        string                    `json:"metadataReason,omitempty"`
	RoomReferences        []RuntimeModRoomReference `json:"roomReferences"`
}

type RuntimeInstallationModInventory struct {
	TargetID          string                   `json:"targetId"`
	InstallationID    string                   `json:"installationId"`
	Items             []RuntimeInstallationMod `json:"items"`
	Total             int                      `json:"total"`
	Current           int                      `json:"current"`
	Outdated          int                      `json:"outdated"`
	Unknown           int                      `json:"unknown"`
	Invalid           int                      `json:"invalid"`
	ObservedAt        time.Time                `json:"observedAt"`
	MetadataWarning   string                   `json:"metadataWarning,omitempty"`
	ReferencesWarning string                   `json:"referencesWarning,omitempty"`
}

type ModList struct {
	Profile         *RoomModProfile `json:"profile,omitempty"`
	Items           []ModState      `json:"items"`
	Total           int             `json:"total"`
	Healthy         int             `json:"healthy"`
	Attention       int             `json:"attention"`
	CheckedAt       time.Time       `json:"checkedAt"`
	MetadataWarning string          `json:"metadataWarning,omitempty"`
}

type InstallRequest struct {
	ModID               string   `json:"modId"`
	WorldIDs            []string `json:"worldIds"`
	Enabled             bool     `json:"enabled"`
	IncludeDependencies bool     `json:"includeDependencies"`
}

type DownloadRequest struct {
	ModID               string `json:"modId"`
	IncludeDependencies bool   `json:"includeDependencies"`
}

type AddToRoomRequest struct {
	WorldIDs            []string `json:"worldIds"`
	Enabled             bool     `json:"enabled"`
	IncludeDependencies bool     `json:"includeDependencies"`
}

type ModActionRequest struct {
	WorldIDs     []string `json:"worldIds"`
	Confirmation string   `json:"confirmation"`
	RemoveFiles  bool     `json:"removeFiles"`
}

type EnableRequest struct {
	WorldIDs                 []string          `json:"worldIds"`
	Enabled                  bool              `json:"enabled"`
	ExpectedRevision         string            `json:"expectedRevision,omitempty"`
	ExpectedRevisions        map[string]string `json:"expectedRevisions,omitempty"`
	ExpectedTopologyRevision string            `json:"expectedTopologyRevision,omitempty"`
}

type ActionResult struct {
	ModIDs             []string `json:"modIds"`
	ProtectionBackupID string   `json:"protectionBackupId,omitempty"`
	Message            string   `json:"message"`
	Warnings           []string `json:"warnings,omitempty"`
}

type ParserResult struct {
	Values         map[string]interface{} `json:"values"`
	Parser         string                 `json:"parser"`
	FallbackUsed   bool                   `json:"fallbackUsed"`
	FallbackReason string                 `json:"fallbackReason,omitempty"`
	Warnings       []string               `json:"warnings"`
}

type ConfigOption struct {
	Value interface{} `json:"value"`
	Label string      `json:"label"`
	Hint  string      `json:"hint,omitempty"`
}

type ConfigField struct {
	Key          string         `json:"key"`
	Label        string         `json:"label"`
	Description  string         `json:"description,omitempty"`
	Type         string         `json:"type"`
	DefaultValue interface{}    `json:"defaultValue,omitempty"`
	Options      []ConfigOption `json:"options"`
}

type ModConfiguration struct {
	Revision       string                 `json:"revision"`
	RoomID         string                 `json:"roomId"`
	WorldID        string                 `json:"worldId"`
	ModID          string                 `json:"modId"`
	Enabled        bool                   `json:"enabled"`
	Parser         string                 `json:"parser"`
	FallbackUsed   bool                   `json:"fallbackUsed"`
	FallbackReason string                 `json:"fallbackReason,omitempty"`
	Warnings       []string               `json:"warnings"`
	RawPreserved   bool                   `json:"rawPreserved"`
	SchemaVersion  string                 `json:"schemaVersion"`
	Fields         []ConfigField          `json:"fields"`
	Values         map[string]interface{} `json:"values"`
	Overrides      map[string]interface{} `json:"overrides"`
	UnknownValues  map[string]interface{} `json:"unknownValues"`
}

type ConfigurationFile struct {
	RoomID   string    `json:"roomId"`
	WorldID  string    `json:"worldId"`
	FileName string    `json:"fileName"`
	Content  string    `json:"content"`
	Exists   bool      `json:"exists"`
	Revision string    `json:"revision"`
	ReadAt   time.Time `json:"readAt"`
}

type ConfigUpdateRequest struct {
	SourceWorldID            string                     `json:"sourceWorldId,omitempty"`
	ExpectedRevision         string                     `json:"expectedRevision"`
	ExpectedRevisions        map[string]string          `json:"expectedRevisions,omitempty"`
	ExpectedTopologyRevision string                     `json:"expectedTopologyRevision,omitempty"`
	WorldIDs                 []string                   `json:"worldIds,omitempty"`
	Enabled                  bool                       `json:"enabled"`
	PreserveEnabled          bool                       `json:"preserveEnabled,omitempty"`
	Patch                    map[string]json.RawMessage `json:"patch"`
}

type ConfigChange struct {
	Path      string      `json:"path"`
	Label     string      `json:"label"`
	Before    interface{} `json:"before,omitempty"`
	After     interface{} `json:"after,omitempty"`
	Operation string      `json:"operation"`
}

type ConfigPreview struct {
	Revision     string         `json:"revision"`
	NextRevision string         `json:"nextRevision"`
	Changes      []ConfigChange `json:"changes"`
	Warnings     []string       `json:"warnings"`
	RawPreserved bool           `json:"rawPreserved"`
}

type ConfigApplyResult struct {
	Revision           string            `json:"revision"`
	Revisions          map[string]string `json:"revisions,omitempty"`
	Changes            []ConfigChange    `json:"changes"`
	Warnings           []string          `json:"warnings"`
	ProtectionBackupID string            `json:"protectionBackupId"`
	WorldIDs           []string          `json:"worldIds,omitempty"`
	PublishedTargets   int               `json:"publishedTargets,omitempty"`
}
