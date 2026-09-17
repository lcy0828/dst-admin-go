package players

import (
	"context"
	"errors"
	"testing"

	"dont/shared"
)

type counterTestRuntime struct {
	statuses map[string]shared.ShardRuntimeStatus
	errs     map[string]error
}

func (r counterTestRuntime) Status(_ context.Context, _, worldID string) (shared.ShardRuntimeStatus, error) {
	return r.statuses[worldID], r.errs[worldID]
}

func TestMaintenanceCountUsesLiveRosterAndDeduplicatesShards(t *testing.T) {
	service, _, _, _, _ := newPlayerTestService(t)
	// A previous sample says someone is online. It must not decide maintenance.
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	paused := true
	runtime := counterTestRuntime{statuses: map[string]shared.ShardRuntimeStatus{
		"master": {State: "running", SessionExists: true, Paused: &paused},
		"caves":  {State: "running", SessionExists: true, Paused: &paused},
	}}
	probe := &playerWorldProbe{items: map[string][]Observation{}}
	counter := NewRuntimeOnlineCounter(service.rooms, runtime, probe)
	count, err := counter.OnlinePlayers(context.Background(), "room")
	if err != nil || count != 0 {
		t.Fatalf("empty live roster used stored player: count=%d err=%v", count, err)
	}
	probe.items["master"] = []Observation{{ID: "KU_ONE"}, {ID: "KU_TWO"}}
	probe.items["caves"] = []Observation{{ID: "KU_TWO"}, {ID: "KU_THREE"}}
	count, err = counter.OnlinePlayers(context.Background(), "room")
	if err != nil || count != 3 {
		t.Fatalf("room players counted per shard: count=%d err=%v", count, err)
	}
}

func TestMaintenanceCountNeverTreatsUnavailableWorldAsEmpty(t *testing.T) {
	service, _, _, _, _ := newPlayerTestService(t)
	runtime := counterTestRuntime{statuses: map[string]shared.ShardRuntimeStatus{
		"master": {State: "stopped"}, "caves": {State: "stopped"},
	}, errs: map[string]error{}}
	probe := &playerWorldProbe{errs: map[string]error{"master": errors.New("must not probe stopped world"), "caves": errors.New("agent unavailable")}}
	counter := NewRuntimeOnlineCounter(service.rooms, runtime, probe)
	if count, err := counter.OnlinePlayers(context.Background(), "room"); err != nil || count != 0 {
		t.Fatalf("stopped worlds need no game probe: count=%d err=%v", count, err)
	}
	for _, status := range []shared.ShardRuntimeStatus{
		{State: "unknown"}, {State: "running", SessionExists: true}, {State: "failed", SessionExists: true},
	} {
		runtime.statuses["caves"] = status
		if _, err := counter.OnlinePlayers(context.Background(), "room"); err == nil {
			t.Fatalf("unavailable world counted as empty: %+v", status)
		}
	}
	runtime.errs["caves"] = errors.New("agent disconnected")
	if _, err := counter.OnlinePlayers(context.Background(), "room"); err == nil {
		t.Fatal("unreachable Agent was treated as empty")
	}
}
