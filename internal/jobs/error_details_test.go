package jobs

import (
	"context"
	"testing"
)

func TestBackupFailureDetailsSurvivePersistenceEventsAndCompletion(t *testing.T) {
	service, store := newTestJobService(t)
	completed := make(chan Job, 1)
	service.AddCompletionListener(func(job Job) { completed <- job })
	job, err := service.Submit("backup-set.create", "room", "", []TargetSpec{{ID: "room", Name: "Room"}}, func(_ context.Context, report func(TargetResult)) error {
		report(TargetResult{TargetID: "room", Status: StatusFailed, Error: &Error{Code: "SAVE_IN_PROGRESS", Message: "save collision", Details: map[string]interface{}{"backupSetId": "backup-1", "barrierAttempts": 3, "artifactState": "not_generated", "barrierReleaseState": "released"}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result := <-completed
	loaded, err := store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []Job{result, loaded, events[len(events)-1].Data} {
		if value.Error == nil || value.Error.Details["artifactState"] != "not_generated" || value.Targets[0].Error.Details["barrierReleaseState"] != "released" {
			t.Fatalf("details lost: %+v", value)
		}
	}
}
