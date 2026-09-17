package dstruntime

import (
	"context"
	"testing"

	"dont/internal/requesttiming"
	"dont/shared"
)

func TestRefreshTimingPreservesFreshReadSequence(t *testing.T) {
	bridge, fixture, roomID, worldID, now := newDistributedBridgeFixture(t)
	fixture.onSend = func(shared.RuntimeConsoleRequest) { advanceRefreshFixture(t, fixture, now) }
	ctx, timing := requesttiming.New(context.Background())
	result, err := bridge.RefreshSnapshots(ctx, roomID, worldID)
	if err != nil || result.Health.Sequence != 4 || result.WorldState.Sequence != 5 {
		t.Fatalf("timing changed fresh collection: %#v, %v", result, err)
	}
	_, metrics := timing.Snapshot()
	counts := map[string]int{}
	for _, metric := range metrics {
		counts[metric.Name] = metric.Calls
	}
	for _, name := range []string{"refresh.lock_wait", "refresh.status", "refresh.previous_health", "refresh.console_send", "refresh.health_read", "refresh.parallel_outputs", "runtime.players_read", "runtime.worldstate_read"} {
		if counts[name] != 1 {
			t.Fatalf("missing or repeated measurement %s: %#v", name, counts)
		}
	}
	if counts["refresh.poll_wait"] != 0 {
		t.Fatal("immediately available results must not add a polling wait")
	}
}
