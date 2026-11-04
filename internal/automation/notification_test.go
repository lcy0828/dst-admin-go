package automation

import (
	"context"
	"testing"

	"dont/internal/jobs"
	"dont/internal/shards"
)

type notificationPlanner struct{ missing bool }

func (p notificationPlanner) Plan(shards.Action, string, []string) ([]jobs.TargetSpec, jobs.Runner, error) {
	return []jobs.TargetSpec{{ID: "master"}, {ID: "caves"}}, func(_ context.Context, report func(jobs.TargetResult)) error {
		report(jobs.TargetResult{TargetID: "master", Status: jobs.StatusRunning})
		report(jobs.TargetResult{TargetID: "master", Status: jobs.StatusSucceeded})
		if !p.missing {
			report(jobs.TargetResult{TargetID: "caves", Status: jobs.StatusFailed})
		}
		return nil
	}, nil
}

func TestAutomationNoticesRetainPerWorldPartialAndMissingResults(t *testing.T) {
	for _, missing := range []bool{false, true} {
		e := &DomainExecutor{roomActions: notificationPlanner{missing: missing}}
		var observed []jobs.Job
		e.ConfigureObserver(func(job jobs.Job) { observed = append(observed, job) })
		_, err := e.Execute(context.Background(), Task{Action: ActionRoomRestart, RoomID: "example"}, "job")
		if err == nil || len(observed) != 2 {
			t.Fatal(err, observed)
		}
		final := observed[1]
		if final.Status != jobs.StatusFailed || final.Outcome != jobs.OutcomePartial || len(final.Targets) != 2 || final.Targets[0].Status != jobs.StatusSucceeded || final.Targets[1].Status != jobs.StatusFailed {
			t.Fatalf("partial result lost: %+v", final)
		}
		if missing && (final.Targets[1].Error == nil || final.Targets[1].Error.Code != "RESULT_UNCONFIRMED") {
			t.Fatal("missing result was presented as an observed failure")
		}
	}
}
