package runtimeaudit

import (
	"context"
	"testing"
	"time"

	"dont/internal/rooms"
	"dont/internal/shards"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type auditRooms struct {
	room   rooms.Room
	worlds []rooms.World
}

func (c auditRooms) List() ([]rooms.Room, error)          { return []rooms.Room{c.room}, nil }
func (c auditRooms) Room(string) (rooms.Room, error)      { return c.room, nil }
func (c auditRooms) Worlds(string) ([]rooms.World, error) { return c.worlds, nil }
func (c auditRooms) World(_, id string) (rooms.World, error) {
	for _, world := range c.worlds {
		if world.ID == id {
			return world, nil
		}
	}
	return rooms.World{}, rooms.ErrWorldNotFound
}

type auditRuntime struct{ status shards.RuntimeStatus }

func (r *auditRuntime) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	return r.status, nil
}

func newAuditService(t *testing.T) (*Service, *auditRuntime, rooms.Room, rooms.World) {
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
	room := rooms.Room{ID: "room", DirectoryName: "Room", Managed: true}
	world := rooms.World{ID: "world", RoomID: room.ID, DirectoryName: "Master", Name: "Master"}
	runtime := &auditRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	service, err := NewService(auditRooms{room: room, worlds: []rooms.World{world}}, runtime, store)
	if err != nil {
		t.Fatal(err)
	}
	return service, runtime, room, world
}

func TestExpectedExitPreservesActionSourceAndReferences(t *testing.T) {
	service, runtime, room, world := newAuditService(t)
	now := time.Date(2026, 8, 13, 8, 30, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	if err := service.RecordAction(ActionRequest{
		RoomID: room.ID, WorldIDs: []string{world.ID}, Action: "stop", Source: SourceAPI, JobID: "job-1", RequestID: "request-1",
	}); err != nil {
		t.Fatal(err)
	}
	runtime.status = shards.RuntimeStatus{State: shards.RuntimeStopped}
	service.recordTransition(room, world,
		observedRuntime{state: shards.RuntimeRunning, sessionExists: true},
		observedRuntime{state: shards.RuntimeStopped, sessionExists: false}, runtime.status)
	list, err := service.List(room.ID, ListFilter{WorldID: world.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 || list.Items[0].Type != EventStopped || list.Items[0].Source != SourceAPI || list.Items[0].JobID != "job-1" || list.Items[0].RequestID != "request-1" {
		t.Fatalf("events = %#v", list.Items)
	}
}

func TestUnexpectedExitIsMarkedExternal(t *testing.T) {
	service, _, room, world := newAuditService(t)
	service.recordTransition(room, world,
		observedRuntime{state: shards.RuntimeRunning, sessionExists: true},
		observedRuntime{state: shards.RuntimeStopped, sessionExists: false}, shards.RuntimeStatus{State: shards.RuntimeStopped})
	list, err := service.List(room.ID, ListFilter{WorldID: world.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Type != EventUnexpectedExit || list.Items[0].Source != SourceExternal || list.Items[0].ReasonCode != "SESSION_DISAPPEARED" {
		t.Fatalf("events = %#v", list.Items)
	}
}
