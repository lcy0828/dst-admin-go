package performance

import (
	"errors"
	"time"
)

var ErrInvalid = errors.New("invalid performance settings or range")
var ErrConflict = errors.New("performance settings changed")

const ScoreVersion = "simulation-speed-v2"

type Settings struct {
	Revision        string `json:"revision"`
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int    `json:"intervalSeconds"`
	RetentionDays   int    `json:"retentionDays"`
	AggregateDays   int    `json:"aggregateDays"`
}

func Defaults() Settings {
	return Settings{Revision: "default", Enabled: true, IntervalSeconds: 30, RetentionDays: 7, AggregateDays: 90}
}
func (v Settings) valid() bool {
	return v.IntervalSeconds >= 15 && v.IntervalSeconds <= 300 && v.RetentionDays >= 1 && v.RetentionDays <= 30 && v.AggregateDays >= v.RetentionDays && v.AggregateDays <= 180
}

type Input struct {
	RoomID, WorldID, WorldName, TargetID, InstallationID, InstanceID, State string
	At                                                                      time.Time
	SimulationSeconds, CaptureSeconds, TimeScale                            *float64
	ObservedAt                                                              time.Time
	Paused                                                                  bool
	NativePerformance                                                       *int
	CPUSeconds                                                              *float64
	CPUSampledAt                                                            time.Time
	ProcessID                                                               int32
	ProcessStartedAt                                                        time.Time
	RSSBytes                                                                uint64
}

type Point struct {
	RoomID            string    `json:"roomId"`
	WorldID           string    `json:"worldId"`
	WorldName         string    `json:"worldName"`
	TargetID          string    `json:"targetId"`
	InstallationID    string    `json:"installationId"`
	InstanceID        string    `json:"instanceId"`
	At                time.Time `json:"at"`
	State             string    `json:"state"`
	Reason            string    `json:"reason,omitempty"`
	Score             *float64  `json:"score"`
	MinimumScore      *float64  `json:"minimumScore"`
	SpeedRatio        *float64  `json:"speedRatio,omitempty"`
	CPUPercent        *float64  `json:"cpuPercent"`
	RSSBytes          uint64    `json:"rssBytes"`
	NativePerformance *int      `json:"nativePerformance,omitempty"`
	WindowSeconds     float64   `json:"windowSeconds"`
	ValidSeconds      float64   `json:"validSeconds"`
	LowSeconds        float64   `json:"lowSeconds"`
	Samples           int       `json:"samples"`
	ScoreVersion      string    `json:"scoreVersion"`
}

type Finding struct {
	Code     string  `json:"code"`
	Severity string  `json:"severity"`
	Value    float64 `json:"value,omitempty"`
}
type Summary struct {
	WorldID         string    `json:"worldId"`
	WorldName       string    `json:"worldName"`
	ScoreVersion    string    `json:"scoreVersion"`
	AverageScore    *float64  `json:"averageScore"`
	MinimumScore    *float64  `json:"minimumScore"`
	ValidSeconds    float64   `json:"validSeconds"`
	LowSeconds      float64   `json:"lowSeconds"`
	ObservedSeconds float64   `json:"observedSeconds"`
	Coverage        float64   `json:"coverage"`
	PeakCPUPercent  *float64  `json:"peakCPUPercent"`
	PeakRSSBytes    uint64    `json:"peakRSSBytes"`
	Findings        []Finding `json:"findings"`
}
type RuntimeEvent struct {
	ID         uint64    `json:"id"`
	WorldID    string    `json:"worldId"`
	WorldName  string    `json:"worldName"`
	At         time.Time `json:"at"`
	Type       string    `json:"type"`
	Source     string    `json:"source"`
	ReasonCode string    `json:"reasonCode,omitempty"`
	JobID      string    `json:"jobId,omitempty"`
}
type Report struct {
	Events            []RuntimeEvent `json:"events"`
	EventsTruncated   bool           `json:"eventsTruncated"`
	EventsWarning     string         `json:"eventsWarning,omitempty"`
	From              time.Time      `json:"from"`
	To                time.Time      `json:"to"`
	ResolutionSeconds int64          `json:"resolutionSeconds"`
	Items             []Point        `json:"items"`
	Worlds            []Summary      `json:"worlds"`
	Truncated         bool           `json:"truncated"`
	ScoreVersion      string         `json:"scoreVersion"`
}
type Query struct {
	WorldID   string
	From, To  time.Time
	MaxPoints int
}
