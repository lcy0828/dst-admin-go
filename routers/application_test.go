package routers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestApplicationStartAndCloseAreIdempotentAndWaitForWorkers(t *testing.T) {
	var starts, stops, workers, finals atomic.Int32
	hooks := applicationHooks{
		start: []func(context.Context) error{func(context.Context) error {
			starts.Add(1)
			return nil
		}},
		workers: []func(context.Context){func(ctx context.Context) {
			workers.Add(1)
			<-ctx.Done()
			workers.Add(-1)
		}},
		stop: []func(context.Context) error{func(context.Context) error {
			stops.Add(1)
			return nil
		}},
		final: []func() error{func() error {
			finals.Add(1)
			return nil
		}},
	}
	application := newApplication(gin.New(), hooks)
	if err := application.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := application.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for workers.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starts.Load() != 1 || workers.Load() != 1 {
		t.Fatalf("starts=%d workers=%d", starts.Load(), workers.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := application.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := application.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 1 || workers.Load() != 0 || finals.Load() != 1 {
		t.Fatalf("stops=%d workers=%d finals=%d", stops.Load(), workers.Load(), finals.Load())
	}
	if err := application.Start(context.Background()); !errors.Is(err, ErrApplicationClosed) {
		t.Fatalf("start after close error = %v", err)
	}
}

func TestApplicationStartFailureRollsBackStartedHooks(t *testing.T) {
	startFailure := errors.New("start failed")
	var stopped, finalized atomic.Int32
	application := newApplication(gin.New(), applicationHooks{
		start: []func(context.Context) error{
			func(context.Context) error { return nil },
			func(context.Context) error { return startFailure },
		},
		stop: []func(context.Context) error{
			func(context.Context) error { stopped.Add(1); return nil },
		},
		final: []func() error{
			func() error { finalized.Add(1); return nil },
		},
	})
	if err := application.Start(context.Background()); !errors.Is(err, startFailure) {
		t.Fatalf("start error = %v", err)
	}
	if stopped.Load() != 1 {
		t.Fatalf("rollback stops = %d", stopped.Load())
	}
	if err := application.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stopped.Load() != 1 || finalized.Load() != 1 {
		t.Fatalf("failed start cleanup stops=%d finals=%d", stopped.Load(), finalized.Load())
	}
}

func TestApplicationRejectsCanceledStartContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	application := newApplication(gin.New(), applicationHooks{})
	if err := application.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("start error = %v", err)
	}
}
