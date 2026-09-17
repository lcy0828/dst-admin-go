package runtimeobservation

import "time"

type State string

const (
	StateFresh       State = "fresh"
	StateRefreshing  State = "refreshing"
	StateStale       State = "stale"
	StateOffline     State = "offline"
	StateUnavailable State = "unavailable"
	StateError       State = "error"
)

type Scope struct {
	TargetID string `json:"targetId,omitempty"`
	RoomID   string `json:"roomId,omitempty"`
}

type Observation struct {
	TargetID         string     `json:"targetId"`
	TargetName       string     `json:"targetName"`
	InstallationID   string     `json:"installationId"`
	State            State      `json:"state"`
	ObservedAt       *time.Time `json:"observedAt,omitempty"`
	ReceivedAt       *time.Time `json:"receivedAt,omitempty"`
	RefreshStartedAt *time.Time `json:"refreshStartedAt,omitempty"`
	Error            string     `json:"error,omitempty"`
}

type RefreshResult struct {
	TargetID       string     `json:"targetId"`
	TargetName     string     `json:"targetName"`
	InstallationID string     `json:"installationId"`
	State          State      `json:"state"`
	ObservedAt     *time.Time `json:"observedAt,omitempty"`
	Error          string     `json:"error,omitempty"`
}

type Event struct {
	Sequence    uint64      `json:"sequence"`
	Type        string      `json:"type"`
	OccurredAt  time.Time   `json:"occurredAt"`
	Observation Observation `json:"observation"`
}
