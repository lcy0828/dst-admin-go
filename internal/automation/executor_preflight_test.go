package automation

import (
	"context"
	"errors"
	"testing"

	"dont/internal/agents"
	"dont/internal/players"
	"dont/internal/runtimeobservation"
	"dont/internal/topology"
)

type scheduledPlayerRuntime struct {
	running bool
	err     error
}

func (*scheduledPlayerRuntime) WorldTargets(string) ([]players.WorldTarget, error) {
	return nil, nil
}

func (*scheduledPlayerRuntime) RefreshWorld(context.Context, string, string) (players.RefreshResult, error) {
	return players.RefreshResult{}, nil
}

func (p *scheduledPlayerRuntime) PrepareScheduledRefresh(context.Context, string, []string) (bool, error) {
	return p.running, p.err
}

func TestScheduledPlayerPreflightDefersTransientRuntimeObservationFailures(t *testing.T) {
	tests := []error{
		&runtimeobservation.UnavailableError{TargetID: "agent:node", State: runtimeobservation.StateOffline, Message: "offline"},
		agents.ErrAgentOffline,
		&topology.ExecutionError{Code: "APPLIED_INVENTORY_STALE", Message: "stale"},
		&topology.ExecutionError{Code: "AGENT_CAPABILITY_MISSING", Message: "upgrade required"},
	}
	for _, runtimeErr := range tests {
		executor := &DomainExecutor{players: &scheduledPlayerRuntime{err: runtimeErr}}
		running, err := executor.ShouldRunScheduled(context.Background(), Task{Action: ActionPlayerRefresh, RoomID: "room"})
		if err != nil || running {
			t.Fatalf("runtime error %v returned running=%v err=%v", runtimeErr, running, err)
		}
	}
}

func TestScheduledPlayerPreflightPreservesUnexpectedFailures(t *testing.T) {
	expected := errors.New("database failed")
	executor := &DomainExecutor{players: &scheduledPlayerRuntime{err: expected}}
	running, err := executor.ShouldRunScheduled(context.Background(), Task{Action: ActionPlayerRefresh, RoomID: "room"})
	if running || !errors.Is(err, expected) {
		t.Fatalf("running=%v err=%v", running, err)
	}
}

func TestScheduledPlayerPreflightKeepsReachableWorldsActive(t *testing.T) {
	executor := &DomainExecutor{players: &scheduledPlayerRuntime{running: true, err: agents.ErrAgentOffline}}
	running, err := executor.ShouldRunScheduled(context.Background(), Task{Action: ActionPlayerRefresh, RoomID: "room"})
	if err != nil || !running {
		t.Fatalf("one unreachable world suppressed reachable worlds: running=%v err=%v", running, err)
	}
}
