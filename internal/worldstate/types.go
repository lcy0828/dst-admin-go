package worldstate

import (
	"errors"
	"time"
)

var (
	ErrInvalidFilter   = errors.New("world state filter is invalid")
	ErrInvalidSnapshot = errors.New("world state snapshot is invalid")
	ErrRoomNotManaged  = errors.New("room must be managed before world state can be sampled")
	ErrWorldNotRunning = errors.New("target world is not running")
	ErrProbeTimedOut   = errors.New("world state probe timed out")
)

type Snapshot struct {
	ID                    uint             `json:"id"`
	RoomID                string           `json:"roomId"`
	WorldID               string           `json:"worldId"`
	WorldName             string           `json:"worldName"`
	WorldRole             string           `json:"worldRole"`
	Season                string           `json:"season,omitempty"`
	Phase                 string           `json:"phase,omitempty"`
	Cycles                *int             `json:"cycles,omitempty"`
	ElapsedDaysInSeason   *int             `json:"elapsedDaysInSeason,omitempty"`
	RemainingDaysInSeason *int             `json:"remainingDaysInSeason,omitempty"`
	SeasonProgress        *float64         `json:"seasonProgress,omitempty"`
	DayProgress           *float64         `json:"dayProgress,omitempty"`
	PhaseProgress         *float64         `json:"phaseProgress,omitempty"`
	Precipitation         string           `json:"precipitation,omitempty"`
	MoonPhase             string           `json:"moonPhase,omitempty"`
	Temperature           *float64         `json:"temperature,omitempty"`
	Wetness               *float64         `json:"wetness,omitempty"`
	Moisture              *float64         `json:"moisture,omitempty"`
	MoistureCeil          *float64         `json:"moistureCeil,omitempty"`
	PrecipitationRate     *float64         `json:"precipitationRate,omitempty"`
	NightmarePhase        string           `json:"nightmarePhase,omitempty"`
	NightmareProgress     *float64         `json:"nightmareProgress,omitempty"`
	HostPerformance       *int             `json:"hostPerformance,omitempty"`
	ObservedAt            time.Time        `json:"observedAt"`
	RuntimeState          string           `json:"runtimeState,omitempty" gorm:"-"`
	Paused                *bool            `json:"paused,omitempty" gorm:"-"`
	RuntimeCode           string           `json:"runtimeCode,omitempty" gorm:"-"`
	RuntimeMessage        string           `json:"runtimeMessage,omitempty" gorm:"-"`
	ObservationState      ObservationState `json:"observationState,omitempty" gorm:"-"`
	ObservationCode       string           `json:"observationCode,omitempty" gorm:"-"`
	ObservationError      string           `json:"observationError,omitempty" gorm:"-"`
	Freshness             Freshness        `json:"freshness,omitempty" gorm:"-"`
	AgeSeconds            int64            `json:"ageSeconds,omitempty" gorm:"-"`
	Stale                 bool             `json:"stale" gorm:"-"`
}

type ObservationState string

const (
	ObservationStateDeferred ObservationState = "deferred"
	ObservationStateFailed   ObservationState = "failed"
	ObservationStatePending  ObservationState = "pending"

	ObservationCodeRoomOperationInProgress = "ROOM_OPERATION_IN_PROGRESS"
)

type Freshness string

const (
	FreshnessLive        Freshness = "live"
	FreshnessPaused      Freshness = "paused"
	FreshnessDelayed     Freshness = "delayed"
	FreshnessStopped     Freshness = "stopped"
	FreshnessUnavailable Freshness = "unavailable"
)

type Observation struct {
	Season                string
	Phase                 string
	Cycles                *int
	ElapsedDaysInSeason   *int
	RemainingDaysInSeason *int
	SeasonProgress        *float64
	DayProgress           *float64
	PhaseProgress         *float64
	Precipitation         string
	MoonPhase             string
	Temperature           *float64
	Wetness               *float64
	Moisture              *float64
	MoistureCeil          *float64
	PrecipitationRate     *float64
	NightmarePhase        string
	NightmareProgress     *float64
	HostPerformance       *int
	CapturedAt            time.Time
}

type List struct {
	Items           []Snapshot `json:"items"`
	Total           int        `json:"total"`
	LastRefreshedAt *time.Time `json:"lastRefreshedAt,omitempty"`
}

type History struct {
	Items   []Snapshot `json:"items"`
	Total   int        `json:"total"`
	Limit   int        `json:"limit"`
	WorldID string     `json:"worldId"`
}

type RefreshResult struct {
	WorldID    string    `json:"worldId"`
	WorldName  string    `json:"worldName"`
	ObservedAt time.Time `json:"observedAt"`
	Message    string    `json:"message"`
	Snapshot   Snapshot  `json:"snapshot"`
}
