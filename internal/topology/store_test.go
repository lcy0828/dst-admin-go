package topology

import (
	"errors"
	"strings"
	"testing"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newTopologyTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "topology_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStoreEnsureReturnsPersistentCreateError(t *testing.T) {
	store := newTopologyTestStore(t)
	if err := store.db.Exec("CREATE TRIGGER topology_fail_insert BEFORE INSERT ON " + store.table + " BEGIN SELECT RAISE(ABORT, 'forced insert failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	_, err := store.Ensure("room", []string{"master"})
	if err == nil || !strings.Contains(err.Error(), "forced insert failure") {
		t.Fatalf("persistent create error=%v", err)
	}
}

func TestStoreReconcilesWorldsAndProtectsRevision(t *testing.T) {
	store := newTopologyTestStore(t)
	initial, err := store.Ensure("room", []string{"master", "caves"})
	if err != nil {
		t.Fatal(err)
	}
	if initial.Revision == "" || len(initial.Placements) != 2 || initial.Placements[0].DesiredTargetID != localTargetID {
		t.Fatalf("initial=%#v", initial)
	}
	updated := append([]storedPlacement(nil), initial.Placements...)
	updated[0].DesiredTargetID = "agent:node-a"
	saved, err := store.Save("room", initial.Revision, updated)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision == initial.Revision || saved.Placements[0].AppliedTargetID != localTargetID {
		t.Fatalf("saved=%#v", saved)
	}
	if _, err := store.Save("room", initial.Revision, updated); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale save error=%v", err)
	}
	reconciled, err := store.Ensure("room", []string{"caves", "moon"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled.Placements) != 2 || reconciled.Placements[0].WorldID != "caves" || reconciled.Placements[1].WorldID != "moon" {
		t.Fatalf("reconciled=%#v", reconciled)
	}
	if reconciled.Placements[1].DesiredTargetID != localTargetID || reconciled.Placements[1].AppliedTargetID != localTargetID {
		t.Fatalf("new world placement=%#v", reconciled.Placements[1])
	}
}
