package runtimeobservation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/agents"
)

func TestMutationBoundaryDoesNotReuseAnOlderInFlightSample(t *testing.T) {
	base := time.Now().UTC()
	var ticks atomic.Int64
	now := func() time.Time { return base.Add(time.Duration(ticks.Load()) * time.Second) }
	source := &observationSource{items: []agents.RuntimeTargetInventory{observationInventory(base)}}
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	source.collect = func(ctx context.Context, item agents.RuntimeTargetInventory) (agents.RuntimeTargetInventory, error) {
		call := calls.Add(1)
		sampled := now()
		if call == 1 {
			close(started)
			select {
			case <-ctx.Done():
				return item, ctx.Err()
			case <-finish:
			}
		}
		item.ObservedAt, item.ReceivedAt = &sampled, &sampled
		return item, nil
	}
	coordinator, _ := New(source, Options{Now: now})
	defer coordinator.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	oldDone := make(chan error, 1)
	go func() { _, err := coordinator.Refresh(ctx, Scope{TargetID: "agent:node"}); oldDone <- err }()
	<-started
	ticks.Store(1)
	boundary := now()
	freshDone := make(chan error, 1)
	go func() {
		freshDone <- coordinator.EnsureRuntimeInstallationFresh(ctx, "agent:node", "default", boundary)
	}()
	close(finish)
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	if err := <-freshDone; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("old in-flight evidence was reused: %d samples", calls.Load())
	}
	if err := coordinator.EnsureRuntimeInstallationFresh(ctx, "agent:node", "default", boundary); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("same-phase fresh evidence was not reused")
	}
}

func TestOperationFreshnessUsesControllerReceiptAndBoundsInvalidSamples(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "agent-clock-skew", true: "invalid-receipt"}[invalid], func(t *testing.T) {
			now := time.Now().UTC()
			source := &observationSource{items: []agents.RuntimeTargetInventory{observationInventory(now)}}
			var calls atomic.Int32
			source.collect = func(_ context.Context, item agents.RuntimeTargetInventory) (agents.RuntimeTargetInventory, error) {
				calls.Add(1)
				past := now.Add(-time.Hour)
				item.ObservedAt = &past
				if invalid {
					item.ReceivedAt = nil
				}
				return item, nil
			}
			c, _ := New(source, Options{Now: func() time.Time { return now }})
			defer c.Close()
			err := c.EnsureRuntimeInstallationFresh(context.Background(), "agent:node", "default", now)
			if invalid != (err != nil) {
				t.Fatalf("invalid=%v error=%v", invalid, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("unexpected repeated collection: %d", calls.Load())
			}
		})
	}
}
