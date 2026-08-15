package kubernetesruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	dnsLabelPattern      = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	dnsSubdomainPattern  = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$`)
	labelValuePattern    = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
	safeDirectoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	digestImagePattern   = regexp.MustCompile(`^[^[:space:]@]+(?:/[^[:space:]@]+)*@sha256:[a-f0-9]{64}$`)
)

func validateProvider(provider Provider) error {
	var failures []string
	if !validOpaqueID(provider.ID) {
		failures = append(failures, "id is required and must not contain control characters")
	}
	if !validDNSLabel(provider.Namespace) {
		failures = append(failures, "namespace must be a DNS label")
	}
	if !validDNSLabel(provider.RuntimeServiceAccountName) {
		failures = append(failures, "runtime service account must be a DNS label")
	}
	if !validDNSLabel(provider.CredentialSecretPrefix) || len(provider.CredentialSecretPrefix) > 40 {
		failures = append(failures, "credential secret prefix must be a DNS label of at most 40 characters")
	}
	if !digestImagePattern.MatchString(provider.RuntimeImage) {
		failures = append(failures, "runtime image must be pinned by a lowercase sha256 digest")
	}
	if provider.MinimumLeaseRemaining < 10*time.Second {
		failures = append(failures, "minimum lease remaining must be at least 10 seconds")
	}
	if provider.MaximumObservationAge < time.Second || provider.MaximumObservationAge > 5*time.Minute {
		failures = append(failures, "maximum observation age must be between 1 second and 5 minutes")
	}
	if provider.NodePortRange.First < 1 || provider.NodePortRange.Last > 65535 || provider.NodePortRange.First > provider.NodePortRange.Last {
		failures = append(failures, "node port range is invalid")
	}
	resources := provider.Resources
	if resources.MinimumCPURequestMilli <= 0 || resources.MaximumCPURequestMilli < resources.MinimumCPURequestMilli {
		failures = append(failures, "CPU request bounds are invalid")
	}
	if resources.MinimumMemoryMiB <= 0 || resources.MaximumMemoryMiB < resources.MinimumMemoryMiB {
		failures = append(failures, "memory bounds are invalid")
	}
	if len(provider.StorageProfiles) == 0 {
		failures = append(failures, "at least one storage profile is required")
	}
	for id, profile := range provider.StorageProfiles {
		if id != profile.ID || !validProfileID(id) {
			failures = append(failures, "storage profile map key and safe profile id must match")
		}
		if !validDNSSubdomain(profile.StorageClassName) {
			failures = append(failures, fmt.Sprintf("storage profile %q has an invalid storage class", id))
		}
		if profile.AccessMode != AccessReadWriteOnce && profile.AccessMode != AccessReadWriteOncePod {
			failures = append(failures, fmt.Sprintf("storage profile %q must use RWO or RWOP", id))
		}
		if profile.ReclaimPolicy != ReclaimRetain {
			failures = append(failures, fmt.Sprintf("storage profile %q must use Retain", id))
		}
		if profile.BindingMode != BindingImmediate && profile.BindingMode != BindingWaitForFirstConsumer {
			failures = append(failures, fmt.Sprintf("storage profile %q has an invalid binding mode", id))
		}
		if !profile.DynamicProvisioning {
			failures = append(failures, fmt.Sprintf("storage profile %q must support dynamic provisioning", id))
		}
		if profile.MinimumGiB <= 0 || profile.MaximumGiB < profile.MinimumGiB {
			failures = append(failures, fmt.Sprintf("storage profile %q has invalid capacity bounds", id))
		}
	}
	if len(provider.ComputeProfiles) == 0 {
		failures = append(failures, "at least one compute profile is required")
	}
	for id, profile := range provider.ComputeProfiles {
		if id != profile.ID || !validProfileID(id) {
			failures = append(failures, "compute profile map key and safe profile id must match")
		}
		for key, value := range profile.NodeSelector {
			if !validLabelKey(key) || len(value) > 63 || !labelValuePattern.MatchString(value) {
				failures = append(failures, fmt.Sprintf("compute profile %q has an invalid node selector", id))
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidProvider, strings.Join(failures, "; "))
	}
	return nil
}

func preflight(provider Provider, request Request, observation Observation, now time.Time) PreflightReport {
	report := PreflightReport{Release: ReleaseExperimental, Issues: []Issue{}, Warnings: []Issue{}}
	add := func(code, field, message string) {
		report.Issues = append(report.Issues, Issue{Code: code, Field: field, Message: message})
	}
	warn := func(code, field, message string) {
		report.Warnings = append(report.Warnings, Issue{Code: code, Field: field, Message: message})
	}

	validateIdentity(provider, request, observation, now, add)
	validateLease(provider, request, observation, now, add)
	validateOwnership(provider, request, observation, add)
	if request.Action == ActionStop {
		report.Ready = len(report.Issues) == 0
		return report
	}
	validateCPU(provider, request, observation, now, add)
	profile, profileExists := provider.StorageProfiles[request.Storage.ProfileID]
	validateStorage(request, observation, profile, profileExists, add, warn)
	validateNetwork(provider, request, add)
	if request.Action != ActionStop && request.Network.Exposure == ExposureInternal && observation.PublishedService.Exists {
		add("PUBLISHED_SERVICE_STILL_EXISTS", "observation.publishedService", "external exposure must be removed with an ownership-checked cleanup before using the internal profile")
	}

	if request.Action == ActionStart {
		validateStartCapabilities(provider.Capabilities, add)
		validateStartPod(request, observation.Pod, add)
	} else if request.Action == ActionProvision && observation.Pod.Exists {
		add("POD_EXISTS_DURING_PROVISION", "action", "provisioning cannot be used to stop or replace an existing Pod")
	}
	report.Ready = len(report.Issues) == 0
	return report
}

func validateIdentity(provider Provider, request Request, observation Observation, now time.Time, add func(string, string, string)) {
	if request.Action != ActionProvision && request.Action != ActionStart && request.Action != ActionStop {
		add("ACTION_INVALID", "action", "action must be provision, start or stop")
	}
	if request.Ref.ProviderID != provider.ID {
		add("PROVIDER_SCOPE_MISMATCH", "ref.providerId", "request is outside the configured Provider scope")
	}
	if !validOpaqueID(request.Ref.RoomID) {
		add("ROOM_ID_INVALID", "ref.roomId", "room id is required and must not contain control characters")
	}
	if !validOpaqueID(request.Ref.WorldID) {
		add("WORLD_ID_INVALID", "ref.worldId", "world id is required and must not contain control characters")
	}
	if observation.Ref != request.Ref {
		add("OBSERVATION_SCOPE_MISMATCH", "observation.ref", "observation does not belong to the requested Shard")
	}
	if !freshObservation(observation.ObservedAt, observation.Stale, now, provider.MaximumObservationAge) {
		add("OBSERVATION_STALE", "observation.observedAt", "a fresh Kubernetes observation is required")
	}
	if request.Action == ActionStop {
		return
	}
	if request.Role != RoleMaster && request.Role != RoleSecondary {
		add("SHARD_ROLE_INVALID", "role", "role must be master or secondary")
	}
	if !safeDirectoryPattern.MatchString(request.ClusterName) {
		add("CLUSTER_NAME_UNSAFE", "clusterName", "cluster name must be a safe directory name, not a path")
	}
	if !safeDirectoryPattern.MatchString(request.ShardDirectory) {
		add("SHARD_DIRECTORY_UNSAFE", "shardDirectory", "shard directory must be a safe name, not a path")
	}
	if request.Role == RoleMaster && request.MasterWorldID != "" {
		add("MASTER_WORLD_UNEXPECTED", "masterWorldId", "a Master Shard must not reference another Master world")
	}
	if request.Role == RoleSecondary {
		if !validOpaqueID(request.MasterWorldID) {
			add("MASTER_WORLD_REQUIRED", "masterWorldId", "a Secondary Shard requires the Master world id")
		} else if request.MasterWorldID == request.Ref.WorldID {
			add("MASTER_WORLD_CONFLICT", "masterWorldId", "a Secondary Shard cannot reference itself as Master")
		}
	}
	if _, exists := provider.ComputeProfiles[request.ComputeProfileID]; !exists {
		add("COMPUTE_PROFILE_UNKNOWN", "computeProfileId", "request must select a registered compute profile")
	}
}

func validateLease(provider Provider, request Request, observation Observation, now time.Time, add func(string, string, string)) {
	lease := request.Lease
	if !validOpaqueID(lease.OperationID) {
		add("OPERATION_ID_INVALID", "lease.operationId", "operation id is required")
	}
	if !validOpaqueID(lease.LeaseID) {
		add("LEASE_ID_INVALID", "lease.leaseId", "lease id is required")
	}
	if lease.FencingToken == 0 {
		add("FENCING_TOKEN_INVALID", "lease.fencingToken", "fencing token must be positive")
	}
	if !validOpaqueID(lease.TopologyRevision) {
		add("TOPOLOGY_REVISION_INVALID", "lease.topologyRevision", "topology revision is required")
	}
	if lease.ExpiresAt.IsZero() || lease.ExpiresAt.Before(now.Add(provider.MinimumLeaseRemaining)) {
		add("LEASE_EXPIRING", "lease.expiresAt", "lease does not have enough remaining lifetime")
	}

	for field, resource := range map[string]ResourceObservation{
		"observation.statefulSet": observation.StatefulSet,
		"observation.pod":         observation.Pod.ResourceObservation,
	} {
		if !resource.Exists {
			continue
		}
		observedFence, ok := annotationFence(resource.Annotations)
		if !ok {
			add("FENCING_METADATA_INVALID", field, "managed runtime resource is missing a valid fencing token")
			continue
		}
		observedLease := resource.Annotations[AnnotationLeaseID]
		observedOperation := resource.Annotations[AnnotationOperationID]
		if lease.FencingToken < observedFence {
			add("FENCING_TOKEN_STALE", "lease.fencingToken", "fencing token is lower than the observed runtime token")
		}
		if lease.FencingToken == observedFence {
			if lease.LeaseID != observedLease {
				add("FENCING_LEASE_CONFLICT", "lease.leaseId", "equal fencing tokens cannot be reused by another lease")
			} else if lease.OperationID != observedOperation {
				add("FENCING_OPERATION_CONFLICT", "lease.operationId", "equal fencing tokens only permit the same idempotent operation")
			}
		}
	}
}

func validateOwnership(provider Provider, request Request, observation Observation, add func(string, string, string)) {
	for field, resource := range map[string]ResourceObservation{
		"observation.statefulSet":      observation.StatefulSet,
		"observation.pod":              observation.Pod.ResourceObservation,
		"observation.pvc":              observation.PVC.ResourceObservation,
		"observation.service":          observation.Service,
		"observation.publishedService": observation.PublishedService,
		"observation.networkPolicy":    observation.NetworkPolicy,
	} {
		if resource.Exists && !hasOwnership(resource, provider.ID, request.Ref.RoomID, request.Ref.WorldID) {
			add("MANAGED_SCOPE_MISMATCH", field, "existing resource does not have the exact managed Provider/Room/World labels and annotations")
		}
	}
	if observation.Pod.Exists {
		if request.ExpectedPodUID == "" {
			add("POD_UID_REQUIRED", "expectedPodUid", "existing Pod ownership requires its expected UID")
		} else if request.ExpectedPodUID != observation.Pod.UID {
			add("POD_UID_MISMATCH", "expectedPodUid", "expected Pod UID does not match the observed Pod")
		}
	} else if request.ExpectedPodUID != "" {
		add("POD_UID_STALE", "expectedPodUid", "expected Pod UID was supplied but no Pod exists")
	}
	if observation.PVC.Exists {
		if request.ExpectedPVCUID == "" {
			add("PVC_UID_REQUIRED", "expectedPvcUid", "existing PVC ownership requires its expected UID")
		} else if request.ExpectedPVCUID != observation.PVC.UID {
			add("PVC_UID_MISMATCH", "expectedPvcUid", "expected PVC UID does not match the observed PVC")
		}
	} else if request.ExpectedPVCUID != "" {
		add("PVC_UID_STALE", "expectedPvcUid", "expected PVC UID was supplied but no PVC exists")
	}
}

func validateCPU(provider Provider, request Request, observation Observation, now time.Time, add func(string, string, string)) {
	cpu := request.CPU
	bounds := provider.Resources
	if cpu.Policy != CPUShared && cpu.Policy != CPUExclusive {
		add("CPU_POLICY_INVALID", "cpu.policy", "CPU policy must be shared or exclusive")
	}
	if cpu.RequestMilli < bounds.MinimumCPURequestMilli || cpu.RequestMilli > bounds.MaximumCPURequestMilli {
		add("CPU_REQUEST_OUT_OF_RANGE", "cpu.requestMilli", "CPU request is outside Provider bounds")
	}
	if cpu.LimitMilli < cpu.RequestMilli || cpu.LimitMilli > bounds.MaximumCPURequestMilli {
		add("CPU_LIMIT_OUT_OF_RANGE", "cpu.limitMilli", "CPU limit must be at least the request and within Provider bounds")
	}
	if cpu.MemoryRequestMiB < bounds.MinimumMemoryMiB || cpu.MemoryRequestMiB > bounds.MaximumMemoryMiB {
		add("MEMORY_REQUEST_OUT_OF_RANGE", "cpu.memoryRequestMiB", "memory request is outside Provider bounds")
	}
	if cpu.MemoryLimitMiB < cpu.MemoryRequestMiB || cpu.MemoryLimitMiB > bounds.MaximumMemoryMiB {
		add("MEMORY_LIMIT_OUT_OF_RANGE", "cpu.memoryLimitMiB", "memory limit must be at least the request and within Provider bounds")
	}
	capacity := observation.CPU
	if !freshObservation(capacity.ObservedAt, capacity.Stale, now, provider.MaximumObservationAge) {
		add("CPU_OBSERVATION_STALE", "observation.cpu", "fresh CPU topology and capacity are required")
		return
	}
	if capacity.ComputeProfileID != request.ComputeProfileID {
		add("CPU_PROFILE_MISMATCH", "observation.cpu.computeProfileId", "CPU observation does not belong to the selected compute profile")
		return
	}
	if capacity.PhysicalCores <= 0 || capacity.LogicalProcessors <= 0 || capacity.ThreadsPerCore <= 0 ||
		capacity.PhysicalCores*capacity.ThreadsPerCore != capacity.LogicalProcessors {
		add("CPU_TOPOLOGY_INVALID", "observation.cpu", "CPU topology must consistently identify physical cores and SMT siblings")
		return
	}
	if capacity.AllocatedCPURequestMilli < 0 || capacity.AllocatedExclusivePhysicalCores < 0 || capacity.SystemReservedPhysicalCores < 0 {
		add("CPU_CAPACITY_INVALID", "observation.cpu", "allocated and reserved CPU capacity cannot be negative")
		return
	}
	reserved := capacity.SystemReservedPhysicalCores
	if reserved < 1 {
		reserved = 1
	}
	availablePhysical := capacity.PhysicalCores - reserved - capacity.AllocatedExclusivePhysicalCores
	if availablePhysical < 0 {
		availablePhysical = 0
	}
	if cpu.Policy == CPUShared {
		allocatableMilli := int64((capacity.PhysicalCores - reserved) * capacity.ThreadsPerCore * 1000)
		availableMilli := allocatableMilli - capacity.AllocatedCPURequestMilli
		if availableMilli < 0 {
			availableMilli = 0
		}
		if cpu.RequestMilli > availableMilli {
			add("CPU_CAPACITY_INSUFFICIENT", "cpu.requestMilli", "CPU request would consume the physical core reserved for the system")
		}
		return
	}
	capabilities := provider.Capabilities
	if !capabilities.CPUManagerStatic || !capabilities.FullPhysicalCoreOnly || !capabilities.SMTTopologyKnown {
		add("EXCLUSIVE_CPU_UNVERIFIED", "cpu.policy", "exclusive CPU requires CPU Manager static, full physical-core policy and verified SMT topology")
	}
	expectedMilli := int64(capacity.ThreadsPerCore * 1000)
	if cpu.RequestMilli != expectedMilli || cpu.LimitMilli != expectedMilli {
		add("EXCLUSIVE_CPU_NOT_ONE_CORE", "cpu.requestMilli", "exclusive CPU must request and limit exactly one physical core including all SMT siblings")
	}
	if cpu.MemoryRequestMiB != cpu.MemoryLimitMiB {
		add("EXCLUSIVE_CPU_NOT_GUARANTEED", "cpu.memoryLimitMiB", "exclusive CPU requires equal memory request and limit for Guaranteed QoS")
	}
	if availablePhysical < 1 {
		add("EXCLUSIVE_CPU_UNAVAILABLE", "observation.cpu", "no physical core remains after preserving system overhead")
	}
	availableMilli := int64((capacity.PhysicalCores-reserved)*capacity.ThreadsPerCore*1000) - capacity.AllocatedCPURequestMilli
	if availableMilli < expectedMilli {
		add("EXCLUSIVE_CPU_REQUEST_CAPACITY_INSUFFICIENT", "observation.cpu", "scheduler CPU request capacity cannot fit one full physical core")
	}
}

func validateStorage(request Request, observation Observation, profile StorageProfile, exists bool, add func(string, string, string), warn func(string, string, string)) {
	if !exists {
		add("STORAGE_PROFILE_UNKNOWN", "storage.profileId", "request must select a registered storage profile")
		return
	}
	if request.Storage.SizeGiB < profile.MinimumGiB || request.Storage.SizeGiB > profile.MaximumGiB {
		add("STORAGE_SIZE_OUT_OF_RANGE", "storage.sizeGiB", "storage request is outside the profile bounds")
	}
	if !profile.SnapshotCapable {
		warn("VOLUME_SNAPSHOT_UNAVAILABLE", "storage.profileId", "CSI snapshots are unavailable; Room backups still require the coordinated manifest fallback")
	}
	if !observation.PVC.Exists {
		return
	}
	pvc := observation.PVC
	if pvc.Phase != "Bound" {
		add("PVC_NOT_BOUND", "observation.pvc.phase", "existing PVC must be Bound")
	}
	if pvc.StorageClassName != profile.StorageClassName {
		add("PVC_STORAGE_CLASS_MISMATCH", "observation.pvc.storageClassName", "existing PVC does not use the selected storage profile")
	}
	if len(pvc.AccessModes) != 1 || pvc.AccessModes[0] != profile.AccessMode {
		add("PVC_ACCESS_MODE_MISMATCH", "observation.pvc.accessModes", "existing PVC must be independently owned with the profile RWO/RWOP mode")
	}
	if pvc.ReclaimPolicy != ReclaimRetain {
		add("PVC_RECLAIM_POLICY_UNSAFE", "observation.pvc.reclaimPolicy", "existing PVC must retain data when its claim is removed")
	}
	if pvc.CapacityGiB < request.Storage.SizeGiB {
		add("PVC_CAPACITY_INSUFFICIENT", "observation.pvc.capacityGiB", "existing PVC is smaller than the requested capacity")
	}
}

func validateNetwork(provider Provider, request Request, add func(string, string, string)) {
	ports := request.Network.Ports
	if len(ports) != len(requiredPortPurposes) {
		add("UDP_PORT_SET_INVALID", "network.ports", "exactly the four typed DST UDP port purposes are required")
	}
	seen := map[int32]PortPurpose{}
	for _, purpose := range requiredPortPurposes {
		port, exists := ports[purpose]
		if !exists || port < 1 || port > 65535 {
			add("UDP_PORT_INVALID", "network.ports."+string(purpose), "a valid UDP port is required")
			continue
		}
		if previous, duplicate := seen[port]; duplicate {
			add("UDP_PORT_DUPLICATE", "network.ports."+string(purpose), fmt.Sprintf("UDP port is already assigned to %s", previous))
		}
		seen[port] = purpose
	}
	for purpose := range ports {
		if !knownPortPurpose(purpose) {
			add("UDP_PORT_PURPOSE_UNKNOWN", "network.ports", "unknown UDP port purposes are not accepted")
		}
	}
	if request.Role == RoleSecondary && !provider.Capabilities.SecondaryMasterDNSVerified {
		add("MASTER_DNS_UNVERIFIED", "role", "Secondary-to-Master Kubernetes Service DNS must be verified before scheduling")
	}

	switch request.Network.Exposure {
	case ExposureInternal:
		if len(request.Network.NodePorts) != 0 || strings.TrimSpace(request.Network.AdvertiseAddress) != "" {
			add("INTERNAL_EXPOSURE_FIELDS_INVALID", "network", "internal exposure cannot set NodePorts or an advertise address")
		}
	case ExposureNodePort:
		if !provider.Capabilities.UDPNodePortVerified || !provider.Capabilities.PublishedUDPEndpointVerified {
			add("UDP_NODEPORT_UNVERIFIED", "network.exposure", "NodePort requires verified UDP forwarding and published endpoint behavior")
		}
		validateAdvertiseAddress(request.Network.AdvertiseAddress, add)
		validateNodePorts(provider.NodePortRange, request, add)
	case ExposureLoadBalancer:
		if !provider.Capabilities.UDPLoadBalancerVerified || !provider.Capabilities.PublishedUDPEndpointVerified {
			add("UDP_LOAD_BALANCER_UNVERIFIED", "network.exposure", "LoadBalancer requires verified UDP and published endpoint behavior")
		}
		validateAdvertiseAddress(request.Network.AdvertiseAddress, add)
		if len(request.Network.NodePorts) != 0 {
			add("LOAD_BALANCER_NODEPORT_FORBIDDEN", "network.nodePorts", "LoadBalancer NodePorts are disabled by the typed workload")
		}
	default:
		add("EXPOSURE_INVALID", "network.exposure", "exposure must be internal, node_port or load_balancer")
	}
}

func validateNodePorts(portRange PortRange, request Request, add func(string, string, string)) {
	required := publicPortPurposes()
	if len(request.Network.NodePorts) != len(required) {
		add("NODEPORT_SET_INVALID", "network.nodePorts", "NodePort exposure requires one explicit port for every public UDP purpose")
	}
	seen := map[int32]PortPurpose{}
	for _, purpose := range required {
		port, exists := request.Network.NodePorts[purpose]
		if !exists || port < portRange.First || port > portRange.Last {
			add("NODEPORT_OUT_OF_RANGE", "network.nodePorts."+string(purpose), "NodePort is missing or outside the configured range")
			continue
		}
		if previous, duplicate := seen[port]; duplicate {
			add("NODEPORT_DUPLICATE", "network.nodePorts."+string(purpose), fmt.Sprintf("NodePort is already assigned to %s", previous))
		}
		seen[port] = purpose
	}
	for purpose := range request.Network.NodePorts {
		if !containsPurpose(required, purpose) {
			add("NODEPORT_PURPOSE_INVALID", "network.nodePorts", "NodePort is configured for a non-local or unknown port purpose")
		}
	}
}

func validateAdvertiseAddress(value string, add func(string, string, string)) {
	value = strings.TrimSpace(value)
	if value == "" {
		add("ADVERTISE_ADDRESS_REQUIRED", "network.advertiseAddress", "external exposure requires a verified advertise address")
		return
	}
	if net.ParseIP(value) != nil {
		return
	}
	if len(value) > 253 || !validDNSSubdomain(strings.ToLower(value)) {
		add("ADVERTISE_ADDRESS_INVALID", "network.advertiseAddress", "advertise address must be an IP address or DNS name")
	}
}

func validateStartCapabilities(capabilities Capabilities, add func(string, string, string)) {
	for _, requirement := range []struct {
		available bool
		code      string
		message   string
	}{
		{capabilities.LeaseFencingAdmission, "LEASE_ADMISSION_UNAVAILABLE", "lease/fencing admission is required before replicas can become 1"},
		{capabilities.LeaseAwareRuntimeSupervisor, "LEASE_SUPERVISOR_UNAVAILABLE", "lease-aware runtime supervisor is required before replicas can become 1"},
		{capabilities.PodUIDOwnershipGate, "POD_OWNERSHIP_GATE_UNAVAILABLE", "Pod UID ownership admission is required before replicas can become 1"},
		{capabilities.PVCUIDOwnershipGate, "PVC_OWNERSHIP_GATE_UNAVAILABLE", "PVC UID ownership admission is required before replicas can become 1"},
		{capabilities.NetworkPolicyEnforced, "NETWORK_POLICY_UNAVAILABLE", "an enforcing NetworkPolicy implementation is required before replicas can become 1"},
	} {
		if !requirement.available {
			add(requirement.code, "provider.capabilities", requirement.message)
		}
	}
}

func validateStartPod(request Request, pod PodObservation, add func(string, string, string)) {
	if !pod.Exists {
		return
	}
	if pod.DeletionTimestamp != nil {
		add("OLD_POD_TERMINATING", "observation.pod", "old Pod deletion must be confirmed before a start plan")
		return
	}
	if strings.EqualFold(pod.Phase, "Unknown") || strings.TrimSpace(pod.Phase) == "" {
		add("OLD_POD_STATE_UNKNOWN", "observation.pod.phase", "old Pod state must be known and deletion confirmed before a start plan")
		return
	}
	observedFence, validFence := annotationFence(pod.Annotations)
	idempotent := validFence && observedFence == request.Lease.FencingToken &&
		pod.Annotations[AnnotationLeaseID] == request.Lease.LeaseID &&
		pod.Annotations[AnnotationOperationID] == request.Lease.OperationID &&
		pod.UID == request.ExpectedPodUID
	if !idempotent {
		add("OLD_POD_STILL_EXISTS", "observation.pod", "an existing Pod cannot be replaced or adopted by a start plan")
	}
}

func expectedScopeLabels(providerID, roomID, worldID string) map[string]string {
	return map[string]string{
		LabelManagedBy:  ManagedByValue,
		LabelComponent:  "dst-shard",
		LabelProviderID: identityLabel(providerID),
		LabelRoomID:     identityLabel(roomID),
		LabelWorldID:    identityLabel(worldID),
	}
}

func hasScope(labels map[string]string, providerID, roomID, worldID string) bool {
	expected := expectedScopeLabels(providerID, roomID, worldID)
	for key, value := range expected {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func hasOwnership(resource ResourceObservation, providerID, roomID, worldID string) bool {
	return hasScope(resource.Labels, providerID, roomID, worldID) &&
		resource.Annotations[AnnotationProviderID] == providerID &&
		resource.Annotations[AnnotationRoomID] == roomID &&
		resource.Annotations[AnnotationWorldID] == worldID
}

func annotationFence(annotations map[string]string) (uint64, bool) {
	value, err := strconv.ParseUint(annotations[AnnotationFencingToken], 10, 64)
	return value, err == nil && value > 0
}

func identityLabel(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

func validOpaqueID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func freshObservation(observedAt time.Time, stale bool, now time.Time, maximumAge time.Duration) bool {
	if stale || observedAt.IsZero() {
		return false
	}
	age := now.Sub(observedAt)
	return age >= -maximumAge && age <= maximumAge
}

func validProfileID(value string) bool {
	return len(value) <= 63 && dnsLabelPattern.MatchString(value)
}

func validDNSLabel(value string) bool {
	return len(value) <= 63 && dnsLabelPattern.MatchString(value)
}

func validDNSSubdomain(value string) bool {
	return len(value) <= 253 && dnsSubdomainPattern.MatchString(value)
}

func validLabelKey(value string) bool {
	prefix, name, hasPrefix := strings.Cut(value, "/")
	if !hasPrefix {
		name = prefix
		prefix = ""
	}
	if prefix != "" && !validDNSSubdomain(prefix) {
		return false
	}
	return len(name) <= 63 && labelValuePattern.MatchString(name) && name != ""
}

func knownPortPurpose(value PortPurpose) bool {
	return containsPurpose(requiredPortPurposes[:], value)
}

func localPortPurposes(role ShardRole) []PortPurpose {
	values := make([]PortPurpose, 0, len(requiredPortPurposes))
	for _, purpose := range requiredPortPurposes {
		if role == RoleMaster || purpose != PortClusterMaster {
			values = append(values, purpose)
		}
	}
	return values
}

func publicPortPurposes() []PortPurpose {
	return []PortPurpose{PortDSTServer, PortSteamAuth, PortSteamMasterServer}
}

func containsPurpose(values []PortPurpose, purpose PortPurpose) bool {
	for _, value := range values {
		if value == purpose {
			return true
		}
	}
	return false
}
