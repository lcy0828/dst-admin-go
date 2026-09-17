package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPauseIfIdleProtectsRunningAndFutureJobs(t *testing.T) {
	s, _ := newTestJobService(t)
	done := make(chan struct{})
	job, err := s.Submit("test", "", "", nil, func(context.Context, func(TargetResult)) error { <-done; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PauseIfIdle(); !errors.Is(err, ErrServiceBusy) {
		t.Fatalf("paused active runner: %v", err)
	}
	close(done)
	waitForJob(t, s, job.ID, StatusSucceeded)
	var resume func()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		resume, err = s.PauseIfIdle()
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	runner := func(context.Context, func(TargetResult)) error { return nil }
	if _, err := s.Submit("blocked", "", "", nil, runner); !errors.Is(err, ErrServiceBusy) {
		t.Fatalf("admitted while paused: %v", err)
	}
	resume()
	job, err = s.Submit("resumed", "", "", nil, runner)
	if err != nil {
		t.Fatal(err)
	}
	waitForJob(t, s, job.ID, StatusSucceeded)
}
