package jobs

import (
	"context"
	"dont/shared"
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

type TransferProgress struct {
	CurrentBytes   int64 `json:"currentBytes"`
	TotalBytes     int64 `json:"totalBytes"`
	BytesPerSecond int64 `json:"bytesPerSecond,omitempty"`
}

type ProgressUpdate struct {
	Progress       int
	Message        string
	Detail         *ProgressDetail
	CurrentBytes   int64
	TotalBytes     int64
	BytesPerSecond int64
}

// ProgressDetail describes the current item, independently of workflow progress.
type ProgressDetail struct {
	Stage          string                          `json:"stage,omitempty"`
	WorkshopID     string                          `json:"workshopId,omitempty"`
	CurrentItem    int                             `json:"currentItem,omitempty"`
	TotalItems     int                             `json:"totalItems,omitempty"`
	TargetID       string                          `json:"targetId,omitempty"`
	InstallationID string                          `json:"installationId,omitempty"`
	Items          []shared.ModDownloadProgress    `json:"items,omitempty"`
	Worlds         []shared.WorldOperationProgress `json:"worlds,omitempty"`
}

type Target struct {
	ID         int64      `json:"id"`
	TargetID   string     `json:"targetId"`
	Name       string     `json:"name"`
	Status     Status     `json:"status"`
	Message    string     `json:"message,omitempty"`
	Warning    *Error     `json:"warning,omitempty"`
	Error      *Error     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type Job struct {
	ID              string            `json:"id"`
	Kind            string            `json:"kind"`
	Status          Status            `json:"status"`
	Outcome         Outcome           `json:"outcome"`
	RoomID          string            `json:"roomId,omitempty"`
	WorldID         string            `json:"worldId,omitempty"`
	Progress        int               `json:"progress"`
	Message         string            `json:"message,omitempty"`
	Transfer        *TransferProgress `json:"transfer,omitempty"`
	ProgressDetail  *ProgressDetail   `json:"progressDetail,omitempty"`
	Error           *Error            `json:"error,omitempty"`
	CancelRequested bool              `json:"cancelRequested"`
	CreatedAt       time.Time         `json:"createdAt"`
	StartedAt       *time.Time        `json:"startedAt,omitempty"`
	FinishedAt      *time.Time        `json:"finishedAt,omitempty"`
	Targets         []Target          `json:"targets"`
}

type TargetSpec struct {
	ID   string
	Name string
}

type TargetResult struct {
	TargetID string
	Status   Status
	Message  string
	Warning  *Error
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
