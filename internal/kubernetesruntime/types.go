// Package kubernetesruntime defines the experimental, typed Kubernetes runtime
// boundary. It deliberately has no kubectl or arbitrary manifest escape hatch.
package kubernetesruntime

import (
	"context"
	"errors"
	"time"
)

const (
	ReleaseExperimental ReleaseStatus = "experimental"

	ManagedByValue = "dst-admin"

	LabelManagedBy  = "app.kubernetes.io/managed-by"
	LabelComponent  = "app.kubernetes.io/component"
	LabelProviderID = "runtime.dst-admin.io/provider-id"
	LabelRoomID     = "runtime.dst-admin.io/room-id"
	LabelWorldID    = "runtime.dst-admin.io/world-id"

	AnnotationProviderID      = "runtime.dst-admin.io/provider"
	AnnotationRoomID          = "runtime.dst-admin.io/room"
	AnnotationWorldID         = "runtime.dst-admin.io/world"
	AnnotationOperationID     = "runtime.dst-admin.io/operation-id"
	AnnotationLeaseID         = "runtime.dst-admin.io/lease-id"
	AnnotationLeaseExpiresAt  = "runtime.dst-admin.io/lease-expires-at"
	AnnotationFencingToken    = "runtime.dst-admin.io/fencing-token"
	AnnotationTopologyVersion = "runtime.dst-admin.io/topology-revision"
	AnnotationStorageRetain   = "runtime.dst-admin.io/storage-retain"
)

var (
	ErrInvalidProvider  = errors.New("kubernetes runtime provider is invalid")
	ErrPreflight        = errors.New("kubernetes runtime preflight failed")
	ErrObservation      = errors.New("kubernetes runtime observation is invalid")
	ErrDisabled         = errors.New("experimental kubernetes runtime provider is disabled")
	ErrUnavailable      = errors.New("experimental kubernetes runtime provider is unavailable")
	ErrProviderMissing  = errors.New("kubernetes runtime provider not found")
	ErrMutationDisabled = errors.New("experimental kubernetes runtime mutations are disabled")
)

type ReleaseStatus string

type Action string

const (
	ActionProvision Action = "provision"
	ActionStart     Action = "start"
	ActionStop      Action = "stop"
)

type ShardRole string

const (
	RoleMaster    ShardRole = "master"
	RoleSecondary ShardRole = "secondary"
)

type CPUPolicy string

const (
	CPUShared    CPUPolicy = "shared"
	CPUExclusive CPUPolicy = "exclusive"
)

type Exposure string

const (
	ExposureInternal     Exposure = "internal"
	ExposureNodePort     Exposure = "node_port"
	ExposureLoadBalancer Exposure = "load_balancer"
)

type PortPurpose string

const (
	PortDSTServer         PortPurpose = "dst_server"
	PortClusterMaster     PortPurpose = "cluster_master"
	PortSteamAuth         PortPurpose = "steam_authentication"
	PortSteamMasterServer PortPurpose = "steam_master_server"
)

var requiredPortPurposes = [...]PortPurpose{
	PortDSTServer,
	PortClusterMaster,
	PortSteamAuth,
	PortSteamMasterServer,
}

type AccessMode string

const (
	AccessReadWriteOnce    AccessMode = "ReadWriteOnce"
	AccessReadWriteOncePod AccessMode = "ReadWriteOncePod"
)

type ReclaimPolicy string

const ReclaimRetain ReclaimPolicy = "Retain"

type VolumeBindingMode string

const (
	BindingImmediate            VolumeBindingMode = "Immediate"
	BindingWaitForFirstConsumer VolumeBindingMode = "WaitForFirstConsumer"
)

// Capabilities are attestations made by the Kubernetes provider after cluster
// admission, runtime image, CNI and node-policy verification. Merely observing
// a Running Pod does not satisfy any of these capabilities.
type Capabilities struct {
	LeaseFencingAdmission        bool `json:"leaseFencingAdmission"`
	LeaseAwareRuntimeSupervisor  bool `json:"leaseAwareRuntimeSupervisor"`
	PodUIDOwnershipGate          bool `json:"podUidOwnershipGate"`
	PVCUIDOwnershipGate          bool `json:"pvcUidOwnershipGate"`
	NetworkPolicyEnforced        bool `json:"networkPolicyEnforced"`
	SecondaryMasterDNSVerified   bool `json:"secondaryMasterDnsVerified"`
	UDPNodePortVerified          bool `json:"udpNodePortVerified"`
	UDPLoadBalancerVerified      bool `json:"udpLoadBalancerVerified"`
	PublishedUDPEndpointVerified bool `json:"publishedUdpEndpointVerified"`
	CPUManagerStatic             bool `json:"cpuManagerStatic"`
	FullPhysicalCoreOnly         bool `json:"fullPhysicalCoreOnly"`
	SMTTopologyKnown             bool `json:"smtTopologyKnown"`
}

type ResourceBounds struct {
	MinimumCPURequestMilli int64 `json:"minimumCpuRequestMilli"`
	MaximumCPURequestMilli int64 `json:"maximumCpuRequestMilli"`
	MinimumMemoryMiB       int64 `json:"minimumMemoryMiB"`
	MaximumMemoryMiB       int64 `json:"maximumMemoryMiB"`
}

type PortRange struct {
	First int32 `json:"first"`
	Last  int32 `json:"last"`
}

// StorageProfile is trusted provider configuration. A request can select its
// ID, but cannot supply a raw StorageClass or access mode.
type StorageProfile struct {
	ID                  string            `json:"id"`
	StorageClassName    string            `json:"storageClassName"`
	AccessMode          AccessMode        `json:"accessMode"`
	ReclaimPolicy       ReclaimPolicy     `json:"reclaimPolicy"`
	BindingMode         VolumeBindingMode `json:"bindingMode"`
	DynamicProvisioning bool              `json:"dynamicProvisioning"`
	MinimumGiB          int64             `json:"minimumGiB"`
	MaximumGiB          int64             `json:"maximumGiB"`
	SnapshotCapable     bool              `json:"snapshotCapable"`
}

// ComputeProfile is trusted provider configuration. NodeSelector is never
// accepted from an operation request.
type ComputeProfile struct {
	ID           string            `json:"id"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

type Provider struct {
	ID                        string                    `json:"id"`
	Namespace                 string                    `json:"namespace"`
	RuntimeServiceAccountName string                    `json:"runtimeServiceAccountName"`
	RuntimeImage              string                    `json:"runtimeImage"`
	CredentialSecretPrefix    string                    `json:"credentialSecretPrefix"`
	MinimumLeaseRemaining     time.Duration             `json:"minimumLeaseRemaining"`
	MaximumObservationAge     time.Duration             `json:"maximumObservationAge"`
	NodePortRange             PortRange                 `json:"nodePortRange"`
	Resources                 ResourceBounds            `json:"resources"`
	Capabilities              Capabilities              `json:"capabilities"`
	StorageProfiles           map[string]StorageProfile `json:"storageProfiles"`
	ComputeProfiles           map[string]ComputeProfile `json:"computeProfiles"`
}

type ShardRef struct {
	ProviderID string `json:"providerId"`
	RoomID     string `json:"roomId"`
	WorldID    string `json:"worldId"`
}

type LeaseProof struct {
	OperationID      string    `json:"operationId"`
	LeaseID          string    `json:"leaseId"`
	FencingToken     uint64    `json:"fencingToken"`
	TopologyRevision string    `json:"topologyRevision"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

type CPURequest struct {
	Policy           CPUPolicy `json:"policy"`
	RequestMilli     int64     `json:"requestMilli"`
	LimitMilli       int64     `json:"limitMilli"`
	MemoryRequestMiB int64     `json:"memoryRequestMiB"`
	MemoryLimitMiB   int64     `json:"memoryLimitMiB"`
}

type CPUObservation struct {
	ComputeProfileID                string    `json:"computeProfileId"`
	PhysicalCores                   int       `json:"physicalCores"`
	LogicalProcessors               int       `json:"logicalProcessors"`
	ThreadsPerCore                  int       `json:"threadsPerCore"`
	AllocatedCPURequestMilli        int64     `json:"allocatedCpuRequestMilli"`
	AllocatedExclusivePhysicalCores int       `json:"allocatedExclusivePhysicalCores"`
	SystemReservedPhysicalCores     int       `json:"systemReservedPhysicalCores"`
	ObservedAt                      time.Time `json:"observedAt"`
	Stale                           bool      `json:"stale"`
}

type NetworkRequest struct {
	Exposure         Exposure              `json:"exposure"`
	Ports            map[PortPurpose]int32 `json:"ports"`
	NodePorts        map[PortPurpose]int32 `json:"nodePorts,omitempty"`
	AdvertiseAddress string                `json:"advertiseAddress,omitempty"`
}

type StorageRequest struct {
	ProfileID string `json:"profileId"`
	SizeGiB   int64  `json:"sizeGiB"`
}

type Request struct {
	Action           Action         `json:"action"`
	Ref              ShardRef       `json:"ref"`
	Role             ShardRole      `json:"role"`
	ClusterName      string         `json:"clusterName"`
	ShardDirectory   string         `json:"shardDirectory"`
	MasterWorldID    string         `json:"masterWorldId,omitempty"`
	ComputeProfileID string         `json:"computeProfileId"`
	Lease            LeaseProof     `json:"lease"`
	CPU              CPURequest     `json:"cpu"`
	Network          NetworkRequest `json:"network"`
	Storage          StorageRequest `json:"storage"`
	ExpectedPodUID   string         `json:"expectedPodUid,omitempty"`
	ExpectedPVCUID   string         `json:"expectedPvcUid,omitempty"`
}

type ResourceObservation struct {
	Exists          bool              `json:"exists"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

type PodObservation struct {
	ResourceObservation
	Phase             string     `json:"phase,omitempty"`
	DeletionTimestamp *time.Time `json:"deletionTimestamp,omitempty"`
}

type PVCObservation struct {
	ResourceObservation
	Phase            string        `json:"phase,omitempty"`
	StorageClassName string        `json:"storageClassName,omitempty"`
	AccessModes      []AccessMode  `json:"accessModes,omitempty"`
	ReclaimPolicy    ReclaimPolicy `json:"reclaimPolicy,omitempty"`
	CapacityGiB      int64         `json:"capacityGiB,omitempty"`
}

type Observation struct {
	Ref              ShardRef            `json:"ref"`
	StatefulSet      ResourceObservation `json:"statefulSet"`
	Pod              PodObservation      `json:"pod"`
	PVC              PVCObservation      `json:"pvc"`
	Service          ResourceObservation `json:"service"`
	PublishedService ResourceObservation `json:"publishedService"`
	NetworkPolicy    ResourceObservation `json:"networkPolicy"`
	CPU              CPUObservation      `json:"cpu"`
	ObservedAt       time.Time           `json:"observedAt"`
	Stale            bool                `json:"stale"`
}

type Issue struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type PreflightReport struct {
	Release  ReleaseStatus `json:"release"`
	Ready    bool          `json:"ready"`
	Issues   []Issue       `json:"issues"`
	Warnings []Issue       `json:"warnings"`
}

type PreflightError struct{ Report PreflightReport }

func (e *PreflightError) Error() string { return ErrPreflight.Error() }
func (e *PreflightError) Unwrap() error { return ErrPreflight }

type Preconditions struct {
	ExpectedPodUID                     string `json:"expectedPodUid,omitempty"`
	ExpectedPodResourceVersion         string `json:"expectedPodResourceVersion,omitempty"`
	ExpectedPVCUID                     string `json:"expectedPvcUid,omitempty"`
	ExpectedPVCResourceVersion         string `json:"expectedPvcResourceVersion,omitempty"`
	ExpectedStatefulSetResourceVersion string `json:"expectedStatefulSetResourceVersion,omitempty"`
}

// TypedMutation contains only objects constructed by this package. Client
// implementations must enforce Preconditions and Lease atomically at apply
// time; they must not accept raw YAML, kubectl arguments or shell commands. A
// stop action must update only the StatefulSet scale subresource.
type TypedMutation struct {
	Release          ReleaseStatus          `json:"release"`
	Action           Action                 `json:"action"`
	Ref              ShardRef               `json:"ref"`
	Namespace        string                 `json:"namespace"`
	Lease            LeaseProof             `json:"lease"`
	Preconditions    Preconditions          `json:"preconditions"`
	StatefulSet      StatefulSet            `json:"statefulSet"`
	PVC              *PersistentVolumeClaim `json:"pvc,omitempty"`
	Service          *Service               `json:"service,omitempty"`
	PublishedService *Service               `json:"publishedService,omitempty"`
	NetworkPolicy    *NetworkPolicy         `json:"networkPolicy,omitempty"`
}

type Client interface {
	Observe(context.Context, ShardRef) (Observation, error)
	Apply(context.Context, TypedMutation) (Observation, error)
}

// Preview is a read-only result. Mutation is present only when every
// preflight gate passes; callers still cannot apply it through the public API.
type Preview struct {
	Observation  Observation     `json:"observation"`
	Preflight    PreflightReport `json:"preflight"`
	Mutation     *TypedMutation  `json:"mutation,omitempty"`
	ApplyAllowed bool            `json:"applyAllowed"`
}
