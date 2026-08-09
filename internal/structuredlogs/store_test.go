package structuredlogs

import (
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newStructuredLogStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "structured_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStoreReplacesWorldSnapshotAndEscapesSearchWildcards(t *testing.T) {
	store := newStructuredLogStore(t)
	now := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	counts, snapshot, err := store.Counts("room", "master")
	if err != nil || len(counts) != 0 || snapshot.State != SnapshotStateUninitialized || snapshot.UpdatedAt != nil || snapshot.LastRefreshedAt != nil {
		t.Fatalf("initial counts=%#v snapshot=%#v err=%v", counts, snapshot, err)
	}
	first := []Entry{
		{Type: TypeSystem, Content: "loading 100% complete", RawContent: "loading 100% complete", SourceCursor: 10},
		{Type: TypeWarning, Content: "old warning", RawContent: "old warning", SourceCursor: 20},
	}
	if err := store.ReplaceWorldSnapshot("room", "master", "Master", first, now); err != nil {
		t.Fatal(err)
	}
	items, total, err := store.List("room", ListFilter{Query: "%", Limit: 10})
	if err != nil || total != 1 || len(items) != 1 || items[0].Content != "loading 100% complete" {
		t.Fatalf("literal wildcard query = %#v, total=%d, err=%v", items, total, err)
	}
	if err := store.ReplaceWorldSnapshot("room", "master", "Master", []Entry{{Type: TypeError, Content: "new failure", RawContent: "new failure", SourceCursor: 5}}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	items, total, err = store.List("room", ListFilter{Limit: 10})
	if err != nil || total != 1 || len(items) != 1 || items[0].Content != "new failure" {
		t.Fatalf("replacement snapshot = %#v, total=%d, err=%v", items, total, err)
	}
	counts, snapshot, err = store.Counts("room", "master")
	if err != nil || counts[TypeError] != 1 || snapshot.State != SnapshotStateReady || snapshot.LastRefreshedAt == nil || !snapshot.LastRefreshedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("counts=%#v snapshot=%#v err=%v", counts, snapshot, err)
	}
	if err := store.ReplaceWorldSnapshot("room", "caves", "Caves", []Entry{{Type: TypeWarning, Content: "cave warning", RawContent: "cave warning", SourceCursor: 6}}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	masterCounts, masterSnapshot, err := store.Counts("room", "master")
	if err != nil || masterCounts[TypeError] != 1 || masterCounts[TypeWarning] != 0 || masterSnapshot.State != SnapshotStateReady || masterSnapshot.LastRefreshedAt == nil || !masterSnapshot.LastRefreshedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("master counts=%#v snapshot=%#v err=%v", masterCounts, masterSnapshot, err)
	}
	cavesCounts, cavesSnapshot, err := store.Counts("room", "caves")
	if err != nil || cavesCounts[TypeWarning] != 1 || cavesCounts[TypeError] != 0 || cavesSnapshot.State != SnapshotStateReady || cavesSnapshot.LastRefreshedAt == nil || !cavesSnapshot.LastRefreshedAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("caves counts=%#v snapshot=%#v err=%v", cavesCounts, cavesSnapshot, err)
	}
}

func TestStoreRuleLifecycleProtectsBuiltIns(t *testing.T) {
	store := newStructuredLogStore(t)
	custom, err := store.CreateRule(Rule{ID: "custom", RoomID: "room", Name: "Custom", LogType: TypeWorld, Pattern: "moon", Enabled: true, Priority: 10})
	if err != nil {
		t.Fatal(err)
	}
	custom.Enabled = false
	custom.Priority = 20
	updated, err := store.UpdateRule(custom)
	if err != nil || updated.Enabled || updated.Priority != 20 {
		t.Fatalf("updated rule = %#v, %v", updated, err)
	}
	if err := store.DeleteRule("room", custom.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Rule("room", custom.ID); err != ErrRuleNotFound {
		t.Fatalf("deleted rule error = %v", err)
	}
	builtIn, err := store.CreateRule(Rule{ID: "built-in", RoomID: "room", Name: "Built in", LogType: TypeSystem, Pattern: "server", Enabled: true, BuiltIn: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRule("room", builtIn.ID); err != ErrBuiltInRule {
		t.Fatalf("built-in delete error = %v", err)
	}
}
