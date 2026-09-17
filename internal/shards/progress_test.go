package shards

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/operationprogress"
	"dont/internal/rooms"
	"dont/shared"
)

func TestStartupProgressOnlyEmitsAdvancingObservedStages(t *testing.T) {
	var stages []string
	ctx := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) { stages = append(stages, update.Stage) })
	report := StartupProgressReporter(ctx)
	for _, stage := range []string{"", "initializing", "initializing", "loading_mods", "loading_world", "loading_mods", "connecting", "connecting"} {
		report(RuntimeStatus{State: RuntimeStarting, StartupStage: stage})
	}
	report(RuntimeStatus{State: RuntimeFailed, StartupStage: "connecting"})
	want := []string{"world.start.initializing", "world.start.loading_mods", "world.start.loading_world", "world.start.connecting"}
	if !reflect.DeepEqual(stages, want) {
		t.Fatalf("stages=%v want=%v", stages, want)
	}
}

func TestWorldProgressRequiresReadinessAndPreservesFailureStage(t *testing.T) {
	for _, outcome := range []string{"ready", "unconfirmed", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			var updates []operationprogress.Update
			ctx := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) { updates = append(updates, update) })
			o := &Operations{}
			o.executeWorldWithProgress(ctx, ActionStart, func(ctx context.Context) worldExecutionResult {
				StartupProgressReporter(ctx)(RuntimeStatus{State: RuntimeStarting, StartupStage: "loading_world"})
				status := shared.ShardRuntimeStatus{State: "starting"}
				if outcome == "ready" {
					status.State = "running"
				}
				reportStartupResult(ctx, ActionStart, status)
				if outcome == "failed" {
					return worldExecutionResult{err: errors.New("port in use")}
				}
				return worldExecutionResult{}
			}, shared.WorldOperationProgress{WorldID: "master", Name: "Master", IsMaster: true})
			last := updates[len(updates)-1].Worlds[0]
			wantPercent := 80
			if outcome == "ready" {
				wantPercent = 100
			}
			if last.Stage != outcome || last.Percent != wantPercent || last.WorldID != "master" || !last.IsMaster {
				t.Fatalf("unexpected final world progress: %+v", last)
			}
			if outcome == "failed" && last.Message != "port in use" {
				t.Fatalf("lost failure: %+v", last)
			}
		})
	}
}

func TestRestartReportsReadyWorldBeforeOtherWorldFinishes(t *testing.T) {
	control := &stagedStartControl{
		masterStarted: make(chan struct{}), dependentStarted: make(chan struct{}), masterReady: make(chan struct{}),
		states: map[string]RuntimeStatus{"Master": {State: RuntimeRunning, SessionExists: true}, "Caves": {State: RuntimeRunning, SessionExists: true}},
	}
	defer close(control.masterReady)
	o := NewOperations(nil, control)
	o.pollInterval, o.startTimeout = time.Millisecond, time.Second
	worlds := []rooms.World{{ID: "master", DirectoryName: "Master", Name: "Master", IsMaster: true}, {ID: "caves", DirectoryName: "Caves", Name: "Caves"}}
	results := make(chan jobs.TargetResult, 2)
	progress := make(chan operationprogress.Update, 20)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = operationprogress.WithReporter(ctx, func(update operationprogress.Update) { progress <- update })
	done := make(chan struct{})
	go func() {
		o.executeRestartPlan(ctx, rooms.Room{DirectoryName: "room"}, worlds, nil, nil, "", func(result jobs.TargetResult) { results <- result })
		close(done)
	}()
	select {
	case result := <-results:
		if result.TargetID != "caves" || result.Status != jobs.StatusSucceeded {
			t.Fatalf("first result=%+v", result)
		}
	case <-ctx.Done():
		t.Fatal("ready Caves result was held until Master finished")
	}
	select {
	case <-done:
		t.Fatal("restart finished before Master was ready")
	default:
	}
	cancel()
	<-done
	found := false
	close(progress)
	for update := range progress {
		if update.Worlds[0].WorldID == "caves" && update.Worlds[0].Stage == "ready" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing independent Caves readiness progress")
	}
}
