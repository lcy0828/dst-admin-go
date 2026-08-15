package kubernetesruntime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type memoryClient struct {
	observation Observation
	observeErr  error
	applyErr    error
	applied     *TypedMutation
	applyResult *Observation
}

func (client *memoryClient) Observe(context.Context, ShardRef) (Observation, error) {
	return client.observation, client.observeErr
}

func (client *memoryClient) Apply(_ context.Context, mutation TypedMutation) (Observation, error) {
	client.applied = &mutation
	if client.applyErr != nil {
		return Observation{}, client.applyErr
	}
	if client.applyResult != nil {
		return *client.applyResult, nil
	}
	return client.observation, nil
}

func testProvider() Provider {
	return Provider{
		ID: "k8s-home", Namespace: "dst-admin-runtime", RuntimeServiceAccountName: "dst-admin-runtime",
		RuntimeImage:           "registry.example.com/dst/runtime@sha256:" + strings.Repeat("a", 64),
		CredentialSecretPrefix: "dst-room", MinimumLeaseRemaining: 30 * time.Second,
		MaximumObservationAge: 30 * time.Second,
		NodePortRange:         PortRange{First: 30000, Last: 32767},
		Resources: ResourceBounds{
			MinimumCPURequestMilli: 250, MaximumCPURequestMilli: 8000,
			MinimumMemoryMiB: 512, MaximumMemoryMiB: 16384,
		},
		Capabilities: Capabilities{
			LeaseFencingAdmission: true, LeaseAwareRuntimeSupervisor: true,
			PodUIDOwnershipGate: true, PVCUIDOwnershipGate: true, NetworkPolicyEnforced: true,
			SecondaryMasterDNSVerified: true, UDPNodePortVerified: true,
			UDPLoadBalancerVerified: true, PublishedUDPEndpointVerified: true,
			CPUManagerStatic: true, FullPhysicalCoreOnly: true, SMTTopologyKnown: true,
		},
		StorageProfiles: map[string]StorageProfile{
			"retained": {
				ID: "retained", StorageClassName: "local-retain", AccessMode: AccessReadWriteOncePod,
				ReclaimPolicy: ReclaimRetain, BindingMode: BindingWaitForFirstConsumer,
				DynamicProvisioning: true, MinimumGiB: 10, MaximumGiB: 200,
			},
		},
		ComputeProfiles: map[string]ComputeProfile{
			"general": {ID: "general", NodeSelector: map[string]string{"dst-admin.io/runtime": "enabled"}},
		},
	}
}

func testRequest(now time.Time) Request {
	return Request{
		Action: ActionProvision,
		Ref:    ShardRef{ProviderID: "k8s-home", RoomID: "room-1", WorldID: "master-world"},
		Role:   RoleMaster, ClusterName: "Cluster_1", ShardDirectory: "Master", ComputeProfileID: "general",
		Lease: LeaseProof{
			OperationID: "operation-1", LeaseID: "lease-1", FencingToken: 7,
			TopologyRevision: "revision-1", ExpiresAt: now.Add(5 * time.Minute),
		},
		CPU: CPURequest{
			Policy: CPUShared, RequestMilli: 1000, LimitMilli: 2000,
			MemoryRequestMiB: 2048, MemoryLimitMiB: 4096,
		},
		Network: NetworkRequest{
			Exposure: ExposureInternal,
			Ports: map[PortPurpose]int32{
				PortDSTServer: 10999, PortClusterMaster: 10889,
				PortSteamAuth: 8768, PortSteamMasterServer: 27018,
			},
		},
		Storage: StorageRequest{ProfileID: "retained", SizeGiB: 20},
	}
}

func testObservation(request Request, now time.Time) Observation {
	return Observation{
		Ref: request.Ref, ObservedAt: now,
		CPU: CPUObservation{
			ComputeProfileID: "general", PhysicalCores: 8, LogicalProcessors: 16, ThreadsPerCore: 2,
			AllocatedCPURequestMilli: 2000, AllocatedExclusivePhysicalCores: 1,
			SystemReservedPhysicalCores: 1, ObservedAt: now,
		},
	}
}

func testDriver(t *testing.T, provider Provider, client Client, now time.Time) *Driver {
	t.Helper()
	driver, err := NewDriver(provider, client)
	if err != nil {
		t.Fatal(err)
	}
	driver.now = func() time.Time { return now }
	return driver
}

func TestNewDriverRejectsFloatingImageAndUnsafeStorage(t *testing.T) {
	provider := testProvider()
	provider.RuntimeImage = "registry.example.com/dst/runtime:latest"
	provider.StorageProfiles["retained"] = StorageProfile{
		ID: "retained", StorageClassName: "shared", AccessMode: "ReadWriteMany",
		ReclaimPolicy: "Delete", BindingMode: BindingImmediate, DynamicProvisioning: true,
		MinimumGiB: 1, MaximumGiB: 20,
	}
	if _, err := NewDriver(provider, &memoryClient{}); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("NewDriver error = %v", err)
	}
}

func TestRequestHasNoArbitraryRuntimeEscapeHatch(t *testing.T) {
	forbidden := map[string]bool{
		"image": true, "command": true, "args": true, "path": true, "mount": true,
		"manifest": true, "yaml": true, "hostport": true, "nodeselector": true,
		"storageclass": true, "serviceaccount": true,
	}
	typeOfRequest := reflect.TypeOf(Request{})
	for index := 0; index < typeOfRequest.NumField(); index++ {
		name := strings.ToLower(typeOfRequest.Field(index).Name)
		if forbidden[name] {
			t.Fatalf("Request exposes forbidden runtime field %q", typeOfRequest.Field(index).Name)
		}
	}
}

func TestProviderConfigurationIsDefensivelyCopied(t *testing.T) {
	provider := testProvider()
	driver, err := NewDriver(provider, &memoryClient{})
	if err != nil {
		t.Fatal(err)
	}
	provider.ComputeProfiles["general"].NodeSelector["dst-admin.io/runtime"] = "changed"
	copy := driver.Provider()
	copy.ComputeProfiles["general"].NodeSelector["dst-admin.io/runtime"] = "also-changed"
	if got := driver.Provider().ComputeProfiles["general"].NodeSelector["dst-admin.io/runtime"]; got != "enabled" {
		t.Fatalf("Provider node selector was mutated: %q", got)
	}
}

func TestProvisionBuildsStoppedTypedWorkload(t *testing.T) {
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	provider, request := testProvider(), testRequest(now)
	client := &memoryClient{observation: testObservation(request, now)}
	driver := testDriver(t, provider, client, now)
	mutation, err := driver.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if mutation.Release != ReleaseExperimental || mutation.StatefulSet.Spec.Replicas != 0 {
		t.Fatalf("release=%q replicas=%d", mutation.Release, mutation.StatefulSet.Spec.Replicas)
	}
	if mutation.Namespace != provider.Namespace || mutation.StatefulSet.Metadata.Namespace != provider.Namespace {
		t.Fatalf("mutation escaped namespace: %#v", mutation)
	}
	if mutation.PVC == nil || mutation.Service == nil || mutation.NetworkPolicy == nil {
		t.Fatalf("provision objects are incomplete: %#v", mutation)
	}
	if mutation.PVC.Spec.AccessModes[0] != string(AccessReadWriteOncePod) || mutation.PVC.Metadata.Annotations[AnnotationStorageRetain] != "true" {
		t.Fatalf("PVC is not independently retained: %#v", mutation.PVC)
	}
	if len(mutation.Service.Spec.Ports) != 4 || len(mutation.NetworkPolicy.Spec.Ingress) != 2 {
		t.Fatalf("master network resources are incomplete: service=%#v policy=%#v", mutation.Service, mutation.NetworkPolicy)
	}
	container := mutation.StatefulSet.Spec.Template.Spec.Containers[0]
	if container.Image != provider.RuntimeImage || len(container.Command) != 1 || container.Command[0] != supervisorPath {
		t.Fatalf("runtime command or image is not fixed: %#v", container)
	}
	security := container.SecurityContext
	if security.AllowPrivilegeEscalation || security.Privileged || !security.ReadOnlyRootFilesystem || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("container security context is unsafe: %#v", security)
	}
	if token := mutation.StatefulSet.Spec.Template.Spec.AutomountServiceAccountToken; token == nil || *token {
		t.Fatalf("service account token must not be mounted: %v", token)
	}
	if len(container.Env) != 2 || container.Env[0].ValueFrom.SecretKeyRef.Key != "cluster-token" || container.Env[1].ValueFrom.SecretKeyRef.Key != "cluster-key" {
		t.Fatalf("runtime credentials must use only fixed Secret keys: %#v", container.Env)
	}
	volumes := mutation.StatefulSet.Spec.Template.Spec.Volumes
	if len(volumes) != 2 || volumes[0].PersistentVolumeClaim == nil || volumes[1].EmptyDir == nil || volumes[1].EmptyDir.Medium != "Memory" {
		t.Fatalf("fixed data/runtime volumes are incomplete: %#v", volumes)
	}
	if got := mutation.StatefulSet.Spec.Template.Spec.NodeSelector["dst-admin.io/runtime"]; got != "enabled" {
		t.Fatalf("trusted compute profile was not applied: %q", got)
	}
	encoded, err := yaml.Marshal(mutation.StatefulSet)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, "apiVersion: apps/v1") || !strings.Contains(text, "kind: StatefulSet") || strings.Contains(text, "kubectl") {
		t.Fatalf("unexpected typed YAML:\n%s", text)
	}
}

func TestStartRequiresAllRuntimeSafetyCapabilities(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Action = ActionStart
	provider.Capabilities = Capabilities{}
	client := &memoryClient{observation: testObservation(request, now)}
	driver := testDriver(t, provider, client, now)
	_, err := driver.Plan(context.Background(), request)
	assertPreflightCodes(t, err,
		"LEASE_ADMISSION_UNAVAILABLE", "LEASE_SUPERVISOR_UNAVAILABLE",
		"POD_OWNERSHIP_GATE_UNAVAILABLE", "PVC_OWNERSHIP_GATE_UNAVAILABLE", "NETWORK_POLICY_UNAVAILABLE",
	)
}

func TestValidStartCanSetExactlyOneReplica(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Action = ActionStart
	client := &memoryClient{observation: testObservation(request, now)}
	driver := testDriver(t, provider, client, now)
	mutation, err := driver.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if mutation.StatefulSet.Spec.Replicas != 1 {
		t.Fatalf("start replicas = %d", mutation.StatefulSet.Spec.Replicas)
	}
	if mutation.StatefulSet.Spec.UpdateStrategy.Type != "OnDelete" {
		t.Fatalf("unsafe update strategy: %#v", mutation.StatefulSet.Spec.UpdateStrategy)
	}
}

func TestStopOnlyScalesWorkloadAndKeepsPVCOutOfMutation(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Action = ActionStop
	request.ExpectedPodUID, request.ExpectedPVCUID = "pod-uid", "pvc-uid"
	observation := existingObservation(provider, request, now, 6, "old-lease", "old-operation")
	client := &memoryClient{observation: observation}
	driver := testDriver(t, provider, client, now)
	mutation, err := driver.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if mutation.StatefulSet.Spec.Replicas != 0 || mutation.PVC != nil || mutation.Service != nil || mutation.PublishedService != nil || mutation.NetworkPolicy != nil {
		t.Fatalf("stop mutation must only scale the StatefulSet: %#v", mutation)
	}
	if mutation.Preconditions.ExpectedPodUID != "pod-uid" || mutation.Preconditions.ExpectedPVCUID != "pvc-uid" {
		t.Fatalf("ownership preconditions missing: %#v", mutation.Preconditions)
	}
}

func TestStopDoesNotDependOnStaleCapacityOrRuntimeProfiles(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Action = ActionStop
	request.ExpectedPodUID, request.ExpectedPVCUID = "pod-uid", "pvc-uid"
	observation := existingObservation(provider, request, now, 6, "old-lease", "old-operation")
	request.Role, request.ClusterName, request.ShardDirectory, request.ComputeProfileID = "", "", "", ""
	request.CPU, request.Network, request.Storage = CPURequest{}, NetworkRequest{}, StorageRequest{}
	observation.CPU = CPUObservation{Stale: true}
	driver := testDriver(t, provider, &memoryClient{observation: observation}, now)
	mutation, err := driver.Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("identity-safe stop was blocked by unrelated runtime profiles: %v", err)
	}
	if mutation.Action != ActionStop || mutation.StatefulSet.Spec.Replicas != 0 || mutation.PVC != nil {
		t.Fatalf("unexpected stop mutation: %#v", mutation)
	}
}

func TestLeaseAndFencingConflictsAreRejected(t *testing.T) {
	now := time.Now().UTC()
	provider := testProvider()
	tests := []struct {
		name   string
		mutate func(*Request, *Observation)
		code   string
	}{
		{
			name: "expiring lease", code: "LEASE_EXPIRING",
			mutate: func(request *Request, _ *Observation) { request.Lease.ExpiresAt = now.Add(5 * time.Second) },
		},
		{
			name: "lower fencing token", code: "FENCING_TOKEN_STALE",
			mutate: func(request *Request, observation *Observation) {
				observation.StatefulSet = managedResource(provider, request.Ref, "sts", 8, "lease-new", "operation-new")
			},
		},
		{
			name: "equal token different lease", code: "FENCING_LEASE_CONFLICT",
			mutate: func(request *Request, observation *Observation) {
				observation.StatefulSet = managedResource(provider, request.Ref, "sts", request.Lease.FencingToken, "other-lease", request.Lease.OperationID)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := testRequest(now)
			observation := testObservation(request, now)
			test.mutate(&request, &observation)
			driver := testDriver(t, provider, &memoryClient{observation: observation}, now)
			_, err := driver.Plan(context.Background(), request)
			assertPreflightCodes(t, err, test.code)
		})
	}
}

func TestPodAndPVCIdentityCannotBeAdopted(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Action = ActionStart
	request.ExpectedPodUID, request.ExpectedPVCUID = "wrong-pod", "wrong-pvc"
	observation := existingObservation(provider, request, now, 6, "old-lease", "old-operation")
	observation.PVC.AccessModes = []AccessMode{"ReadWriteMany"}
	observation.PVC.ReclaimPolicy = "Delete"
	driver := testDriver(t, provider, &memoryClient{observation: observation}, now)
	_, err := driver.Plan(context.Background(), request)
	assertPreflightCodes(t, err, "POD_UID_MISMATCH", "PVC_UID_MISMATCH", "PVC_ACCESS_MODE_MISMATCH", "PVC_RECLAIM_POLICY_UNSAFE", "OLD_POD_STILL_EXISTS")
}

func TestManagedLabelScopeAndPathInputsAreStrict(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.ClusterName = "../Cluster_1"
	observation := testObservation(request, now)
	observation.Service = ResourceObservation{
		Exists: true, UID: "foreign-service", Labels: map[string]string{LabelManagedBy: ManagedByValue},
	}
	driver := testDriver(t, provider, &memoryClient{observation: observation}, now)
	_, err := driver.Plan(context.Background(), request)
	assertPreflightCodes(t, err, "CLUSTER_NAME_UNSAFE", "MANAGED_SCOPE_MISMATCH")
}

func TestStaleExpectedUIDsCannotCreateReplacementResources(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.ExpectedPodUID, request.ExpectedPVCUID = "old-pod", "old-pvc"
	driver := testDriver(t, provider, &memoryClient{observation: testObservation(request, now)}, now)
	_, err := driver.Plan(context.Background(), request)
	assertPreflightCodes(t, err, "POD_UID_STALE", "PVC_UID_STALE")
}

func TestObservationAgeIsEnforcedEvenWhenClientDoesNotSetStale(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	observation := testObservation(request, now)
	observation.ObservedAt = now.Add(-time.Minute)
	observation.CPU.ObservedAt = now.Add(-time.Minute)
	driver := testDriver(t, provider, &memoryClient{observation: observation}, now)
	_, err := driver.Plan(context.Background(), request)
	assertPreflightCodes(t, err, "OBSERVATION_STALE", "CPU_OBSERVATION_STALE")
}

func TestInternalProfileDoesNotSilentlyLeavePublicService(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	observation := testObservation(request, now)
	observation.PublishedService = managedResource(provider, request.Ref, "public-service", 6, "old-lease", "old-operation")
	driver := testDriver(t, provider, &memoryClient{observation: observation}, now)
	_, err := driver.Plan(context.Background(), request)
	assertPreflightCodes(t, err, "PUBLISHED_SERVICE_STILL_EXISTS")
}

func TestIdempotentSameLeaseFenceOperationMayKeepItsPod(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Action = ActionStart
	request.ExpectedPodUID, request.ExpectedPVCUID = "pod-uid", "pvc-uid"
	observation := existingObservation(provider, request, now, request.Lease.FencingToken, request.Lease.LeaseID, request.Lease.OperationID)
	driver := testDriver(t, provider, &memoryClient{observation: observation}, now)
	mutation, err := driver.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if mutation.StatefulSet.Spec.Replicas != 1 {
		t.Fatalf("replicas = %d", mutation.StatefulSet.Spec.Replicas)
	}
}

func TestTerminatingPodBlocksStartEvenForIdempotentOperation(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Action = ActionStart
	request.ExpectedPodUID, request.ExpectedPVCUID = "pod-uid", "pvc-uid"
	observation := existingObservation(provider, request, now, request.Lease.FencingToken, request.Lease.LeaseID, request.Lease.OperationID)
	deleting := now.Add(-time.Second)
	observation.Pod.DeletionTimestamp = &deleting
	driver := testDriver(t, provider, &memoryClient{observation: observation}, now)
	_, err := driver.Plan(context.Background(), request)
	assertPreflightCodes(t, err, "OLD_POD_TERMINATING")
}

func TestExclusiveCPURequiresOneFullPhysicalCoreAndGuaranteedQoS(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.CPU = CPURequest{
		Policy: CPUExclusive, RequestMilli: 1000, LimitMilli: 2000,
		MemoryRequestMiB: 2048, MemoryLimitMiB: 4096,
	}
	observation := testObservation(request, now)
	driver := testDriver(t, provider, &memoryClient{observation: observation}, now)
	_, err := driver.Plan(context.Background(), request)
	assertPreflightCodes(t, err, "EXCLUSIVE_CPU_NOT_ONE_CORE", "EXCLUSIVE_CPU_NOT_GUARANTEED")

	request.CPU.RequestMilli, request.CPU.LimitMilli, request.CPU.MemoryLimitMiB = 2000, 2000, 2048
	driver = testDriver(t, provider, &memoryClient{observation: observation}, now)
	if _, err := driver.Plan(context.Background(), request); err != nil {
		t.Fatalf("valid exclusive CPU rejected: %v", err)
	}
}

func TestNodePortRequiresTypedUniquePortsAndVerifiedEndpoint(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Network.Exposure = ExposureNodePort
	request.Network.AdvertiseAddress = "dst.example.com"
	request.Network.NodePorts = map[PortPurpose]int32{
		PortDSTServer: 31000, PortSteamAuth: 31002, PortSteamMasterServer: 31003,
	}
	driver := testDriver(t, provider, &memoryClient{observation: testObservation(request, now)}, now)
	mutation, err := driver.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if mutation.Service.Spec.Type != "ClusterIP" || len(mutation.Service.Spec.Ports) != 4 {
		t.Fatalf("internal Master service is invalid: %#v", mutation.Service)
	}
	if mutation.PublishedService == nil || mutation.PublishedService.Spec.Type != "NodePort" || mutation.PublishedService.Spec.Ports[0].NodePort != 31000 || len(mutation.PublishedService.Spec.Ports) != 3 {
		t.Fatalf("public NodePort service is invalid: %#v", mutation.PublishedService)
	}
	args := mutation.StatefulSet.Spec.Template.Spec.Containers[0].Args
	if !containsArgumentPair(args, "--advertise-address", "dst.example.com") || !containsArgumentPair(args, "--published-dst-server-port", "31000") {
		t.Fatalf("published endpoint arguments are incomplete: %#v", args)
	}

	request.Network.NodePorts[PortSteamAuth] = 31000
	provider.Capabilities.PublishedUDPEndpointVerified = false
	driver = testDriver(t, provider, &memoryClient{observation: testObservation(request, now)}, now)
	_, err = driver.Plan(context.Background(), request)
	assertPreflightCodes(t, err, "NODEPORT_DUPLICATE", "UDP_NODEPORT_UNVERIFIED")
}

func TestLoadBalancerDisablesImplicitNodePorts(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Network.Exposure = ExposureLoadBalancer
	request.Network.AdvertiseAddress = "203.0.113.10"
	driver := testDriver(t, provider, &memoryClient{observation: testObservation(request, now)}, now)
	mutation, err := driver.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if mutation.PublishedService == nil || mutation.PublishedService.Spec.Type != "LoadBalancer" || mutation.PublishedService.Spec.AllocateLoadBalancerNodePorts == nil || *mutation.PublishedService.Spec.AllocateLoadBalancerNodePorts {
		t.Fatalf("LoadBalancer must explicitly disable hidden NodePorts: %#v", mutation.PublishedService)
	}
}

func TestSecondaryUsesStableMasterServiceDNSAndDoesNotExposeMasterPort(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Ref.WorldID, request.Role, request.ShardDirectory = "caves-world", RoleSecondary, "Caves"
	request.MasterWorldID = "master-world"
	observation := testObservation(request, now)
	driver := testDriver(t, provider, &memoryClient{observation: observation}, now)
	mutation, err := driver.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(mutation.Service.Spec.Ports) != 3 || len(mutation.NetworkPolicy.Spec.Ingress) != 1 {
		t.Fatalf("secondary exposes a non-local Master port: %#v %#v", mutation.Service, mutation.NetworkPolicy)
	}
	masterDNS := workloadName(ShardRef{ProviderID: provider.ID, RoomID: request.Ref.RoomID, WorldID: request.MasterWorldID}) + "." + provider.Namespace + ".svc"
	args := mutation.StatefulSet.Spec.Template.Spec.Containers[0].Args
	if !containsArgumentPair(args, "--master-address", masterDNS) {
		t.Fatalf("master DNS missing from fixed supervisor arguments: %#v", args)
	}
}

func TestApplyPassesOnlyTypedMutationAndRejectsEscapedObservation(t *testing.T) {
	now := time.Now().UTC()
	provider, request := testProvider(), testRequest(now)
	request.Action = ActionStart
	observation := testObservation(request, now)
	escaped := observation
	escaped.StatefulSet = ResourceObservation{Exists: true, Labels: map[string]string{LabelManagedBy: ManagedByValue}}
	client := &memoryClient{observation: observation, applyResult: &escaped}
	driver := testDriver(t, provider, client, now)
	if _, err := driver.Apply(context.Background(), request); !errors.Is(err, ErrObservation) {
		t.Fatalf("Apply error = %v", err)
	}
	if client.applied == nil || client.applied.Release != ReleaseExperimental {
		t.Fatalf("typed mutation was not applied: %#v", client.applied)
	}
}

func existingObservation(provider Provider, request Request, now time.Time, fence uint64, leaseID, operationID string) Observation {
	observation := testObservation(request, now)
	observation.StatefulSet = managedResource(provider, request.Ref, "sts-uid", fence, leaseID, operationID)
	observation.Pod = PodObservation{
		ResourceObservation: managedResource(provider, request.Ref, "pod-uid", fence, leaseID, operationID),
		Phase:               "Running",
	}
	observation.PVC = PVCObservation{
		ResourceObservation: managedResource(provider, request.Ref, "pvc-uid", fence, leaseID, operationID),
		Phase:               "Bound", StorageClassName: "local-retain", AccessModes: []AccessMode{AccessReadWriteOncePod},
		ReclaimPolicy: ReclaimRetain, CapacityGiB: 30,
	}
	observation.Service = managedResource(provider, request.Ref, "service-uid", fence, leaseID, operationID)
	if request.Network.Exposure != ExposureInternal {
		observation.PublishedService = managedResource(provider, request.Ref, "published-service-uid", fence, leaseID, operationID)
	}
	observation.NetworkPolicy = managedResource(provider, request.Ref, "policy-uid", fence, leaseID, operationID)
	return observation
}

func managedResource(provider Provider, ref ShardRef, uid string, fence uint64, leaseID, operationID string) ResourceObservation {
	return ResourceObservation{
		Exists: true, UID: uid, ResourceVersion: "rv-" + uid,
		Labels: expectedScopeLabels(provider.ID, ref.RoomID, ref.WorldID),
		Annotations: map[string]string{
			AnnotationProviderID: provider.ID, AnnotationRoomID: ref.RoomID, AnnotationWorldID: ref.WorldID,
			AnnotationFencingToken: strconvUint(fence), AnnotationLeaseID: leaseID, AnnotationOperationID: operationID,
		},
	}
}

func strconvUint(value uint64) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	buffer := make([]byte, 0, 20)
	for value > 0 {
		buffer = append(buffer, digits[value%10])
		value /= 10
	}
	for left, right := 0, len(buffer)-1; left < right; left, right = left+1, right-1 {
		buffer[left], buffer[right] = buffer[right], buffer[left]
	}
	return string(buffer)
}

func assertPreflightCodes(t *testing.T, err error, expected ...string) {
	t.Helper()
	var failure *PreflightError
	if !errors.As(err, &failure) {
		t.Fatalf("expected PreflightError, got %v", err)
	}
	actual := map[string]bool{}
	for _, issue := range failure.Report.Issues {
		actual[issue.Code] = true
	}
	for _, code := range expected {
		if !actual[code] {
			t.Fatalf("missing issue %q in %#v", code, failure.Report.Issues)
		}
	}
}

func containsArgumentPair(arguments []string, key, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == key && arguments[index+1] == value {
			return true
		}
	}
	return false
}
