package players

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/shards"
)

type samplingTestRuntime struct {
	*playerTestRuntime
	statuses map[string]shards.RuntimeStatus
	errs     map[string]error
}

func TestPausedPreflightMergesFinalDisconnectWithoutResamplingVitals(t *testing.T) {
	service, legacy, _, _, probe := newPlayerTestService(t)
	ctx := context.Background()
	if _, err := service.RefreshWorld(ctx, "room", "master"); err != nil {
		t.Fatal(err)
	}
	before, err := service.store.Get("room", "KU_ONE")
	if err != nil {
		t.Fatal(err)
	}
	disconnected := before.LastRefreshedAt.Add(2 * time.Second)
	probe.historyItems = []Observation{{ID: "KU_ONE", Name: "Willow", LastSeenAt: disconnected, LastDisconnectedAt: disconnected}}
	probe.err = errors.New("paused telemetry must not be sampled")
	paused := true
	service.runtime = &samplingTestRuntime{playerTestRuntime: legacy, statuses: map[string]shards.RuntimeStatus{
		"master": {State: shards.RuntimeRunning, Paused: &paused},
		"caves":  {State: shards.RuntimeRunning, Paused: &paused},
	}}
	service.now = func() time.Time { return disconnected.Add(time.Minute) }
	active, err := service.PrepareScheduledRefresh(ctx, "room", []string{"master"})
	if err != nil || active {
		t.Fatalf("paused preflight: active=%v err=%v", active, err)
	}
	after, err := service.store.Get("room", "KU_ONE")
	if err != nil || after.Online || after.Age != before.Age || after.Prefab != before.Prefab || !after.LastRefreshedAt.Equal(disconnected) {
		t.Fatalf("final disconnect was lost or game data resampled: before=%+v after=%+v err=%v", before, after, err)
	}
}

func (r *samplingTestRuntime) StatusFor(_ context.Context, roomID, worldID string) (shards.RuntimeStatus, error) {
	if roomID != "room" {
		return shards.RuntimeStatus{}, rooms.ErrRoomNotFound
	}
	if err := r.errs[worldID]; err != nil {
		return shards.RuntimeStatus{}, err
	}
	status, ok := r.statuses[worldID]
	if !ok {
		return shards.RuntimeStatus{}, rooms.ErrWorldNotFound
	}
	return status, nil
}

func TestScheduledPausedPlayersSendNoRefreshOrFallbackAndResumeAutomatically(t *testing.T) {
	for _, local := range []bool{true, false} {
		for _, snapshotErr := range []error{dstruntime.ErrSnapshotStale, dstruntime.ErrSnapshotUnavailable} {
			name := "local/" + snapshotErr.Error()
			if !local {
				name = "agent/" + snapshotErr.Error()
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				service, legacy, _, _, _ := newPlayerTestService(t)
				if _, err := service.RefreshWorld(ctx, "room", "master"); err != nil {
					t.Fatal(err)
				}
				before, err := service.store.Get("room", "KU_ONE")
				if err != nil {
					t.Fatal(err)
				}
				paused, unpaused := true, false
				legacy.local = &local
				runtime := &samplingTestRuntime{playerTestRuntime: legacy, statuses: map[string]shards.RuntimeStatus{
					"master": {State: shards.RuntimeRunning, Paused: &paused},
					"caves":  {State: shards.RuntimeRunning, Paused: &paused},
				}}
				service.runtime = runtime
				native, fallback := &telemetryTestProbe{}, &telemetryTestProbe{err: errors.New("unexpected console fallback")}
				reader := &refreshableTelemetryTestReader{
					telemetryTestReader: &telemetryTestReader{err: snapshotErr},
					refresh: dstruntime.SnapshotRefreshResult{Players: dstruntime.Snapshot{
						CapturedAt: service.now().Add(time.Minute), Players: []dstruntime.SnapshotPlayer{{ID: "KU_ONE", Name: "Updated"}},
					}},
				}
				probe, err := NewTelemetryProbe(native, reader, fallback, runtime)
				if err != nil {
					t.Fatal(err)
				}
				probe.ConfigureRemoteFallback(fallback)
				service.probe = probe
				for range 3 {
					if active, err := service.PrepareScheduledRefresh(ctx, "room", nil); err != nil || active {
						t.Fatalf("paused preflight: active=%v err=%v", active, err)
					}
					outcomes, err := service.RefreshScheduledWorlds(ctx, "room", []string{"master", "caves"})
					if err != nil || len(outcomes) != 2 || !outcomes[0].Deferred || !outcomes[1].Deferred {
						t.Fatalf("paused refresh=%+v err=%v", outcomes, err)
					}
				}
				after, err := service.store.Get("room", "KU_ONE")
				if err != nil || !after.Online || after.PresenceStatus != FreshnessStale || after.Name != before.Name || after.Age != before.Age ||
					!after.LastSeenAt.Equal(before.LastSeenAt) || !after.LastRefreshedAt.Equal(before.LastRefreshedAt) ||
					reader.calls != 0 || reader.refreshCalls != 0 || fallback.calls != 0 || native.calls != 0 {
					t.Fatalf("paused sampling changed data or probed game: after=%+v reads=%d refreshes=%d fallback=%d native=%d err=%v", after, reader.calls, reader.refreshCalls, fallback.calls, native.calls, err)
				}
				runtime.statuses["master"] = shards.RuntimeStatus{State: shards.RuntimeRunning, Paused: &unpaused}
				if active, err := service.PrepareScheduledRefresh(ctx, "room", nil); err != nil || !active {
					t.Fatalf("resumed preflight: active=%v err=%v", active, err)
				}
				outcomes, err := service.RefreshScheduledWorlds(ctx, "room", []string{"master", "caves"})
				if err != nil || outcomes[0].Err != nil || outcomes[0].Deferred || !outcomes[1].Deferred || reader.refreshCalls != 1 {
					t.Fatalf("resumed refresh=%+v refreshes=%d err=%v", outcomes, reader.refreshCalls, err)
				}
				if _, err := service.RefreshWorld(ctx, "room", "caves"); err != nil || reader.refreshCalls != 2 || fallback.calls != 0 {
					t.Fatalf("explicit paused refresh failed: refreshes=%d fallback=%d err=%v", reader.refreshCalls, fallback.calls, err)
				}
			})
		}
	}
}

func TestScheduledMixedWorldsPreservePausedPlayers(t *testing.T) {
	service, legacy, _, _, _ := newPlayerTestService(t)
	ctx := context.Background()
	service.probe = &playerWorldProbe{items: map[string][]Observation{
		"master": {{ID: "KU_MASTER", Name: "Master player", GameplayState: GameplayStateAlive}},
		"caves":  {{ID: "KU_CAVES", Name: "Caves player", GameplayState: GameplayStateAlive}},
	}}
	if _, err := service.RefreshWorlds(ctx, "room", []string{"master", "caves"}); err != nil {
		t.Fatal(err)
	}
	before, err := service.store.Get("room", "KU_MASTER")
	if err != nil {
		t.Fatal(err)
	}
	paused, unpaused := true, false
	service.runtime = &samplingTestRuntime{playerTestRuntime: legacy, statuses: map[string]shards.RuntimeStatus{
		"master": {State: shards.RuntimeRunning, Paused: &paused},
		"caves":  {State: shards.RuntimeRunning, Paused: &unpaused},
	}}
	service.probe = &playerWorldProbe{items: map[string][]Observation{
		"caves": {{ID: "KU_CAVES", Name: "Updated caves player", GameplayState: GameplayStateAlive}},
	}, errs: map[string]error{"master": errors.New("paused world must not be sampled")}}
	service.now = func() time.Time { return before.LastRefreshedAt.Add(time.Minute) }
	outcomes, err := service.RefreshScheduledWorlds(ctx, "room", []string{"master", "caves"})
	if err != nil || !outcomes[0].Deferred || outcomes[1].Err != nil || outcomes[1].Deferred {
		t.Fatalf("mixed outcomes=%+v err=%v", outcomes, err)
	}
	after, err := service.store.Get("room", "KU_MASTER")
	if err != nil || !after.Online || after.PresenceStatus != FreshnessStale || after.Name != before.Name ||
		!after.LastSeenAt.Equal(before.LastSeenAt) || !after.LastRefreshedAt.Equal(before.LastRefreshedAt) {
		t.Fatalf("paused player changed: before=%+v after=%+v err=%v", before, after, err)
	}
	active, err := service.store.Get("room", "KU_CAVES")
	if err != nil || active.Name != "Updated caves player" || !active.LastRefreshedAt.Equal(service.now()) {
		t.Fatalf("active world was not sampled: %+v err=%v", active, err)
	}
}

func TestScheduledPlayerPauseUnknownStillSamplesAndRechecksAfterPreflight(t *testing.T) {
	ctx := context.Background()
	service, legacy, _, _, _ := newPlayerTestService(t)
	probe := &telemetryTestProbe{}
	service.probe = probe
	runtime := &samplingTestRuntime{playerTestRuntime: legacy, statuses: map[string]shards.RuntimeStatus{
		"master": {State: shards.RuntimeRunning},
	}}
	service.runtime = runtime
	if active, err := service.PrepareScheduledRefresh(ctx, "room", []string{"master"}); err != nil || !active {
		t.Fatalf("unknown pause preflight: active=%v err=%v", active, err)
	}
	if _, err := service.RefreshScheduledWorlds(ctx, "room", []string{"master"}); err != nil || probe.calls != 1 {
		t.Fatalf("unknown pause sampling: calls=%d err=%v", probe.calls, err)
	}
	paused := true
	runtime.statuses["master"] = shards.RuntimeStatus{State: shards.RuntimeRunning, Paused: &paused}
	outcomes, err := service.RefreshScheduledWorlds(ctx, "room", []string{"master"})
	if err != nil || !outcomes[0].Deferred || probe.calls != 1 {
		t.Fatalf("pause after preflight was ignored: outcomes=%+v calls=%d err=%v", outcomes, probe.calls, err)
	}
}
