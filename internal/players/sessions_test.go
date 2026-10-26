package players

import (
	"context"
	"dont/internal/shards"
	"errors"
	"testing"
	"time"
)

func TestSaveSessionResetArchivesPlayersAndRejectsLateSamples(t *testing.T) {
	store := newPlayerTestStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	health := 80.0
	old := stampTelemetryObservation(Observation{ID: "KU_ONE", Name: "Player", Age: 36, Prefab: "wilson", HealthPercent: &health, GameplayState: GameplayStateAlive}, SourceRuntime, at)
	apply := func(world, session string, when time.Time, obs ...Observation) {
		t.Helper()
		if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{{WorldID: world, WorldName: world, SessionID: session, ObservedAt: when, Source: SourceRuntime, Observations: obs}}); err != nil {
			t.Fatal(err)
		}
	}
	apply("Master", "OLD", at, old)
	apply("Caves", "CAVE", at, stampTelemetryObservation(Observation{ID: "KU_TWO", Name: "Other", Age: 12}, SourceRuntime, at))
	apply("Master", "NEW", at.Add(time.Minute))
	apply("Master", "OLD", at.Add(time.Second), old)
	if _, err := store.Get("room", "KU_ONE"); err != ErrPlayerNotFound {
		t.Fatalf("old player reappeared: %v", err)
	}
	if _, err := store.Get("room", "KU_TWO"); err != nil {
		t.Fatalf("other shard lost: %v", err)
	}
	// Scanning archived logs must not repopulate the new save.
	if _, err := store.MergeWorldHistory("room", "Master", "Master", []Observation{old}, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	items, total, err := store.List("room", ListFilter{Limit: 25})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("current: %v %d %v", items, total, err)
	}
	history, total, err := store.List("room", ListFilter{Scope: "history", Limit: 25})
	if err != nil || total != 1 || !history[0].Historical || history[0].SessionID != "OLD" || history[0].Fields["healthPercent"].Status != FreshnessStale {
		t.Fatalf("history: %+v %d %v", history, total, err)
	}
	// Same KU in a fresh save starts with the current zero age and no old vitals.
	apply("Master", "NEW", at.Add(3*time.Minute), stampTelemetryObservation(Observation{ID: "KU_ONE", Name: "Player", GameplayState: GameplayStateSelectingCharacter}, SourceRuntime, at.Add(3*time.Minute)))
	current, err := store.Get("room", "KU_ONE")
	if err != nil || current.Age != 0 || current.HealthPercent != nil || current.Prefab != "" || current.SessionID != "NEW" {
		t.Fatalf("inherited old save values: %+v %v", current, err)
	}
	// Same-session restart preserves history; a real zero overwrites a prior age.
	apply("Master", "NEW", at.Add(4*time.Minute), old)
	apply("Master", "NEW", at.Add(5*time.Minute), stampTelemetryObservation(Observation{ID: "KU_ONE", Name: "Player", Age: 0}, SourceRuntime, at.Add(5*time.Minute)))
	current, _ = store.Get("room", "KU_ONE")
	if current.Age != 0 {
		t.Fatalf("zero age discarded: %+v", current)
	}
	_, total, _ = store.List("room", ListFilter{Scope: "history", Limit: 25})
	if total != 1 {
		t.Fatal("ordinary restart archived the same save")
	}
}

func TestSaveSessionUpgradeAndCrossWorldMigration(t *testing.T) {
	store := newPlayerTestStore(t)
	at := time.Now().UTC()
	obs := stampTelemetryObservation(Observation{ID: "KU_ONE", Name: "Player", Age: 10, GameplayState: GameplayStateAlive}, SourceRuntime, at)
	if err := store.ReplaceWorldSnapshot("room", "Master", "Master", []Observation{obs}, at); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveWorldSession("room", "Master", "M", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("room", "KU_ONE"); err != ErrPlayerNotFound {
		t.Fatal("unscoped legacy record relabeled as current")
	}
	for i, world := range []string{"Master", "Caves", "Master"} {
		session := map[string]string{"Master": "M", "Caves": "C"}[world]
		if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{{WorldID: world, WorldName: world, SessionID: session, ObservedAt: at.Add(time.Duration(i+2) * time.Minute), Source: SourceRuntime, Observations: []Observation{obs}}}); err != nil {
			t.Fatal(err)
		}
		player, err := store.Get("room", "KU_ONE")
		if err != nil || player.WorldID != world || player.SessionID != session {
			t.Fatalf("migration failed: %+v %v", player, err)
		}
	}
	if err := store.ObserveRuntimePresence("room", []string{"Master"}, nil, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	player, _ := store.Get("room", "KU_ONE")
	if player.Fields["age"].Status != FreshnessStale {
		t.Fatal("offline age is live")
	}
	stale, err := store.StaleCount("room")
	if err != nil || stale != 1 {
		t.Fatalf("stale=%d err=%v", stale, err)
	}
	_, online, staleOnline, _, _ := store.Counts("room")
	if online != 0 || staleOnline != 0 {
		t.Fatal("offline history changed safety online counts")
	}
}

type sessionTestProbe struct {
	*playerTestProbe
	session string
	at      time.Time
}

func (p *sessionTestProbe) CurrentSession(context.Context, string, string) (string, time.Time, error) {
	return p.session, p.at, nil
}

func TestPausedPreflightDetectsSaveResetWithoutTelemetryOrPageVisit(t *testing.T) {
	service, runtime, sender, _, probe := newPlayerTestService(t)
	at := time.Now().UTC()
	if err := service.store.ReplaceRoomSnapshots("room", []worldSnapshot{{WorldID: "master", WorldName: "Master", SessionID: "OLD", ObservedAt: at, Observations: []Observation{{ID: "KU_OLD", Name: "Old", Age: 36}}}}); err != nil {
		t.Fatal(err)
	}
	paused := true
	service.runtime = &samplingTestRuntime{playerTestRuntime: runtime, statuses: map[string]shards.RuntimeStatus{"master": {State: shards.RuntimeRunning, Paused: &paused}}}
	service.probe = &sessionTestProbe{playerTestProbe: probe, session: "NEW", at: at.Add(time.Minute)}
	probe.err = errors.New("must not sample paused telemetry")
	active, err := service.PrepareScheduledRefresh(context.Background(), "room", []string{"master"})
	if err != nil || active {
		t.Fatalf("preflight=%t %v", active, err)
	}
	if _, err := service.store.Get("room", "KU_OLD"); err != ErrPlayerNotFound {
		t.Fatal("paused new save kept old player")
	}
	if len(sender.scripts) != 0 {
		t.Fatal("passive reset detection sent console input")
	}
}

func TestCurrentSessionDisconnectDoesNotImportOldCharacter(t *testing.T) {
	store := newPlayerTestStore(t)
	at := time.Now().UTC()
	observation := stampTelemetryObservation(Observation{ID: "KU_ONE", Name: "Current", GameplayState: GameplayStateSelectingCharacter}, SourceRuntime, at)
	if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{{WorldID: "master", WorldName: "Master", SessionID: "NEW", ObservedAt: at, Observations: []Observation{observation}}}); err != nil {
		t.Fatal(err)
	}
	disconnected := at.Add(time.Second)
	old := Observation{ID: "KU_ONE", Name: "Old name", Prefab: "wendy", Age: 36, FirstSeenAt: at.Add(-time.Hour), LastSeenAt: disconnected, LastDisconnectedAt: disconnected}
	if _, err := store.MergeWorldHistory("room", "master", "Master", []Observation{old}, disconnected); err != nil {
		t.Fatal(err)
	}
	player, err := store.Get("room", "KU_ONE")
	if err != nil || player.Online || player.Name != "Current" || player.Prefab != "" || player.Age != 0 || player.FirstSeenAt.Before(at) {
		t.Fatalf("old metadata crossed session: %+v %v", player, err)
	}
}
