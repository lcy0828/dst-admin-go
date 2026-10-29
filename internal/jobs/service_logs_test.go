package jobs

import (
	"context"
	"io"
	"testing"

	"dont/internal/servicelogs"
	"dont/shared"
)

func TestJobFailureIsQueryableByJobIDAfterRestart(t *testing.T) {
	root := t.TempDir()
	closeLogs := servicelogs.Start(servicelogs.Config{Root: root, Component: "controller", Console: io.Discard}, false)
	service, _ := newTestJobService(t)
	done := make(chan Job, 1)
	service.AddCompletionListener(func(j Job) { done <- j })
	job, err := service.Submit("backup.create", "room-1", "", []TargetSpec{{ID: "caves", Name: "Caves"}}, func(_ context.Context, report func(TargetResult)) error {
		report(TargetResult{TargetID: "caves", Status: StatusFailed, Error: &Error{Code: "SNAPSHOT_FAILED", Message: `barrier failed api_key="private-key"`}})
		return nil
	})
	if err != nil {
		closeLogs()
		t.Fatal(err)
	}
	<-done
	closeLogs()
	page, err := servicelogs.Query(context.Background(), root, shared.ServiceLogQuery{JobID: job.ID})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range page.Items {
		seen[entry.Code] = true
		if entry.JobID != job.ID || entry.RoomID != "room-1" {
			t.Fatal("lost association", entry)
		}
		if entry.Message == `barrier failed api_key="private-key"` {
			t.Fatal("secret leaked")
		}
	}
	for _, code := range []string{"JOB_QUEUED", "JOB_RUNNING", "SNAPSHOT_FAILED", "JOB_COMPLETED"} {
		if !seen[code] {
			t.Fatal("missing event", code)
		}
	}
}
