package consoledispatch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDispatcherSerializesOneShardAndAllowsDifferentShards(t *testing.T) {
	dispatcher := New()
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	run := func(key string) <-chan error {
		result := make(chan error, 1)
		go func() {
			result <- dispatcher.Dispatch(context.Background(), key, Request{Execute: func(context.Context) error {
				current := active.Add(1)
				for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
				}
				started <- struct{}{}
				<-release
				active.Add(-1)
				return nil
			}})
		}()
		return result
	}
	first := run("room/master")
	<-started
	second := run("room/master")
	select {
	case <-started:
		t.Fatal("same shard executed concurrently")
	case <-time.After(30 * time.Millisecond):
	}
	third := run("room/caves")
	<-started
	if maximum.Load() != 2 {
		t.Fatalf("maximum active=%d", maximum.Load())
	}
	close(release)
	for _, result := range []<-chan error{first, second, third} {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
}

func TestDispatcherCoalescesBackgroundWork(t *testing.T) {
	dispatcher := New()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	request := Request{Class: ClassBackground, CoalesceKey: "telemetry", Execute: func(context.Context) error {
		calls.Add(1)
		close(started)
		<-release
		return errors.New("probe failed")
	}}
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	wait.Add(1)
	go func() {
		defer wait.Done()
		errorsSeen <- dispatcher.Dispatch(context.Background(), "room/master", request)
	}()
	<-started
	wait.Add(1)
	go func() {
		defer wait.Done()
		errorsSeen <- dispatcher.Dispatch(context.Background(), "room/master", request)
	}()
	lane := dispatcher.lane("room/master")
	for deadline := time.Now().Add(time.Second); ; {
		lane.mu.Lock()
		joined := lane.shared["telemetry"] != nil && lane.shared["telemetry"].waiters == 2
		lane.mu.Unlock()
		if joined {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second background request did not join the in-flight request")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err == nil || err.Error() != "probe failed" {
			t.Fatalf("coalesced error=%v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestPauseWaitsForInflightAndRejectsNewCommands(t *testing.T) {
	dispatcher := New()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- dispatcher.Dispatch(context.Background(), "room/master", Request{Execute: func(context.Context) error {
			close(started)
			<-release
			return nil
		}})
	}()
	<-started
	paused := make(chan error, 1)
	go func() { paused <- dispatcher.Pause(context.Background(), "room/master") }()
	for deadline := time.Now().Add(time.Second); dispatcher.Health("room/master").Accepting && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if err := dispatcher.Dispatch(context.Background(), "room/master", Request{Execute: func(context.Context) error { return nil }}); !errors.Is(err, ErrPaused) {
		t.Fatalf("dispatch while paused=%v", err)
	}
	select {
	case err := <-paused:
		t.Fatalf("pause returned before in-flight command: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Resume("room/master"); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Dispatch(context.Background(), "room/master", Request{Execute: func(context.Context) error { return nil }}); err != nil {
		t.Fatal(err)
	}
}
