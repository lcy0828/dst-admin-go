package players

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHistoryRoutesCrossShardDisconnectToActualWorldEvenWhenSnapshotFails(t *testing.T) {
	service, _, _, _, probe := newPlayerTestService(t)
	at := service.now().Add(-time.Minute)
	probe.err = errors.New("telemetry unavailable")
	probe.historyItems = []Observation{{ID: "KU_ONE", Name: "Player", FirstSeenAt: at, LastSeenAt: at.Add(5 * time.Second), LastDisconnectedAt: at.Add(5 * time.Second), HistoryWorldName: "Caves"}}
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err == nil {
		t.Fatal("snapshot failure was hidden")
	}
	p, err := service.store.Get("room", "KU_ONE")
	if err != nil || p.WorldID != "caves" || p.WorldName != "洞穴" || p.Online || !p.LastSeenAt.Equal(at.Add(5*time.Second)) {
		t.Fatalf("cross-shard history=%#v err=%v", p, err)
	}
}

func TestHistoryRecoversMissedShortConnectionWithoutErasingVitals(t *testing.T) {
	store := newPlayerTestStore(t)
	at := time.Date(2026, 9, 10, 15, 45, 59, 0, time.UTC)
	health := 22.0
	obs := stampTelemetryObservation(Observation{ID: "KU_ONE", Name: "Player", Prefab: "wendy", Health: &health, GameplayState: GameplayStateAlive}, SourceRuntime, at)
	if err := store.ReplaceWorldSnapshot("room", "caves", "Caves", []Observation{obs}, at); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWorldSnapshot("room", "caves", "Caves", nil, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	joined := at.Add(time.Hour)
	left := joined.Add(5 * time.Second)
	_, err := store.MergeWorldHistory("room", "master", "Master", []Observation{{
		ID: "KU_ONE", Name: "Player", FirstSeenAt: at.Add(-time.Hour), LastSeenAt: left, LastConnectedAt: joined, LastDisconnectedAt: left,
	}}, left.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := store.Get("room", "KU_ONE")
	if p.Online || p.WorldID != "master" || !p.WorldConfirmed || !p.LastSeenAt.Equal(left) || p.LastSeenSource != SourceNativeLog || p.LastDisconnectedAt == nil || !p.LastDisconnectedAt.Equal(left) || p.Health == nil || *p.Health != 22 || p.Prefab != "wendy" {
		t.Fatalf("recovered=%#v", p)
	}
}

func TestHistoryNeverInventsSeenTimesOrReplacesNewerOnlineState(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Now().UTC()
	_, err := store.MergeWorldHistory("room", "master", "Master", []Observation{{ID: "KU_OLD", Name: "Old"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := store.Get("room", "KU_OLD")
	if !p.FirstSeenAt.IsZero() || !p.LastSeenAt.IsZero() || p.WorldConfirmed {
		t.Fatalf("import fabricated presence=%#v", p)
	}
	obs := stampTelemetryObservation(Observation{ID: "KU_ONE", Name: "Current", Prefab: "willow", GameplayState: GameplayStateAlive}, SourceRuntime, now)
	store.ReplaceWorldSnapshot("room", "caves", "Caves", []Observation{obs}, now)
	old := now.Add(-time.Hour)
	store.MergeWorldHistory("room", "master", "Master", []Observation{{ID: "KU_ONE", Name: "Old", Prefab: "wendy", FirstSeenAt: old, LastSeenAt: old, LastDisconnectedAt: old, Fields: FieldStates{"prefab": historicalField(old)}}}, now)
	p, _ = store.Get("room", "KU_ONE")
	if !p.Online || p.Name != "Current" || p.Prefab != "willow" || p.WorldID != "caves" || !p.LastSeenAt.Equal(now) {
		t.Fatalf("history replaced live evidence=%#v", p)
	}
}

func TestSelectingClientDoesNotAcquireFictitiousCavesLocation(t *testing.T) {
	store := newPlayerTestStore(t)
	at := time.Now().UTC().Add(-time.Minute)
	obs := stampTelemetryObservation(Observation{ID: "KU_ONE", Name: "Selecting", GameplayState: GameplayStateSelectingCharacter}, SourceRuntime, at)
	store.ReplaceRoomSnapshots("room", []worldSnapshot{
		{WorldID: "master", WorldName: "Master", Source: SourceRuntime, ObservedAt: at, Observations: []Observation{obs}},
		{WorldID: "caves", WorldName: "Caves", Source: SourceRuntime, ObservedAt: at.Add(time.Second), Observations: []Observation{obs}},
	})
	p, _ := store.Get("room", "KU_ONE")
	if p.WorldConfirmed {
		t.Fatalf("cluster client acquired local evidence=%#v", p)
	}
	store.MergeWorldHistory("room", "master", "Master", []Observation{{ID: "KU_ONE", Name: "Selecting", FirstSeenAt: at, LastSeenAt: at, LastConnectedAt: at}}, at.Add(time.Second))
	p, _ = store.Get("room", "KU_ONE")
	if !p.WorldConfirmed || p.WorldID != "master" {
		t.Fatalf("authentication did not establish location=%#v", p)
	}
	// The next cluster-wide sample must preserve the last confirmed world.
	store.ReplaceRoomSnapshots("room", []worldSnapshot{{WorldID: "caves", WorldName: "Caves", Source: SourceRuntime, ObservedAt: at.Add(2 * time.Second), Observations: []Observation{obs}}})
	p, _ = store.Get("room", "KU_ONE")
	if p.WorldID != "master" || p.Fields["world"].Status != FreshnessStale {
		t.Fatalf("global client replaced known world=%#v", p)
	}
}

func TestHistoryEnrichesExistingUnknownCharacterAcrossRefreshes(t *testing.T) {
	store := newPlayerTestStore(t)
	at := time.Now().UTC().Add(-time.Hour)
	store.MergeWorldHistory("room", "master", "Master", []Observation{{ID: "KU_ONE", Name: "Player", FirstSeenAt: at, LastSeenAt: at}}, time.Now())
	store.MergeWorldHistory("room", "master", "Master", []Observation{{ID: "KU_ONE", Name: "Player", Prefab: "wendy", FirstSeenAt: at, LastSeenAt: at.Add(time.Second), Fields: FieldStates{"prefab": historicalField(at.Add(time.Second))}}}, time.Now())
	p, _ := store.Get("room", "KU_ONE")
	if p.Prefab != "wendy" || !p.FirstSeenAt.Equal(at) {
		t.Fatalf("existing history was skipped=%#v", p)
	}
}

func TestHistoryIdentityLimitIsIndependentOfOnlineLimit(t *testing.T) {
	values := make([]Observation, 65)
	for i := range values {
		values[i] = Observation{ID: string(rune('A' + i)), Name: "Player"}
	}
	// Use valid, distinct identifiers for all 65 historical players.
	for i := range values {
		values[i].ID = "KU_" + time.Unix(int64(i), 0).Format("150405")
	}
	if err := validateHistoryObservations(values); err != nil {
		t.Fatal(err)
	}
	if err := validateObservations(values); err == nil {
		t.Fatal("online limit no longer enforced")
	}
}
