package rooms

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"dont/shared"
)

func TestRuntimeModePreferencesPersistAndRejectDelayedWrites(t *testing.T) {
	service, _ := newTestService(t)
	store := service.store
	if err := store.RememberRuntimeMode("room", "world", shared.RuntimePerformanceModeLuaJIT, ""); err != nil {
		t.Fatal(err)
	}
	values, err := NewStore(store.db, "test_").RuntimeModePreferences("room")
	if err != nil {
		t.Fatal(err)
	}
	revision := values["world"].Revision
	if values["world"].Mode != shared.RuntimePerformanceModeLuaJIT || revision == "" {
		t.Fatalf("values=%+v", values)
	}
	if err := store.RememberRuntimeMode("room", "world", shared.RuntimePerformanceModeGame, revision); err != nil {
		t.Fatal(err)
	}
	if err := store.RememberRuntimeMode("room", "world", shared.RuntimePerformanceModeLuaJIT, revision); !errors.Is(err, ErrRuntimePreferenceConflict) {
		t.Fatalf("stale update accepted: %v", err)
	}
	if err := store.RememberRuntimeMode("room", "world", shared.RuntimePerformanceModeLuaJIT, ""); !errors.Is(err, ErrRuntimePreferenceConflict) {
		t.Fatalf("duplicate insert accepted: %v", err)
	}
	if err := store.Unadopt("room"); err != nil {
		t.Fatal(err)
	}
	values, err = store.RuntimeModePreferences("room")
	if err != nil || len(values) != 0 {
		t.Fatalf("orphan preference: %+v %v", values, err)
	}
}

func TestWorldRecreationDoesNotInheritDeletedWorldMode(t *testing.T) {
	service, root := newTestService(t)
	room, err := service.Create(CreateRequest{DirectoryName: "runtime_recreate", Name: "模式生命周期", GameMode: "survival", MaxPlayers: 6})
	if err != nil {
		t.Fatal(err)
	}
	world, err := service.CreateWorld(room.ID, CreateWorldRequest{DirectoryName: "Caves2", Type: "cave"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.store.RememberRuntimeMode(room.ID, world.ID, shared.RuntimePerformanceModeLuaJIT, ""); err != nil {
		t.Fatal(err)
	}
	deleted, err := service.DeleteWorld(room.ID, world.ID, DeleteWorldRequest{Confirmation: room.Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, room.DirectoryName, deleted.RecoveryName, "server.ini")); err != nil {
		t.Fatalf("recovery data was lost: %v", err)
	}
	values, err := service.RuntimeModePreferences(room.ID)
	if err != nil || len(values) != 0 {
		t.Fatalf("deleted preference remains: %+v %v", values, err)
	}
	// Simulate a stale record left by a directory managed outside the panel.
	if err := service.store.RememberRuntimeMode(room.ID, world.ID, shared.RuntimePerformanceModeLuaJIT, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateWorld(room.ID, CreateWorldRequest{DirectoryName: "Caves2", Type: "cave"}); err != nil {
		t.Fatal(err)
	}
	values, err = service.RuntimeModePreferences(room.ID)
	if err != nil || len(values) != 0 {
		t.Fatalf("new world inherited old preference: %+v %v", values, err)
	}
}
