package jobs

import (
	"context"
	"time"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

type Outcome string

const (
	OutcomePending Outcome = "pending"
	OutcomeFull    Outcome = "full"
	OutcomePartial Outcome = "partial"
	OutcomeNone    Outcome = "none"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Target struct {
	ID         int64      `json:"id"`
	TargetID   string     `json:"targetId"`
	Name       string     `json:"name"`
	Status     Status     `json:"status"`
	Message    string     `json:"message,omitempty"`
	Error      *Error     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type Job struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	Status          Status     `json:"status"`
	Outcome         Outcome    `json:"outcome"`
	RoomID          string     `json:"roomId,omitempty"`
	WorldID         string     `json:"worldId,omitempty"`
	Progress        int        `json:"progress"`
	Message         string     `json:"message,omitempty"`
	Error           *Error     `json:"error,omitempty"`
	CancelRequested bool       `json:"cancelRequested"`
	CreatedAt       time.Time  `json:"createdAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	Targets         []Target   `json:"targets"`
}

type TargetSpec struct {
	ID   string
	Name string
}

type TargetResult struct {
	TargetID string
	Status   Status
	Message  string
	Error    *Error
}

type Runner func(context.Context, func(TargetResult)) error

type Event struct {
	ID        int64     `json:"id"`
	JobID     string    `json:"jobId"`
	Type      string    `json:"type"`
	Data      Job       `json:"data"`
	CreatedAt time.Time `json:"createdAt"`
}

type ListFilter struct {
	Status Status
	Kind   string
	Limit  int
	Offset int
}

type EventWindow struct {
	FirstID int64
	LastID  int64
}

type RetentionPolicy struct {
	EventMaxAge time.Duration
	EventLimit  int
	JobMaxAge   time.Duration
	JobLimit    int
}

type RetentionResult struct {
	EventsDeleted  int64
	JobsDeleted    int64
	TargetsDeleted int64
}
