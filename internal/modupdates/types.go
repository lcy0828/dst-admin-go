package modupdates

import (
	"errors"
	"time"
)

var (
	ErrInvalidInput     = errors.New("mod update policy input is invalid")
	ErrRevisionConflict = errors.New("mod update policy revision changed")
	ErrBusy             = errors.New("mod update operation is already running")
)

type Status string

const (
	StatusIdle              Status = "idle"
	StatusChecking          Status = "checking"
	StatusAvailable         Status = "available"
	StatusPreparing         Status = "preparing"
	StatusPrepared          Status = "prepared"
	StatusWaitingForPlayers Status = "waiting_for_players"
	StatusScheduled         Status = "scheduled"
	StatusActivating        Status = "activating"
	StatusVerifying         Status = "verifying"
	StatusLoaded            Status = "loaded"
	StatusBlocked           Status = "blocked"
	StatusRolledBack        Status = "rolled_back"
)

type Policy struct {
	RoomID               string    `json:"roomId"`
	AutoCheck            bool      `json:"autoCheck"`
	AutoPrepare          bool      `json:"autoPrepare"`
	ApplyWhenEmpty       bool      `json:"applyWhenEmpty"`
	RestartWithPlayers   bool      `json:"restartWithPlayers"`
	GameAnnouncement     bool      `json:"gameAnnouncement"`
	EmptyGraceSeconds    int       `json:"emptyGraceSeconds"`
	CheckIntervalMinutes int       `json:"checkIntervalMinutes"`
	Revision             string    `json:"revision,omitempty"`
	CreatedAt            time.Time `json:"createdAt,omitempty"`
	UpdatedAt            time.Time `json:"updatedAt,omitempty"`
}

type PolicyInput struct {
	AutoCheck            bool   `json:"autoCheck"`
	AutoPrepare          bool   `json:"autoPrepare"`
	ApplyWhenEmpty       bool   `json:"applyWhenEmpty"`
	RestartWithPlayers   bool   `json:"restartWithPlayers"`
	GameAnnouncement     bool   `json:"gameAnnouncement"`
	EmptyGraceSeconds    int    `json:"emptyGraceSeconds"`
	CheckIntervalMinutes int    `json:"checkIntervalMinutes"`
	ExpectedRevision     string `json:"expectedRevision"`
}

type State struct {
	RoomID               string     `json:"roomId"`
	Status               Status     `json:"status"`
	AvailableModIDs      []string   `json:"availableModIds"`
	PreparedPlanHash     string     `json:"preparedPlanHash,omitempty"`
	PublicationID        string     `json:"publicationId,omitempty"`
	OnlinePlayers        int        `json:"onlinePlayers"`
	StaleOnlinePlayers   int        `json:"staleOnlinePlayers"`
	PresenceFresh        bool       `json:"presenceFresh"`
	AnnouncementPlanHash string     `json:"announcementPlanHash,omitempty"`
	ErrorCode            string     `json:"errorCode,omitempty"`
	ErrorMessage         string     `json:"errorMessage,omitempty"`
	LastCheckedAt        *time.Time `json:"lastCheckedAt,omitempty"`
	PreparedAt           *time.Time `json:"preparedAt,omitempty"`
	EmptySince           *time.Time `json:"emptySince,omitempty"`
	NextCheckAt          *time.Time `json:"nextCheckAt,omitempty"`
	NextActionAt         *time.Time `json:"nextActionAt,omitempty"`
	UpdatedAt            time.Time  `json:"updatedAt"`
}

type Overview struct {
	Policy Policy `json:"policy"`
	State  State  `json:"state"`
}

type DueAction string

const (
	DueCheck     DueAction = "check"
	DueCheckOnly DueAction = "check_only"
	DueApply     DueAction = "apply"
	DueApplyNow  DueAction = "apply_now"
)

type DueItem struct {
	RoomID string
	Action DueAction
}

func DefaultPolicy(roomID string) Policy {
	return Policy{
		RoomID: roomID, AutoCheck: false, AutoPrepare: true, ApplyWhenEmpty: true,
		RestartWithPlayers: false, GameAnnouncement: true, EmptyGraceSeconds: 60, CheckIntervalMinutes: 5,
	}
}

func DefaultState(roomID string, now time.Time) State {
	return State{RoomID: roomID, Status: StatusIdle, AvailableModIDs: []string{}, UpdatedAt: now.UTC()}
}
