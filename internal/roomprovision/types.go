package roomprovision

import (
	"errors"
	"time"
)

var (
	ErrNotFound        = errors.New("room provision operation not found")
	ErrInvalidInput    = errors.New("room provision input is invalid")
	ErrTargetExists    = errors.New("provision target already contains the managed shard")
	ErrTargetNotReady  = errors.New("provision target is unavailable")
	ErrMigrationNeeded = errors.New("an existing remote shard must be moved with placement migration")
	ErrTopologyChanged = errors.New("room topology changed during provisioning")
	ErrRecoveryNeeded  = errors.New("room provisioning requires recovery")
)

type Status string

const (
	StatusRunning          Status = "running"
	StatusSucceeded        Status = "succeeded"
	StatusRolledBack       Status = "rolled_back"
	StatusRecoveryRequired Status = "recovery_required"
	StatusFailed           Status = "failed"
)

type Operation struct {
	ID               string    `json:"id"`
	RoomID           string    `json:"roomId"`
	RoomName         string    `json:"roomName"`
	TopologyRevision string    `json:"topologyRevision"`
	AppliedRevision  string    `json:"appliedRevision,omitempty"`
	LeaseID          string    `json:"leaseId,omitempty"`
	FencingToken     uint64    `json:"fencingToken"`
	Phase            string    `json:"phase"`
	Status           Status    `json:"status"`
	Failure          string    `json:"failure,omitempty"`
	SourceJobID      string    `json:"sourceJobId,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	Steps            []Step    `json:"steps"`
}

type Step struct {
	ID             string    `json:"id"`
	OperationID    string    `json:"operationId"`
	WorldID        string    `json:"worldId"`
	WorldName      string    `json:"worldName"`
	TargetID       string    `json:"targetId"`
	InstallationID string    `json:"installationId"`
	Cluster        string    `json:"cluster"`
	Shard          string    `json:"shard"`
	MigrationID    string    `json:"migrationId,omitempty"`
	Phase          string    `json:"phase"`
	Size           int64     `json:"size"`
	SHA256         string    `json:"sha256,omitempty"`
	Failure        string    `json:"failure,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type Request struct {
	ExpectedRevision string `json:"expectedRevision"`
	Confirmation     string `json:"confirmation"`
}
