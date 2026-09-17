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
	EndpointTarget(context.Context, string, string) (runtimedriver.RuntimeEndpoint, runtimedriver.Target, error)
}

type localReleaseVersion interface {
	ObserveReleaseInstallation(context.Context) (shared.RuntimeGameVersionResult, error)
	UpdateReleaseInstallation(context.Context, string, bool) (shared.RuntimeGameVersionResult, error)
}

type DriverReleaseRuntime struct {
	router releaseRuntimeRouter
}

func NewDriverReleaseRuntime(router releaseRuntimeRouter) (*DriverReleaseRuntime, error) {
	if router == nil {
		return nil, ErrReleaseInvalid
	}
	return &DriverReleaseRuntime{router: router}, nil
}

type LocalGameVersionDriver struct{ local localReleaseVersion }

func NewLocalGameVersionDriver(local localReleaseVersion) (*LocalGameVersionDriver, error) {
	if local == nil {
		return nil, ErrReleaseInvalid
	}
	return &LocalGameVersionDriver{local: local}, nil
}

func (d *LocalGameVersionDriver) ObserveGameVersion(ctx context.Context, _ runtimedriver.Target) (shared.RuntimeGameVersionResult, error) {
	return d.local.ObserveReleaseInstallation(ctx)
}

func (d *LocalGameVersionDriver) UpdateGameVersion(ctx context.Context, _ runtimedriver.Target, _ runtimedriver.Operation, expected string, cleanCache bool) (shared.RuntimeGameVersionResult, error) {
	return d.local.UpdateReleaseInstallation(ctx, expected, cleanCache)
}

func (r *DriverReleaseRuntime) ObserveInstallation(ctx context.Context, plan ReleaseInstallationPlan) (shared.RuntimeGameVersionResult, error) {
	endpoint, target, err := r.resolveInstallation(ctx, plan)
	if err != nil {
		return shared.RuntimeGameVersionResult{}, err
	}
	if endpoint.GameVersion == nil || !runtimedriver.HasEndpointCapability(endpoint, target, runtimedriver.CapabilityGameUpdate) {
		return shared.RuntimeGameVersionResult{}, runtimedriver.ErrCapabilityMissing
	}
	return endpoint.GameVersion.ObserveGameVersion(ctx, target)
}

func (r *DriverReleaseRuntime) UpdateInstallation(ctx context.Context, plan ReleaseInstallationPlan, operation runtimedriver.Operation, expected string, cleanCache bool) (shared.RuntimeGameVersionResult, error) {
	updateContext, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	endpoint, target, err := r.resolveInstallation(updateContext, plan)
	if err != nil {
		return shared.RuntimeGameVersionResult{}, err
	}
	if endpoint.GameVersion == nil || !runtimedriver.HasEndpointCapability(endpoint, target, runtimedriver.CapabilityGameUpdate) {
		return shared.RuntimeGameVersionResult{}, runtimedriver.ErrCapabilityMissing
	}
	return endpoint.GameVersion.UpdateGameVersion(updateContext, target, operation, expected, cleanCache)
}

func (r *DriverReleaseRuntime) Status(ctx context.Context, shard ReleaseShardPlan) (shared.ShardRuntimeStatus, error) {
	endpoint, target, err := r.resolveShard(ctx, shard)
	if err != nil {
		return shared.ShardRuntimeStatus{}, err
	}
	return endpoint.Driver.Status(ctx, target)
}

func (r *DriverReleaseRuntime) Stop(ctx context.Context, shard ReleaseShardPlan, operation runtimedriver.Operation) error {
	return r.executeShard(ctx, shard, operation, shared.ShardActionStop)
}

func (r *DriverReleaseRuntime) Start(ctx context.Context, shard ReleaseShardPlan, operation runtimedriver.Operation) error {
	return r.executeShard(ctx, shard, operation, shared.ShardActionStart)
}

func (r *DriverReleaseRuntime) CaptureLogCursor(ctx context.Context, shard ReleaseShardPlan) (string, int64, error) {
	endpoint, target, err := r.resolveShard(ctx, shard)
	if err != nil {
		return "", 0, err
	}
	chunk, err := endpoint.Driver.ReadLogs(ctx, target, shared.RuntimeLogRequest{Cursor: -1, MaxBytes: 4096, MaxLines: 1})
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, nil
	}
	return chunk.FileID, chunk.Cursor, err
}

func (r *DriverReleaseRuntime) ReadLogs(ctx context.Context, shard ReleaseShardPlan, fileID string, cursor int64) (shared.RuntimeLogChunk, error) {
	endpoint, target, err := r.resolveShard(ctx, shard)
	if err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	return endpoint.Driver.ReadLogs(ctx, target, shared.RuntimeLogRequest{FileID: fileID, Cursor: cursor, MaxBytes: 128 * 1024, MaxLines: 1000})
}

func (r *DriverReleaseRuntime) executeShard(ctx context.Context, shard ReleaseShardPlan, operation runtimedriver.Operation, action shared.ShardAction) error {
	endpoint, target, err := r.resolveShard(ctx, shard)
	if err != nil {
		return err
	}
	_, err = endpoint.Driver.ExecuteShard(ctx, target, operation, action, 2*time.Minute)
	return err
}

func (r *DriverReleaseRuntime) resolveInstallation(ctx context.Context, plan ReleaseInstallationPlan) (runtimedriver.RuntimeEndpoint, runtimedriver.Target, error) {
	if len(plan.Shards) == 0 {
		return runtimedriver.RuntimeEndpoint{}, runtimedriver.Target{}, ErrReleaseInvalid
	}
	driver, target, err := r.resolveShard(ctx, plan.Shards[0])
	if err != nil {
		return runtimedriver.RuntimeEndpoint{}, runtimedriver.Target{}, err
	}
	if target.TargetID != plan.TargetID || target.InstallationID != plan.InstallationID {
		return runtimedriver.RuntimeEndpoint{}, runtimedriver.Target{}, ErrReleaseTopologyChanged
	}
	return driver, target, nil
}

func (r *DriverReleaseRuntime) resolveShard(ctx context.Context, shard ReleaseShardPlan) (runtimedriver.RuntimeEndpoint, runtimedriver.Target, error) {
	endpoint, target, err := r.router.EndpointTarget(ctx, shard.RoomID, shard.WorldID)
	if err != nil {
		return runtimedriver.RuntimeEndpoint{}, runtimedriver.Target{}, err
	}
	if target.TargetID != shard.TargetID || target.InstallationID != shard.InstallationID || target.TopologyRevision != shard.TopologyRevision || target.RoomID != shard.RoomID || target.WorldID != shard.WorldID {
		return runtimedriver.RuntimeEndpoint{}, runtimedriver.Target{}, ErrReleaseTopologyChanged
	}
	return endpoint, target, nil
}

var _ ReleaseRuntime = (*DriverReleaseRuntime)(nil)
