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

// EventFields accepts Klei's representation of an empty Lua table. Nonempty
// arrays are not field dictionaries and must still fail event validation.
type EventFields map[string]any

func (f *EventFields) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		if len(items) != 0 {
			return errors.New("activity fields must be an object or an empty Lua table")
		}
		*f = EventFields{}
		return nil
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*f = value
	return nil
}

type Event struct {
	ID                 string      `json:"id"`
	RoomID             string      `json:"roomId"`
	WorldID            string      `json:"worldId"`
	ProducerInstanceID string      `json:"producerInstanceId"`
	SessionID          string      `json:"sessionId"`
	Sequence           int64       `json:"sequence"`
	OccurredAtUnix     int64       `json:"occurredAtUnix"`
	OccurredAt         time.Time   `json:"occurredAt"`
	ReceivedAt         time.Time   `json:"receivedAt"`
	Kind               string      `json:"kind"`
	UserID             string      `json:"userId"`
	Name               string      `json:"name"`
	Prefab             string      `json:"prefab"`
	Position           *Position   `json:"position,omitempty"`
	ContinuousRangeID  string      `json:"continuousRangeId,omitempty"`
	Fields             EventFields `json:"fields"`
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
	PublicationPhase   string   `json:"publicationPhase,omitempty"`
	WriteUnconfirmed   bool     `json:"writeUnconfirmed,omitempty"`
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
	UserID     string    `json:"userId,omitempty"`
	WorldID    string    `json:"worldId,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	From       time.Time `json:"from,omitempty"`
	To         time.Time `json:"to,omitempty"`
	Before     string    `json:"before,omitempty"`
	Limit      int       `json:"limit,omitempty"`
	Projection string    `json:"projection,omitempty"`
}
type Page struct {
	Items      []Event    `json:"items"`
	NextCursor string     `json:"nextCursor"`
	HasMore    bool       `json:"hasMore"`
	Limit      int        `json:"limit"`
	Coverage   []Coverage `json:"coverage"`
}
type WorldStatus struct {
	WorldID            string       `json:"worldId"`
	WorldName          string       `json:"worldName"`
	State              string       `json:"state"`
	DesiredRevision    string       `json:"desiredRevision"`
	AppliedRevision    string       `json:"appliedRevision"`
	SourceUpdatedAt    *time.Time   `json:"sourceUpdatedAt,omitempty"`
	Pending            bool         `json:"pending"`
	LastCollectedAt    *time.Time   `json:"lastCollectedAt,omitempty"`
	LastEventAt        *time.Time   `json:"lastEventAt,omitempty"`
	Dropped            int64        `json:"dropped"`
	GapCount           int64        `json:"gapCount"`
	Error              string       `json:"error,omitempty"`
	SyncState          string       `json:"syncState"`
	Backlog            int64        `json:"backlog"`
	ScannedGeneration  int64        `json:"scannedGeneration"`
	SourceGeneration   int64        `json:"sourceGeneration"`
	ProducerInstanceID string       `json:"producerInstanceId"`
	SessionID          string       `json:"sessionId"`
	NodePending        int          `json:"nodePending"`
	PublicationPhase   string       `json:"publicationPhase,omitempty"`
	WriteUnconfirmed   bool         `json:"writeUnconfirmed"`
	LastAttemptAt      *time.Time   `json:"lastAttemptAt,omitempty"`
	NextRetryAt        *time.Time   `json:"nextRetryAt,omitempty"`
	ConfigError        string       `json:"configError,omitempty"`
	Coverage           Coverage     `json:"coverage"`
	Diagnostics        []Diagnostic `json:"diagnostics"`
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
	Coverage      []Coverage      `json:"coverage"`
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

// Coverage describes retained history; it never infers missing action times or counts.
type Coverage struct {
	WorldID           string `json:"worldId"`
	PendingSegments   int64  `json:"pendingSegments"`
	IsolatedSegments  int64  `json:"isolatedSegments"`
	UnavailableRanges int64  `json:"unavailableRanges"`
	RecoveredSegments int64  `json:"recoveredSegments"`
	Complete          bool   `json:"complete"`
	LegacyUnknown     bool   `json:"legacyUnknown"`
}
type Diagnostic struct {
	Code        string    `json:"code"`
	Field       string    `json:"field,omitempty"`
	Generation  int64     `json:"generation"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	FirstSeenAt time.Time `json:"firstSeenAt"`
	State       string    `json:"state"`
}
type TrajectoryPoint struct {
	ID                 string    `json:"id"`
	UserID             string    `json:"userId"`
	WorldID            string    `json:"worldId"`
	ProducerInstanceID string    `json:"producerInstanceId"`
	SessionID          string    `json:"sessionId"`
	OccurredAt         time.Time `json:"occurredAt"`
	Position           *Position `json:"position"`
	Source             string    `json:"source"`
	ContinuousRangeID  string    `json:"continuousRangeId,omitempty"`
}
type TrajectoryPage struct {
	Items      []TrajectoryPoint `json:"items"`
	NextCursor string            `json:"nextCursor"`
	HasMore    bool              `json:"hasMore"`
	Limit      int               `json:"limit"`
	Coverage   []Coverage        `json:"coverage"`
}
type SyncRequest struct {
	Accepted bool   `json:"accepted"`
	Status   string `json:"status"`
}
