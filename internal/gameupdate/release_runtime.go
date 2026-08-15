package gameupdate

import (
	"context"
	"errors"
	"os"
	"time"

	"dont/internal/runtimedriver"
	"dont/shared"
)

type releaseRuntimeRouter interface {
	DriverTarget(context.Context, string, string) (runtimedriver.Driver, runtimedriver.Target, error)
}

type localReleaseVersion interface {
	ObserveReleaseInstallation(context.Context) (shared.RuntimeGameVersionResult, error)
	UpdateReleaseInstallation(context.Context, string, bool) (shared.RuntimeGameVersionResult, error)
}

type DriverReleaseRuntime struct {
	router releaseRuntimeRouter
	local  localReleaseVersion
}

func NewDriverReleaseRuntime(router releaseRuntimeRouter, local localReleaseVersion) (*DriverReleaseRuntime, error) {
	if router == nil || local == nil {
		return nil, ErrReleaseInvalid
	}
	return &DriverReleaseRuntime{router: router, local: local}, nil
}

func (r *DriverReleaseRuntime) ObserveInstallation(ctx context.Context, plan ReleaseInstallationPlan) (shared.RuntimeGameVersionResult, error) {
	if plan.TargetID == "local" {
		return r.local.ObserveReleaseInstallation(ctx)
	}
	driver, target, err := r.resolveInstallation(ctx, plan)
	if err != nil {
		return shared.RuntimeGameVersionResult{}, err
	}
	versionDriver, ok := driver.(runtimedriver.GameVersionDriver)
	if !ok || !runtimedriver.HasCapability(driver, runtimedriver.CapabilityGameUpdate) {
		return shared.RuntimeGameVersionResult{}, runtimedriver.ErrCapabilityMissing
	}
	return versionDriver.ObserveGameVersion(ctx, target)
}

func (r *DriverReleaseRuntime) UpdateInstallation(ctx context.Context, plan ReleaseInstallationPlan, operation runtimedriver.Operation, expected string, cleanCache bool) (shared.RuntimeGameVersionResult, error) {
	updateContext, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if plan.TargetID == "local" {
		return r.local.UpdateReleaseInstallation(updateContext, expected, cleanCache)
	}
	driver, target, err := r.resolveInstallation(updateContext, plan)
	if err != nil {
		return shared.RuntimeGameVersionResult{}, err
	}
	versionDriver, ok := driver.(runtimedriver.GameVersionDriver)
	if !ok || !runtimedriver.HasCapability(driver, runtimedriver.CapabilityGameUpdate) {
		return shared.RuntimeGameVersionResult{}, runtimedriver.ErrCapabilityMissing
	}
	return versionDriver.UpdateGameVersion(updateContext, target, operation, expected, cleanCache)
}

func (r *DriverReleaseRuntime) Status(ctx context.Context, shard ReleaseShardPlan) (shared.ShardRuntimeStatus, error) {
	driver, target, err := r.resolveShard(ctx, shard)
	if err != nil {
		return shared.ShardRuntimeStatus{}, err
	}
	return driver.Status(ctx, target)
}

func (r *DriverReleaseRuntime) Stop(ctx context.Context, shard ReleaseShardPlan, operation runtimedriver.Operation) error {
	return r.executeShard(ctx, shard, operation, shared.ShardActionStop)
}

func (r *DriverReleaseRuntime) Start(ctx context.Context, shard ReleaseShardPlan, operation runtimedriver.Operation) error {
	return r.executeShard(ctx, shard, operation, shared.ShardActionStart)
}

func (r *DriverReleaseRuntime) CaptureLogCursor(ctx context.Context, shard ReleaseShardPlan) (string, int64, error) {
	driver, target, err := r.resolveShard(ctx, shard)
	if err != nil {
		return "", 0, err
	}
	chunk, err := driver.ReadLogs(ctx, target, shared.RuntimeLogRequest{Cursor: -1, MaxBytes: 4096, MaxLines: 1})
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, nil
	}
	return chunk.FileID, chunk.Cursor, err
}

func (r *DriverReleaseRuntime) ReadLogs(ctx context.Context, shard ReleaseShardPlan, fileID string, cursor int64) (shared.RuntimeLogChunk, error) {
	driver, target, err := r.resolveShard(ctx, shard)
	if err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	return driver.ReadLogs(ctx, target, shared.RuntimeLogRequest{FileID: fileID, Cursor: cursor, MaxBytes: 128 * 1024, MaxLines: 1000})
}

func (r *DriverReleaseRuntime) executeShard(ctx context.Context, shard ReleaseShardPlan, operation runtimedriver.Operation, action shared.ShardAction) error {
	driver, target, err := r.resolveShard(ctx, shard)
	if err != nil {
		return err
	}
	if target.TargetID == "local" {
		operation.Key, operation.LeaseID, operation.FencingToken, operation.LeaseExpiresAt = "", "", 0, nil
	}
	_, err = driver.ExecuteShard(ctx, target, operation, action, 2*time.Minute)
	return err
}

func (r *DriverReleaseRuntime) resolveInstallation(ctx context.Context, plan ReleaseInstallationPlan) (runtimedriver.Driver, runtimedriver.Target, error) {
	if len(plan.Shards) == 0 {
		return nil, runtimedriver.Target{}, ErrReleaseInvalid
	}
	driver, target, err := r.resolveShard(ctx, plan.Shards[0])
	if err != nil {
		return nil, runtimedriver.Target{}, err
	}
	if target.TargetID != plan.TargetID || target.InstallationID != plan.InstallationID {
		return nil, runtimedriver.Target{}, ErrReleaseTopologyChanged
	}
	return driver, target, nil
}

func (r *DriverReleaseRuntime) resolveShard(ctx context.Context, shard ReleaseShardPlan) (runtimedriver.Driver, runtimedriver.Target, error) {
	driver, target, err := r.router.DriverTarget(ctx, shard.RoomID, shard.WorldID)
	if err != nil {
		return nil, runtimedriver.Target{}, err
	}
	if target.TargetID != shard.TargetID || target.InstallationID != shard.InstallationID || target.TopologyRevision != shard.TopologyRevision || target.RoomID != shard.RoomID || target.WorldID != shard.WorldID {
		return nil, runtimedriver.Target{}, ErrReleaseTopologyChanged
	}
	return driver, target, nil
}

var _ ReleaseRuntime = (*DriverReleaseRuntime)(nil)
