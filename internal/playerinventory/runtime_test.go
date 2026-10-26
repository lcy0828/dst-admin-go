package playerinventory

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"dont/shared"
)

func TestCoalescedReadsAndIndependentCancellation(t *testing.T) {
	r := NewCoalescer(2, 0)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	read := func(ctx context.Context) (shared.PlayerInventory, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return baseResult(testUser, "live"), nil
		case <-ctx.Done():
			return shared.PlayerInventory{}, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, e := r.Do(ctx, "key", "world", read); first <- e }()
	<-started
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			v, e := r.Do(context.Background(), "key", "world", read)
			if e == nil && v.PlayerID != testUser {
				e = errors.New("wrong result")
			}
			done <- e
		}()
	}
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		n := r.flights["key"].waiters
		r.mu.Unlock()
		if n == 21 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiters did not join")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if e := <-first; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate reads", calls.Load())
	}
	close(release)
	for i := 0; i < 20; i++ {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
}

func TestAllWaitersCancelAndFrequencyIsBounded(t *testing.T) {
	r := NewCoalescer(1, 2*time.Second)
	stopped := make(chan struct{})
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _ = r.Do(ctx, "key", "world", func(ctx context.Context) (shared.PlayerInventory, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return shared.PlayerInventory{}, ctx.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("abandoned work still running")
	}
	// The first task can still be finishing; either a canceled flight or the
	// frequency limiter must prevent a second execution.
	var calls atomic.Int32
	_, _ = r.Do(context.Background(), "key", "world", func(context.Context) (shared.PlayerInventory, error) {
		calls.Add(1)
		return shared.PlayerInventory{}, nil
	})
	if calls.Load() != 0 {
		t.Fatal("frequency limit bypassed")
	}
}
