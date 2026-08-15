package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"dont/internal/runtimecpu"
	"dont/shared"
)

type cpuRuntimeControl interface {
	ExecuteCPU(context.Context, string, string, shared.RuntimeAction, shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error)
}

func (a *Agent) executeCPUAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(request, shared.RuntimeOutcomeConfirmed, "CPU 策略已执行并回读")
	var observed shared.RuntimeCPUResult
	var err error
	if installation.Driver == "container" {
		control, controlErr := a.runtimeControl(installation)
		if controlErr != nil {
			err = controlErr
		} else if cpu, ok := control.(cpuRuntimeControl); ok {
			observed, err = cpu.ExecuteCPU(ctx, request.Cluster, request.Shard, request.Action, *request.CPU)
		} else {
			err = errors.New("容器 Runtime 不支持 CPU 策略")
		}
	} else {
		executor, createErr := runtimecpu.NewNative(runtimecpu.NativeConfig{Platform: runtime.GOOS, ServerRoot: installation.ServerPath})
		if createErr != nil {
			err = createErr
		} else {
			switch request.Action {
			case shared.RuntimeActionCPUPrepare:
				observed, err = executor.Prepare(ctx, installation.ID, request.Cluster, request.Shard, *request.CPU)
			case shared.RuntimeActionCPUApply:
				observed, err = executor.Apply(ctx, installation.ID, request.Cluster, request.Shard, *request.CPU)
			case shared.RuntimeActionCPUObserve:
				observed, err = executor.Observe(ctx, installation.ID, request.Cluster, request.Shard, *request.CPU)
			default:
				err = errors.New("CPU Runtime 动作不受支持")
			}
		}
	}
	result.CPU = &observed
	if request.Action == shared.RuntimeActionCPUObserve {
		result.Outcome = shared.RuntimeOutcomeObserved
	}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

type containerInspectCPU struct {
	ID     string `json:"Id"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	HostConfig struct {
		NanoCPUs   int64  `json:"NanoCpus"`
		CpusetCpus string `json:"CpusetCpus"`
	} `json:"HostConfig"`
}

func (c *containerShardRuntime) ExecuteCPU(ctx context.Context, cluster, shard string, action shared.RuntimeAction, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	request.LogicalCPUIds = runtimecpu.SortedUnique(request.LogicalCPUIds)
	if !shared.IsRuntimeCPUPolicy(request.Policy) || request.Policy == shared.RuntimeCPUPolicyNone && len(request.LogicalCPUIds) != 0 || request.Policy != shared.RuntimeCPUPolicyNone && len(request.LogicalCPUIds) == 0 {
		return shared.RuntimeCPUResult{}, errors.New("CPU 策略负载无效")
	}
	before, err := c.find(ctx, cluster, shard)
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	beforeInstanceID := before.ID
	if before.State == "running" || before.State == "restarting" {
		beforeInstanceID, err = c.runtimeInstanceID(ctx, before.ID)
		if err != nil {
			return shared.RuntimeCPUResult{}, err
		}
	}
	if action == shared.RuntimeActionCPUPrepare || action == shared.RuntimeActionCPUApply {
		cpus, cpuset := "0", ""
		if request.Policy != shared.RuntimeCPUPolicyNone {
			cpus, cpuset = strconv.Itoa(len(request.LogicalCPUIds)), runtimecpu.FormatCPUSet(request.LogicalCPUIds)
		}
		if _, err := c.cli.Run(ctx, "update", "--cpus", cpus, "--cpuset-cpus", cpuset, before.ID); err != nil {
			return shared.RuntimeCPUResult{}, fmt.Errorf("更新受管分片容器 CPU 策略: %w", err)
		}
	}
	after, err := c.find(ctx, cluster, shard)
	if err != nil {
		return shared.RuntimeCPUResult{}, err
	}
	if before.ID != after.ID {
		return shared.RuntimeCPUResult{}, runtimecpu.ErrInstanceChanged
	}
	afterInstanceID := after.ID
	if after.State == "running" || after.State == "restarting" {
		afterInstanceID, err = c.runtimeInstanceID(ctx, after.ID)
		if err != nil {
			return shared.RuntimeCPUResult{}, err
		}
	}
	if beforeInstanceID != afterInstanceID {
		return shared.RuntimeCPUResult{}, runtimecpu.ErrInstanceChanged
	}
	output, err := c.cli.Run(ctx, "inspect", "--format", "{{json .}}", after.ID)
	if err != nil {
		return shared.RuntimeCPUResult{}, fmt.Errorf("回读受管分片容器 CPU 策略: %w", err)
	}
	var inspect containerInspectCPU
	if err := json.Unmarshal(output, &inspect); err != nil {
		return shared.RuntimeCPUResult{}, errors.New("容器 CPU 回读不是有效 JSON")
	}
	if strings.ToLower(strings.TrimSpace(inspect.ID)) != after.ID || inspect.Config.Labels["com.dst-admin.managed"] != "true" || inspect.Config.Labels["com.dst-admin.installation"] != c.installation.ID || inspect.Config.Labels["com.dst-admin.cluster"] != cluster || inspect.Config.Labels["com.dst-admin.shard"] != shard {
		return shared.RuntimeCPUResult{}, errors.New("容器 CPU 回读身份与受管 Placement 不一致")
	}
	effective, err := runtimecpu.ParseCPUSet(inspect.HostConfig.CpusetCpus)
	if err != nil {
		return shared.RuntimeCPUResult{}, runtimecpu.ErrCPUNotApplied
	}
	expectedNano := int64(len(request.LogicalCPUIds)) * 1_000_000_000
	if request.Policy == shared.RuntimeCPUPolicyNone {
		expectedNano = 0
	}
	if inspect.HostConfig.NanoCPUs != expectedNano || !equalCPUIds(effective, request.LogicalCPUIds) {
		return shared.RuntimeCPUResult{}, runtimecpu.ErrCPUNotApplied
	}
	state := shared.RuntimeCPUStatePrepared
	enforced := false
	if request.Policy == shared.RuntimeCPUPolicyNone {
		state, enforced = shared.RuntimeCPUStateReleased, true
	} else if inspect.State.Running {
		state, enforced = shared.RuntimeCPUStateApplied, true
	}
	if action == shared.RuntimeActionCPUApply && request.Policy != shared.RuntimeCPUPolicyNone && !inspect.State.Running {
		return shared.RuntimeCPUResult{}, errors.New("容器尚未运行，无法确认 CPU 策略已作用于分片进程")
	}
	return shared.RuntimeCPUResult{
		Policy: request.Policy, LogicalCPUIds: request.LogicalCPUIds, EffectiveCPUIds: effective,
		State: state, RuntimeKind: "container", InstanceID: afterInstanceID,
		QuotaMicros: int64(len(request.LogicalCPUIds)) * 100000, PeriodMicros: 100000,
		Enforced: enforced, InstanceRunning: inspect.State.Running, ObservedAt: time.Now().UTC(),
	}, nil
}

func equalCPUIds(left, right []int) bool {
	left, right = runtimecpu.SortedUnique(left), runtimecpu.SortedUnique(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
