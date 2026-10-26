package notifications

import (
	"errors"
	"time"
)

var (
	ErrInvalid  = errors.New("notification input is invalid")
	ErrNotFound = errors.New("notification channel or delivery not found")
	ErrConflict = errors.New("notification channel has changed")
	ErrLimit    = errors.New("notification capacity reached")
	ErrSecrets  = errors.New("notification credentials are unavailable")
)

var EventKinds = []string{"room.result", "backup.result", "game.result", "mod.result", "agent.status", "security.login", "software.available", "agent.software.available", "custom"}

const DefaultTemplate = `{"event_id":"{{id}}","event":"{{event}}","title":"{{title}}","message":"{{message}}","status":"{{status}}","room_id":"{{roomId}}","target_id":"{{targetId}}","job_id":"{{jobId}}","timestamp":"{{timestamp}}"}`

type Event struct {
	Version    string    `json:"version,omitempty"`
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Message    string    `json:"message"`
	Status     string    `json:"status"`
	Severity   string    `json:"severity"`
	RoomID     string    `json:"roomId,omitempty"`
	TargetID   string    `json:"targetId,omitempty"`
	JobID      string    `json:"jobId,omitempty"`
	OccurredAt time.Time `json:"occurredAt"`
}

// Endpoint URLs and headers are write-only, including URL path/query tokens.
type Channel struct {
	ID                string    `json:"id"`
	Revision          string    `json:"revision"`
	Name              string    `json:"name"`
	Type              string    `json:"type"`
	Enabled           bool      `json:"enabled"`
	Events            []string  `json:"events"`
	FailuresOnly      bool      `json:"failuresOnly"`
	Recipients        []string  `json:"recipients"`
	Method            string    `json:"method"`
	Format            string    `json:"format"`
	Template          string    `json:"template"`
	ResponseMode      string    `json:"responseMode"`
	URLConfigured     bool      `json:"urlConfigured"`
	HeadersConfigured bool      `json:"headersConfigured"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

type ChannelInput struct {
	Revision     string            `json:"revision"`
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	Enabled      bool              `json:"enabled"`
	Events       []string          `json:"events"`
	FailuresOnly bool              `json:"failuresOnly"`
	Recipients   []string          `json:"recipients"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers"`
	Method       string            `json:"method"`
	Format       string            `json:"format"`
	Template     string            `json:"template"`
	ResponseMode string            `json:"responseMode"`
}

type credentials struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type Delivery struct {
	ID            string     `json:"id"`
	ChannelID     string     `json:"channelId"`
	ChannelName   string     `json:"channelName"`
	Event         Event      `json:"event"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	ErrorCode     string     `json:"errorCode,omitempty"`
	HTTPStatus    int        `json:"httpStatus,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	NextAttemptAt time.Time  `json:"nextAttemptAt"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
}

type SendResult struct {
	HTTPStatus int
	ErrorCode  string
	Retryable  bool
}
