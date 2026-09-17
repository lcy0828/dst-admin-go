package worldstate

import (
	"testing"
	"time"

	"dont/internal/rooms"
	"dont/internal/shards"
)

func TestPausedDataIsSeparateFromDelayedStoppedAndUnavailable(t *testing.T) {
	paused, unpaused := true, false
	now := time.Now().UTC()
	for _, test := range []struct {
		name      string
		state     shards.RuntimeState
		paused    *bool
		observed  time.Time
		freshness Freshness
		stale     bool
	}{
		{"paused old data", shards.RuntimeRunning, &paused, now.Add(-24 * time.Hour), FreshnessPaused, false},
		{"paused recent data", shards.RuntimeRunning, &paused, now, FreshnessPaused, false},
		{"unknown pause", shards.RuntimeRunning, nil, now.Add(-time.Hour), FreshnessDelayed, true},
		{"resumed old data", shards.RuntimeRunning, &unpaused, now.Add(-time.Hour), FreshnessDelayed, true},
		{"resumed fresh data", shards.RuntimeRunning, &unpaused, now, FreshnessLive, false},
		{"stopped", shards.RuntimeStopped, &paused, now, FreshnessStopped, true},
		{"failed", shards.RuntimeFailed, &paused, now, FreshnessStopped, true},
		{"starting", shards.RuntimeStarting, &paused, now, FreshnessDelayed, true},
		{"missing data", shards.RuntimeRunning, &paused, time.Time{}, FreshnessUnavailable, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := decorateSnapshot(Snapshot{ObservedAt: test.observed, Paused: test.paused}, test.state, now)
			if got.Freshness != test.freshness || got.Stale != test.stale || got.RuntimeState != string(test.state) {
				t.Fatalf("snapshot=%+v", got)
			}
			if test.state != shards.RuntimeRunning && got.Paused != nil {
				t.Fatal("non-running process retained a pause observation")
			}
		})
	}
}

func TestPausedWorldStillReportsReadFailure(t *testing.T) {
	paused := true
	service := &Service{now: time.Now}
	value := service.snapshotFromRead("room", rooms.World{ID: "master"}, listWorldRead{
		runtimeStatus:    shards.RuntimeStatus{State: shards.RuntimeRunning, Paused: &paused},
		observationState: ObservationStateFailed, observationError: "permission denied",
	})
	if value.Paused == nil || !*value.Paused || value.Freshness != FreshnessUnavailable || value.ObservationError != "permission denied" {
		t.Fatalf("paused read error was hidden: %+v", value)
	}
}
