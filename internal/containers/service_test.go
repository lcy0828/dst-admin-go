package containers

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/jobs"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func TestContainerLifecycleUsesJobsAndConfirmation(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "container_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(NewMemoryTransport(), jobService)
	if err != nil {
		t.Fatal(err)
	}
	const cavesID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	job, err := service.Run(cavesID, ActionStart, ActionInput{})
	if err != nil {
		t.Fatal(err)
	}
	waitContainerJob(t, jobService, job.ID, jobs.StatusSucceeded)
	if _, err := service.Run(cavesID, ActionStart, ActionInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second start error=%v", err)
	}
	job, err = service.Run(cavesID, ActionStop, ActionInput{})
	if err != nil {
		t.Fatal(err)
	}
	waitContainerJob(t, jobService, job.ID, jobs.StatusSucceeded)
	if _, err := service.Run(cavesID, ActionRemove, ActionInput{Confirmation: "wrong"}); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("remove confirmation error=%v", err)
	}
	job, err = service.Run(cavesID, ActionRemove, ActionInput{Confirmation: "dstserver-caves"})
	if err != nil {
		t.Fatal(err)
	}
	waitContainerJob(t, jobService, job.ID, jobs.StatusSucceeded)
	if result := service.List(context.Background()); result.Total != 1 || result.Items[0].Name != "dstserver-surface" {
		t.Fatalf("containers=%#v", result)
	}
}

func waitContainerJob(t *testing.T, service *jobs.Service, id string, status jobs.Status) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(id)
		if err == nil && job.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("container job %s did not become %s", id, status)
}
