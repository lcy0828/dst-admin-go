package runtimedriver

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/dstruntime"
	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"
)

func TestAgentLuaMaintenancePreservesDeferredAndRestoredState(t *testing.T) {
	for _, test := range []struct {
		name      string
		value     *shared.RuntimeLuaResult
		outcome   shared.RuntimeOutcome
		err       error
		deferred  bool
		wantError bool
	}{
		{name: "deferred", value: &shared.RuntimeLuaResult{State: "installed", Version: "2.4.10", Deferred: true}, err: errors.New("remote busy"), deferred: true, wantError: true},
		{name: "restored after failure", value: &shared.RuntimeLuaResult{State: "outdated", Version: "2.4.10"}, err: errors.New("activation failed"), wantError: true},
		{name: "failed outcome", value: &shared.RuntimeLuaResult{State: "installed"}, outcome: shared.RuntimeOutcomeFailed, wantError: true},
		{name: "missing result", outcome: shared.RuntimeOutcomeConfirmed, wantError: true},
		{name: "confirmed", value: &shared.RuntimeLuaResult{State: "installed", Version: dstruntime.RuntimeVersion, Active: true}, outcome: shared.RuntimeOutcomeConfirmed},
	} {
		t.Run(test.name, func(t *testing.T) {
			driver, err := NewAgent(failedMigrationExecutor{result: agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{LuaRuntime: test.value, Outcome: test.outcome}}, err: test.err})
			if err != nil {
				t.Fatal(err)
			}
			value, err := driver.EnsureLuaRuntime(context.Background(), Target{TargetID: "agent:node", InstallationID: "default", Cluster: "Cluster_1", Shard: "Master"}, Operation{}, shared.RuntimeLuaRequest{Version: dstruntime.RuntimeVersion})
			if (err != nil) != test.wantError || errors.Is(err, dstruntime.ErrRuntimeRefreshDeferred) != test.deferred {
				t.Fatalf("result=%+v err=%v", value, err)
			}
			if test.value != nil && value != *test.value {
				t.Fatalf("node result lost: %+v", value)
			}
		})
	}
}

type luaScopePlacement struct {
	*cpuLifecyclePlacement
	resolved int
}

func (p *luaScopePlacement) ResolveExecution(context.Context, string, string) (topology.ExecutionPlacement, error) {
	p.resolved++
	return topology.ExecutionPlacement{}, errors.New("scope lookup must not collect inventory")
}

type luaScopeExecutor struct {
	failedMigrationExecutor
	nodeScope string
}

func (e *luaScopeExecutor) RuntimeNodeScope(string, string) (string, error) { return e.nodeScope, nil }

func TestLuaRuntimeScopeTracksAppliedPlacementWithoutCollectingInventory(t *testing.T) {
	placement := &luaScopePlacement{cpuLifecyclePlacement: &cpuLifecyclePlacement{placement: topology.ExecutionPlacement{
		Room: rooms.Room{DirectoryName: "Cluster_1"}, World: rooms.World{DirectoryName: "Master"},
		AppliedTargetID: "agent:node-a", AppliedInstallationID: "primary", Revision: "revision-a",
	}}}
	executor := &luaScopeExecutor{nodeScope: "boot-a"}
	remote, err := NewAgent(executor)
	if err != nil {
		t.Fatal(err)
	}
	local := &Native{saveRoot: "/srv/dst/local"}
	router, err := NewRouter(placement, cpuLifecycleLease{}, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	read := func() string {
		scope, err := router.LuaRuntimeScope("room", "world")
		if err != nil {
			t.Fatal(err)
		}
		return scope
	}
	previous := read()
	for _, change := range []func(){
		func() { placement.placement.AppliedTargetID = "agent:node-b" },
		func() { placement.placement.AppliedInstallationID = "secondary" },
		func() { placement.placement.Revision = "revision-b" },
		func() { executor.nodeScope = "boot-b" },
		func() {
			placement.placement.AppliedTargetID = LocalTargetID
			placement.placement.AppliedInstallationID = "default"
		},
		func() { local.saveRoot = "/srv/dst/new-local" },
	} {
		change()
		current := read()
		if current == previous {
			t.Fatal("changed location retained scope")
		}
		previous = current
	}
	if placement.resolved != 0 {
		t.Fatalf("scope performed %d execution resolutions", placement.resolved)
	}
}

func TestLuaMaintenanceRoomContentionRemainsRetryable(t *testing.T) {
	const roomID = "lua-maintenance-busy-room"
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	router := &Router{}
	value, err := router.EnsureLuaRuntime(ctx, roomID, "world", shared.RuntimeLuaRequest{Version: dstruntime.RuntimeVersion})
	if !errors.Is(err, dstruntime.ErrRuntimeRefreshDeferred) || !value.Deferred || ctx.Err() != nil {
		t.Fatalf("room contention did not promptly defer maintenance: %+v %v (context: %v)", value, err, ctx.Err())
	}
	cancel()
	value, err = router.EnsureLuaRuntime(ctx, roomID, "world", shared.RuntimeLuaRequest{Version: dstruntime.RuntimeVersion})
	if !errors.Is(err, context.Canceled) || value.Deferred {
		t.Fatalf("caller cancellation classified as retryable: %+v %v", value, err)
	}
}
