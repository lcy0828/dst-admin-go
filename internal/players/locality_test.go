package players

import (
	"context"
	"testing"
	"time"

	"dont/internal/dstruntime"
)

func TestPresenceCountsOnlyLocalEntitiesButKeepsSelectingClientsOnline(t *testing.T) {
	s, reader, _ := newInventoryLocalityService(t, 0, "master")
	value := reader.values["master"]
	value.Players[0].GameplayState = GameplayStateAlive
	reader.values["master"] = value
	result, err := s.RefreshPresence(context.Background(), "room")
	if err != nil || !result.Fresh || result.Online != 1 || !result.Worlds[0].CountKnown || result.Worlds[0].Count != 1 || result.Worlds[1].Count != 0 {
		t.Fatal(result, err)
	}
	player, err := s.store.Get("room", inventoryTestPlayer)
	if err != nil || !player.WorldConfirmed || player.WorldID != "master" || player.PresenceConflict || len(player.ObservedWorldIDs) != 1 {
		t.Fatal(player, err)
	}
	// Selecting and migrating connections without entities remain online.
	later := s.now().Add(time.Second)
	s.now = func() time.Time { return later }
	for world, value := range reader.values {
		value.CapturedAt = later
		value.Players[0].LocalEntity = entityEvidence(false)
		value.Players[0].GameplayState = GameplayStateSelectingCharacter
		reader.values[world] = value
	}
	result, err = s.RefreshPresence(context.Background(), "room")
	if err != nil || !result.Fresh || result.Online != 1 || result.UnlocatedOnline != 1 || result.Worlds[0].Count != 0 || result.Worlds[1].Count != 0 {
		t.Fatal(result, err)
	}
	player, err = s.store.Get("room", inventoryTestPlayer)
	if err != nil || player.WorldConfirmed || player.WorldID != "master" || player.PresenceConflict || len(player.ObservedWorldIDs) != 0 || player.Fields["world"].Status != FreshnessStale {
		t.Fatal(player, err)
	}
}

func TestLocalEntityFlagOverridesLegacyVitalsAndStateInference(t *testing.T) {
	value := float64(90)
	obs := Observation{ID: "KU_ONE", Name: "Player", GameplayState: GameplayStateAlive, HealthPercent: &value, LocalEntity: entityEvidence(false)}
	if candidateHasLocalPlayer(snapshotCandidate{observation: obs}) {
		t.Fatal("explicit nonlocal evidence was ignored")
	}
	obs.LocalEntity = nil
	if !candidateHasLocalPlayer(snapshotCandidate{observation: obs}) {
		t.Fatal("legacy evidence compatibility lost")
	}
	decoded := observationsFromRuntime(dstruntime.Snapshot{CapturedAt: time.Now(), Players: []dstruntime.SnapshotPlayer{{ID: "KU_ONE", Name: "Player", LocalEntity: entityEvidence(false)}}})
	if len(decoded) != 1 || decoded[0].LocalEntity == nil || *decoded[0].LocalEntity {
		t.Fatal(decoded)
	}
}

func TestClusterObserverIsIndependentOfLastKnownWorld(t *testing.T) {
	store := newPlayerTestStore(t)
	at := time.Now().UTC()
	local := stampTelemetryObservation(Observation{ID: "KU_ONE", Name: "Player", LocalEntity: entityEvidence(true), GameplayState: GameplayStateAlive}, SourceRuntime, at)
	if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{{WorldID: "master", WorldName: "Master", ObservedAt: at, Source: SourceRuntime, Observations: []Observation{local}}}); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Second)
	cluster := stampTelemetryObservation(Observation{ID: "KU_ONE", Name: "Player", LocalEntity: entityEvidence(false), GameplayState: GameplayStateUnknown}, SourceRuntime, at)
	if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{
		{WorldID: "master", WorldName: "Master", ObservedAt: at, Stopped: true},
		{WorldID: "caves", WorldName: "Caves", ObservedAt: at, Source: SourceRuntime, Observations: []Observation{cluster}},
	}); err != nil {
		t.Fatal(err)
	}
	p, err := store.Get("room", "KU_ONE")
	if err != nil || !p.Online || p.PresenceStatus != FreshnessLive || p.WorldID != "master" || p.WorldConfirmed {
		t.Fatal(p, err)
	}
	// An old world stopping or becoming unreachable cannot invalidate a newer
	// connection observation from a different shard.
	if err := store.ObserveRuntimePresence("room", []string{"master"}, nil, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	p, _ = store.Get("room", "KU_ONE")
	if !p.Online || p.PresenceStatus != FreshnessLive {
		t.Fatal(p)
	}
	if err := store.MarkWorldStale("room", "caves"); err != nil {
		t.Fatal(err)
	}
	p, _ = store.Get("room", "KU_ONE")
	if !p.Online || p.PresenceStatus != FreshnessStale {
		t.Fatal(p)
	}
	// The actual observer's complete empty list settles the old connection.
	if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{{WorldID: "caves", WorldName: "Caves", ObservedAt: at.Add(2 * time.Second), Source: SourceRuntime}}); err != nil {
		t.Fatal(err)
	}
	p, _ = store.Get("room", "KU_ONE")
	if p.Online {
		t.Fatal(p)
	}
}

func TestPresenceTwoPlayersOnDifferentShardsAndMigrationLocation(t *testing.T) {
	s, reader, _ := newInventoryLocalityService(t, 0, "master")
	for world, value := range reader.values {
		value.Players = append(value.Players, dstruntime.SnapshotPlayer{ID: "KU_SECOND", Name: "Second", LocalEntity: entityEvidence(world == "caves"), GameplayState: GameplayStateGhost})
		reader.values[world] = value
	}
	result, err := s.RefreshPresence(context.Background(), "room")
	if err != nil || result.Online != 2 || result.UnlocatedOnline != 0 || result.Worlds[0].Count != 1 || result.Worlds[1].Count != 1 {
		t.Fatal(result, err)
	}
	value := reader.values["master"]
	value.CapturedAt = value.CapturedAt.Add(time.Second)
	value.Players[0].GameplayState = GameplayStateMigrating
	reader.values["master"] = value
	if _, err := s.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.store.Get("room", inventoryTestPlayer)
	if !p.Online || p.WorldConfirmed || p.GameplayState != GameplayStateMigrating {
		t.Fatal(p)
	}
}
