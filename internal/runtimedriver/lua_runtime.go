package runtimedriver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/operationlease"
	"dont/internal/roomops"
	"dont/shared"
)

type LuaRuntimeDriver interface {
	EnsureLuaRuntime(context.Context, Target, Operation, shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error)
}

// LuaRuntimeScope uses only applied placement and registered node metadata.
// Report reads must never refresh fleet inventory or contact an Agent.
func (r *Router) LuaRuntimeScope(roomID, worldID string) (string, error) {
	placement, err := r.placements.AppliedPlacement(roomID, worldID)
	if err != nil {
		return "", err
	}
	target := targetFromPlacement(placement, roomID, worldID)
	endpoint, err := r.endpoints.ResolveEndpoint(target.TargetID, target.InstallationID)
	if err != nil {
		return "", err
	}
	var nodeScope string
	switch driver := endpoint.Driver.(type) {
	case *Native:
		nodeScope = driver.saveRoot
	case *Agent:
		if source, ok := driver.executor.(interface {
			RuntimeNodeScope(string, string) (string, error)
		}); ok {
			nodeScope, err = source.RuntimeNodeScope(target.TargetID, target.InstallationID)
			if err != nil {
				return "", err
			}
		}
	}
	data, err := json.Marshal([]string{target.TargetID, target.InstallationID, target.TopologyRevision, target.Cluster, target.Shard, nodeScope})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (d *Native) EnsureLuaRuntime(ctx context.Context, target Target, op Operation, request shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
	if err := validateTarget(target); err != nil {
		return shared.RuntimeLuaResult{}, err
	}
	value, err := dstruntime.EnsureNodeWorld(ctx, d.saveRoot, target.Cluster, target.Shard, request, d.control)
	outcome := shared.RuntimeOutcomeConfirmed
	if err != nil {
		outcome = shared.RuntimeOutcomeFailed
	}
	evidenceErr := d.remember(shared.RuntimeOperationEvidence{OperationID: op.ID, Action: string(shared.RuntimeActionLuaEnsure), Completed: true, Outcome: outcome, Message: value.Message, ObservedAt: time.Now().UTC()})
	return value, errors.Join(err, evidenceErr)
}

func (d *Agent) EnsureLuaRuntime(ctx context.Context, target Target, op Operation, value shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
	request := runtimeRequest(target, op, shared.RuntimeActionLuaEnsure)
	request.LuaRuntime = &value
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	if value := result.Result.LuaRuntime; value != nil {
		if value.Deferred {
			return *value, errors.Join(dstruntime.ErrRuntimeRefreshDeferred, err)
		}
		if err == nil && result.Result.Outcome != shared.RuntimeOutcomeConfirmed {
			err = errors.New("Agent 未确认 Runtime 更新成功：" + result.Result.Message)
		}
		return *value, err
	}
	if err != nil {
		return shared.RuntimeLuaResult{}, err
	}
	return shared.RuntimeLuaResult{}, errors.New("Agent 未返回 Runtime 更新结果")
}

func (r *Router) EnsureLuaRuntime(ctx context.Context, roomID, worldID string, request shared.RuntimeLuaRequest) (shared.RuntimeLuaResult, error) {
	ctx, release, err := roomops.TryAcquire(ctx, roomID)
	if err != nil {
		if errors.Is(err, roomops.ErrBusy) {
			return shared.RuntimeLuaResult{Deferred: true, Message: "房间正在执行其他操作，稍后自动重试 Runtime 更新"}, errors.Join(dstruntime.ErrRuntimeRefreshDeferred, err)
		}
		return shared.RuntimeLuaResult{}, err
	}
	defer release()
	driver, target, err := r.DriverTarget(ctx, roomID, worldID)
	if err != nil {
		return shared.RuntimeLuaResult{}, err
	}
	updater, ok := driver.(LuaRuntimeDriver)
	if !ok {
		return shared.RuntimeLuaResult{}, ErrCapabilityMissing
	}
	lease, borrowed := operationlease.BorrowedLease(ctx, roomID)
	if !borrowed || !lease.ExpiresAt.After(time.Now()) {
		lease, err = r.leases.Acquire(ctx, roomID, "runtime.lua.ensure:"+worldID, r.leaseTTL)
		if err != nil {
			return shared.RuntimeLuaResult{}, err
		}
		defer r.leases.Release(lease)
	}
	expires := lease.ExpiresAt.UTC()
	op := Operation{ID: newOperationID(), Key: newOperationID(), LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, LeaseExpiresAt: &expires}
	return updater.EnsureLuaRuntime(ctx, target, op, request)
}
