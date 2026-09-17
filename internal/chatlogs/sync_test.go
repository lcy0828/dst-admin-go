package chatlogs

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/logstream"
	"dont/internal/rooms"
	"dont/shared"
)

type persistentChatCatalog struct {
	room   rooms.Room
	worlds []rooms.World
}

func (c persistentChatCatalog) Room(string) (rooms.Room, error) { return c.room, nil }
func (c persistentChatCatalog) List() ([]rooms.Room, error)     { return []rooms.Room{c.room}, nil }
func (c persistentChatCatalog) Worlds(string) ([]rooms.World, error) {
	return append([]rooms.World(nil), c.worlds...), nil
}

type generationFixture struct {
	generation shared.RuntimeChatLogGeneration
	lines      []shared.RuntimeLogLine
}

type generationReaderFixture struct {
	worlds map[string][]generationFixture
	errors map[string]error
}

type blockingGenerationReader struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingGenerationReader) ListChatLogGenerations(ctx context.Context, _, _ string) ([]shared.RuntimeChatLogGeneration, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	select {
	case <-r.release:
		return []shared.RuntimeChatLogGeneration{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *blockingGenerationReader) ReadChatLogGeneration(context.Context, string, string, shared.RuntimeChatLogRequest) (shared.RuntimeChatLogResult, error) {
	return shared.RuntimeChatLogResult{}, errors.New("unexpected generation read")
}

func (f *generationReaderFixture) ListChatLogGenerations(_ context.Context, _ string, worldID string) ([]shared.RuntimeChatLogGeneration, error) {
	if err := f.errors[worldID]; err != nil {
		return nil, err
	}
	result := make([]shared.RuntimeChatLogGeneration, 0, len(f.worlds[worldID]))
	for _, fixture := range f.worlds[worldID] {
		result = append(result, fixture.generation)
	}
	return result, nil
}

func (f *generationReaderFixture) ReadChatLogGeneration(_ context.Context, _ string, worldID string, request shared.RuntimeChatLogRequest) (shared.RuntimeChatLogResult, error) {
	for _, fixture := range f.worlds[worldID] {
		if fixture.generation.ID != request.GenerationID {
			continue
		}
		lines := make([]shared.RuntimeLogLine, 0)
		cursor := request.Cursor
		for _, line := range fixture.lines {
			if line.Cursor <= request.Cursor {
				continue
			}
			lines = append(lines, line)
			cursor = line.Cursor
		}
		generation := fixture.generation
		return shared.RuntimeChatLogResult{Generation: &generation, Cursor: cursor, Lines: lines, Complete: cursor == generation.Size}, nil
	}
	return shared.RuntimeChatLogResult{}, errors.New("generation missing")
}

func TestPersistentServiceBackfillsArchivesAfterRestart(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", Name: "Master", Role: rooms.WorldRoleMaster}
	catalog := persistentChatCatalog{room: room, worlds: []rooms.World{master}}
	started := time.Date(2026, time.August, 23, 19, 8, 35, 0, time.UTC)
	reader := &generationReaderFixture{worlds: map[string][]generationFixture{
		master.ID: {{
			generation: shared.RuntimeChatLogGeneration{ID: "current", FileName: "server_chat_log.txt", Size: 50, StartedAt: started, UpdatedAt: started.Add(time.Minute)},
			lines:      []shared.RuntimeLogLine{{Cursor: 50, Text: "[00:06:03]: [Say] (KU_ONE) lcy: before restart"}},
		}},
	}, errors: map[string]error{}}
	service, err := NewService(catalog, chatSnapshots{}, WithPersistence(store, reader))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := service.SyncRoom(context.Background(), room.ID); err != nil || result.Imported != 1 {
		t.Fatalf("initial sync=%#v err=%v", result, err)
	}

	archive := reader.worlds[master.ID][0]
	archive.generation.Archived = true
	archive.generation.FileName = "server_chat_log_2026-08-23-20-00-00.txt"
	afterStarted := started.Add(time.Hour)
	reader.worlds[master.ID] = []generationFixture{archive, {
		generation: shared.RuntimeChatLogGeneration{ID: "after-restart", FileName: "server_chat_log.txt", Size: 48, StartedAt: afterStarted, UpdatedAt: afterStarted.Add(time.Minute)},
		lines:      []shared.RuntimeLogLine{{Cursor: 48, Text: "[00:00:05]: [Say] (KU_ONE) lcy: after restart"}},
	}}
	// A new service instance simulates the management process restarting while
	// keeping the same SQLite database.
	restarted, err := NewService(catalog, chatSnapshots{}, WithPersistence(store, reader))
	if err != nil {
		t.Fatal(err)
	}
	list, err := restarted.List(context.Background(), room.ID, Filter{Limit: 20})
	if err != nil || list.Total != 2 || list.SyncState != "review" || list.UncertainTimes != 2 {
		t.Fatalf("restarted list=%#v err=%v", list, err)
	}
}

func TestPersistentServiceKeepsSavedHistoryWhenOneWorldIsOffline(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", Name: "Master", Role: rooms.WorldRoleMaster}
	caves := rooms.World{ID: "caves", Name: "Caves", Role: rooms.WorldRoleCaves}
	catalog := persistentChatCatalog{room: room, worlds: []rooms.World{master, caves}}
	started := time.Date(2026, time.August, 23, 19, 8, 35, 0, time.UTC)
	reader := &generationReaderFixture{worlds: map[string][]generationFixture{
		master.ID: {{
			generation: shared.RuntimeChatLogGeneration{ID: "master", FileName: "server_chat_log.txt", Size: 40, StartedAt: started, UpdatedAt: started},
			lines:      []shared.RuntimeLogLine{{Cursor: 40, Text: "[00:00:01]: [Join Announcement] lcy"}},
		}},
	}, errors: map[string]error{caves.ID: errors.New("Agent 离线")}}
	service, err := NewService(catalog, chatSnapshots{value: logstream.RoomSnapshot{}}, WithPersistence(store, reader))
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), room.ID, Filter{Limit: 20})
	if err != nil || list.Total != 1 || list.SyncState != "partial" || list.SyncMessage == "" {
		t.Fatalf("partial list=%#v err=%v", list, err)
	}
}

func TestPersistentServiceFallsBackToCurrentLogsForOlderAgent(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}
	catalog := persistentChatCatalog{room: room, worlds: []rooms.World{master}}
	started := time.Date(2026, time.August, 23, 19, 8, 35, 0, time.UTC)
	reader := &generationReaderFixture{worlds: map[string][]generationFixture{}, errors: map[string]error{master.ID: errors.New("unsupported action")}}
	current := chatSnapshots{value: logstream.RoomSnapshot{
		RoomID: room.ID, Available: 1, Worlds: []logstream.WorldSnapshot{{
			WorldID: master.ID, WorldName: master.Name, WorldRole: master.Role,
			Snapshot: &logstream.Snapshot{FileName: "server_chat_log.txt", Size: 45, StartedAt: started, UpdatedAt: started.Add(time.Minute), Lines: []logstream.Line{{
				Cursor: 45, Text: "[00:00:01]: [Say] (KU_ONE) lcy: legacy agent",
			}}},
		}},
	}}
	service, err := NewService(catalog, current, WithPersistence(store, reader))
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), room.ID, Filter{Limit: 20})
	if err != nil || list.Total != 1 || list.SyncState != "partial" || list.Items[0].Content != "legacy agent" {
		t.Fatalf("fallback list=%#v err=%v", list, err)
	}
}

func TestPersistentServiceDoesNotMarkTruncatedFallbackCaughtUp(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}
	started := time.Date(2026, time.August, 23, 19, 8, 35, 0, time.UTC)
	reader := &generationReaderFixture{worlds: map[string][]generationFixture{}, errors: map[string]error{master.ID: errors.New("unsupported action")}}
	current := chatSnapshots{value: logstream.RoomSnapshot{
		RoomID: room.ID, Available: 1, Worlds: []logstream.WorldSnapshot{{
			WorldID: master.ID, WorldName: master.Name, WorldRole: master.Role,
			Snapshot: &logstream.Snapshot{
				FileName: "server_chat_log.txt", Size: 4096, StartedAt: started,
				UpdatedAt: started.Add(time.Minute), Truncated: true,
				Lines: []logstream.Line{{Cursor: 4096, Text: "[00:00:01]: [Say] (KU_ONE) lcy: tail"}},
			},
		}},
	}}
	service, err := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{master}}, current, WithPersistence(store, reader))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SyncRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	generationID := fallbackGenerationID(room.DirectoryName, master.DirectoryName, started, started.Add(time.Minute))
	state, err := store.Generation(room.ID, master.ID, generationID)
	if err != nil || state.Cursor != 0 || state.CaughtUp {
		t.Fatalf("truncated fallback was treated as complete: %#v err=%v", state, err)
	}
	list, err := store.List(room.ID, Filter{Limit: 10})
	if err != nil || list.Total != 1 || list.Items[0].Content != "tail" {
		t.Fatalf("truncated fallback tail was not retained: %#v err=%v", list, err)
	}
}

func TestPersistentServiceListDoesNotWaitForBackgroundSync(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Name: "Room", Managed: true}
	master := rooms.World{ID: "master", Name: "Master", Role: rooms.WorldRoleMaster}
	reader := &blockingGenerationReader{started: make(chan struct{}), release: make(chan struct{})}
	service, err := NewService(
		persistentChatCatalog{room: room, worlds: []rooms.World{master}},
		chatSnapshots{}, WithPersistence(store, reader),
	)
	if err != nil {
		t.Fatal(err)
	}
	syncDone := make(chan error, 1)
	go func() {
		_, err := service.SyncRoom(context.Background(), room.ID)
		syncDone <- err
	}()
	<-reader.started

	listDone := make(chan error, 1)
	go func() {
		_, err := service.List(context.Background(), room.ID, Filter{Limit: 10})
		listDone <- err
	}()
	select {
	case err := <-listDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("chat list waited for an existing background sync")
	}
	close(reader.release)
	if err := <-syncDone; err != nil {
		t.Fatal(err)
	}
}
