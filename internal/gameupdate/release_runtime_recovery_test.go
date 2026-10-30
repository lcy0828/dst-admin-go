package gameupdate

import (
	"context"
	"errors"
	"testing"

	"dont/internal/installationmaintenance"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/shared"
)

type unavailableModeRuntime struct {
	*coordinatorRuntime
	startErr  error
	starts    []runtimedriver.Operation
	integrity string
}

func (r *unavailableModeRuntime) Start(ctx context.Context, shard ReleaseShardPlan, op runtimedriver.Operation) error {
	r.starts = append(r.starts, op)
	if op.RuntimeMode != shared.RuntimePerformanceModeGame {
		return r.startErr
	}
	return r.coordinatorRuntime.Start(ctx, shard, op)
}
func (r *unavailableModeRuntime) ObserveInstallation(ctx context.Context, target ReleaseInstallationPlan) (shared.RuntimeGameVersionResult, error) {
	value, err := r.coordinatorRuntime.ObserveInstallation(ctx, target)
	value.IntegrityError = r.integrity
	return value, err
}

func runtimeRecoveryFixture(t *testing.T, startErr error) (releaseCoordinatorFixture, *unavailableModeRuntime, ReleasePlan) {
	t.Helper()
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	r := &unavailableModeRuntime{coordinatorRuntime: f.runtime, startErr: startErr}
	f.coordinator.runtime = r
	f.coordinator.planner.runtime = r
	for key, state := range f.runtime.states {
		state.RuntimeMode = shared.RuntimePerformanceModeLuaJIT
		f.runtime.states[key] = state
	}
	plan := selectedFixturePlan(t, f)
	return f, r, plan
}

func TestReleaseFallsBackOnlyAfterDefiniteModeRejection(t *testing.T) {
	for _, unavailable := range []error{shards.ErrRuntimeModeUnavailable, errors.New("RUNTIME_MODE_UNAVAILABLE: missing accelerator")} {
		f, r, plan := runtimeRecoveryFixture(t, unavailable)
		value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "fallback", Plan: plan})
		if err != nil || value.Stage != ReleaseStageSucceeded {
			t.Fatalf("stage=%s err=%v", value.Stage, err)
		}
		if len(r.starts) != 4 {
			t.Fatalf("starts=%v", r.starts)
		}
		for i := 0; i < len(r.starts); i += 2 {
			if r.starts[i].ID == r.starts[i+1].ID || r.starts[i+1].RuntimeMode != shared.RuntimePerformanceModeGame {
				t.Fatal("fallback reused original operation")
			}
		}
		persisted, err := f.coordinator.Get(value.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, result := range persisted.Shards {
			if result.WarningCode != "RUNTIME_FALLBACK_GAME" || result.RuntimeMode != shared.RuntimePerformanceModeGame {
				t.Fatalf("missing warning: %+v", result)
			}
		}
		for _, shard := range persisted.Plan.Installations[0].Shards {
			if shard.RuntimeMode != shared.RuntimePerformanceModeLuaJIT {
				t.Fatal("changed original preference")
			}
		}
	}
}

func TestReleaseDoesNotFallBackOnUnknownStartFailure(t *testing.T) {
	f, r, plan := runtimeRecoveryFixture(t, errors.New("connection lost"))
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "uncertain-start", Plan: plan})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) || value.Stage != ReleaseStageRecoveryRequired || len(r.starts) != 2 {
		t.Fatalf("stage=%s starts=%v err=%v", value.Stage, r.starts, err)
	}
	for _, result := range value.Shards {
		if result.WarningCode != "" {
			t.Fatal("unexpected fallback")
		}
	}
}

func TestReleaseRetriesConfirmationOfFallbackWithoutRestartOrPlanMutation(t *testing.T) {
	f, r, plan := runtimeRecoveryFixture(t, shards.ErrRuntimeModeUnavailable)
	plan.Policy.LoadConfirmation = ReleaseLoadConfirmationLogs
	plan.PlanHash, _ = calculateReleasePlanHash(plan)
	f.runtime.logErrors["room-a\x00Caves"] = errors.New("temporary log failure")
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "fallback-confirmation", Plan: plan})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) {
		t.Fatal(err)
	}
	starts := len(r.starts)
	delete(f.runtime.logErrors, "room-a\x00Caves")
	value, err = f.coordinator.Retry(context.Background(), value.ID)
	if err != nil || value.Stage != ReleaseStageSucceeded || len(r.starts) != starts {
		t.Fatalf("stage=%s starts=%d err=%v", value.Stage, len(r.starts), err)
	}
	if value.Plan.Installations[0].Shards[0].RuntimeMode != shared.RuntimePerformanceModeLuaJIT {
		t.Fatal("mutated confirmed plan")
	}
}

func TestFinishRecoveryVerifiesAndReleasesMaintenanceWithoutProcessMutations(t *testing.T) {
	f, _, plan := runtimeRecoveryFixture(t, errors.New("start failed"))
	value, _ := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "finish-recovery", Plan: plan})
	before := len(f.runtime.mutationEvents())
	gate := installationmaintenance.New(f.coordinator.store.CheckMaintenance)
	f.coordinator.ConfigureMaintenance(gate)
	if _, _, err := gate.Acquire(context.Background(), releaseResources(plan), ""); !errors.Is(err, installationmaintenance.ErrBusy) {
		t.Fatal(err)
	}
	value, err := f.coordinator.RetryConfirmed(context.Background(), value.ID, ReleaseRetryRequest{FinishWithoutRestart: true, PlanHash: plan.PlanHash, Confirmation: plan.PlanHash}, "finish-job")
	if err != nil || value.Stage != ReleaseStageAbandoned {
		t.Fatalf("stage=%s err=%v", value.Stage, err)
	}
	if len(f.runtime.mutationEvents()) != before {
		t.Fatal("finish mutated game")
	}
	if err := f.coordinator.store.Migrate(); err != nil {
		t.Fatal(err)
	}
	_, release, err := gate.Acquire(context.Background(), releaseResources(plan), "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := f.coordinator.Retry(context.Background(), value.ID); !errors.Is(err, ErrReleaseConflict) {
		t.Fatal("abandoned release retryable", err)
	}
}

func TestFinishRecoveryRejectsUnsafeOrUnconfirmedExit(t *testing.T) {
	for _, scenario := range []string{"unconfirmed", "corrupt", "unknown-process", "unknown-write", "failed-write"} {
		t.Run(scenario, func(t *testing.T) {
			f, r, plan := runtimeRecoveryFixture(t, errors.New("start failed"))
			value, _ := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "finish-blocked", Plan: plan})
			request := ReleaseRetryRequest{FinishWithoutRestart: true, PlanHash: plan.PlanHash, Confirmation: plan.PlanHash}
			switch scenario {
			case "unconfirmed":
				request.PlanHash, request.Confirmation = "", ""
			case "corrupt":
				r.integrity = "StateFlags=1158"
			case "unknown-process":
				f.runtime.states["room-a\x00Master"] = shared.ShardRuntimeStatus{State: "stopped", SessionExists: true}
			case "unknown-write", "failed-write":
				value.Installations[0].Stage = ReleaseStageFailed
				value.Installations[0].WriteState = scenario
				if _, err := f.coordinator.store.Save(value); err != nil {
					t.Fatal(err)
				}
			}
			value, err := f.coordinator.RetryConfirmed(context.Background(), value.ID, request, "finish-job")
			if err == nil || value.Stage == ReleaseStageAbandoned {
				t.Fatal("unsafe finish", value.Stage, err)
			}
			if err := f.coordinator.store.CheckMaintenance(context.Background(), releaseResources(plan), ""); !errors.Is(err, installationmaintenance.ErrBusy) {
				t.Fatal("lost maintenance", err)
			}
		})
	}
}
