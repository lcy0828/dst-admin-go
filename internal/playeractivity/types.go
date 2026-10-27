package playeractivity

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

var ErrInvalid = errors.New("invalid player activity input")
var ErrConflict = errors.New("player activity settings changed")
var ErrNotFound = errors.New("managed activity room not found")

type Settings struct {
	Revision                string `json:"revision"`
	Enabled                 bool   `json:"enabled"`
	Positions               bool   `json:"positions"`
	Actions                 bool   `json:"actions"`
	Combat                  bool   `json:"combat"`
	PositionIntervalSeconds int    `json:"positionIntervalSeconds"`
	MinimumDistance         int    `json:"minimumDistance"`
	CombatIntervalSeconds   int    `json:"combatIntervalSeconds"`
	FlushIntervalSeconds    int    `json:"flushIntervalSeconds"`
	SyncIntervalSeconds     int    `json:"syncIntervalSeconds"`
	RetentionDays           int    `json:"retentionDays"`
	MaximumMiB              int    `json:"maximumMiB"`
}

func Defaults() Settings {
	return Settings{Revision: "default", Enabled: true, Positions: true, Actions: true, Combat: true, PositionIntervalSeconds: 15, MinimumDistance: 10, CombatIntervalSeconds: 10, FlushIntervalSeconds: 5, SyncIntervalSeconds: 10, RetentionDays: 7, MaximumMiB: 512}
}

type SettingsPatch struct {
	Revision                string `json:"revision"`
	Enabled                 *bool  `json:"enabled,omitempty"`
	Positions               *bool  `json:"positions,omitempty"`
	Actions                 *bool  `json:"actions,omitempty"`
	Combat                  *bool  `json:"combat,omitempty"`
	PositionIntervalSeconds *int   `json:"positionIntervalSeconds,omitempty"`
	MinimumDistance         *int   `json:"minimumDistance,omitempty"`
	CombatIntervalSeconds   *int   `json:"combatIntervalSeconds,omitempty"`
	FlushIntervalSeconds    *int   `json:"flushIntervalSeconds,omitempty"`
	SyncIntervalSeconds     *int   `json:"syncIntervalSeconds,omitempty"`
	RetentionDays           *int   `json:"retentionDays,omitempty"`
	MaximumMiB              *int   `json:"maximumMiB,omitempty"`
}

func (c Settings) valid() bool {
	return c.PositionIntervalSeconds >= 5 && c.PositionIntervalSeconds <= 300 && c.MinimumDistance >= 0 && c.MinimumDistance <= 100 &&
		c.CombatIntervalSeconds >= 5 && c.CombatIntervalSeconds <= 120 && c.FlushIntervalSeconds >= 2 && c.FlushIntervalSeconds <= 30 &&
		c.SyncIntervalSeconds >= 5 && c.SyncIntervalSeconds <= 60 && c.RetentionDays >= 1 && c.RetentionDays <= 90 && c.MaximumMiB >= 16 && c.MaximumMiB <= 2048
}

type Position struct {
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Z    float64 `json:"z"`
	Tile *int    `json:"tile,omitempty"`
}
type Event struct {
	ID                 string         `json:"id"`
	RoomID             string         `json:"roomId"`
	WorldID            string         `json:"worldId"`
	ProducerInstanceID string         `json:"producerInstanceId"`
	SessionID          string         `json:"sessionId"`
	Sequence           int64          `json:"sequence"`
	OccurredAtUnix     int64          `json:"occurredAtUnix"`
	OccurredAt         time.Time      `json:"occurredAt"`
	ReceivedAt         time.Time      `json:"receivedAt"`
	Kind               string         `json:"kind"`
	UserID             string         `json:"userId"`
	Name               string         `json:"name"`
	Prefab             string         `json:"prefab"`
	Position           *Position      `json:"position,omitempty"`
	Fields             map[string]any `json:"fields"`
}
type Slot struct {
	Slot               int    `json:"slot"`
	Generation         int64  `json:"generation"`
	ProducerInstanceID string `json:"producerInstanceId"`
	SessionID          string `json:"sessionId"`
	ShardID            string `json:"shardId"`
	FirstSequence      int64  `json:"firstSequence"`
	LastSequence       int64  `json:"lastSequence"`
}
type Manifest struct {
	SchemaVersion      int      `json:"schemaVersion"`
	ProducerVersion    string   `json:"producerVersion"`
	ProducerInstanceID string   `json:"producerInstanceId"`
	SessionID          string   `json:"sessionId"`
	ShardID            string   `json:"shardId"`
	Generation         int64    `json:"generation"`
	Slots              Slots    `json:"slots"`
	Pending            int      `json:"pending"`
	Dropped            int64    `json:"dropped"`
	Config             Settings `json:"config"`
	PlayerCount        int      `json:"playerCount"`
	UpdatedAtUnix      int64    `json:"updatedAtUnix"`
	LastError          string   `json:"lastError"`
}
type Segment struct {
	SchemaVersion      int     `json:"schemaVersion"`
	ProducerVersion    string  `json:"producerVersion"`
	ProducerInstanceID string  `json:"producerInstanceId"`
	SessionID          string  `json:"sessionId"`
	ShardID            string  `json:"shardId"`
	Generation         int64   `json:"generation"`
	FirstSequence      int64   `json:"firstSequence"`
	LastSequence       int64   `json:"lastSequence"`
	Events             []Event `json:"events"`
}
type Query struct {
	UserID  string    `json:"userId,omitempty"`
	WorldID string    `json:"worldId,omitempty"`
	Kind    string    `json:"kind,omitempty"`
	From    time.Time `json:"from,omitempty"`
	To      time.Time `json:"to,omitempty"`
	Before  string    `json:"before,omitempty"`
	Limit   int       `json:"limit,omitempty"`
}
type Page struct {
	Items      []Event `json:"items"`
	NextCursor string  `json:"nextCursor"`
	HasMore    bool    `json:"hasMore"`
	Limit      int     `json:"limit"`
}
type WorldStatus struct {
	WorldID         string     `json:"worldId"`
	WorldName       string     `json:"worldName"`
	State           string     `json:"state"`
	DesiredRevision string     `json:"desiredRevision"`
	AppliedRevision string     `json:"appliedRevision"`
	SourceUpdatedAt *time.Time `json:"sourceUpdatedAt,omitempty"`
	Pending         bool       `json:"pending"`
	LastCollectedAt *time.Time `json:"lastCollectedAt,omitempty"`
	LastEventAt     *time.Time `json:"lastEventAt,omitempty"`
	Dropped         int64      `json:"dropped"`
	GapCount        int64      `json:"gapCount"`
	Error           string     `json:"error,omitempty"`
}
type Status struct {
	Settings     Settings      `json:"settings"`
	Worlds       []WorldStatus `json:"worlds"`
	StoredEvents int64         `json:"storedEvents"`
	StoredBytes  int64         `json:"storedBytes"`
}
type CombatSummary struct {
	UserID             string    `json:"userId"`
	Direction          string    `json:"direction"`
	WorldID            string    `json:"worldId"`
	ProducerInstanceID string    `json:"producerInstanceId"`
	TargetGUID         string    `json:"targetGuid"`
	TargetPrefab       string    `json:"targetPrefab"`
	Epic               bool      `json:"epic"`
	Hits               int64     `json:"hits"`
	Damage             *float64  `json:"damage,omitempty"`
	UnknownDamageHits  int64     `json:"unknownDamageHits"`
	Kills              int64     `json:"kills"`
	FirstAt            time.Time `json:"firstAt"`
	LastAt             time.Time `json:"lastAt"`
}
type CombatResult struct {
	Items         []CombatSummary `json:"items"`
	Truncated     bool            `json:"truncated"`
	ScannedEvents int             `json:"scannedEvents"`
}

// DST encodes an empty Lua table as {}. Only accept that exact empty object as an empty array.
type Slots []Slot

func (s *Slots) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "{}" {
		*s = Slots{}
		return nil
	}
	type plain Slots
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*s = Slots(v)
	return nil
}
