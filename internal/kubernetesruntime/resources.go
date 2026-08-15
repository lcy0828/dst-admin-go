package kubernetesruntime

// The resource definitions below intentionally model only fields emitted by
// this Driver. They are not a general-purpose Kubernetes manifest API.

type TypeMeta struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
}

type ObjectMeta struct {
	Name            string            `json:"name" yaml:"name"`
	Namespace       string            `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Labels          map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty" yaml:"resourceVersion,omitempty"`
}

type LabelSelector struct {
	MatchLabels map[string]string `json:"matchLabels" yaml:"matchLabels"`
}

type StatefulSet struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta      `json:"metadata" yaml:"metadata"`
	Spec     StatefulSetSpec `json:"spec" yaml:"spec"`
}

type StatefulSetSpec struct {
	ServiceName         string          `json:"serviceName" yaml:"serviceName"`
	Replicas            int32           `json:"replicas" yaml:"replicas"`
	PodManagementPolicy string          `json:"podManagementPolicy" yaml:"podManagementPolicy"`
	Selector            LabelSelector   `json:"selector" yaml:"selector"`
	Template            PodTemplateSpec `json:"template" yaml:"template"`
	UpdateStrategy      UpdateStrategy  `json:"updateStrategy" yaml:"updateStrategy"`
}

type UpdateStrategy struct {
	Type string `json:"type" yaml:"type"`
}

type PodTemplateSpec struct {
	Metadata ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec     PodSpec    `json:"spec" yaml:"spec"`
}

type PodSpec struct {
	ServiceAccountName            string             `json:"serviceAccountName" yaml:"serviceAccountName"`
	AutomountServiceAccountToken  *bool              `json:"automountServiceAccountToken" yaml:"automountServiceAccountToken"`
	TerminationGracePeriodSeconds int64              `json:"terminationGracePeriodSeconds" yaml:"terminationGracePeriodSeconds"`
	NodeSelector                  map[string]string  `json:"nodeSelector,omitempty" yaml:"nodeSelector,omitempty"`
	SecurityContext               PodSecurityContext `json:"securityContext" yaml:"securityContext"`
	Containers                    []Container        `json:"containers" yaml:"containers"`
	Volumes                       []Volume           `json:"volumes" yaml:"volumes"`
}

type PodSecurityContext struct {
	RunAsNonRoot   bool           `json:"runAsNonRoot" yaml:"runAsNonRoot"`
	RunAsUser      int64          `json:"runAsUser" yaml:"runAsUser"`
	RunAsGroup     int64          `json:"runAsGroup" yaml:"runAsGroup"`
	FSGroup        int64          `json:"fsGroup" yaml:"fsGroup"`
	SeccompProfile SeccompProfile `json:"seccompProfile" yaml:"seccompProfile"`
}

type SeccompProfile struct {
	Type string `json:"type" yaml:"type"`
}

type Container struct {
	Name            string                   `json:"name" yaml:"name"`
	Image           string                   `json:"image" yaml:"image"`
	ImagePullPolicy string                   `json:"imagePullPolicy" yaml:"imagePullPolicy"`
	Command         []string                 `json:"command" yaml:"command"`
	Args            []string                 `json:"args" yaml:"args"`
	Env             []EnvVar                 `json:"env" yaml:"env"`
	Ports           []ContainerPort          `json:"ports" yaml:"ports"`
	Resources       ResourceRequirements     `json:"resources" yaml:"resources"`
	SecurityContext ContainerSecurityContext `json:"securityContext" yaml:"securityContext"`
	ReadinessProbe  Probe                    `json:"readinessProbe" yaml:"readinessProbe"`
	StartupProbe    Probe                    `json:"startupProbe" yaml:"startupProbe"`
	VolumeMounts    []VolumeMount            `json:"volumeMounts" yaml:"volumeMounts"`
}

type EnvVar struct {
	Name      string       `json:"name" yaml:"name"`
	ValueFrom EnvVarSource `json:"valueFrom" yaml:"valueFrom"`
}

type EnvVarSource struct {
	SecretKeyRef SecretKeySelector `json:"secretKeyRef" yaml:"secretKeyRef"`
}

type SecretKeySelector struct {
	Name string `json:"name" yaml:"name"`
	Key  string `json:"key" yaml:"key"`
}

type ContainerPort struct {
	Name          string `json:"name" yaml:"name"`
	ContainerPort int32  `json:"containerPort" yaml:"containerPort"`
	Protocol      string `json:"protocol" yaml:"protocol"`
}

type ResourceRequirements struct {
	Requests map[string]string `json:"requests" yaml:"requests"`
	Limits   map[string]string `json:"limits" yaml:"limits"`
}

type ContainerSecurityContext struct {
	AllowPrivilegeEscalation bool             `json:"allowPrivilegeEscalation" yaml:"allowPrivilegeEscalation"`
	ReadOnlyRootFilesystem   bool             `json:"readOnlyRootFilesystem" yaml:"readOnlyRootFilesystem"`
	Privileged               bool             `json:"privileged" yaml:"privileged"`
	Capabilities             CapabilitiesDrop `json:"capabilities" yaml:"capabilities"`
}

type CapabilitiesDrop struct {
	Drop []string `json:"drop" yaml:"drop"`
}

type Probe struct {
	Exec                ExecAction `json:"exec" yaml:"exec"`
	InitialDelaySeconds int32      `json:"initialDelaySeconds,omitempty" yaml:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int32      `json:"periodSeconds" yaml:"periodSeconds"`
	TimeoutSeconds      int32      `json:"timeoutSeconds" yaml:"timeoutSeconds"`
	FailureThreshold    int32      `json:"failureThreshold" yaml:"failureThreshold"`
}

type ExecAction struct {
	Command []string `json:"command" yaml:"command"`
}

type VolumeMount struct {
	Name      string `json:"name" yaml:"name"`
	MountPath string `json:"mountPath" yaml:"mountPath"`
}

type Volume struct {
	Name                  string                             `json:"name" yaml:"name"`
	PersistentVolumeClaim *PersistentVolumeClaimVolumeSource `json:"persistentVolumeClaim,omitempty" yaml:"persistentVolumeClaim,omitempty"`
	EmptyDir              *EmptyDirVolumeSource              `json:"emptyDir,omitempty" yaml:"emptyDir,omitempty"`
}

type PersistentVolumeClaimVolumeSource struct {
	ClaimName string `json:"claimName" yaml:"claimName"`
}

type EmptyDirVolumeSource struct {
	Medium    string `json:"medium,omitempty" yaml:"medium,omitempty"`
	SizeLimit string `json:"sizeLimit,omitempty" yaml:"sizeLimit,omitempty"`
}

type PersistentVolumeClaim struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta                `json:"metadata" yaml:"metadata"`
	Spec     PersistentVolumeClaimSpec `json:"spec" yaml:"spec"`
}

type PersistentVolumeClaimSpec struct {
	AccessModes      []string                   `json:"accessModes" yaml:"accessModes"`
	StorageClassName string                     `json:"storageClassName" yaml:"storageClassName"`
	Resources        VolumeResourceRequirements `json:"resources" yaml:"resources"`
}

type VolumeResourceRequirements struct {
	Requests map[string]string `json:"requests" yaml:"requests"`
}

type Service struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta  `json:"metadata" yaml:"metadata"`
	Spec     ServiceSpec `json:"spec" yaml:"spec"`
}

type ServiceSpec struct {
	Type                          string            `json:"type" yaml:"type"`
	Selector                      map[string]string `json:"selector" yaml:"selector"`
	Ports                         []ServicePort     `json:"ports" yaml:"ports"`
	AllocateLoadBalancerNodePorts *bool             `json:"allocateLoadBalancerNodePorts,omitempty" yaml:"allocateLoadBalancerNodePorts,omitempty"`
}

type ServicePort struct {
	Name       string `json:"name" yaml:"name"`
	Protocol   string `json:"protocol" yaml:"protocol"`
	Port       int32  `json:"port" yaml:"port"`
	TargetPort int32  `json:"targetPort" yaml:"targetPort"`
	NodePort   int32  `json:"nodePort,omitempty" yaml:"nodePort,omitempty"`
}

type NetworkPolicy struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta        `json:"metadata" yaml:"metadata"`
	Spec     NetworkPolicySpec `json:"spec" yaml:"spec"`
}

type NetworkPolicySpec struct {
	PodSelector LabelSelector              `json:"podSelector" yaml:"podSelector"`
	PolicyTypes []string                   `json:"policyTypes" yaml:"policyTypes"`
	Ingress     []NetworkPolicyIngressRule `json:"ingress" yaml:"ingress"`
}

type NetworkPolicyIngressRule struct {
	From  []NetworkPolicyPeer `json:"from,omitempty" yaml:"from,omitempty"`
	Ports []NetworkPolicyPort `json:"ports" yaml:"ports"`
}

type NetworkPolicyPeer struct {
	PodSelector *LabelSelector `json:"podSelector,omitempty" yaml:"podSelector,omitempty"`
}

type NetworkPolicyPort struct {
	Protocol string `json:"protocol" yaml:"protocol"`
	Port     int32  `json:"port" yaml:"port"`
}
