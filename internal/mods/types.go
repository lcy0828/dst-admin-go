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
	ErrModInfoUnavailable  = errors.New("modinfo.lua is unavailable")
	ErrRevisionConflict    = errors.New("mod configuration revision has changed")
	ErrNoChanges           = errors.New("mod configuration has no changes")
	ErrConfirmationNeeded  = errors.New("exact room name confirmation is required")
	ErrSteamKeyRequired    = errors.New("steam web api key is required for text search")
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
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	AuthorID      string    `json:"authorId,omitempty"`
	Author        string    `json:"author,omitempty"`
	Description   string    `json:"description,omitempty"`
	PreviewURL    string    `json:"previewUrl,omitempty"`
	Subscriptions int64     `json:"subscriptions"`
	Score         float64   `json:"score"`
	UpdatedAt     time.Time `json:"updatedAt,omitempty"`
	Dependencies  []string  `json:"dependencies"`
	Tags          []string  `json:"tags"`
}

type SearchResult struct {
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
	Configured        bool        `json:"configured"`
	Downloaded        bool        `json:"downloaded"`
	Installed         bool        `json:"installed"`
	Loaded            bool        `json:"loaded"`
	Enabled           bool        `json:"enabled"`
	ConfiguredWorlds  []string    `json:"configuredWorlds"`
	EnabledWorlds     []string    `json:"enabledWorlds"`
	InstalledWorlds   []string    `json:"installedWorlds"`
	LoadedWorlds      []string    `json:"loadedWorlds"`
	LocalUpdatedAt    *time.Time  `json:"localUpdatedAt,omitempty"`
	WorkshopManifest  bool        `json:"workshopManifest"`
	ManifestUpdatedAt *time.Time  `json:"manifestUpdatedAt,omitempty"`
	Health            HealthState `json:"health"`
	HealthMessage     string      `json:"healthMessage"`
	RepairAction      string      `json:"repairAction,omitempty"`
	Parser            string      `json:"parser,omitempty"`
	FallbackUsed      bool        `json:"fallbackUsed"`
	FallbackReason    string      `json:"fallbackReason,omitempty"`
	Warnings          []string    `json:"warnings"`
}

type ModList struct {
	Items           []ModState `json:"items"`
	Total           int        `json:"total"`
	Healthy         int        `json:"healthy"`
	Attention       int        `json:"attention"`
	CheckedAt       time.Time  `json:"checkedAt"`
	MetadataWarning string     `json:"metadataWarning,omitempty"`
}

type InstallRequest struct {
	ModID               string   `json:"modId"`
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
	WorldIDs []string `json:"worldIds"`
	Enabled  bool     `json:"enabled"`
}

type ActionResult struct {
	ModIDs             []string `json:"modIds"`
	ProtectionBackupID string   `json:"protectionBackupId,omitempty"`
	Message            string   `json:"message"`
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
	ExpectedRevision string                     `json:"expectedRevision"`
	Enabled          bool                       `json:"enabled"`
	Patch            map[string]json.RawMessage `json:"patch"`
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
	Revision           string         `json:"revision"`
	Changes            []ConfigChange `json:"changes"`
	Warnings           []string       `json:"warnings"`
	ProtectionBackupID string         `json:"protectionBackupId"`
}
