// Package aicontrol shares bounded, authenticated platform tools between the
// browser assistant and MCP clients. It never runs a separate local Runtime.
package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"dont/internal/apicontract"
)

var (
	ErrInvalid     = errors.New("AI_INVALID")
	ErrDenied      = errors.New("AI_DENIED")
	ErrConflict    = errors.New("AI_CONFLICT")
	ErrBusy        = errors.New("AI_BUSY")
	ErrUnavailable = errors.New("AI_UNAVAILABLE")
	ErrUncertain   = errors.New("AI_RESULT_UNCERTAIN")
)

type Config struct {
	Revision      string      `json:"revision"`
	Enabled       bool        `json:"enabled"`
	Endpoint      string      `json:"endpoint"`
	Model         string      `json:"model"`
	KeyConfigured bool        `json:"keyConfigured"`
	Limits        *ChatLimits `json:"limits"`
}
type ConfigInput struct {
	Config
	APIKey   string `json:"apiKey"`
	ClearKey bool   `json:"clearKey"`
}
type Token struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}
type TokenInput struct {
	Name    string `json:"name"`
	Days    int    `json:"days"`
	AdminID int    `json:"-"`
}
type Principal struct {
	ID      string
	AdminID int
	RoomID  string // Optional actual credential scope; page selection is only context.
}
type Arguments struct {
	Refresh            bool   `json:"refresh,omitempty"`
	RoomID             string `json:"roomId,omitempty"`
	WorldID            string `json:"worldId,omitempty"`
	JobID              string `json:"jobId,omitempty"`
	CallID             string `json:"callId,omitempty"`
	RunID              string `json:"runId,omitempty"`
	RuntimeMode        string `json:"runtimeMode,omitempty"`
	Limit              int    `json:"limit,omitempty"`
	Offset             int    `json:"offset,omitempty"`
	Query              string `json:"query,omitempty"`
	Action             string `json:"action,omitempty"`
	Command            string `json:"command,omitempty"`
	RequestID          string `json:"requestId,omitempty"`
	IncludeStartupPlan bool   `json:"includeStartupPlan,omitempty"`
}
type Tool struct {
	Metadata    *apicontract.ToolMetadata `json:"metadata,omitempty"`
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	InputSchema map[string]any            `json:"inputSchema"`
	Mutation    bool                      `json:"mutation"`
}
type Call struct {
	ID              string    `gorm:"primary_key;type:varchar(64)" json:"id"`
	CallKey         string    `gorm:"unique_index;type:varchar(64)" json:"-"`
	Fingerprint     string    `gorm:"type:varchar(64)" json:"-"`
	Source          string    `gorm:"type:varchar(64);index" json:"source"`
	Tool            string    `gorm:"type:varchar(64)" json:"tool"`
	RoomID          string    `gorm:"type:varchar(255)" json:"roomId,omitempty"`
	Arguments       string    `gorm:"type:text" json:"arguments"`
	Status          string    `gorm:"type:varchar(24);index" json:"status"`
	Result          string    `gorm:"type:text" json:"result,omitempty"`
	ResultAvailable bool      `gorm:"-" json:"resultAvailable,omitempty"`
	CreatedAt       time.Time `gorm:"index" json:"createdAt"`
}
type Invocation struct {
	CallID string          `json:"callId"`
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data,omitempty"`
}
type Backend interface {
	Invoke(context.Context, string, Arguments, string) (json.RawMessage, error)
}

type APIBackend interface {
	InvokeAPI(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error)
}
