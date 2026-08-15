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

func TestDispatcherBoundsPendingWorkButStillCoalesces(t *testing.T) {
	dispatcher := NewWithPendingLimit(1)
	started := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	request := Request{Class: ClassBackground, CoalesceKey: "telemetry", Execute: func(context.Context) error {
		close(started)
		<-release
		return nil
	}}
	go func() { first <- dispatcher.Dispatch(context.Background(), "room/master", request) }()
	<-started

	joined := make(chan error, 1)
	go func() { joined <- dispatcher.Dispatch(context.Background(), "room/master", request) }()
	queued := make(chan error, 1)
	go func() {
		queued <- dispatcher.Dispatch(context.Background(), "room/master", Request{Execute: func(context.Context) error { return nil }})
	}()
	for deadline := time.Now().Add(time.Second); dispatcher.Health("room/master").Pending != 1; {
		if time.Now().After(deadline) {
			t.Fatal("foreground command did not enter the bounded wait slot")
		}
		time.Sleep(time.Millisecond)
	}
	if err := dispatcher.Dispatch(context.Background(), "room/master", Request{Execute: func(context.Context) error { return nil }}); !errors.Is(err, ErrCapacityReached) {
		t.Fatalf("dispatch beyond capacity=%v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherRejectsCommandsBoundToOldInstance(t *testing.T) {
	dispatcher := New()
	if err := dispatcher.BindInstance("room/master", "instance-one"); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Dispatch(context.Background(), "room/master", Request{InstanceID: "instance-two", Execute: func(context.Context) error { return nil }}); !errors.Is(err, ErrInstanceChanged) {
		t.Fatalf("dispatch to changed instance=%v", err)
	}
	if err := dispatcher.BindInstance("room/master", "instance-two"); err != nil {
		t.Fatal(err)
	}
	if health := dispatcher.Health("room/master"); health.InstanceID != "instance-two" || health.Status != "ready" {
		t.Fatalf("health=%#v", health)
	}
}

func TestMaintenanceLeaseDrainsAndGatesWriters(t *testing.T) {
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
	leaseResult := make(chan *MaintenanceLease, 1)
	leaseError := make(chan error, 1)
	go func() {
		lease, err := dispatcher.BeginMaintenance(context.Background(), "room/master", "operator", "")
		leaseResult <- lease
		leaseError <- err
	}()
	for deadline := time.Now().Add(time.Second); ; {
		health := dispatcher.Health("room/master")
		if health.Maintenance {
			if health.Status != "maintenance" || health.MaintenanceOwner != "operator" || health.Accepting {
				t.Fatalf("maintenance health=%#v", health)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("maintenance did not close the lane")
		}
		time.Sleep(time.Millisecond)
	}
	if err := dispatcher.Dispatch(context.Background(), "room/master", Request{Execute: func(context.Context) error { return nil }}); !errors.Is(err, ErrPaused) {
		t.Fatalf("dispatch during maintenance=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lease := <-leaseResult
	if err := <-leaseError; err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if !dispatcher.Health("room/master").Accepting {
		t.Fatal("lane did not reopen after maintenance")
	}
}

func TestDirtyInputRequiresNewInstanceBeforeResume(t *testing.T) {
	dispatcher := New()
	if err := dispatcher.BindInstance("room/master", "instance-one"); err != nil {
		t.Fatal(err)
	}
	dispatcher.MarkInputDirty("room/master")
	if err := dispatcher.Resume("room/master"); !errors.Is(err, ErrInputDirty) {
		t.Fatalf("resume dirty input=%v", err)
	}
	if err := dispatcher.BindInstance("room/master", "instance-two"); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Resume("room/master"); err != nil {
		t.Fatal(err)
	}
}
