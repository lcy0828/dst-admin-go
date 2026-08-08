package console

import (
	"context"
	"strings"
	"testing"

	"dont/internal/rooms"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type testRooms struct{ managed bool }

func (r testRooms) Room(id string) (rooms.Room, error) {
	return rooms.Room{ID: id, DirectoryName: "room", Name: "周末服", Managed: r.managed}, nil
}
func (testRooms) World(roomID, worldID string) (rooms.World, error) {
	return rooms.World{ID: worldID, RoomID: roomID, DirectoryName: "Master", Name: "Master"}, nil
}

type captureSender struct {
	script string
	err    error
}

func (s *captureSender) Send(_ context.Context, _, _, script string) error {
	s.script = script
	return s.err
}

func newConsoleService(t *testing.T, sender *captureSender) *Service {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(testRooms{managed: true}, sender, store)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestBuiltinArgumentsAreLuaQuotedAndRunIsPersisted(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	run, err := service.Execute(context.Background(), "room-id", "world-id", ExecuteRequest{
		CommandID: "announce", Arguments: map[string]interface{}{"message": `hello\"); c_shutdown()`},
	})
	if err != nil || run.Status != RunSent || run.LogQuery != run.ID {
		t.Fatalf("run = %#v, %v", run, err)
	}
	if !strings.Contains(sender.script, `c_announce("hello\\\"); c_shutdown()")`) {
		t.Fatalf("unsafe or unexpected script: %s", sender.script)
	}
	runs, total, err := service.Runs(ListFilter{RoomID: "room-id"})
	if err != nil || total != 1 || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("runs = %#v, %d, %v", runs, total, err)
	}
}

func TestCriticalAndRawCommandsRequireExactRoomConfirmation(t *testing.T) {
	service := newConsoleService(t, &captureSender{})
	if _, err := service.Execute(context.Background(), "room", "world", ExecuteRequest{CommandID: "rollback", Arguments: map[string]interface{}{"days": float64(1)}}); err != ErrConfirmationNeeded {
		t.Fatalf("rollback confirmation error = %v", err)
	}
	if _, err := service.ExecuteRaw(context.Background(), "room", "world", RawRequest{Command: "c_save()", Confirmation: "wrong"}); err != ErrConfirmationNeeded {
		t.Fatalf("raw confirmation error = %v", err)
	}
	if _, err := service.ExecuteRaw(context.Background(), "room", "world", RawRequest{Command: "c_save()", Confirmation: "周末服"}); err != nil {
		t.Fatalf("raw command: %v", err)
	}
}
