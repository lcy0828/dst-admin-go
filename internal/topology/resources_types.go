package topology

import (
	"errors"
	"time"

	"dont/shared"
)

var (
	ErrResourceConflict = errors.New("runtime resource preflight found a conflict")
	ErrResourceNotFound = errors.New("runtime infrastructure resource not found")
	ErrCPUNotSupported  = errors.New("requested CPU policy is not supported by the execution environment")
	ErrCPUAllocation    = errors.New("CPU allocation is invalid or conflicts with another Shard")
)

type ProviderKind string

const (
	ProviderLocal ProviderKind = "local"
	ProviderAgent ProviderKind = "agent"
)

type EnvironmentKind string

const (
	EnvironmentNative    EnvironmentKind = "native"
	EnvironmentContainer EnvironmentKind = "container"
)

type NetworkMode string

const (
	NetworkHost   NetworkMode = "host"
	NetworkBridge NetworkMode = "bridge"
)

type PortPurpose string

const (
	PortDSTServer         PortPurpose = "dst_server"
	PortClusterMaster     PortPurpose = "cluster_master"
	PortSteamAuth         PortPurpose = "steam_authentication"
	PortSteamMasterServer PortPurpose = "steam_master_server"
)

type ReservationState string

const (
	ReservationActive    ReservationState = "active"
	ReservationPlanned   ReservationState = "planned"
	ReservationObserved  ReservationState = "observed"
	ReservationReleasing ReservationState = "releasing"
	ReservationReleased  ReservationState = "released"
)

type CPUPolicy string

const (
	CPUPolicyNone      CPUPolicy = "none"
	CPUPolicyShared    CPUPolicy = "shared"
	CPUPolicyExclusive CPUPolicy = "exclusive"
)

type CPUExecutionState string

const (
	CPUExecutionDesired  CPUExecutionState = "desired"
	CPUExecutionPrepared CPUExecutionState = "prepared"
	CPUExecutionApplied  CPUExecutionState = "applied"
	CPUExecutionReleased CPUExecutionState = "released"
	CPUExecutionFailed   CPUExecutionState = "failed"
)

type RuntimeProvider struct {
	ID           string       `json:"id"`
	TargetID     string       `json:"targetId"`
	Kind         ProviderKind `json:"kind"`
	DisplayName  string       `json:"displayName"`
	OS           string       `json:"os"`
	Arch         string       `json:"arch"`
	Online       bool         `json:"online"`
	Capabilities []string     `json:"capabilities"`
	ObservedAt   *time.Time   `json:"observedAt,omitempty"`
	UpdatedAt    time.Time    `json:"updatedAt"`
}

type ExecutionEnvironment struct {
	ID               string              `json:"id"`
	ProviderID       string              `json:"providerId"`
	TargetID         string              `json:"targetId"`
	Kind             EnvironmentKind     `json:"kind"`
	Driver           string              `json:"driver"`
	NetworkProfileID string              `json:"networkProfileId"`
	CPU              shared.CPUInventory `json:"cpu"`
	ObservedAt       *time.Time          `json:"observedAt,omitempty"`
	UpdatedAt        time.Time           `json:"updatedAt"`
}

type NetworkProfile struct {
	ID               string      `json:"id"`
	EnvironmentID    string      `json:"environmentId"`
	Name             string      `json:"name"`
	Mode             NetworkMode `json:"mode"`
	ScopeID          string      `json:"scopeId"`
	BindAddress      string      `json:"bindAddress"`
	AdvertiseAddress string      `json:"advertiseAddress"`
	UpdatedAt        time.Time   `json:"updatedAt"`
}

type PortReservation struct {
	ID               string           `json:"id"`
	EnvironmentID    string           `json:"environmentId"`
	NetworkProfileID string           `json:"networkProfileId"`
	ScopeID          string           `json:"scopeId"`
	TargetID         string           `json:"targetId"`
	RoomID           string           `json:"roomId,omitempty"`
	WorldID          string           `json:"worldId,omitempty"`
	Cluster          string           `json:"cluster"`
	Shard            string           `json:"shard"`
	Purpose          PortPurpose      `json:"purpose"`
	Protocol         string           `json:"protocol"`
	BindAddress      string           `json:"bindAddress"`
	Port             int              `json:"port"`
	State            ReservationState `json:"state"`
	Managed          bool             `json:"managed"`
	LeaseID          string           `json:"leaseId,omitempty"`
	OwnerID          string           `json:"ownerId,omitempty"`
	ExpiresAt        *time.Time       `json:"expiresAt,omitempty"`
	ActivatedAt      *time.Time       `json:"activatedAt,omitempty"`
	ReleasedAt       *time.Time       `json:"releasedAt,omitempty"`
	UpdatedAt        time.Time        `json:"updatedAt"`
}

type PortRequest struct {
	WorldID   string      `json:"worldId"`
	Shard     string      `json:"shard"`
	Purpose   PortPurpose `json:"purpose"`
	Preferred int         `json:"preferred"`
	Strict    bool        `json:"strict"`
}

type PortAllocationRequest struct {
	OwnerID  string        `json:"ownerId"`
	TargetID string        `json:"targetId"`
	RoomID   string        `json:"roomId"`
	Cluster  string        `json:"cluster"`
	TTL      time.Duration `json:"-"`
	Requests []PortRequest `json:"requests"`
}

type PortAllocation struct {
	LeaseID      string            `json:"leaseId"`
	Reservations []PortReservation `json:"reservations"`
	ExpiresAt    time.Time         `json:"expiresAt"`
}

type CPUAllocation struct {
	ID                  string                   `json:"id"`
	EnvironmentID       string                   `json:"environmentId"`
	TargetID            string                   `json:"targetId"`
	RoomID              string                   `json:"roomId"`
	WorldID             string                   `json:"worldId"`
	Policy              CPUPolicy                `json:"policy"`
	LogicalCPUIds       []int                    `json:"logicalCpuIds"`
	PhysicalCoreKeys    []string                 `json:"physicalCoreKeys"`
	AllowSMTSiblingRisk bool                     `json:"allowSmtSiblingRisk"`
	Warnings            []string                 `json:"warnings"`
	ExecutionState      CPUExecutionState        `json:"executionState"`
	Observed            *shared.RuntimeCPUResult `json:"observed,omitempty"`
	ExecutionError      string                   `json:"executionError,omitempty"`
	UpdatedAt           time.Time                `json:"updatedAt"`
}

type ResourceConflict struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	ScopeID  string `json:"scopeId,omitempty"`
	Port     int    `json:"port,omitempty"`
	TargetID string `json:"targetId,omitempty"`
	RoomID   string `json:"roomId,omitempty"`
	WorldID  string `json:"worldId,omitempty"`
}

type ResourcePreflight struct {
	Ready     bool               `json:"ready"`
	Warnings  []string           `json:"warnings"`
	Conflicts []ResourceConflict `json:"conflicts"`
}

type InfrastructureSnapshot struct {
	Providers        []RuntimeProvider      `json:"providers"`
	Environments     []ExecutionEnvironment `json:"environments"`
	NetworkProfiles  []NetworkProfile       `json:"networkProfiles"`
	PortReservations []PortReservation      `json:"portReservations"`
	CPUAllocations   []CPUAllocation        `json:"cpuAllocations"`
	Preflight        ResourcePreflight      `json:"preflight"`
	CapacityPolicy   CapacityPolicy         `json:"capacityPolicy"`
	ObservedAt       time.Time              `json:"observedAt"`
}

type NetworkProfileUpdate struct {
	Name             string `json:"name"`
	BindAddress      string `json:"bindAddress"`
	AdvertiseAddress string `json:"advertiseAddress"`
}

type CPUAllocationUpdate struct {
	RoomID              string    `json:"roomId"`
	WorldID             string    `json:"worldId"`
	EnvironmentID       string    `json:"environmentId"`
	Policy              CPUPolicy `json:"policy"`
	LogicalCPUIds       []int     `json:"logicalCpuIds"`
	AllowSMTSiblingRisk bool      `json:"allowSmtSiblingRisk"`
}

type ResourceConflictError struct {
	Preflight ResourcePreflight
}

func (e *ResourceConflictError) Error() string { return ErrResourceConflict.Error() }
func (e *ResourceConflictError) Unwrap() error { return ErrResourceConflict }

type ResourceFieldError struct {
	Fields map[string]string
}

func (e *ResourceFieldError) Error() string { return ErrCPUAllocation.Error() }
