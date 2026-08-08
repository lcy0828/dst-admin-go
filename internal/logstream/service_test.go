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
	room  rooms.Room
	world rooms.World
}

func (c testCatalog) Room(string) (rooms.Room, error)           { return c.room, nil }
func (c testCatalog) World(string, string) (rooms.World, error) { return c.world, nil }

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
