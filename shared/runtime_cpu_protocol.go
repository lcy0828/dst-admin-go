package shared

import "time"

type RuntimeCPUPolicy string

const (
	RuntimeCPUPolicyNone      RuntimeCPUPolicy = "none"
	RuntimeCPUPolicyShared    RuntimeCPUPolicy = "shared"
	RuntimeCPUPolicyExclusive RuntimeCPUPolicy = "exclusive"
)

type RuntimeCPUState string

const (
	RuntimeCPUStatePrepared RuntimeCPUState = "prepared"
	RuntimeCPUStateApplied  RuntimeCPUState = "applied"
	RuntimeCPUStateReleased RuntimeCPUState = "released"
	RuntimeCPUStateUnknown  RuntimeCPUState = "unknown"
)

// RuntimeCPURequest carries policy intent only. Process, cgroup and container
// identities are resolved from the Agent's trusted installation inventory.
type RuntimeCPURequest struct {
	Policy        RuntimeCPUPolicy `json:"policy"`
	LogicalCPUIds []int            `json:"logical_cpu_ids,omitempty"`
}

type RuntimeCPUResult struct {
	Policy          RuntimeCPUPolicy `json:"policy"`
	LogicalCPUIds   []int            `json:"logical_cpu_ids,omitempty"`
	EffectiveCPUIds []int            `json:"effective_cpu_ids,omitempty"`
	State           RuntimeCPUState  `json:"state"`
	RuntimeKind     string           `json:"runtime_kind"`
	InstanceID      string           `json:"instance_id,omitempty"`
	PID             int32            `json:"pid,omitempty"`
	QuotaMicros     int64            `json:"quota_micros,omitempty"`
	PeriodMicros    int64            `json:"period_micros,omitempty"`
	Enforced        bool             `json:"enforced"`
	InstanceRunning bool             `json:"instance_running"`
	ObservedAt      time.Time        `json:"observed_at"`
}

func IsRuntimeCPUPolicy(value RuntimeCPUPolicy) bool {
	return value == RuntimeCPUPolicyNone || value == RuntimeCPUPolicyShared || value == RuntimeCPUPolicyExclusive
}
