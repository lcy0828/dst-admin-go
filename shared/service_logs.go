package shared

import "time"

const ServiceLogsCommand = "service.logs.v1"

// Service logs are scoped to a node's trusted application data directory.
// Requests never accept a filesystem path.
type ServiceLogEntry struct {
	ID          string     `json:"id"`
	Time        time.Time  `json:"time"`
	Level       string     `json:"level"`
	Component   string     `json:"component"`
	Version     string     `json:"version,omitempty"`
	BootID      string     `json:"bootId,omitempty"`
	Message     string     `json:"message"`
	Stack       string     `json:"stack,omitempty"`
	RequestID   string     `json:"requestId,omitempty"`
	JobID       string     `json:"jobId,omitempty"`
	OperationID string     `json:"operationId,omitempty"`
	RoomID      string     `json:"roomId,omitempty"`
	WorldID     string     `json:"worldId,omitempty"`
	Code        string     `json:"code,omitempty"`
	Method      string     `json:"method,omitempty"`
	Path        string     `json:"path,omitempty"`
	Status      int        `json:"status,omitempty"`
	DurationMS  int64      `json:"durationMs,omitempty"`
	Repeated    int        `json:"repeated,omitempty"`
	FirstAt     *time.Time `json:"firstAt,omitempty"`
	Truncated   bool       `json:"truncated,omitempty"`
}

type ServiceLogQuery struct {
	From        time.Time `json:"from,omitempty"`
	To          time.Time `json:"to,omitempty"`
	Level       string    `json:"level,omitempty"`
	Component   string    `json:"component,omitempty"`
	Keyword     string    `json:"keyword,omitempty"`
	RequestID   string    `json:"requestId,omitempty"`
	JobID       string    `json:"jobId,omitempty"`
	OperationID string    `json:"operationId,omitempty"`
	Cursor      string    `json:"cursor,omitempty"`
	AfterID     string    `json:"afterId,omitempty"`
	Limit       int       `json:"limit,omitempty"`
}

type ServiceLogSettings struct {
	Enabled       bool       `json:"enabled"`
	RetentionDays int        `json:"retentionDays"`
	MaxMiB        int        `json:"maxMiB"`
	DebugUntil    *time.Time `json:"debugUntil,omitempty"`
	Revision      string     `json:"revision"`
}

type ServiceLogHealth struct {
	Persistence string     `json:"persistence"`
	LastError   string     `json:"lastError,omitempty"`
	Dropped     uint64     `json:"dropped"`
	WriteErrors uint64     `json:"writeErrors"`
	LastWriteAt *time.Time `json:"lastWriteAt,omitempty"`
}

type ServiceLogSource struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	NodeID    string             `json:"nodeId"`
	Online    bool               `json:"online"`
	Supported bool               `json:"supported"`
	Bytes     int64              `json:"bytes"`
	Files     int                `json:"files"`
	OldestAt  *time.Time         `json:"oldestAt,omitempty"`
	NewestAt  *time.Time         `json:"newestAt,omitempty"`
	Health    ServiceLogHealth   `json:"health,omitzero"`
	Settings  ServiceLogSettings `json:"settings,omitzero"`
}

type ServiceLogPage struct {
	Items        []ServiceLogEntry `json:"items"`
	NextCursor   string            `json:"nextCursor,omitempty"`
	HasMore      bool              `json:"hasMore"`
	Limited      bool              `json:"limited"`
	CoverageLost bool              `json:"coverageLost"`
	ScannedBytes int64             `json:"scannedBytes"`
	From         time.Time         `json:"from"`
	To           time.Time         `json:"to"`
	Health       ServiceLogHealth  `json:"health"`
}

type ServiceLogRequest struct {
	Action   string              `json:"action"`
	Query    ServiceLogQuery     `json:"query"`
	EntryID  string              `json:"entryId,omitempty"`
	Radius   int                 `json:"radius,omitempty"`
	Settings *ServiceLogSettings `json:"settings,omitempty"`
}

type ServiceLogResult struct {
	Page      *ServiceLogPage     `json:"page,omitempty"`
	Source    *ServiceLogSource   `json:"source,omitempty"`
	Settings  *ServiceLogSettings `json:"settings,omitempty"`
	ErrorCode string              `json:"errorCode,omitempty"`
}
