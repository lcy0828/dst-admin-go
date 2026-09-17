package players

import (
	"testing"
	"time"
)

func actionRefreshSnapshot(world, local, remote string, state string, health float64, at time.Time) worldSnapshot {
	return worldSnapshot{WorldID: world, WorldName: world, ObservedAt: at, Source: SourceRuntime,
		Observations: stampTelemetryObservations([]Observation{
			{ID: local, Name: local, GameplayState: GameplayStateAlive, Health: &health},
			{ID: remote, Name: remote, GameplayState: state},
		}, SourceRuntime, at)}
}

func TestActionRefreshPreservesPlayersOnUnsampledShard(t *testing.T) {
	for _, target := range []string{"master", "caves"} {
		for _, remoteState := range []string{GameplayStateLoading, GameplayStateSelectingCharacter} {
			t.Run(target+"/"+remoteState, func(t *testing.T) {
				store := newPlayerTestStore(t)
				now := time.Now().UTC().Truncate(time.Second)
				other := "caves"
				if target == other {
					other = "master"
				}
				if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{
					actionRefreshSnapshot(target, "KU_TARGET", "KU_OTHER", remoteState, 150, now),
					actionRefreshSnapshot(other, "KU_OTHER", "KU_TARGET", remoteState, 150, now),
				}); err != nil {
					t.Fatal(err)
				}
				before, err := store.Get("room", "KU_OTHER")
				if err != nil {
					t.Fatal(err)
				}
				if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{
					actionRefreshSnapshot(target, "KU_TARGET", "KU_OTHER", remoteState, 100, now.Add(time.Second)),
				}); err != nil {
					t.Fatal(err)
				}
				after, err := store.Get("room", "KU_OTHER")
				if err != nil || after.GameplayState != GameplayStateAlive || !after.Online || after.WorldID != other || after.PresenceConflict ||
					after.Health == nil || *after.Health != 150 || !after.LastRefreshedAt.Equal(before.LastRefreshedAt) ||
					after.Fields["health"].Status != FreshnessLive || after.Fields["world"].Status != FreshnessLive {
					t.Fatalf("unrelated player's state changed: %#v, %v", after, err)
				}
				updated, err := store.Get("room", "KU_TARGET")
				if err != nil || updated.Health == nil || *updated.Health != 100 || !updated.LastRefreshedAt.Equal(now.Add(time.Second)) {
					t.Fatalf("acted-on player was not refreshed: %#v, %v", updated, err)
				}
			})
		}
	}
}

func TestActionRefreshAcceptsRealMigrationAndOwnerObservation(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, step := range []struct {
		snapshots []worldSnapshot
		world     string
		gameplay  string
		freshness FreshnessStatus
	}{
		{[]worldSnapshot{actionRefreshSnapshot("master", "KU_PLAYER", "KU_OTHER", GameplayStateLoading, 150, now)}, "master", GameplayStateAlive, FreshnessLive},
		// A real entity on the destination is stronger evidence even when the
		// old shard is not part of this refresh.
		{[]worldSnapshot{actionRefreshSnapshot("caves", "KU_PLAYER", "KU_OTHER", GameplayStateLoading, 100, now.Add(time.Second))}, "caves", GameplayStateAlive, FreshnessLive},
		// When the owner itself reports no entity, retain its location as
		// historical and expose the loading state rather than freezing it live.
		{[]worldSnapshot{actionRefreshSnapshot("caves", "KU_OTHER", "KU_PLAYER", GameplayStateLoading, 100, now.Add(2*time.Second))}, "caves", GameplayStateLoading, FreshnessStale},
	} {
		if err := store.ReplaceRoomSnapshots("room", step.snapshots); err != nil {
			t.Fatal(err)
		}
		player, err := store.Get("room", "KU_PLAYER")
		if err != nil || player.WorldID != step.world || player.GameplayState != step.gameplay || player.Fields["health"].Status != step.freshness {
			t.Fatalf("authoritative observation lost: %#v, %v", player, err)
		}
	}
}
