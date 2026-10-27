package gameupdate

import (
	"context"
	"testing"

	"dont/internal/runtimedriver"
)

type releaseProgressRuntime struct {
	*coordinatorRuntime
	beforeStop  func(ReleaseShardPlan)
	beforeStart func(ReleaseShardPlan)
}

func (r *releaseProgressRuntime) Stop(ctx context.Context, shard ReleaseShardPlan, operation runtimedriver.Operation) error {
	r.beforeStop(shard)
	return r.coordinatorRuntime.Stop(ctx, shard, operation)
}

func (r *releaseProgressRuntime) Start(ctx context.Context, shard ReleaseShardPlan, operation runtimedriver.Operation) error {
	r.beforeStart(shard)
	return r.coordinatorRuntime.Start(ctx, shard, operation)
}

func TestReleaseProgressPersistsInFlightLifecycleStages(t *testing.T) {
	fixture := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationLogs)
	if err := fixture.coordinator.ConfigureMutationObserver(&releaseMutationObserver{}); err != nil {
		t.Fatal(err)
	}
	const id = "release-progress"
	stops, starts := 0, 0
	check := func(shard ReleaseShardPlan, stage ReleaseStage) {
		t.Helper()
		value, err := fixture.coordinator.store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		result := releaseShardResult(&value, shard.RoomID, shard.WorldID)
		if value.Stage != stage || result == nil || result.Stage != stage {
			t.Fatalf("in-flight stage=%s shard=%#v, want %s", value.Stage, result, stage)
		}
		if result.LoadConfirmedAt != nil {
			t.Fatal("a dispatched lifecycle action was presented as a confirmed world load")
		}
	}
	fixture.coordinator.runtime = &releaseProgressRuntime{
		coordinatorRuntime: fixture.runtime,
		beforeStop: func(shard ReleaseShardPlan) {
			stops++
			check(shard, ReleaseStageStopping)
		},
		beforeStart: func(shard ReleaseShardPlan) {
			starts++
			check(shard, ReleaseStageRestarting)
		},
	}
	value, err := fixture.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: id, Plan: fixture.plan})
	if err != nil || value.Stage != ReleaseStageSucceeded {
		t.Fatalf("stage=%s error=%v", value.Stage, err)
	}
	if stops != 3 || starts != 3 {
		t.Fatalf("stops=%d starts=%d; originally stopped worlds must stay stopped", stops, starts)
	}
	for _, shard := range value.Shards {
		if shard.WasRunning && (shard.LoadConfirmedAt == nil || shard.RuntimeState != "running") {
			t.Fatalf("restored world missing completion evidence: %#v", shard)
		}
	}
}
