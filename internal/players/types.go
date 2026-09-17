package players

import (
	"errors"
	"time"
)

var (
	ErrInvalidPlayer        = errors.New("player identifier is invalid")
	ErrPlayerNotFound       = errors.New("player was not found")
	ErrInvalidFilter        = errors.New("player filter is invalid")
	ErrInvalidAction        = errors.New("player action is invalid")
	ErrConfirmationRequired = errors.New("player action confirmation is required")
	ErrRoomNotManaged       = errors.New("room must be managed before players can be managed")
	ErrWorldNotRunning      = errors.New("target world is not running")
	ErrProbeTimedOut        = errors.New("player probe timed out")
)

type FieldError struct {
	Fields map[string]string
}

func (e *FieldError) Error() string { return ErrInvalidAction.Error() }
func (e *FieldError) Unwrap() error { return ErrInvalidAction }

type Player struct {
	ID                   string          `json:"id"`
	RoomID               string          `json:"roomId"`
	WorldID              string          `json:"worldId"`
	WorldName            string          `json:"worldName"`
	WorldConfirmed       bool            `json:"worldConfirmed"`
	Name                 string          `json:"name"`
	Prefab               string          `json:"prefab,omitempty"`
	GameplayState        string          `json:"gameplayState,omitempty"`
	Online               bool            `json:"online"`
	Banned               bool            `json:"banned"`
	BanReason            string          `json:"banReason,omitempty"`
	BannedAt             *time.Time      `json:"bannedAt,omitempty"`
	BanExpiresAt         *time.Time      `json:"banExpiresAt,omitempty"`
	Admin                bool            `json:"admin"`
	Age                  int             `json:"age"`
	NetID                string          `json:"netId,omitempty"`
	NetScore             *int            `json:"netScore,omitempty"`
	Performance          *int            `json:"performance,omitempty"`
	HealthPercent        *float64        `json:"healthPercent,omitempty"`
	HungerPercent        *float64        `json:"hungerPercent,omitempty"`
	SanityPercent        *float64        `json:"sanityPercent,omitempty"`
	Health               *float64        `json:"health,omitempty"`
	HealthMax            *float64        `json:"healthMax,omitempty"`
	Hunger               *float64        `json:"hunger,omitempty"`
	HungerMax            *float64        `json:"hungerMax,omitempty"`
	Sanity               *float64        `json:"sanity,omitempty"`
	SanityMax            *float64        `json:"sanityMax,omitempty"`
	Temperature          *float64        `json:"temperature,omitempty"`
	Moisture             *float64        `json:"moisture,omitempty"`
	FirstSeenAt          time.Time       `json:"firstSeenAt"`
	LastSeenAt           time.Time       `json:"lastSeenAt"`
	LastSeenSource       DataSource      `json:"lastSeenSource,omitempty"`
	LastConnectedAt      *time.Time      `json:"lastConnectedAt,omitempty"`
	LastDisconnectedAt   *time.Time      `json:"lastDisconnectedAt,omitempty"`
	StatusChangedAt      time.Time       `json:"statusChangedAt"`
	LastRefreshedAt      time.Time       `json:"lastRefreshedAt"`
	PresenceStatus       FreshnessStatus `json:"presenceStatus"`
	PresenceObservedAt   *time.Time      `json:"presenceObservedAt,omitempty"`
	Fields               FieldStates     `json:"fields"`
	PresenceConflict     bool            `json:"presenceConflict"`
	ObservedWorldIDs     []string        `json:"observedWorldIds"`
	AccessListsAvailable bool            `json:"accessListsAvailable"`
	AccessWarning        string          `json:"accessWarning,omitempty"`
}

type FreshnessStatus string

const (
	FreshnessLive        FreshnessStatus = "live"
	FreshnessStale       FreshnessStatus = "stale"
	FreshnessUnavailable FreshnessStatus = "unavailable"
)

type FieldState struct {
	Source     DataSource      `json:"source"`
	ObservedAt *time.Time      `json:"observedAt,omitempty"`
	Status     FreshnessStatus `json:"status"`
}

type FieldStates map[string]FieldState

const (
	GameplayStateSelectingCharacter = "selecting_character"
	GameplayStateLoading            = "loading"
	GameplayStateAlive              = "alive"
	GameplayStateDead               = "dead"
	GameplayStateGhost              = "ghost"
	GameplayStateMigrating          = "migrating"
	GameplayStateUnknown            = "unknown"
)

type Observation struct {
	HistoryWorldID     string
	HistoryWorldName   string
	FirstSeenAt        time.Time
	LastSeenAt         time.Time
	LastConnectedAt    time.Time
	LastDisconnectedAt time.Time
	ID                 string
	Name               string
	Prefab             string
	GameplayState      string
	Admin              bool
	Age                int
	NetID              string
	NetScore           *int
	HealthPercent      *float64
	HungerPercent      *float64
	SanityPercent      *float64
	Health             *float64
	HealthMax          *float64
	Hunger             *float64
	HungerMax          *float64
	Sanity             *float64
	SanityMax          *float64
	Temperature        *float64
	Moisture           *float64
	Fields             FieldStates
}

type ListFilter struct {
	Query           string
	Status          string
	WorldID         string
	Prefab          string
	Limit           int
	Offset          int
	SkipAccessLists bool
}

type List struct {
	Items                []Player   `json:"items"`
	Total                int        `json:"total"`
	Online               int        `json:"online"`
	StaleOnline          int        `json:"staleOnline"`
	Offline              int        `json:"offline"`
	Banned               int        `json:"banned"`
	Limit                int        `json:"limit"`
	Offset               int        `json:"offset"`
	LastRefreshedAt      *time.Time `json:"lastRefreshedAt,omitempty"`
	AccessListsAvailable bool       `json:"accessListsAvailable"`
	Warnings             []string   `json:"warnings,omitempty"`
}

type WorldTarget struct {
	ID   string
	Name string
}

type RefreshResult struct {
	WorldID    string          `json:"worldId"`
	Count      int             `json:"count"`
	Running    bool            `json:"running"`
	Source     DataSource      `json:"source"`
	Status     FreshnessStatus `json:"status"`
	ObservedAt *time.Time      `json:"observedAt,omitempty"`
	Warning    string          `json:"warning,omitempty"`
	Message    string          `json:"message"`
}

type RefreshOutcome struct {
	WorldID  string
	Result   RefreshResult
	Deferred bool
	Err      error
}

// PresenceSnapshot is a safety-oriented room observation. Fresh is true only
// when every running world produced live telemetry during this collection.
type PresenceSnapshot struct {
	RoomID      string          `json:"roomId"`
	Online      int             `json:"online"`
	StaleOnline int             `json:"staleOnline"`
	Fresh       bool            `json:"fresh"`
	CheckedAt   time.Time       `json:"checkedAt"`
	Worlds      []PresenceWorld `json:"worlds"`
	Warnings    []string        `json:"warnings,omitempty"`
}

type PresenceWorld struct {
	WorldID    string          `json:"worldId"`
	Running    bool            `json:"running"`
	Count      int             `json:"count"`
	Status     FreshnessStatus `json:"status"`
	ObservedAt *time.Time      `json:"observedAt,omitempty"`
	Warning    string          `json:"warning,omitempty"`
}

type Action string

const (
	ActionKick            Action = "kick"
	ActionBan             Action = "ban"
	ActionUnban           Action = "unban"
	ActionAnnounce        Action = "announce"
	ActionKill            Action = "kill"
	ActionGodMode         Action = "god-mode"
	ActionCreativeMode    Action = "creative-mode"
	ActionResurrect       Action = "resurrect"
	ActionChangeCharacter Action = "change-character"
)

type ActionRequest struct {
	WorldID      string `json:"worldId"`
	Message      string `json:"message"`
	Confirmation string `json:"confirmation"`
	Reason       string `json:"reason"`
	Duration     string `json:"duration"`
	Enabled      *bool  `json:"enabled"`
}

type Ban struct {
	RoomID    string     `json:"roomId"`
	PlayerID  string     `json:"playerId"`
	Reason    string     `json:"reason"`
	Duration  string     `json:"duration"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

type ActionResult struct {
	PlayerID           string `json:"playerId"`
	WorldID            string `json:"worldId,omitempty"`
	Action             Action `json:"action"`
	Message            string `json:"message"`
	Warning            string `json:"warning,omitempty"`
	ProtectionBackupID string `json:"protectionBackupId,omitempty"`
}
