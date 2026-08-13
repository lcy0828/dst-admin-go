package runtimeaudit

import "time"

type EventType string

const (
	EventStartRequested   EventType = "start_requested"
	EventStopRequested    EventType = "stop_requested"
	EventRestartRequested EventType = "restart_requested"
	EventCleanupRequested EventType = "cleanup_requested"
	EventSessionStarted   EventType = "session_started"
	EventRunning          EventType = "running"
	EventFailed           EventType = "failed"
	EventStopped          EventType = "stopped"
	EventUnexpectedExit   EventType = "unexpected_exit"
)

type Source string

const (
	SourceAPI           Source = "api"
	SourceAutomation    Source = "automation"
	SourceGameUpdate    Source = "game_update"
	SourceSystemMonitor Source = "system_monitor"
	SourceExternal      Source = "external"
)

type Event struct {
	ID               uint64    `json:"id"`
	RoomID           string    `json:"roomId"`
	WorldID          string    `json:"worldId"`
	RoomDirectory    string    `json:"roomDirectory"`
	WorldDirectory   string    `json:"worldDirectory"`
	Type             EventType `json:"type"`
	Action           string    `json:"action,omitempty"`
	Source           Source    `json:"source"`
	PreviousState    string    `json:"previousState,omitempty"`
	RuntimeState     string    `json:"runtimeState,omitempty"`
	ReasonCode       string    `json:"reasonCode,omitempty"`
	Message          string    `json:"message,omitempty"`
	JobID            string    `json:"jobId,omitempty"`
	RequestID        string    `json:"requestId,omitempty"`
	ExpectedExit     bool      `json:"expectedExit"`
	ExpectedObserved bool      `json:"expectedObserved"`
	OccurredAt       time.Time `json:"occurredAt"`
}

type ListFilter struct {
	WorldID string
	Type    EventType
	Limit   int
}

type List struct {
	Items []Event `json:"items"`
	Total int     `json:"total"`
}

type ActionRequest struct {
	RoomID    string
	WorldIDs  []string
	Action    string
	Source    Source
	JobID     string
	RequestID string
}
