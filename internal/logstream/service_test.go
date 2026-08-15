package logstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/rooms"
)

type testCatalog struct {
	room   rooms.Room
	world  rooms.World
	worlds []rooms.World
}

func (c testCatalog) Room(string) (rooms.Room, error)           { return c.room, nil }
func (c testCatalog) World(string, string) (rooms.World, error) { return c.world, nil }
func (c testCatalog) Worlds(string) ([]rooms.World, error) {
	if len(c.worlds) > 0 {
		return c.worlds, nil
	}
	return []rooms.World{c.world}, nil
}

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	worldPath := filepath.Join(root, "room", "Master")
	if err := os.MkdirAll(worldPath, 0750); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(root, testCatalog{
		room:  rooms.Room{ID: rooms.EncodeID("room"), DirectoryName: "room"},
		world: rooms.World{ID: rooms.EncodeID("Master"), DirectoryName: "Master"},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.pollInterval = 5 * time.Millisecond
	return service, filepath.Join(worldPath, "server_log.txt")
}

func TestRoomSnapshotKeepsAvailableWorldsWhenAnotherLogIsMissing(t *testing.T) {
	root := t.TempDir()
	masterID := rooms.EncodeID("Master")
	cavesID := rooms.EncodeID("Caves")
	for _, name := range []string{"Master", "Caves"} {
		if err := os.MkdirAll(filepath.Join(root, "room", name), 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "room", "Master", "server_log.txt"), []byte("master ready\n"), 0640); err != nil {
		t.Fatal(err)
	}
	catalog := testCatalog{
		room: rooms.Room{ID: rooms.EncodeID("room"), DirectoryName: "room"},
		worlds: []rooms.World{
			{ID: masterID, DirectoryName: "Master", Name: "地面", Role: rooms.WorldRoleMaster},
			{ID: cavesID, DirectoryName: "Caves", Name: "洞穴", Role: rooms.WorldRoleCaves},
		},
	}
	service, err := NewService(root, roomSnapshotCatalog{testCatalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.RoomSnapshot(context.Background(), catalog.room.ID, 10, "ready")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial || result.Available != 1 || result.Unavailable != 1 || len(result.Worlds) != 2 {
		t.Fatalf("room snapshot summary = %#v", result)
	}
	if result.Worlds[0].Snapshot == nil || len(result.Worlds[0].Snapshot.Lines) != 1 || result.Worlds[0].Problem != nil {
		t.Fatalf("master snapshot = %#v", result.Worlds[0])
	}
	if result.Worlds[1].Snapshot != nil || result.Worlds[1].Problem == nil || result.Worlds[1].Problem.Code != "LOG_NOT_FOUND" {
		t.Fatalf("caves snapshot = %#v", result.Worlds[1])
	}
}

type roomSnapshotCatalog struct{ testCatalog }

func (c roomSnapshotCatalog) World(_ string, worldID string) (rooms.World, error) {
	for _, world := range c.worlds {
		if world.ID == worldID {
			return world, nil
		}
	}
	return rooms.World{}, rooms.ErrWorldNotFound
}

func TestSnapshotFiltersAndDoesNotCreateMissingLogs(t *testing.T) {
	service, path := newTestService(t)
	if _, err := service.Snapshot("room", "world", 10, ""); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("missing log error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("snapshot created a missing log: %v", err)
	}
	if err := os.WriteFile(path, []byte("alpha\nbeta warning\ngamma\n"), 0640); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot("room", "world", 10, "WARNING")
	if err != nil || len(snapshot.Lines) != 1 || snapshot.Lines[0].Text != "beta warning" {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
}

func TestReadTailStopsAtCapturedFileSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_log.txt")
	first := "first line\n"
	if err := os.WriteFile(path, []byte(first+"appended later\n"), 0640); err != nil {
		t.Fatal(err)
	}
	lines, truncated, err := readTail(path, int64(len(first)), 10, "")
	if err != nil || truncated || len(lines) != 1 || lines[0].Text != "first line" {
		t.Fatalf("bounded tail = %#v, truncated=%v, err=%v", lines, truncated, err)
	}
}

func TestFollowEmitsAppendAndStopsWithContext(t *testing.T) {
	service, path := newTestService(t)
	if err := os.WriteFile(path, []byte("ready\n"), 0640); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 4)
	followErrors := make(chan error, 1)
	go func() {
		followErrors <- service.Follow(ctx, "room", "world", 10, func(event Event) error {
			events <- event
			return nil
		})
	}()
	if event := <-events; event.Type != "connected" || event.Snapshot == nil || len(event.Snapshot.Lines) != 1 {
		t.Fatalf("connected event = %#v", event)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("next line\n")
	_ = file.Close()
	select {
	case event := <-events:
		if event.Type != "line" || event.Line == nil || event.Line.Text != "next line" {
			t.Fatalf("line event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for appended line")
	}
	cancel()
	if err := <-followErrors; !errors.Is(err, context.Canceled) {
		t.Fatalf("follow error = %v", err)
	}
}
