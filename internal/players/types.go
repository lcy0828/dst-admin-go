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
	ID              string     `json:"id"`
	RoomID          string     `json:"roomId"`
	WorldID         string     `json:"worldId"`
	WorldName       string     `json:"worldName"`
	Name            string     `json:"name"`
	Prefab          string     `json:"prefab,omitempty"`
	Online          bool       `json:"online"`
	Banned          bool       `json:"banned"`
	BanReason       string     `json:"banReason,omitempty"`
	BannedAt        *time.Time `json:"bannedAt,omitempty"`
	BanExpiresAt    *time.Time `json:"banExpiresAt,omitempty"`
	Admin           bool       `json:"admin"`
	Age             int        `json:"age"`
	NetID           string     `json:"netId,omitempty"`
	Performance     int        `json:"performance"`
	HealthPercent   *float64   `json:"healthPercent,omitempty"`
	HungerPercent   *float64   `json:"hungerPercent,omitempty"`
	SanityPercent   *float64   `json:"sanityPercent,omitempty"`
	Temperature     *float64   `json:"temperature,omitempty"`
	Moisture        *float64   `json:"moisture,omitempty"`
	FirstSeenAt     time.Time  `json:"firstSeenAt"`
	LastSeenAt      time.Time  `json:"lastSeenAt"`
	StatusChangedAt time.Time  `json:"statusChangedAt"`
	LastRefreshedAt time.Time  `json:"lastRefreshedAt"`
}

type Observation struct {
	ID            string
	Name          string
	Prefab        string
	Admin         bool
	Age           int
	NetID         string
	Performance   int
	HealthPercent *float64
	HungerPercent *float64
	SanityPercent *float64
	Temperature   *float64
	Moisture      *float64
}

type ListFilter struct {
	Query   string
	Status  string
	WorldID string
	Prefab  string
	Limit   int
	Offset  int
}

type List struct {
	Items           []Player   `json:"items"`
	Total           int        `json:"total"`
	Online          int        `json:"online"`
	Offline         int        `json:"offline"`
	Banned          int        `json:"banned"`
	Limit           int        `json:"limit"`
	Offset          int        `json:"offset"`
	LastRefreshedAt *time.Time `json:"lastRefreshedAt,omitempty"`
}

type WorldTarget struct {
	ID   string
	Name string
}

type RefreshResult struct {
	WorldID string `json:"worldId"`
	Count   int    `json:"count"`
	Running bool   `json:"running"`
	Message string `json:"message"`
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
