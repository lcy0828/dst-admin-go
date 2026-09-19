package modcontrol

import (
	"context"
	"errors"
	"os"
	"time"

	"dont/internal/modpublication"
	"dont/internal/runtimedriver"
	"dont/shared"
)

type ActivationRuntime struct {
	router *runtimedriver.Router
}

func NewActivationRuntime(router *runtimedriver.Router) (*ActivationRuntime, error) {
	if router == nil {
		return nil, ErrInvalidRequest
	}
	return &ActivationRuntime{router: router}, nil
}

func (r *ActivationRuntime) Status(ctx context.Context, world modpublication.WorldPlan) (modpublication.ShardRuntimeObservation, error) {
	status, err := r.router.Status(ctx, world.RoomID, world.WorldID)
	return modpublication.ShardRuntimeObservation{State: status.State, SessionExists: status.SessionExists, RuntimeMode: status.RuntimeMode}, err
}

func (r *ActivationRuntime) CaptureLogCursor(ctx context.Context, world modpublication.WorldPlan) (modpublication.LogCursor, error) {
	chunk, err := r.router.ReadLogs(ctx, world.RoomID, world.WorldID, shared.RuntimeLogRequest{Cursor: -1, MaxBytes: 4096, MaxLines: 1})
	if errors.Is(err, os.ErrNotExist) {
		return modpublication.LogCursor{}, nil
	}
	return modpublication.LogCursor{FileID: chunk.FileID, Cursor: chunk.Cursor}, err
}

func (r *ActivationRuntime) Stop(ctx context.Context, world modpublication.WorldPlan, operation modpublication.RuntimeOperation) error {
	return r.execute(ctx, world, operation, shared.ShardActionStop)
}

func (r *ActivationRuntime) Start(ctx context.Context, world modpublication.WorldPlan, operation modpublication.RuntimeOperation) error {
	return r.execute(ctx, world, operation, shared.ShardActionStart)
}

func (r *ActivationRuntime) ReadLogs(ctx context.Context, world modpublication.WorldPlan, cursor modpublication.LogCursor) (modpublication.ShardLogObservation, error) {
	chunk, err := r.router.ReadLogs(ctx, world.RoomID, world.WorldID, shared.RuntimeLogRequest{
		FileID: cursor.FileID, Cursor: cursor.Cursor, MaxBytes: 128 * 1024, MaxLines: 1000,
	})
	if err != nil {
		return modpublication.ShardLogObservation{}, err
	}
	result := modpublication.ShardLogObservation{Cursor: modpublication.LogCursor{FileID: chunk.FileID, Cursor: chunk.Cursor}}
	for _, line := range chunk.Lines {
		result.Lines = append(result.Lines, line.Text)
	}
	return result, nil
}

func (r *ActivationRuntime) execute(ctx context.Context, world modpublication.WorldPlan, operation modpublication.RuntimeOperation, action shared.ShardAction) error {
	driver, target, err := r.router.DriverTarget(ctx, world.RoomID, world.WorldID)
	if err != nil {
		return err
	}
	fence, found := activationFence(operation.Fences, world.RoomID)
	if !found {
		return modpublication.ErrInvalidInput
	}
	expires := fence.ExpiresAt.UTC()
	request := runtimedriver.Operation{
		ID: operation.IdempotencyKey, Key: fence.OperationKey, LeaseID: fence.LeaseID,
		FencingToken: fence.FencingToken, LeaseExpiresAt: &expires,
	}
	if action == shared.ShardActionStart {
		request.RuntimeMode = operation.RuntimeMode
		request.LaunchOptions.SkipUpdateServerMods = true
	}
	_, err = driver.ExecuteShard(ctx, target, request, action, 60*time.Second)
	return err
}

func activationFence(fences []modpublication.Fence, roomID string) (modpublication.Fence, bool) {
	for _, fence := range fences {
		if fence.RoomID == roomID {
			return fence, true
		}
	}
	return modpublication.Fence{}, false
}

var _ modpublication.ActivationRuntime = (*ActivationRuntime)(nil)
