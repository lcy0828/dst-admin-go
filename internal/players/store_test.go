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
