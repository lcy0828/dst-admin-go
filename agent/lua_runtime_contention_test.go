package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/roomops"
	"dont/internal/shards"
	"dont/shared"
)

func TestAgentRuntimeMaintenanceDefersOuterLockAndCanRetry(t *testing.T) {
	a, installation := newShardOperationAgent(t, &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}})
	request := runtimeOperationRequest(shared.RuntimeActionLuaEnsure)
	request.LuaRuntime = &shared.RuntimeLuaRequest{Version: dstruntime.RuntimeVersion, InstallIfMissing: true}
	_, release, err := roomops.Acquire(context.Background(), request.InstallationID+"\x00"+request.Cluster)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	value, err := a.executeRuntimeOperationContext(ctx, string(request.Action), &request, 30)
	if !errors.Is(err, dstruntime.ErrRuntimeRefreshDeferred) || value.LuaRuntime == nil || !value.LuaRuntime.Deferred || ctx.Err() != nil {
		t.Fatalf("Agent did not promptly defer the occupied room: %+v %v", value, err)
	}
	if value.OperationID != request.OperationID || value.Action != request.Action || value.InstallationID != request.InstallationID || value.Cluster != request.Cluster || value.Shard != request.Shard {
		t.Fatalf("deferred reply lost request identity: %+v", value)
	}
	path := filepath.Join(installation.SavePath, request.Cluster, request.Shard, "customcommands.lua")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("busy operation published Runtime files: %v", err)
	}
	cancel()
	value, err = a.executeRuntimeOperationContext(ctx, string(request.Action), &request, 30)
	if !errors.Is(err, context.Canceled) || value.LuaRuntime != nil {
		t.Fatalf("canceled request was classified as a deferred update: %+v %v", value, err)
	}
	release()
	// The deferred attempt did not begin or journal a mutation, so the same
	// request can proceed after contention without replaying a cached failure.
	value, err = a.executeRuntimeOperation(string(request.Action), &request, 30)
	if err != nil || value.Idempotent || value.LuaRuntime == nil || !value.LuaRuntime.Changed {
		t.Fatalf("retry after releasing room failed: %+v %v", value, err)
	}
}
