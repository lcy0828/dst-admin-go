package kubernetesruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

const supervisorPath = "/usr/local/bin/dst-admin-runtime-supervisor"

var portNames = map[PortPurpose]string{
	PortDSTServer:         "dst-server",
	PortClusterMaster:     "cluster-master",
	PortSteamAuth:         "steam-auth",
	PortSteamMasterServer: "steam-master",
}

func buildMutation(provider Provider, request Request, observation Observation) TypedMutation {
	name := workloadName(request.Ref)
	labels := expectedScopeLabels(provider.ID, request.Ref.RoomID, request.Ref.WorldID)
	annotations := operationAnnotations(provider.ID, request)
	selector := map[string]string{
		LabelManagedBy:  labels[LabelManagedBy],
		LabelComponent:  labels[LabelComponent],
		LabelProviderID: labels[LabelProviderID],
		LabelRoomID:     labels[LabelRoomID],
		LabelWorldID:    labels[LabelWorldID],
	}
	replicas := int32(0)
	if request.Action == ActionStart {
		replicas = 1
	}
	automountToken := false
	containerPorts := make([]ContainerPort, 0, 4)
	for _, purpose := range localPortPurposes(request.Role) {
		containerPorts = append(containerPorts, ContainerPort{
			Name: portNames[purpose], ContainerPort: request.Network.Ports[purpose], Protocol: "UDP",
		})
	}
	profile := provider.ComputeProfiles[request.ComputeProfileID]
	pvcName := name + "-data"
	credentialSecret := credentialSecretName(provider, request.Ref.RoomID)
	workload := StatefulSet{
		TypeMeta: TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		Metadata: ObjectMeta{
			Name: name, Namespace: provider.Namespace, Labels: cloneMap(labels),
			Annotations: cloneMap(annotations), ResourceVersion: observation.StatefulSet.ResourceVersion,
		},
		Spec: StatefulSetSpec{
			ServiceName: name, Replicas: replicas, PodManagementPolicy: "OrderedReady",
			Selector:       LabelSelector{MatchLabels: cloneMap(selector)},
			UpdateStrategy: UpdateStrategy{Type: "OnDelete"},
			Template: PodTemplateSpec{
				Metadata: ObjectMeta{Labels: cloneMap(labels), Annotations: cloneMap(annotations)},
				Spec: PodSpec{
					ServiceAccountName:            provider.RuntimeServiceAccountName,
					AutomountServiceAccountToken:  &automountToken,
					TerminationGracePeriodSeconds: 90,
					NodeSelector:                  cloneMap(profile.NodeSelector),
					SecurityContext: PodSecurityContext{
						RunAsNonRoot: true, RunAsUser: 10001, RunAsGroup: 10001, FSGroup: 10001,
						SeccompProfile: SeccompProfile{Type: "RuntimeDefault"},
					},
					Containers: []Container{{
						Name: "dst-runtime", Image: provider.RuntimeImage, ImagePullPolicy: "IfNotPresent",
						Command: []string{supervisorPath}, Args: supervisorArguments(provider, request),
						Env: []EnvVar{
							{Name: "DST_CLUSTER_TOKEN", ValueFrom: EnvVarSource{SecretKeyRef: SecretKeySelector{Name: credentialSecret, Key: "cluster-token"}}},
							{Name: "DST_CLUSTER_KEY", ValueFrom: EnvVarSource{SecretKeyRef: SecretKeySelector{Name: credentialSecret, Key: "cluster-key"}}},
						},
						Ports: containerPorts,
						Resources: ResourceRequirements{
							Requests: map[string]string{"cpu": cpuQuantity(request.CPU.RequestMilli), "memory": memoryQuantity(request.CPU.MemoryRequestMiB)},
							Limits:   map[string]string{"cpu": cpuQuantity(request.CPU.LimitMilli), "memory": memoryQuantity(request.CPU.MemoryLimitMiB)},
						},
						SecurityContext: ContainerSecurityContext{
							AllowPrivilegeEscalation: false, ReadOnlyRootFilesystem: true, Privileged: false,
							Capabilities: CapabilitiesDrop{Drop: []string{"ALL"}},
						},
						ReadinessProbe: Probe{
							Exec:          ExecAction{Command: []string{supervisorPath, "health", "--ready"}},
							PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3,
						},
						StartupProbe: Probe{
							Exec:          ExecAction{Command: []string{supervisorPath, "health", "--ready"}},
							PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 120,
						},
						VolumeMounts: []VolumeMount{
							{Name: "data", MountPath: "/data"},
							{Name: "runtime", MountPath: "/run/dst-admin"},
						},
					}},
					Volumes: []Volume{
						{Name: "data", PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: pvcName}},
						{Name: "runtime", EmptyDir: &EmptyDirVolumeSource{Medium: "Memory", SizeLimit: "64Mi"}},
					},
				},
			},
		},
	}
	mutation := TypedMutation{
		Release: ReleaseExperimental, Action: request.Action, Ref: request.Ref, Namespace: provider.Namespace,
		Lease: request.Lease,
		Preconditions: Preconditions{
			ExpectedPodUID: request.ExpectedPodUID, ExpectedPodResourceVersion: observation.Pod.ResourceVersion,
			ExpectedPVCUID: request.ExpectedPVCUID, ExpectedPVCResourceVersion: observation.PVC.ResourceVersion,
			ExpectedStatefulSetResourceVersion: observation.StatefulSet.ResourceVersion,
		},
		StatefulSet: workload,
	}
	if request.Action == ActionStop {
		return mutation
	}
	storage := provider.StorageProfiles[request.Storage.ProfileID]
	requestedGiB := request.Storage.SizeGiB
	if observation.PVC.Exists && observation.PVC.CapacityGiB > requestedGiB {
		requestedGiB = observation.PVC.CapacityGiB
	}
	mutation.PVC = &PersistentVolumeClaim{
		TypeMeta: TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		Metadata: ObjectMeta{
			Name: pvcName, Namespace: provider.Namespace, Labels: cloneMap(labels),
			Annotations:     withAnnotation(annotations, AnnotationStorageRetain, "true"),
			ResourceVersion: observation.PVC.ResourceVersion,
		},
		Spec: PersistentVolumeClaimSpec{
			AccessModes: []string{string(storage.AccessMode)}, StorageClassName: storage.StorageClassName,
			Resources: VolumeResourceRequirements{Requests: map[string]string{"storage": fmt.Sprintf("%dGi", requestedGiB)}},
		},
	}
	service := buildService(provider, request, observation, labels, annotations, selector, name)
	policy := buildNetworkPolicy(provider, request, observation, labels, annotations, selector, name)
	mutation.Service = &service
	if request.Network.Exposure != ExposureInternal {
		published := buildPublishedService(provider, request, observation, labels, annotations, selector, name+"-public")
		mutation.PublishedService = &published
	}
	mutation.NetworkPolicy = &policy
	return mutation
}

func buildService(provider Provider, request Request, observation Observation, labels, annotations, selector map[string]string, name string) Service {
	ports := make([]ServicePort, 0, 4)
	for _, purpose := range localPortPurposes(request.Role) {
		port := request.Network.Ports[purpose]
		ports = append(ports, ServicePort{Name: portNames[purpose], Protocol: "UDP", Port: port, TargetPort: port})
	}
	return Service{
		TypeMeta: TypeMeta{APIVersion: "v1", Kind: "Service"},
		Metadata: ObjectMeta{
			Name: name, Namespace: provider.Namespace, Labels: cloneMap(labels),
			Annotations: cloneMap(annotations), ResourceVersion: observation.Service.ResourceVersion,
		},
		Spec: ServiceSpec{
			Type: "ClusterIP", Selector: cloneMap(selector), Ports: ports,
		},
	}
}

func buildPublishedService(provider Provider, request Request, observation Observation, labels, annotations, selector map[string]string, name string) Service {
	serviceType := "NodePort"
	allocateNodePorts := (*bool)(nil)
	if request.Network.Exposure == ExposureLoadBalancer {
		serviceType = "LoadBalancer"
		value := false
		allocateNodePorts = &value
	}
	ports := make([]ServicePort, 0, 3)
	for _, purpose := range publicPortPurposes() {
		port := request.Network.Ports[purpose]
		servicePort := ServicePort{Name: portNames[purpose], Protocol: "UDP", Port: port, TargetPort: port}
		if request.Network.Exposure == ExposureNodePort {
			servicePort.NodePort = request.Network.NodePorts[purpose]
		}
		ports = append(ports, servicePort)
	}
	return Service{
		TypeMeta: TypeMeta{APIVersion: "v1", Kind: "Service"},
		Metadata: ObjectMeta{
			Name: name, Namespace: provider.Namespace, Labels: cloneMap(labels),
			Annotations: cloneMap(annotations), ResourceVersion: observation.PublishedService.ResourceVersion,
		},
		Spec: ServiceSpec{
			Type: serviceType, Selector: cloneMap(selector), Ports: ports,
			AllocateLoadBalancerNodePorts: allocateNodePorts,
		},
	}
}

func buildNetworkPolicy(provider Provider, request Request, observation Observation, labels, annotations, selector map[string]string, name string) NetworkPolicy {
	publicPorts := make([]NetworkPolicyPort, 0, 3)
	for _, purpose := range []PortPurpose{PortDSTServer, PortSteamAuth, PortSteamMasterServer} {
		publicPorts = append(publicPorts, NetworkPolicyPort{Protocol: "UDP", Port: request.Network.Ports[purpose]})
	}
	ingress := []NetworkPolicyIngressRule{{Ports: publicPorts}}
	if request.Role == RoleMaster {
		roomPeers := LabelSelector{MatchLabels: map[string]string{
			LabelManagedBy: ManagedByValue, LabelComponent: "dst-shard",
			LabelProviderID: identityLabel(provider.ID), LabelRoomID: identityLabel(request.Ref.RoomID),
		}}
		ingress = append(ingress, NetworkPolicyIngressRule{
			From:  []NetworkPolicyPeer{{PodSelector: &roomPeers}},
			Ports: []NetworkPolicyPort{{Protocol: "UDP", Port: request.Network.Ports[PortClusterMaster]}},
		})
	}
	return NetworkPolicy{
		TypeMeta: TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		Metadata: ObjectMeta{
			Name: name, Namespace: provider.Namespace, Labels: cloneMap(labels),
			Annotations: cloneMap(annotations), ResourceVersion: observation.NetworkPolicy.ResourceVersion,
		},
		Spec: NetworkPolicySpec{
			PodSelector: LabelSelector{MatchLabels: cloneMap(selector)}, PolicyTypes: []string{"Ingress"}, Ingress: ingress,
		},
	}
}

func supervisorArguments(provider Provider, request Request) []string {
	arguments := []string{
		"run-shard",
		"--provider-id", provider.ID,
		"--room-id", request.Ref.RoomID,
		"--world-id", request.Ref.WorldID,
		"--cluster", request.ClusterName,
		"--shard", request.ShardDirectory,
		"--role", string(request.Role),
		"--operation-id", request.Lease.OperationID,
		"--lease-id", request.Lease.LeaseID,
		"--lease-expires-at", request.Lease.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"--fencing-token", strconv.FormatUint(request.Lease.FencingToken, 10),
		"--topology-revision", request.Lease.TopologyRevision,
		"--server-port", strconv.Itoa(int(request.Network.Ports[PortDSTServer])),
		"--cluster-master-port", strconv.Itoa(int(request.Network.Ports[PortClusterMaster])),
		"--steam-auth-port", strconv.Itoa(int(request.Network.Ports[PortSteamAuth])),
		"--steam-master-port", strconv.Itoa(int(request.Network.Ports[PortSteamMasterServer])),
	}
	if request.Role == RoleSecondary {
		masterRef := ShardRef{ProviderID: provider.ID, RoomID: request.Ref.RoomID, WorldID: request.MasterWorldID}
		masterDNS := workloadName(masterRef) + "." + provider.Namespace + ".svc"
		arguments = append(arguments, "--master-address", masterDNS)
	}
	if request.Network.Exposure != ExposureInternal {
		arguments = append(arguments, "--advertise-address", request.Network.AdvertiseAddress)
	}
	if request.Network.Exposure == ExposureNodePort {
		for _, purpose := range publicPortPurposes() {
			arguments = append(arguments, "--published-"+portNames[purpose]+"-port", strconv.Itoa(int(request.Network.NodePorts[purpose])))
		}
	}
	return arguments
}

func operationAnnotations(providerID string, request Request) map[string]string {
	return map[string]string{
		AnnotationProviderID:      providerID,
		AnnotationRoomID:          request.Ref.RoomID,
		AnnotationWorldID:         request.Ref.WorldID,
		AnnotationOperationID:     request.Lease.OperationID,
		AnnotationLeaseID:         request.Lease.LeaseID,
		AnnotationLeaseExpiresAt:  request.Lease.ExpiresAt.UTC().Format(time.RFC3339Nano),
		AnnotationFencingToken:    strconv.FormatUint(request.Lease.FencingToken, 10),
		AnnotationTopologyVersion: request.Lease.TopologyRevision,
	}
}

func workloadName(ref ShardRef) string {
	digest := sha256.Sum256([]byte(ref.ProviderID + "\x00" + ref.RoomID + "\x00" + ref.WorldID))
	return "dst-shard-" + hex.EncodeToString(digest[:10])
}

func credentialSecretName(provider Provider, roomID string) string {
	return provider.CredentialSecretPrefix + "-" + identityLabel(roomID)
}

func cpuQuantity(milli int64) string {
	if milli%1000 == 0 {
		return strconv.FormatInt(milli/1000, 10)
	}
	return strconv.FormatInt(milli, 10) + "m"
}

func memoryQuantity(mib int64) string { return strconv.FormatInt(mib, 10) + "Mi" }

func cloneMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func withAnnotation(values map[string]string, key, value string) map[string]string {
	result := cloneMap(values)
	result[key] = value
	return result
}
