package chatlogs

import (
	"context"
	"dont/internal/rooms"
	"dont/shared"
	"testing"
	"time"
)

type timedGenerationFixture struct {
	*generationReaderFixture
	at time.Time
}

func (f timedGenerationFixture) ReadChatLogGeneration(ctx context.Context, r, w string, q shared.RuntimeChatLogRequest) (shared.RuntimeChatLogResult, error) {
	value, err := f.generationReaderFixture.ReadChatLogGeneration(ctx, r, w, q)
	value.TimeVersion = shared.ChatTimeVersion
	value.TimesReady = true
	for _, line := range value.Lines {
		at := f.at
		value.Times = append(value.Times, shared.RuntimeChatLogTime{Cursor: line.Cursor, OccurredAt: &at})
	}
	return value, err
}
func TestRepairReplaysEOFAddsPlainAnnouncementsAndCorrectsExistingDates(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Managed: true}
	world := rooms.World{ID: "master", Role: rooms.WorldRoleMaster}
	start := time.Date(2026, 9, 6, 15, 51, 17, 0, time.UTC)
	g := shared.RuntimeChatLogGeneration{ID: "boot", FileName: "server_chat_log.txt", StartedAt: start, UpdatedAt: start.Add(6 * 24 * time.Hour), Size: 200, TimeVersion: shared.ChatTimeVersion}
	text := "[06:07:28]: [Say] (KU_ONE) ql: 你下来的话带点加血的给我"
	old := parsedChatEntry(t, room.ID, world, start, 100, text)
	if _, err := store.ImportGeneration(room.ID, world, g, []parsedEntry{old}, 200, start); err != nil {
		t.Fatal(err)
	}
	correct := start.Add(4*24*time.Hour + 6*time.Hour + 7*time.Minute + 28*time.Second)
	reader := timedGenerationFixture{generationReaderFixture: &generationReaderFixture{worlds: map[string][]generationFixture{world.ID: {{generation: g, lines: []shared.RuntimeLogLine{{Cursor: 100, Text: text}, {Cursor: 200, Text: "[06:07:28]: [Announcement] 世界已保存"}}}}}}, at: correct}
	svc, err := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{world}}, chatSnapshots{}, WithPersistence(store, reader))
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		result, err := svc.RepairRoom(context.Background(), room.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantImported := 1
		if pass == 1 {
			wantImported = 0
		}
		if result.Imported != wantImported {
			t.Fatalf("pass %d result=%+v", pass, result)
		}
		list, err := store.List(room.ID, Filter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if list.Total != 2 || list.SyncState != "ready" || list.Counts[KindAnnouncement] != 1 {
			t.Fatalf("list=%+v", list)
		}
		for _, item := range list.Items {
			if item.OccurredAt == nil || !item.OccurredAt.Equal(correct) {
				t.Fatalf("date=%v", item.OccurredAt)
			}
		}
	}
}

func TestRepairCheckpointRollbackAndPreservesLiveCursor(t *testing.T) {
	store := newChatStore(t)
	world := rooms.World{ID: "master", Role: rooms.WorldRoleMaster}
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	g := shared.RuntimeChatLogGeneration{ID: "boot", FileName: "server_chat_log.txt", StartedAt: start, UpdatedAt: start.Add(5 * 24 * time.Hour), Size: 300}
	entry := parsedChatEntry(t, "room", world, start, 100, "[00:00:01]: [Say] (KU_ONE) lcy: same")
	if _, err := store.ImportGeneration("room", world, g, []parsedEntry{entry}, 300, start); err != nil {
		t.Fatal(err)
	}
	entry.timeVersion = parserVersion
	correct := start.Add(4*24*time.Hour + time.Second)
	entry.OccurredAt = &correct
	if err := store.db.Exec("CREATE TRIGGER repair_fail BEFORE UPDATE ON " + store.generationsTable + " BEGIN SELECT RAISE(FAIL, 'checkpoint failed'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := store.importGeneration("room", world, g, []parsedEntry{entry}, 100, start, batchProgress{version: parserVersion, repair: true}); err == nil {
		t.Fatal("checkpoint failure ignored")
	}
	list, _ := store.List("room", Filter{Limit: 10})
	if list.Items[0].OccurredAt.Equal(correct) {
		t.Fatal("time committed without checkpoint")
	}
	store.db.Exec("DROP TRIGGER repair_fail")
	if _, err := store.importGeneration("room", world, g, []parsedEntry{entry}, 100, start, batchProgress{version: parserVersion, repair: true}); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Generation("room", world.ID, g.ID)
	if state.Cursor != 300 || state.RepairCursor != 100 || state.ParserVersion != 0 {
		t.Fatalf("progress=%+v", state)
	}
	entry.Content = "changed text"
	if _, err := store.importGeneration("room", world, g, []parsedEntry{entry}, 200, start, batchProgress{version: parserVersion, repair: true}); err == nil {
		t.Fatal("source conflict overwritten")
	}
	list, _ = store.List("room", Filter{Limit: 10})
	if list.Items[0].Content != "same" {
		t.Fatal("original body lost")
	}
}

func TestMissingGenerationsRetainMessagesAndReportReview(t *testing.T) {
	store := newChatStore(t)
	world := rooms.World{ID: "master"}
	start := time.Now().UTC()
	g := shared.RuntimeChatLogGeneration{ID: "old", FileName: "server_chat_log.txt", StartedAt: start, UpdatedAt: start, Size: 100}
	entry := parsedChatEntry(t, "room", world, start, 100, "[00:00:00]: [Say] (KU_ONE) lcy: retained")
	store.ImportGeneration("room", world, g, []parsedEntry{entry}, 100, start)
	if err := store.observeGenerations("room", world.ID, nil); err != nil {
		t.Fatal(err)
	}
	store.MarkSync("room", "ready", "", &start)
	list, err := store.List("room", Filter{Limit: 10})
	if err != nil || list.Total != 1 || list.UnavailableGenerations != 1 || list.PendingGenerations != 0 || list.SyncState != "review" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
}

// One committed chunk before cancellation; restarting the service must resume
// that checkpoint while keeping an append beyond the old EOF.
type cancelingChatReader struct {
	timedGenerationFixture
	cancel  context.CancelFunc
	reads   int
	cursors []int64
}

func (f *cancelingChatReader) ReadChatLogGeneration(ctx context.Context, r, w string, q shared.RuntimeChatLogRequest) (shared.RuntimeChatLogResult, error) {
	f.cursors = append(f.cursors, q.Cursor)
	result, err := f.timedGenerationFixture.ReadChatLogGeneration(ctx, r, w, q)
	if len(result.Lines) > 1 {
		result.Lines = result.Lines[:1]
		result.Times = result.Times[:1]
		result.Cursor = result.Lines[0].Cursor
		result.Complete = result.Cursor == result.Generation.Size
	}
	f.reads++
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	return result, err
}
func TestRepairCancellationResumesAndAcceptsNewMessages(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Managed: true}
	world := rooms.World{ID: "master", Role: rooms.WorldRoleMaster}
	start := time.Now().UTC()
	g := shared.RuntimeChatLogGeneration{ID: "boot", FileName: "server_chat_log.txt", StartedAt: start, UpdatedAt: start.Add(time.Hour), Size: 200, TimeVersion: shared.ChatTimeVersion}
	lines := []shared.RuntimeLogLine{{Cursor: 100, Text: "[00:00:01]: [Say] (KU_ONE) lcy: old one"}, {Cursor: 200, Text: "[00:00:02]: [Say] (KU_ONE) lcy: old two"}}
	old := []parsedEntry{parsedChatEntry(t, room.ID, world, start, 100, lines[0].Text), parsedChatEntry(t, room.ID, world, start, 200, lines[1].Text)}
	store.ImportGeneration(room.ID, world, g, old, 200, start)
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelingChatReader{timedGenerationFixture: timedGenerationFixture{generationReaderFixture: &generationReaderFixture{worlds: map[string][]generationFixture{world.ID: {{generation: g, lines: lines}}}}, at: start.Add(time.Second)}, cancel: cancel}
	catalog := persistentChatCatalog{room: room, worlds: []rooms.World{world}}
	svc, _ := NewService(catalog, chatSnapshots{}, WithPersistence(store, reader))
	if _, err := svc.RepairRoom(ctx, room.ID); err == nil {
		t.Fatal("cancellation ignored")
	}
	state, _ := store.Generation(room.ID, world.ID, g.ID)
	if state.RepairCursor != 100 || state.Cursor != 200 {
		t.Fatalf("checkpoint=%+v", state)
	}
	fixture := &reader.worlds[world.ID][0]
	fixture.generation.Size = 300
	fixture.lines = append(fixture.lines, shared.RuntimeLogLine{Cursor: 300, Text: "[00:00:03]: [Say] (KU_ONE) lcy: live"})
	reader.cursors = nil
	restarted, _ := NewService(catalog, chatSnapshots{}, WithPersistence(store, reader))
	if _, err := restarted.RepairRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	if len(reader.cursors) < 2 || reader.cursors[0] != 200 || reader.cursors[1] != 100 {
		t.Fatalf("live append not prioritized before repair: %v", reader.cursors)
	}
	list, _ := store.List(room.ID, Filter{Limit: 10})
	if list.Total != 3 || list.PendingGenerations != 0 {
		t.Fatalf("list=%+v", list)
	}
}
