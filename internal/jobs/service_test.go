package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newTestJobService(t *testing.T) (*Service, *Store) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store, NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

func TestPersistsPartialTargetResultsAndReplayableEvents(t *testing.T) {
	service, _ := newTestJobService(t)
	job, err := service.Submit("room.start", "room-1", "", []TargetSpec{{ID: "master", Name: "Master"}, {ID: "caves", Name: "Caves"}}, func(_ context.Context, report func(TargetResult)) error {
		report(TargetResult{TargetID: "master", Status: StatusSucceeded, Message: "已启动"})
		report(TargetResult{TargetID: "caves", Status: StatusFailed, Error: &Error{Code: "START_FAILED", Message: "端口被占用"}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForJob(t, service, job.ID, StatusFailed)
	if completed.Outcome != OutcomePartial || completed.Progress != 100 || completed.Error == nil || completed.Error.Code != "PARTIAL_FAILURE" {
		t.Fatalf("unexpected completed job: %#v", completed)
	}
	if completed.Targets[0].Status != StatusSucceeded || completed.Targets[1].Status != StatusFailed {
		t.Fatalf("target results lost: %#v", completed.Targets)
	}
	events, err := service.EventsAfter(0, 100)
	if err != nil || len(events) < 5 {
		t.Fatalf("events = %#v, %v", events, err)
	}
	last := events[len(events)-1]
	if last.Type != "job.completed" || last.Data.ID != job.ID || last.Data.Outcome != OutcomePartial {
		t.Fatalf("unexpected last event: %#v", last)
	}
}

func TestCancelMarksUnfinishedTargetsAndJob(t *testing.T) {
	service, _ := newTestJobService(t)
	started := make(chan struct{})
	job, err := service.Submit("room.stop", "room-1", "", []TargetSpec{{ID: "master", Name: "Master"}}, func(ctx context.Context, _ func(TargetResult)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := service.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	completed := waitForJob(t, service, job.ID, StatusCanceled)
	if completed.Outcome != OutcomeNone || completed.Targets[0].Status != StatusCanceled || !completed.CancelRequested {
		t.Fatalf("unexpected canceled job: %#v", completed)
	}
	if _, err := service.Cancel(job.ID); !errors.Is(err, ErrNotCancelable) {
		t.Fatalf("second cancel error = %v", err)
	}
}

func TestRecoversQueuedAndRunningJobsAfterRestart(t *testing.T) {
	_, store := newTestJobService(t)
	job, _, err := store.Create("server.update", "", "", []TargetSpec{{ID: "local", Name: "当前节点"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.MarkRunning(job.ID); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store, NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != StatusFailed || recovered.Error == nil || recovered.Error.Code != "SERVER_RESTARTED" || recovered.Targets[0].Status != StatusFailed {
		t.Fatalf("unexpected recovered job: %#v", recovered)
	}
}

func waitForJob(t *testing.T, service *Service, jobID string, status Status) Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(jobID)
		if err == nil && job.Status == status {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := service.Get(jobID)
	t.Fatalf("job did not reach %s: %#v, %v", status, job, err)
	return Job{}
}
