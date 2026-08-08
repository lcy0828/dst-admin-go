package console

import "time"

type Risk string

const (
	RiskLow      Risk = "low"
	RiskMedium   Risk = "medium"
	RiskHigh     Risk = "high"
	RiskCritical Risk = "critical"
)

type Parameter struct {
	Name        string      `json:"name"`
	Label       string      `json:"label"`
	Type        string      `json:"type"`
	Required    bool        `json:"required"`
	Description string      `json:"description,omitempty"`
	Options     []string    `json:"options,omitempty"`
	Minimum     *int        `json:"minimum,omitempty"`
	Maximum     *int        `json:"maximum,omitempty"`
	Default     interface{} `json:"default,omitempty"`
}

type Definition struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Category    string      `json:"category"`
	Risk        Risk        `json:"risk"`
	Parameters  []Parameter `json:"parameters"`
	Script      string      `json:"script"`
	IsBuiltin   bool        `json:"isBuiltin"`
}

type RunStatus string

const (
	RunSending RunStatus = "sending"
	RunSent    RunStatus = "sent"
	RunFailed  RunStatus = "failed"
)

type Run struct {
	ID           string                 `json:"id"`
	RoomID       string                 `json:"roomId"`
	WorldID      string                 `json:"worldId"`
	Mode         string                 `json:"mode"`
	CommandID    string                 `json:"commandId,omitempty"`
	Name         string                 `json:"name"`
	Risk         Risk                   `json:"risk"`
	Arguments    map[string]interface{} `json:"arguments,omitempty"`
	RawCommand   string                 `json:"rawCommand,omitempty"`
	Status       RunStatus              `json:"status"`
	Message      string                 `json:"message,omitempty"`
	ErrorCode    string                 `json:"errorCode,omitempty"`
	ErrorMessage string                 `json:"errorMessage,omitempty"`
	LogQuery     string                 `json:"logQuery"`
	CreatedAt    time.Time              `json:"createdAt"`
	FinishedAt   *time.Time             `json:"finishedAt,omitempty"`
}

type ExecuteRequest struct {
	CommandID    string                 `json:"commandId"`
	Arguments    map[string]interface{} `json:"arguments"`
	Confirmation string                 `json:"confirmation"`
}

type RawRequest struct {
	Command      string `json:"command"`
	Confirmation string `json:"confirmation"`
}

type ListFilter struct {
	RoomID  string
	WorldID string
	Limit   int
	Offset  int
}
