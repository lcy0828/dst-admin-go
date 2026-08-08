package containers

import (
	"context"
	"errors"
	"time"
)

var (
	ErrUnavailable          = errors.New("docker is unavailable")
	ErrNotFound             = errors.New("container not found")
	ErrInvalidInput         = errors.New("container action input is invalid")
	ErrConfirmationRequired = errors.New("container confirmation is required")
	ErrConflict             = errors.New("container action conflicts with current state")
)

type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
	ActionRemove  Action = "remove"
)

type Container struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Command   string            `json:"command"`
	State     string            `json:"state"`
	Status    string            `json:"status"`
	Ports     string            `json:"ports"`
	Labels    map[string]string `json:"labels"`
	Running   bool              `json:"running"`
	Managed   bool              `json:"managed"`
	CreatedAt string            `json:"createdAt"`
}

type List struct {
	Available  bool        `json:"available"`
	Error      string      `json:"error,omitempty"`
	Items      []Container `json:"items"`
	Total      int         `json:"total"`
	ObservedAt time.Time   `json:"observedAt"`
}

type ActionInput struct {
	Confirmation string `json:"confirmation"`
}

type ActionResult struct {
	Output string `json:"output"`
}

type Transport interface {
	Available() bool
	List(context.Context) ([]Container, error)
	Run(context.Context, string, Action) (ActionResult, error)
}
