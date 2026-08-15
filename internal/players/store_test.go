package players

import (
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newPlayerTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStoreSnapshotPreservesHistoryAndMarksMissingPlayersOffline(t *testing.T) {
	store := newPlayerTestStore(t)
	first := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	health := 75.0
	if err := store.ReplaceWorldSnapshot("room", "master", "地面", []Observation{{
		ID: "KU_ONE", Name: "Willow", Prefab: "willow", HealthPercent: &health,
	}}, first); err != nil {
		t.Fatal(err)
	}
	second := first.Add(time.Minute)
	if err := store.ReplaceWorldSnapshot("room", "master", "地面", []Observation{}, second); err != nil {
		t.Fatal(err)
	}
	player, err := store.Get("room", "KU_ONE")
	if err != nil {
		t.Fatal(err)
	}
	if player.Online || !player.FirstSeenAt.Equal(first) || !player.LastSeenAt.Equal(first) || !player.StatusChangedAt.Equal(second) {
		t.Fatalf("history was not preserved: %#v", player)
	}
	items, total, err := store.List("room", ListFilter{Query: "will", Status: "offline", Limit: 25})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("filtered list failed: total=%d items=%#v err=%v", total, items, err)
	}
}

func TestStoreMovesPlayerBetweenWorldsWithoutDuplicateIdentity(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Now().UTC()
	observation := []Observation{{ID: "KU_ONE", Name: "Wilson", Prefab: "wilson"}}
	if err := store.ReplaceWorldSnapshot("room", "master", "地面", observation, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWorldSnapshot("room", "caves", "洞穴", observation, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	items, total, err := store.List("room", ListFilter{Limit: 25})
	if err != nil || total != 1 || len(items) != 1 || items[0].WorldID != "caves" || !items[0].Online {
		t.Fatalf("player move produced an invalid state: total=%d items=%#v err=%v", total, items, err)
	}
}

func TestRoomSnapshotsChooseNewestWorldForMigratingPlayer(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if err := store.ReplaceWorldSnapshot("room", "master", "地面", []Observation{{ID: "KU_ONE", Name: "Wilson"}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{
		{WorldID: "master", WorldName: "地面", ObservedAt: now.Add(time.Second), Observations: []Observation{{ID: "KU_ONE", Name: "Wilson"}}},
		{WorldID: "caves", WorldName: "洞穴", ObservedAt: now.Add(2 * time.Second), Observations: []Observation{{ID: "KU_ONE", Name: "Wilson"}}},
	}); err != nil {
		t.Fatal(err)
	}
	player, err := store.Get("room", "KU_ONE")
	if err != nil || !player.Online || player.WorldID != "caves" || !player.LastRefreshedAt.Equal(now.Add(2*time.Second)) ||
		!player.PresenceConflict || len(player.ObservedWorldIDs) != 2 {
		t.Fatalf("newest shard did not win migration: player=%#v err=%v", player, err)
	}
}

func TestRoomSnapshotsKeepExistingWorldWhenCaptureTimesTie(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if err := store.ReplaceWorldSnapshot("room", "master", "地面", []Observation{{ID: "KU_ONE", Name: "Wilson"}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{
		{WorldID: "caves", WorldName: "洞穴", ObservedAt: now.Add(time.Second), Observations: []Observation{{ID: "KU_ONE", Name: "Wilson"}}},
		{WorldID: "master", WorldName: "地面", ObservedAt: now.Add(time.Second), Observations: []Observation{{ID: "KU_ONE", Name: "Wilson"}}},
	}); err != nil {
		t.Fatal(err)
	}
	player, err := store.Get("room", "KU_ONE")
	if err != nil || player.WorldID != "master" || !player.PresenceConflict || len(player.ObservedWorldIDs) != 2 {
		t.Fatalf("tie did not preserve existing shard: player=%#v err=%v", player, err)
	}
}

func TestRoomSnapshotsDoNotReportOldWorldAsPresenceConflict(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{
		{WorldID: "master", WorldName: "地面", ObservedAt: now, Observations: []Observation{{ID: "KU_ONE", Name: "Wilson"}}},
		{WorldID: "caves", WorldName: "洞穴", ObservedAt: now.Add(30 * time.Second), Observations: []Observation{{ID: "KU_ONE", Name: "Wilson"}}},
	}); err != nil {
		t.Fatal(err)
	}
	player, err := store.Get("room", "KU_ONE")
	if err != nil || player.WorldID != "caves" || player.PresenceConflict || len(player.ObservedWorldIDs) != 1 || player.ObservedWorldIDs[0] != "caves" {
		t.Fatalf("old world was reported as a live conflict: player=%#v err=%v", player, err)
	}
}

func TestEmptyOtherWorldSnapshotDoesNotOverwriteLocalPlayer(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Now().UTC()
	netScore := 0
	if err := store.ReplaceWorldSnapshot("room", "master", "地面", []Observation{{
		ID: "KU_ONE", Name: "Wilson", Prefab: "wilson", NetScore: &netScore,
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWorldSnapshot("room", "caves", "洞穴", []Observation{}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	player, err := store.Get("room", "KU_ONE")
	if err != nil || !player.Online || player.WorldID != "master" || player.NetScore == nil || *player.NetScore != 0 {
		t.Fatalf("other world snapshot overwrote local player: player=%#v err=%v", player, err)
	}
}

func TestPartialNativeObservationPreservesKnownPlayerDetails(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	if err := store.ReplaceWorldSnapshot("room", "master", "地面", []Observation{{
		ID: "KU_ONE", Name: "Willow", Prefab: "wendy", Age: 42, NetID: "7656119", Admin: true,
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWorldSnapshot("room", "master", "地面", []Observation{{
		ID: "KU_ONE", Name: "Willow", Admin: false,
	}}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	player, err := store.Get("room", "KU_ONE")
	if err != nil || player.Prefab != "wendy" || player.Age != 42 || player.NetID != "7656119" || player.Admin {
		t.Fatalf("partial native observation erased known details: player=%#v err=%v", player, err)
	}
}

func TestStoreMergesHistoricalPlayersAsOfflineWithoutOverwritingCurrentState(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	netScore := 0
	if err := store.ReplaceWorldSnapshot("room", "master", "地面", []Observation{{
		ID: "KU_CURRENT", Name: "Willow", Prefab: "willow", NetScore: &netScore,
	}}, now); err != nil {
		t.Fatal(err)
	}
	inserted, err := store.MergeWorldHistory("room", "caves", "洞穴", []Observation{
		{ID: "KU_CURRENT", Name: "Stale", Prefab: "wilson"},
		{ID: "KU_HISTORY", Name: "Wendy", Prefab: "wendy"},
	}, now.Add(time.Minute))
	if err != nil || inserted != 1 {
		t.Fatalf("historical merge failed: inserted=%d err=%v", inserted, err)
	}
	current, err := store.Get("room", "KU_CURRENT")
	if err != nil || !current.Online || current.Name != "Willow" || current.WorldID != "master" || current.NetScore == nil {
		t.Fatalf("historical merge overwrote current state: player=%#v err=%v", current, err)
	}
	historical, err := store.Get("room", "KU_HISTORY")
	if err != nil || historical.Online || historical.WorldID != "caves" || historical.NetScore != nil || historical.Performance != nil {
		t.Fatalf("historical player was not stored as offline: player=%#v err=%v", historical, err)
	}
}

func TestStorePersistsAndFindsExpiredBans(t *testing.T) {
	store := newPlayerTestStore(t)
	now := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	store.now = func() time.Time { return now }
	if err := store.SaveBan(Ban{
		RoomID: "room", PlayerID: "KU_ONE", Reason: "测试原因", Duration: "1h", CreatedAt: now, ExpiresAt: &expiresAt,
	}); err != nil {
		t.Fatal(err)
	}
	bans, err := store.Bans("room")
	if err != nil || bans["KU_ONE"].Reason != "测试原因" {
		t.Fatalf("ban was not persisted: %#v err=%v", bans, err)
	}
	expired, err := store.ExpiredBans(expiresAt.Add(time.Second))
	if err != nil || len(expired) != 1 || expired[0].PlayerID != "KU_ONE" {
		t.Fatalf("expired ban was not found: %#v err=%v", expired, err)
	}
	if err := store.DeleteBan("room", "KU_ONE"); err != nil {
		t.Fatal(err)
	}
	bans, err = store.Bans("room")
	if err != nil || len(bans) != 0 {
		t.Fatalf("ban was not deleted: %#v err=%v", bans, err)
	}
}
