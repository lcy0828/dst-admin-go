package gamenotifications

import (
	"errors"
	"time"
)

const (
	MaxMessageRunes        = 500
	DefaultCountdownSecond = 60
	MinCountdownSeconds    = 10
	MaxCountdownSeconds    = 600
)

var (
	ErrNotFound      = errors.New("game notification not found")
	ErrInvalidInput  = errors.New("game notification input is invalid")
	ErrRoomUnmanaged = errors.New("room must be managed before game notifications can be sent")
)

type Source string

const (
	SourceManual      Source = "manual"
	SourceRoomStop    Source = "room_stop"
	SourceRoomRestart Source = "room_restart"
	SourceGameUpdate  Source = "game_update"
	SourceModSync     Source = "mod_sync"
	SourceAutomation  Source = "automation"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusSending   Status = "sending"
	StatusSucceeded Status = "succeeded"
	StatusPartial   Status = "partial"
	StatusFailed    Status = "failed"
	StatusSkipped   Status = "skipped"
	StatusCanceled  Status = "canceled"
)

type DeliveryStatus string

const (
	DeliveryQueued    DeliveryStatus = "queued"
	DeliverySucceeded DeliveryStatus = "succeeded"
	DeliveryFailed    DeliveryStatus = "failed"
	DeliverySkipped   DeliveryStatus = "skipped"
	DeliveryCanceled  DeliveryStatus = "canceled"
)

type FieldError struct {
	Fields map[string]string
}

func (e *FieldError) Error() string { return ErrInvalidInput.Error() }
func (e *FieldError) Unwrap() error { return ErrInvalidInput }

type Delivery struct {
	ID               int64          `json:"id"`
	NotificationID   string         `json:"notificationId"`
	WorldID          string         `json:"worldId"`
	WorldName        string         `json:"worldName"`
	TargetID         string         `json:"targetId,omitempty"`
	AgentID          string         `json:"agentId,omitempty"`
	Status           DeliveryStatus `json:"status"`
	Message          string         `json:"message,omitempty"`
	ErrorCode        string         `json:"errorCode,omitempty"`
	ErrorMessage     string         `json:"errorMessage,omitempty"`
	TopologyRevision string         `json:"topologyRevision,omitempty"`
	SentAt           *time.Time     `json:"sentAt,omitempty"`
	ObservedAt       *time.Time     `json:"observedAt,omitempty"`
}

type Notification struct {
	ID            string     `json:"id"`
	RoomID        string     `json:"roomId"`
	RoomName      string     `json:"roomName"`
	Message       string     `json:"message"`
	Source        Source     `json:"source"`
	Status        Status     `json:"status"`
	JobID         string     `json:"jobId,omitempty"`
	SuccessCount  int        `json:"successCount"`
	FailureCount  int        `json:"failureCount"`
	SkippedCount  int        `json:"skippedCount"`
	CanceledCount int        `json:"canceledCount"`
	CreatedAt     time.Time  `json:"createdAt"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
	Deliveries    []Delivery `json:"deliveries"`
}

type Policy struct {
	RoomID           string    `json:"roomId"`
	Enabled          bool      `json:"enabled"`
	CountdownSeconds int       `json:"countdownSeconds"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type SendInput struct {
	RoomID  string `json:"roomId"`
	Message string `json:"message"`
}

type PolicyInput struct {
	Enabled          bool `json:"enabled"`
	CountdownSeconds int  `json:"countdownSeconds"`
}

type ListFilter struct {
	RoomID string
	Limit  int
	Offset int
}

type List struct {
	Items  []Notification `json:"items"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}
