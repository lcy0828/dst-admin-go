package players

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/shards"
)

func passiveFixture(t *testing.T) (*Service, *inventoryTelemetryFixture, *playerTestSender, *telemetryTestProbe) {
	t.Helper()
	s, _, sender, _, _ := newPlayerTestService(t)
	reader := &inventoryTelemetryFixture{values: map[string]dstruntime.Snapshot{}}
	for _, world := range []string{"master", "caves"} {
		reader.values[world] = dstruntime.Snapshot{SessionID: "session-" + world, LocalityKnown: true, CapturedAt: s.now(), Players: []dstruntime.SnapshotPlayer{}}
	}
	fallback := &telemetryTestProbe{err: errors.New("read APIs must never send Lua")}
	probe, err := NewTelemetryProbe(fallback, reader, fallback)
	if err != nil {
		t.Fatal(err)
	}
	s.probe = probe
	return s, reader, sender, fallback
}

func TestPassivePlayersRecoverAnOfflinePlayerOnAnotherShardWithoutTasksOrLua(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "agent"}[remote], func(t *testing.T) {
			s, reader, sender, fallback := passiveFixture(t)
			probe := s.probe.(*TelemetryProbe)
			probe.locality = &playerTestRuntime{local: entityEvidence(!remote)}
			before := s.now().Add(-24 * time.Hour)
			old := stampTelemetryObservation(Observation{ID: "KU_TEST", Name: "Test player", LocalEntity: entityEvidence(true), GameplayState: GameplayStateAlive}, SourceRuntime, before)
			if err := s.store.ReplaceRoomSnapshots("room", []worldSnapshot{{WorldID: "caves", WorldName: "Caves", SessionID: "session-caves", ObservedAt: before, Observations: []Observation{old}}}); err != nil {
				t.Fatal(err)
			}
			if err := s.store.MarkPlayerOffline("room", "KU_TEST", before.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			value := reader.values["master"]
			value.Players = []dstruntime.SnapshotPlayer{{ID: "KU_TEST", Name: "Test player", LocalEntity: entityEvidence(true), GameplayState: GameplayStateAlive}}
			reader.values["master"] = value
			if err := s.SyncPassivePlayers(context.Background(), "room"); err != nil {
				t.Fatal(err)
			}
			player, err := s.Player("room", "KU_TEST")
			if err != nil || !player.Online || !player.WorldConfirmed || player.WorldID != "master" || player.PresenceStatus != FreshnessLive || !player.LastRefreshedAt.Equal(s.now()) {
				t.Fatalf("fresh artifacts did not replace the old offline record: %+v err=%v", player, err)
			}
			if len(sender.scripts) != 0 || fallback.calls != 0 || len(reader.refreshes) != 0 {
				t.Fatal("passive observation invoked an active probe")
			}
		})
	}
}

func TestPassivePlayerFailuresPreservePresenceAndNeverFallBackLocally(t *testing.T) {
	for _, failure := range []error{agents.ErrAgentOffline, dstruntime.ErrRuntimeVersionMismatch, dstruntime.ErrSnapshotStale} {
		t.Run(failure.Error(), func(t *testing.T) {
			s, reader, sender, fallback := passiveFixture(t)
			value := reader.values["master"]
			value.Players = []dstruntime.SnapshotPlayer{{ID: "KU_TEST", Name: "Player", LocalEntity: entityEvidence(true)}}
			reader.values["master"] = value
			if err := s.SyncPassivePlayers(context.Background(), "room"); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Player("room", "KU_TEST")
			next := s.now().Add(passivePlayerInterval)
			s.now = func() time.Time { return next }
			reader.readErr = failure
			if err := s.SyncPassivePlayers(context.Background(), "room"); !errors.Is(err, failure) {
				t.Fatalf("missing observation failure: %v", err)
			}
			after, _ := s.Player("room", "KU_TEST")
			if !after.Online || after.PresenceStatus != FreshnessStale || !after.LastSeenAt.Equal(before.LastSeenAt) || !after.LastRefreshedAt.Equal(before.LastRefreshedAt) {
				t.Fatalf("failed read invented an offline state or a sample: %+v", after)
			}
			if len(sender.scripts) != 0 || fallback.calls != 0 || len(reader.refreshes) != 0 {
				t.Fatal("failed passive read sent a command or read a local fallback")
			}
		})
	}
}

func TestPassivePlayersReadFreshArtifactsWhilePausedAndReconcileStoppedProcesses(t *testing.T) {
	s, reader, _, _ := passiveFixture(t)
	paused := true
	runtime := &samplingTestRuntime{playerTestRuntime: s.runtime.(*playerTestRuntime), statuses: map[string]shards.RuntimeStatus{
		"master": {State: shards.RuntimeRunning, Paused: &paused},
		"caves":  {State: shards.RuntimeRunning, Paused: &paused},
	}}
	s.runtime = runtime
	value := reader.values["master"]
	value.Players = []dstruntime.SnapshotPlayer{{ID: "KU_TEST", Name: "Player", LocalEntity: entityEvidence(true)}}
	reader.values["master"] = value
	if err := s.SyncPassivePlayers(context.Background(), "room"); err != nil {
		t.Fatal(err)
	}
	player, _ := s.Player("room", "KU_TEST")
	if !player.Online || player.PresenceStatus != FreshnessLive {
		t.Fatal("a pause flag hid a valid newly published player snapshot")
	}
	next := s.now().Add(passivePlayerInterval)
	s.now = func() time.Time { return next }
	runtime.statuses["master"] = shards.RuntimeStatus{State: shards.RuntimeStopped, Paused: &paused}
	if err := s.SyncPassivePlayers(context.Background(), "room"); err != nil {
		t.Fatal(err)
	}
	player, _ = s.Player("room", "KU_TEST")
	if player.Online || !player.LastSeenAt.Equal(value.CapturedAt) {
		t.Fatalf("confirmed process stop was not reconciled or invented a player sample: %+v", player)
	}
}

type passiveTestProbe struct {
	playerTestProbe
	read func(context.Context, string) (SnapshotResult, error)
}

func (p *passiveTestProbe) PassiveSnapshot(ctx context.Context, _, world string) (SnapshotResult, error) {
	return p.read(ctx, world)
}

func TestPassivePlayerReadsCoalesceAndRespectCallerCancellation(t *testing.T) {
	s, _, _, _, _ := newPlayerTestService(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.probe = &passiveTestProbe{read: func(ctx context.Context, world string) (SnapshotResult, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return SnapshotResult{SessionID: "session-" + world, LocalityKnown: true, ObservedAt: s.now()}, nil
		case <-ctx.Done():
			return SnapshotResult{}, ctx.Err()
		}
	}}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := s.SyncPassivePlayers(context.Background(), "room"); err != nil {
				t.Error(err)
			}
		})
	}
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.SyncPassivePlayers(ctx, "room"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reader waited for another request: %v", err)
	}
	close(release)
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("duplicate readers caused %d artifact reads, want two shards", calls.Load())
	}
	if err := s.SyncPassivePlayers(context.Background(), "room"); err != nil || calls.Load() != 2 {
		t.Fatalf("read within the cache window repeated work: %d %v", calls.Load(), err)
	}
	next := s.now().Add(passivePlayerInterval)
	s.now = func() time.Time { return next }
	if err := s.SyncPassivePlayers(context.Background(), "room"); err != nil || calls.Load() != 4 {
		t.Fatalf("cache did not resume observation: %d %v", calls.Load(), err)
	}
}

func TestPassivePlayersRejectExpiredOrUnscopedSnapshots(t *testing.T) {
	for _, broken := range []string{"expired", "future", "session", "locality"} {
		t.Run(broken, func(t *testing.T) {
			s, reader, _, _ := passiveFixture(t)
			for world, value := range reader.values {
				switch broken {
				case "expired":
					value.CapturedAt = s.now().Add(-time.Minute)
				case "future":
					value.CapturedAt = s.now().Add(time.Minute)
				case "session":
					value.SessionID = ""
				case "locality":
					value.LocalityKnown = false
				}
				reader.values[world] = value
			}
			if err := s.SyncPassivePlayers(context.Background(), "room"); !errors.Is(err, dstruntime.ErrSnapshotStale) {
				t.Fatalf("invalid evidence was accepted: %v", err)
			}
		})
	}
}

type passiveMultiCatalog struct{ playerTestCatalog }

func (c passiveMultiCatalog) Room(id string) (rooms.Room, error) {
	room := c.room
	room.ID = id
	return room, nil
}

func (c passiveMultiCatalog) Worlds(string) ([]rooms.World, error) {
	return c.worlds, nil
}

func TestPassivePlayerReadsBoundGlobalWorkAndRemoteWaits(t *testing.T) {
	s, _, _, _, _ := newPlayerTestService(t)
	s.rooms = passiveMultiCatalog{s.rooms.(playerTestCatalog)}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	s.probe = &passiveTestProbe{read: func(ctx context.Context, world string) (SnapshotResult, error) {
		if world == "master" {
			entered <- struct{}{}
		}
		select {
		case <-release:
			return SnapshotResult{SessionID: "session", LocalityKnown: true, ObservedAt: s.now()}, nil
		case <-ctx.Done():
			return SnapshotResult{}, ctx.Err()
		}
	}}
	var wg sync.WaitGroup
	for _, room := range []string{"a", "b"} {
		wg.Go(func() {
			if err := s.SyncPassivePlayers(context.Background(), room); err != nil {
				t.Error(err)
			}
		})
	}
	<-entered
	<-entered
	if err := s.SyncPassivePlayers(context.Background(), "c"); !errors.Is(err, errPassivePlayersBusy) {
		t.Fatalf("a third room was allowed to add work: %v", err)
	}
	close(release)
	wg.Wait()

	s.probe = &passiveTestProbe{read: func(ctx context.Context, _ string) (SnapshotResult, error) {
		<-ctx.Done()
		return SnapshotResult{}, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.SyncPassivePlayers(ctx, "c"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unresponsive node did not honor the caller deadline: %v", err)
	}
}
