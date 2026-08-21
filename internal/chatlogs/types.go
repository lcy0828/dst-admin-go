package chatlogs

import (
	"errors"
	"time"

	"dont/internal/rooms"
)

var (
	ErrInvalidFilter  = errors.New("chat log filter is invalid")
	ErrRoomNotManaged = errors.New("room must be managed before chat logs can be read")
)

type Kind string

const (
	KindSay          Kind = "say"
	KindWhisper      Kind = "whisper"
	KindAnnouncement Kind = "announcement"
)

type Filter struct {
	Query   string
	WorldID string
	Kind    Kind
	Limit   int
	Offset  int
}

type Source struct {
	WorldID   string          `json:"worldId"`
	WorldName string          `json:"worldName"`
	WorldRole rooms.WorldRole `json:"worldRole"`
}

type Problem struct {
	WorldID   string `json:"worldId"`
	WorldName string `json:"worldName"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type Entry struct {
	ID               string     `json:"id"`
	Kind             Kind       `json:"kind"`
	AnnouncementType string     `json:"announcementType,omitempty"`
	PlayerID         string     `json:"playerId,omitempty"`
	PlayerName       string     `json:"playerName,omitempty"`
	Content          string     `json:"content"`
	SourceTimestamp  string     `json:"sourceTimestamp"`
	OccurredAt       *time.Time `json:"occurredAt,omitempty"`
	Sources          []Source   `json:"sources"`
}

type List struct {
	Items             []Entry      `json:"items"`
	Total             int          `json:"total"`
	Counts            map[Kind]int `json:"counts"`
	Limit             int          `json:"limit"`
	Offset            int          `json:"offset"`
	Partial           bool         `json:"partial"`
	Truncated         bool         `json:"truncated"`
	AvailableWorlds   int          `json:"availableWorlds"`
	UnavailableWorlds int          `json:"unavailableWorlds"`
	StartedAt         *time.Time   `json:"startedAt,omitempty"`
	UpdatedAt         *time.Time   `json:"updatedAt,omitempty"`
	Problems          []Problem    `json:"problems"`
}
