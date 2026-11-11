// Package locations stores administrator-selected destinations. It does not
// write game saves, run a collector, or infer live positions from old samples.
package locations

import (
	"dont/internal/landmarks"
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var (
	ErrInvalid     = errors.New("LOCATION_INVALID")
	ErrConflict    = errors.New("LOCATION_CHANGED")
	ErrNotFound    = errors.New("LOCATION_NOT_FOUND")
	ErrLimit       = errors.New("LOCATION_LIMIT")
	ErrBusy        = errors.New("TELEPORT_BUSY")
	ErrUnavailable = errors.New("TELEPORT_UNAVAILABLE")
	ErrDenied      = errors.New("TELEPORT_DENIED")
	ErrOldWorld    = errors.New("LOCATION_OLD_WORLD")
)

type Point struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

func (p Point) Valid() bool {
	return !math.IsNaN(p.X) && !math.IsNaN(p.Z) && !math.IsInf(p.X, 0) && !math.IsInf(p.Z, 0) && math.Abs(p.X) <= 100000 && math.Abs(p.Z) <= 100000
}

type Location struct {
	NameScope         *string    `json:"-" gorm:"unique_index"`
	DeletedAt         *time.Time `json:"-" gorm:"index"`
	ID                string     `json:"id" gorm:"primary_key;type:varchar(36)"`
	RoomID            string     `json:"roomId" gorm:"index;type:varchar(255)"`
	WorldID           string     `json:"worldId" gorm:"index;type:varchar(255)"`
	Name              string     `json:"name"`
	NameKey           string     `json:"-" gorm:"index"`
	X                 float64    `json:"x"`
	Z                 float64    `json:"z"`
	Source            string     `json:"source"`
	SourceRef         string     `json:"sourceRef,omitempty"`
	SourceTime        time.Time  `json:"sourceTime"`
	SessionID         string     `json:"sessionId"`
	BindingRevision   string     `json:"-"`
	Revision          int64      `json:"revision"`
	ExecutionRevision int64      `json:"executionRevision"`
	CreatedBy         string     `json:"-"`
	UpdatedBy         string     `json:"-"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

func (p Location) Point() Point { return Point{p.X, p.Z} }

type SaveInput struct {
	Name     string   `json:"name"`
	WorldID  string   `json:"worldId"`
	PlayerID string   `json:"playerId,omitempty"`
	X        *float64 `json:"x,omitempty"`
	Z        *float64 `json:"z,omitempty"`
	MapID    string   `json:"mapId,omitempty"`
	Revision int64    `json:"revision,omitempty"`
}

type PrepareInput struct {
	PlayerID       string   `json:"playerId"`
	WorldID        string   `json:"worldId,omitempty"`
	LocationID     string   `json:"locationId,omitempty"`
	LocationName   string   `json:"locationName,omitempty"`
	LandmarkRef    string   `json:"landmarkRef,omitempty"`
	TargetPlayerID string   `json:"targetPlayerId,omitempty"`
	X              *float64 `json:"x,omitempty"`
	Z              *float64 `json:"z,omitempty"`
}

type Effect struct {
	State       string `json:"state"`
	OperationID string `json:"operationId"`
	PlayerID    string `json:"playerId"`
	PlayerName  string `json:"playerName,omitempty"`
	SessionID   string `json:"sessionId,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Point       *Point `json:"point,omitempty"`
	Before      *Point `json:"before,omitempty"`
	After       *Point `json:"after,omitempty"`
	ExpiresAt   int64  `json:"expiresAt,omitempty"`
	ObservedAt  int64  `json:"observedAt,omitempty"`
}

type Preview struct {
	Candidates       []landmarks.Point `json:"candidates,omitempty" gorm:"-"`
	NeedsReview      bool              `json:"needsReview"`
	ID               string            `json:"id" gorm:"primary_key;type:varchar(36)"`
	RoomID           string            `json:"roomId" gorm:"index"`
	WorldID          string            `json:"worldId" gorm:"index"`
	Actor            string            `json:"-" gorm:"index"`
	PlayerID         string            `json:"playerId" gorm:"index"`
	PlayerName       string            `json:"playerName"`
	LocationID       string            `json:"locationId,omitempty"`
	LocationName     string            `json:"locationName,omitempty"`
	LocationRevision int64             `json:"-"`
	Identity         string            `json:"-"`
	BindingRevision  string            `json:"-"`
	TargetPlayerID   string            `json:"-"`
	X                float64           `json:"x"`
	Z                float64           `json:"z"`
	State            string            `json:"state" gorm:"index"`
	Reason           string            `json:"reason,omitempty"`
	RunID            string            `json:"runId,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
	ExpiresAt        time.Time         `json:"expiresAt"`
	ServerTime       time.Time         `json:"serverTime" gorm:"-"`
	Effect           *Effect           `json:"effect,omitempty" gorm:"-"`
	EffectJSON       string            `json:"-" gorm:"type:text"`
}

func cleanName(name string) (string, string, error) {
	name = norm.NFC.String(strings.TrimSpace(name))
	if !utf8.ValidString(name) || len(name) > 192 || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 48 {
		return "", "", ErrInvalid
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return "", "", ErrInvalid
		}
	}
	return name, strings.ToLower(norm.NFKC.String(name)), nil
}
