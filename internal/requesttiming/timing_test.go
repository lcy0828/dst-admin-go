package requesttiming

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTimingIsRequestScopedAndConcurrent(t *testing.T) {
	ctx, recorder := New(context.Background())
	master := Scope(ctx, "Master")
	caves := Scope(ctx, "Caves")
	var workers sync.WaitGroup
	for range 20 {
		for _, ctx := range []context.Context{master, caves} {
			workers.Add(1)
			go func() {
				defer workers.Done()
				Start(ctx, "read")()
			}()
		}
	}
	workers.Wait()
	elapsed, metrics := recorder.Snapshot()
	if elapsed <= 0 || len(metrics) != 2 || metrics[0].Name != "Caves.read" || metrics[1].Name != "Master.read" {
		t.Fatalf("unexpected timings: %s %#v", elapsed, metrics)
	}
	for _, metric := range metrics {
		if metric.Calls != 20 || metric.DurationMS < 0 {
			t.Fatalf("concurrent measurements were lost: %#v", metric)
		}
	}
	metrics[0].Calls = 0
	_, next := recorder.Snapshot()
	if next[0].Calls != 20 {
		t.Fatal("caller changed the retained measurements")
	}
	_, another := New(context.Background())
	if _, metrics := another.Snapshot(); len(metrics) != 0 {
		t.Fatal("a new request reused previous measurements")
	}
}

func TestTimingPreservesContextAndIsInactiveUnlessEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	if Scope(ctx, "unused") != ctx {
		t.Fatal("disabled timing changed the context")
	}
	Start(ctx, "unused")()
	timed, recorder := New(ctx)
	finish := Start(Scope(timed, "world"), "read")
	cancel()
	finish()
	if timed.Err() != context.Canceled {
		t.Fatal("timing swallowed request cancellation")
	}
	if _, metrics := recorder.Snapshot(); len(metrics) != 1 {
		t.Fatal("cancelled operations must still record elapsed time")
	}
}

func TestServerTimingUsesWallTimeInsteadOfSummingParallelStages(t *testing.T) {
	header := Header(100*time.Millisecond, []Metric{
		{Name: "Master.read", DurationMS: 90, Calls: 2},
		{Name: "Caves.read", DurationMS: 95, Calls: 1},
	})
	if !strings.HasPrefix(header, "handler;dur=100.000,") || !strings.Contains(header, `Master.read;dur=90.000;desc="2 calls"`) {
		t.Fatalf("unexpected Server-Timing header: %s", header)
	}
}

func TestTimingRejectsInvalidNamesAndBoundsOutput(t *testing.T) {
	ctx, recorder := New(context.Background())
	for _, name := range []string{"", "secret;desc=\"value\"", "line\r\ninjected", strings.Repeat("x", 161)} {
		Start(ctx, name)()
	}
	if _, metrics := recorder.Snapshot(); len(metrics) != 0 {
		t.Fatal("unsafe timing names were retained")
	}
	for index := 0; index < 200; index++ {
		Start(ctx, strings.Repeat("stage", 20)+fmt.Sprint(index))()
	}
	elapsed, metrics := recorder.Snapshot()
	if len(metrics) != 128 {
		t.Fatalf("timing records are not bounded: %d", len(metrics))
	}
	metrics = append(metrics, Metric{Name: "line\r\ninjected", Calls: 1})
	header := Header(elapsed, metrics)
	if len(header) > 6000 || strings.ContainsAny(header, "\r\n") || strings.Contains(header, "injected") {
		t.Fatal("Server-Timing header is unsafe or unbounded")
	}
}
