package modupdates

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"dont/internal/jobs"
	"dont/internal/operationprogress"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/shared"
)

type restartWorldCatalog []rooms.World

func (w restartWorldCatalog) Worlds(string) ([]rooms.World, error) { return w, nil }

type restartWorldOperations struct {
	states    map[string]shards.RuntimeStatus
	statusErr error
	selected  []string
	failWorld string
	progress  func(context.Context)
}

func (o *restartWorldOperations) StatusFor(_ context.Context, _, id string) (shards.RuntimeStatus, error) {
	return o.states[id], o.statusErr
}
func (o *restartWorldOperations) Plan(action shards.Action, _ string, ids []string) ([]jobs.TargetSpec, jobs.Runner, error) {
	if action != shards.ActionRestart || len(ids) == 0 {
		return nil, nil, errors.New("invalid restart selection")
	}
	o.selected = append([]string(nil), ids...)
	targets := make([]jobs.TargetSpec, 0, len(ids))
	for _, id := range ids {
		targets = append(targets, jobs.TargetSpec{ID: id, Name: id})
	}
	return targets, func(ctx context.Context, report func(jobs.TargetResult)) error {
		if o.progress != nil {
			o.progress(ctx)
		}
		for _, id := range ids {
			result := jobs.TargetResult{TargetID: id, Status: jobs.StatusSucceeded}
			if id == o.failWorld {
				result.Status, result.Error = jobs.StatusFailed, &jobs.Error{Code: "START_FAILED", Message: "startup failed"}
			}
			report(result)
		}
		return nil
	}, nil
}

func TestRestartSelectsOnlyRunningWorldsAndReportsFailure(t *testing.T) {
	for _, scenario := range []string{"both", "master-only", "stopped", "unknown", "read-error", "partial-failure"} {
		t.Run(scenario, func(t *testing.T) {
			worlds := restartWorldCatalog{{ID: "Master", Name: "Master"}, {ID: "Caves", Name: "Caves"}}
			operations := &restartWorldOperations{states: map[string]shards.RuntimeStatus{
				"Master": {State: shards.RuntimeRunning, SessionExists: true},
				"Caves":  {State: shards.RuntimeRunning, SessionExists: true},
			}}
			want := []string{"Master", "Caves"}
			switch scenario {
			case "master-only":
				operations.states["Caves"] = shards.RuntimeStatus{State: shards.RuntimeStopped}
				want = []string{"Master"}
			case "stopped":
				operations.states["Master"], operations.states["Caves"] = shards.RuntimeStatus{State: shards.RuntimeStopped}, shards.RuntimeStatus{State: shards.RuntimeStopped}
				want = nil
			case "unknown":
				operations.states["Caves"] = shards.RuntimeStatus{State: shards.RuntimeUnknown}
				want = nil
			case "read-error":
				operations.statusErr, want = errors.New("Agent offline"), nil
			case "partial-failure":
				operations.failWorld = "Caves"
			}
			var updates []operationprogress.Update
			ctx := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) { updates = append(updates, update) })
			err := NewWorldRestarter(worlds, operations).RestartRunningWorlds(ctx, "room", "job")
			wantError := scenario == "unknown" || scenario == "read-error" || scenario == "partial-failure"
			if (err != nil) != wantError || !reflect.DeepEqual(operations.selected, want) {
				t.Fatalf("selected=%v want=%v err=%v", operations.selected, want, err)
			}
			if scenario == "partial-failure" && (!strings.Contains(err.Error(), "Caves") || !strings.Contains(err.Error(), "startup failed")) {
				t.Fatalf("world failure omitted: %v", err)
			}
			if len(want) > 0 {
				wantPercent := 99
				if scenario == "partial-failure" {
					wantPercent = 89
				}
				if len(updates) != len(want)+1 || updates[0].Percent != 80 || updates[len(updates)-1].Percent != wantPercent {
					t.Fatalf("missing restart progress: %+v", updates)
				}
				last := updates[len(updates)-1].Message
				if scenario == "partial-failure" {
					items := updates[len(updates)-1].Worlds
					if !strings.Contains(last, "已就绪 1/2") || items[0].Stage != "ready" || items[1].Stage != "failed" || items[1].Message != "startup failed" {
						t.Fatalf("partial startup presented as success: %+v", updates[len(updates)-1])
					}
				}
				if scenario == "master-only" && (strings.Contains(last, "Caves") || !strings.Contains(last, "已就绪 1/1")) {
					t.Fatalf("stopped world included in progress: %s", last)
				}
			} else if len(updates) != 0 {
				t.Fatalf("unexpected startup progress: %+v", updates)
			}
		})
	}
}

func TestRestartRetainsLogStagesAndDoesNotTurnUnconfirmedStartupIntoReady(t *testing.T) {
	worlds := restartWorldCatalog{{ID: "Master", Name: "Master", IsMaster: true}, {ID: "Caves", Name: "Caves"}}
	operations := &restartWorldOperations{states: map[string]shards.RuntimeStatus{
		"Master": {State: shards.RuntimeRunning}, "Caves": {State: shards.RuntimeRunning},
	}}
	operations.progress = func(ctx context.Context) {
		for _, item := range []shared.WorldOperationProgress{
			{WorldID: "Master", Name: "Master", IsMaster: true, Stage: "loading_world", Percent: 80},
			{WorldID: "Caves", Name: "Caves", Stage: "loading_mods", Percent: 55},
			{WorldID: "Caves", Name: "Caves", Stage: "unconfirmed", Percent: 55, Message: "still starting"},
		} {
			operationprogress.Report(ctx, operationprogress.Update{Worlds: []shared.WorldOperationProgress{item}})
		}
	}
	var updates []operationprogress.Update
	ctx := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) { updates = append(updates, update) })
	if err := NewWorldRestarter(worlds, operations).RestartRunningWorlds(ctx, "room", "job"); err != nil {
		t.Fatal(err)
	}
	if updates[2].Worlds[0].Stage != "loading_world" || updates[2].Worlds[1].Stage != "loading_mods" {
		t.Fatalf("lost independent stage: %+v", updates[2])
	}
	last := updates[len(updates)-1]
	if last.Worlds[0].Stage != "ready" || !last.Worlds[0].IsMaster || last.Worlds[1].Stage != "unconfirmed" || last.Worlds[1].Percent != 55 || last.Percent >= 99 {
		t.Fatalf("unconfirmed world presented as ready: %+v", last)
	}
	if updates[0].Worlds[0].Stage != "queued" {
		t.Fatal("later progress mutated a previous event")
	}
}
