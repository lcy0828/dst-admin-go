package players

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/shards"
)

func TestScheduledPresenceRecoversAfterProcessLossOrControllerRestart(t *testing.T) {
	for _, test := range []struct {
		name    string
		state   shards.RuntimeState
		err     error
		offline bool
	}{
		{"process disappeared", shards.RuntimeStopped, nil, true},
		{"crash or OOM", shards.RuntimeFailed, nil, true},
		{"new process loading", shards.RuntimeStarting, nil, false},
		{"process stopping", shards.RuntimeState("stopping"), nil, false},
		{"unknown process", shards.RuntimeUnknown, nil, false},
		{"remote machine unreachable", shards.RuntimeUnknown, agents.ErrAgentOffline, false},
		{"restarted world already paused", shards.RuntimeRunning, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			service, legacy, sender, access, _ := newPlayerTestService(t)
			if _, err := service.RefreshWorld(ctx, "room", "master"); err != nil {
				t.Fatal(err)
			}
			before, err := service.store.Get("room", "KU_ONE")
			if err != nil {
				t.Fatal(err)
			}
			paused := true
			runtime := &samplingTestRuntime{playerTestRuntime: legacy, statuses: map[string]shards.RuntimeStatus{
				// Even a leftover pause flag must not override the process state.
				"master": {State: test.state, Paused: &paused},
				"caves":  {State: shards.RuntimeRunning, Paused: &paused},
			}, errs: map[string]error{"master": test.err}}
			probe := &telemetryTestProbe{items: []Observation{{ID: "KU_ONE", Name: "Reconnected"}}}
			// Reconstruct the service around the existing database, as after a
			// Controller restart. Recovery cannot rely on in-memory transitions.
			recovered, err := NewService(service.rooms, runtime, sender, access, service.store, probe)
			if err != nil {
				t.Fatal(err)
			}
			recovered.now = func() time.Time { return before.LastRefreshedAt.Add(time.Minute) }
			active, err := recovered.PrepareScheduledRefresh(ctx, "room", nil)
			if active || !errors.Is(err, test.err) || probe.calls != 0 || len(sender.scripts) != 0 {
				t.Fatalf("inactive recovery sent commands: active=%v probes=%d scripts=%v err=%v", active, probe.calls, sender.scripts, err)
			}
			after, err := recovered.store.Get("room", "KU_ONE")
			if err != nil || after.Online == test.offline || after.PresenceStatus != FreshnessStale ||
				!after.LastRefreshedAt.Equal(before.LastRefreshedAt) || !after.LastSeenAt.Equal(before.LastSeenAt) {
				t.Fatalf("incorrect recovered presence: before=%+v after=%+v err=%v", before, after, err)
			}
			list, err := recovered.List("room", ListFilter{Limit: 25, SkipAccessLists: true})
			if err != nil || list.Online != 0 || test.offline && list.Offline != 1 || !test.offline && list.StaleOnline != 1 {
				t.Fatalf("API still advertises old live presence: %+v err=%v", list, err)
			}
			// Once reconciled, another tick must neither rewrite records nor
			// advance timestamps merely to advertise an identical observation.
			if err := recovered.store.db.Exec("CREATE TRIGGER reject_repeated_presence_write BEFORE UPDATE ON test_player BEGIN SELECT RAISE(ABORT, 'unexpected repeated presence write'); END").Error; err != nil {
				t.Fatal(err)
			}
			if _, err := recovered.PrepareScheduledRefresh(ctx, "room", nil); !errors.Is(err, test.err) {
				t.Fatalf("repeated recovery: %v", err)
			}
			if err := recovered.store.db.Exec("DROP TRIGGER reject_repeated_presence_write").Error; err != nil {
				t.Fatal(err)
			}
			again, err := recovered.store.Get("room", "KU_ONE")
			if err != nil || !reflect.DeepEqual(after, again) {
				t.Fatalf("identical state rewrote presence: %+v err=%v", again, err)
			}
			delete(runtime.errs, "master")
			runtime.statuses["master"] = shards.RuntimeStatus{State: shards.RuntimeRunning}
			if active, err := recovered.PrepareScheduledRefresh(ctx, "room", nil); err != nil || !active {
				t.Fatalf("reconnected world did not resume: active=%v err=%v", active, err)
			}
			if outcomes, err := recovered.RefreshScheduledWorlds(ctx, "room", []string{"master", "caves"}); err != nil || outcomes[0].Err != nil || !outcomes[1].Deferred || probe.calls != 1 {
				t.Fatalf("reconnected sampling: %+v err=%v", outcomes, err)
			}
			player, err := recovered.store.Get("room", "KU_ONE")
			if err != nil || !player.Online || player.PresenceStatus != FreshnessLive || player.Name != "Reconnected" {
				t.Fatalf("new observation did not restore presence: %+v err=%v", player, err)
			}
		})
	}
}

func TestMixedSamplingDoesNotDeclareUnconfirmedWorldOffline(t *testing.T) {
	for _, state := range []shards.RuntimeState{shards.RuntimeStarting, shards.RuntimeState("stopping"), shards.RuntimeUnknown} {
		t.Run(string(state), func(t *testing.T) {
			service, legacy, _, _, _ := newPlayerTestService(t)
			ctx := context.Background()
			if _, err := service.RefreshWorld(ctx, "room", "master"); err != nil {
				t.Fatal(err)
			}
			service.runtime = &samplingTestRuntime{playerTestRuntime: legacy, statuses: map[string]shards.RuntimeStatus{
				"master": {State: state}, "caves": {State: shards.RuntimeRunning},
			}}
			probe := &telemetryTestProbe{}
			service.probe = probe
			outcomes, err := service.RefreshScheduledWorlds(ctx, "room", []string{"master", "caves"})
			if err != nil || outcomes[0].Err == nil || outcomes[1].Err != nil || probe.calls != 1 {
				t.Fatalf("uncertain world was probed: %+v calls=%d err=%v", outcomes, probe.calls, err)
			}
			player, err := service.store.Get("room", "KU_ONE")
			if err != nil || !player.Online || player.PresenceStatus != FreshnessStale {
				t.Fatalf("unknown presence was treated as offline: %+v err=%v", player, err)
			}
		})
	}
}
