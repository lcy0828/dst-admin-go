package structuredlogs

import (
	"errors"
	"time"
)

var (
	ErrInvalidFilter  = errors.New("structured log filter is invalid")
	ErrInvalidRule    = errors.New("structured log rule is invalid")
	ErrRuleNotFound   = errors.New("structured log rule was not found")
	ErrBuiltInRule    = errors.New("built-in structured log rule cannot be deleted")
	ErrRoomNotManaged = errors.New("room must be managed before structured logs can be used")
)

type MatchMode string

const (
	MatchModeSingle    MatchMode = "single"
	MatchModeMultiLine MatchMode = "multi_line"
	MatchModeHeadTail  MatchMode = "head_tail"
)

type LogType string

const (
	TypeSystem  LogType = "system"
	TypeChat    LogType = "chat"
	TypePlayer  LogType = "player"
	TypeEntity  LogType = "entity"
	TypeWorld   LogType = "world"
	TypeError   LogType = "error"
	TypeWarning LogType = "warning"
	TypeUnknown LogType = "unknown"
)

type Entry struct {
	ID              int64      `json:"id"`
	RoomID          string     `json:"roomId"`
	WorldID         string     `json:"worldId"`
	WorldName       string     `json:"worldName"`
	Type            LogType    `json:"type"`
	Content         string     `json:"content"`
	RawContent      string     `json:"rawContent"`
	RuleID          string     `json:"ruleId,omitempty"`
	RuleName        string     `json:"ruleName,omitempty"`
	SourceCursor    int64      `json:"sourceCursor"`
	SourceTimestamp string     `json:"sourceTimestamp,omitempty"`
	OccurredAt      *time.Time `json:"occurredAt,omitempty"`
	ObservedAt      time.Time  `json:"observedAt"`
}

type ListFilter struct {
	Query   string
	WorldID string
	Type    LogType
	Limit   int
	Offset  int
}

type List struct {
	Items           []Entry         `json:"items"`
	Total           int             `json:"total"`
	Counts          map[LogType]int `json:"counts"`
	Limit           int             `json:"limit"`
	Offset          int             `json:"offset"`
	LastRefreshedAt *time.Time      `json:"lastRefreshedAt,omitempty"`
}

type Rule struct {
	ID          string    `json:"id"`
	RoomID      string    `json:"roomId"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	LogType     LogType   `json:"logType"`
	Pattern     string    `json:"pattern"`
	Regex       bool      `json:"regex"`
	Enabled     bool      `json:"enabled"`
	Priority    int       `json:"priority"`
	MatchMode   MatchMode `json:"matchMode"`
	TailPattern string    `json:"tailPattern"`
	BuiltIn     bool      `json:"builtIn"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type RuleInput struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	LogType     LogType   `json:"logType"`
	Pattern     string    `json:"pattern"`
	Regex       bool      `json:"regex"`
	Enabled     bool      `json:"enabled"`
	Priority    int       `json:"priority"`
	MatchMode   MatchMode `json:"matchMode"`
	TailPattern string    `json:"tailPattern"`
}

type RuleTestInput struct {
	RuleInput
	Sample string `json:"sample"`
}

type RuleTestResult struct {
	Matched bool   `json:"matched"`
	Content string `json:"content"`
}

type RefreshResult struct {
	WorldID   string `json:"worldId"`
	Count     int    `json:"count"`
	Truncated bool   `json:"truncated"`
	Message   string `json:"message"`
}

type ClearResult struct {
	RoomID    string    `json:"roomId"`
	WorldID   string    `json:"worldId"`
	Deleted   int64     `json:"deleted"`
	ClearedAt time.Time `json:"clearedAt"`
}

type FieldError struct{ Fields map[string]string }

func (e *FieldError) Error() string { return ErrInvalidRule.Error() }
func (e *FieldError) Unwrap() error { return ErrInvalidRule }
