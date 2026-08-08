package backups

import "time"

type Kind string

const (
	KindManual     Kind = "manual"
	KindUpload     Kind = "upload"
	KindSnapshot   Kind = "snapshot"
	KindProtection Kind = "protection"
	KindImported   Kind = "imported"
)

type Backup struct {
	ID              string     `json:"id"`
	RoomID          string     `json:"roomId"`
	Name            string     `json:"name"`
	Kind            Kind       `json:"kind"`
	FileName        string     `json:"fileName"`
	SourceName      string     `json:"sourceName,omitempty"`
	Size            int64      `json:"size"`
	ContentSize     int64      `json:"contentSize"`
	FileCount       int        `json:"fileCount"`
	SHA256          string     `json:"sha256,omitempty"`
	Status          string     `json:"status"`
	ValidationError string     `json:"validationError,omitempty"`
	VerifiedAt      *time.Time `json:"verifiedAt,omitempty"`
	SourceJobID     string     `json:"sourceJobId,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

type Policy struct {
	RoomID         string     `json:"roomId"`
	Enabled        bool       `json:"enabled"`
	IntervalMinute int        `json:"intervalMinutes"`
	MaxSnapshots   int        `json:"maxSnapshots"`
	NextRunAt      *time.Time `json:"nextRunAt,omitempty"`
	LastRunAt      *time.Time `json:"lastRunAt,omitempty"`
	LastJobID      string     `json:"lastJobId,omitempty"`
	LastError      string     `json:"lastError,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

type CreateRequest struct {
	Name string `json:"name"`
}

type RenameRequest struct {
	Name string `json:"name"`
}

type RestoreRequest struct {
	Confirmation string `json:"confirmation"`
}

type DeleteRequest struct {
	Confirmation string `json:"confirmation"`
}

type PolicyRequest struct {
	Enabled        bool       `json:"enabled"`
	IntervalMinute int        `json:"intervalMinutes"`
	MaxSnapshots   int        `json:"maxSnapshots"`
	NextRunAt      *time.Time `json:"nextRunAt,omitempty"`
}
