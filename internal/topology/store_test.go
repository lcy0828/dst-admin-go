package topology

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dont/internal/rooms"

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

func TestStoreRetainsRemoteWorldWhenLocalDirectoryDisappears(t *testing.T) {
	store := newTopologyTestStore(t)
	world := rooms.World{ID: "master", DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}
	initial, err := store.EnsureWorlds("room", []rooms.World{world})
	if err != nil {
		t.Fatal(err)
	}
	placements := append([]storedPlacement(nil), initial.Placements...)
	placements[0].DesiredTargetID = "agent:node"
	placements[0].AppliedTargetID = "agent:node"
	if _, err := store.Save("room", initial.Revision, placements); err != nil {
		t.Fatal(err)
	}
	retained, err := store.EnsureWorlds("room", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(retained.Placements) != 1 || retained.Placements[0].WorldDirectoryName != "Master" || retained.Placements[0].WorldRole != rooms.WorldRoleMaster {
		t.Fatalf("retained=%#v", retained)
	}
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

func TestStoreMigratePreservesLegacyCPUAllocation(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	store := NewStore(db, "legacy_test_")
	if err := db.Exec(`CREATE TABLE legacy_test_cpu_allocation (
		id varchar(64) PRIMARY KEY,
		environment_id varchar(64) NOT NULL,
		target_id varchar(160) NOT NULL,
		room_id varchar(255) NOT NULL,
		world_id varchar(255) NOT NULL,
		policy varchar(24) NOT NULL,
		logical_cpu_ids TEXT NOT NULL,
		physical_core_keys TEXT NOT NULL,
		allow_smt_sibling_risk bool,
		warnings TEXT NOT NULL,
		created_at datetime,
		updated_at datetime
	)`).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	if err := db.Exec(`INSERT INTO legacy_test_cpu_allocation (
		id, environment_id, target_id, room_id, world_id, policy,
		logical_cpu_ids, physical_core_keys, allow_smt_sibling_risk,
		warnings, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		allocationResourceID("room", "master"), "environment-local", localTargetID,
		"room", "master", string(CPUPolicyShared), "[]", "[]", false, "[]", now, now,
	).Error; err != nil {
		t.Fatal(err)
	}

	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate legacy CPU allocation: %v", err)
	}
	allocation, err := store.CPUAllocation("room", "master")
	if err != nil {
		t.Fatal(err)
	}
	if allocation.Policy != CPUPolicyShared || allocation.ExecutionState != CPUExecutionDesired || allocation.Observed != nil || allocation.ExecutionError != "" {
		t.Fatalf("migrated allocation=%#v", allocation)
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
