package worldstate

import (
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newWorldStateStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "world_state_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStoreKeepsLatestStateAndBoundedHistory(t *testing.T) {
	store := newWorldStateStore(t)
	store.retention = 2
	base := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	for index := 0; index < 3; index++ {
		cycles := index + 40
		performance := index
		if _, err := store.Append(Snapshot{RoomID: "room", WorldID: "master", WorldName: "Master", WorldRole: "master", Season: "autumn", Cycles: &cycles, HostPerformance: &performance, ObservedAt: base.Add(time.Duration(index) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Append(Snapshot{RoomID: "room", WorldID: "caves", WorldName: "Caves", WorldRole: "caves", Season: "winter", ObservedAt: base}); err != nil {
		t.Fatal(err)
	}
	current, err := store.Current("room")
	if err != nil || len(current) != 2 {
		t.Fatalf("current=%#v err=%v", current, err)
	}
	var master Snapshot
	for _, item := range current {
		if item.WorldID == "master" {
			master = item
		}
	}
	if master.Cycles == nil || *master.Cycles != 42 || master.HostPerformance == nil || *master.HostPerformance != 2 {
		t.Fatalf("latest master state = %#v", master)
	}
	history, total, err := store.History("room", "master", 10)
	if err != nil || total != 2 || len(history) != 2 || history[0].Cycles == nil || *history[0].Cycles != 42 {
		t.Fatalf("history=%#v total=%d err=%v", history, total, err)
	}
}
