package automation

import (
	"errors"
	"time"
)

var (
	ErrInvalidInput     = errors.New("automation input is invalid")
	ErrGroupNotFound    = errors.New("automation group not found")
	ErrTaskNotFound     = errors.New("automation task not found")
	ErrGroupNotEmpty    = errors.New("automation group still contains tasks")
	ErrRevisionConflict = errors.New("automation revision conflict")
	ErrTaskRunning      = errors.New("automation task is already running")
	ErrImportDigest     = errors.New("automation import preview has changed")
	ErrImportInvalid    = errors.New("automation import document is invalid")
	ErrUnsafeAction     = errors.New("automation action is not allowed")
)

type FieldError struct {
	Fields map[string]string
}

func (e *FieldError) Error() string { return ErrInvalidInput.Error() }
func (e *FieldError) Unwrap() error { return ErrInvalidInput }

type Action string

const (
	ActionRoomStart            Action = "room.start"
	ActionRoomStop             Action = "room.stop"
	ActionRoomRestart          Action = "room.restart"
	ActionBackupCreate         Action = "backup.create"
	ActionBackupPrune          Action = "backup.prune"
	ActionCommandExecute       Action = "command.execute"
	ActionPlayerRefresh        Action = "player.refresh"
	ActionStructuredLogRefresh Action = "log.structured.refresh"
	ActionWorldStateRefresh    Action = "world.state.refresh"
)

type Group struct {
	ID          string    `json:"id"`
	RoomID      string    `json:"roomId"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	TaskCount   int       `json:"taskCount"`
	Revision    string    `json:"revision"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type GroupInput struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	Enabled          bool   `json:"enabled"`
	ExpectedRevision string `json:"expectedRevision"`
}

type Task struct {
	ID             string                 `json:"id"`
	RoomID         string                 `json:"roomId"`
	GroupID        string                 `json:"groupId"`
	GroupName      string                 `json:"groupName"`
	Name           string                 `json:"name"`
	Description    string                 `json:"description"`
	Enabled        bool                   `json:"enabled"`
	Schedule       string                 `json:"schedule"`
	Timezone       string                 `json:"timezone"`
	Action         Action                 `json:"action"`
	WorldIDs       []string               `json:"worldIds"`
	Parameters     map[string]interface{} `json:"parameters"`
	TimeoutSeconds int                    `json:"timeoutSeconds"`
	NextRunAt      *time.Time             `json:"nextRunAt,omitempty"`
	LastRunAt      *time.Time             `json:"lastRunAt,omitempty"`
	LastStatus     RunStatus              `json:"lastStatus,omitempty"`
	LastJobID      string                 `json:"lastJobId,omitempty"`
	Revision       string                 `json:"revision"`
	CreatedAt      time.Time              `json:"createdAt"`
	UpdatedAt      time.Time              `json:"updatedAt"`
}

type TaskInput struct {
	GroupID          string                 `json:"groupId"`
	Name             string                 `json:"name"`
	Description      string                 `json:"description"`
	Enabled          bool                   `json:"enabled"`
	Schedule         string                 `json:"schedule"`
	Timezone         string                 `json:"timezone"`
	Action           Action                 `json:"action"`
	WorldIDs         []string               `json:"worldIds"`
	Parameters       map[string]interface{} `json:"parameters"`
	TimeoutSeconds   int                    `json:"timeoutSeconds"`
	ExpectedRevision string                 `json:"expectedRevision"`
}

type RunStatus string

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCanceled  RunStatus = "canceled"
	RunSkipped   RunStatus = "skipped"
)

type Trigger string

const (
	TriggerManual   Trigger = "manual"
	TriggerSchedule Trigger = "schedule"
)

type Run struct {
	ID         string     `json:"id"`
	TaskID     string     `json:"taskId"`
	TaskName   string     `json:"taskName"`
	GroupID    string     `json:"groupId"`
	GroupName  string     `json:"groupName"`
	RoomID     string     `json:"roomId"`
	Action     Action     `json:"action"`
	Trigger    Trigger    `json:"trigger"`
	Status     RunStatus  `json:"status"`
	JobID      string     `json:"jobId,omitempty"`
	Output     string     `json:"output,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	DurationMs int64      `json:"durationMs"`
	CreatedAt  time.Time  `json:"createdAt"`
}

type RunFilter struct {
	TaskID  string
	GroupID string
	Status  RunStatus
	Limit   int
	Offset  int
}

type RunList struct {
	Items  []Run `json:"items"`
	Total  int   `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

type DailyStat struct {
	Date      string `json:"date"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
	Skipped   int    `json:"skipped"`
}

type Stats struct {
	Days              int         `json:"days"`
	Total             int         `json:"total"`
	Succeeded         int         `json:"succeeded"`
	Failed            int         `json:"failed"`
	Canceled          int         `json:"canceled"`
	Skipped           int         `json:"skipped"`
	SuccessRate       float64     `json:"successRate"`
	AverageDurationMs int64       `json:"averageDurationMs"`
	Daily             []DailyStat `json:"daily"`
}

type ActionDefinition struct {
	ID          Action   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	NeedsWorld  bool     `json:"needsWorld"`
	Parameters  []string `json:"parameters"`
}

type ExecutionResult struct {
	Message string
}

type Document struct {
	Version int             `json:"version"`
	Groups  []DocumentGroup `json:"groups"`
	Tasks   []DocumentTask  `json:"tasks"`
}

type DocumentGroup struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

type DocumentTask struct {
	GroupKey       string                 `json:"groupKey"`
	Name           string                 `json:"name"`
	Description    string                 `json:"description"`
	Enabled        bool                   `json:"enabled"`
	Schedule       string                 `json:"schedule"`
	Timezone       string                 `json:"timezone"`
	Action         Action                 `json:"action"`
	WorldIDs       []string               `json:"worldIds"`
	Parameters     map[string]interface{} `json:"parameters"`
	TimeoutSeconds int                    `json:"timeoutSeconds"`
}

type ImportIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type ImportPreview struct {
	Digest     string        `json:"digest"`
	Valid      bool          `json:"valid"`
	GroupCount int           `json:"groupCount"`
	TaskCount  int           `json:"taskCount"`
	Issues     []ImportIssue `json:"issues"`
}

type ImportRequest struct {
	Document Document `json:"document"`
	Digest   string   `json:"digest"`
	Replace  bool     `json:"replace"`
}

type ImportResult struct {
	GroupsCreated int `json:"groupsCreated"`
	TasksCreated  int `json:"tasksCreated"`
}
