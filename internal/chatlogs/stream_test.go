package chatlogs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"dont/internal/rooms"
	"dont/shared"
	"github.com/google/uuid"
)

func streamImport(t *testing.T, s *Store, world string, cursor int64, line string, archived bool) {
	t.Helper()
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := rooms.World{ID: world, Name: world}
	g := shared.RuntimeChatLogGeneration{ID: world, FileName: world, Size: cursor, StartedAt: started, UpdatedAt: started, Archived: archived}
	e := parsedChatEntry(t, "room", w, started, cursor, line)
	if _, err := s.ImportGeneration("room", w, g, []parsedEntry{e}, cursor, time.Now()); err != nil {
		t.Fatal(err)
	}
}
func streamWindow(t *testing.T, s *Store, kind Kind, cursor string) StreamWindow {
	t.Helper()
	w, err := s.StreamWindow("room", kind, cursor)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func TestChatJournalCreatesUpdatesFiltersAndSurvivesRestart(t *testing.T) {
	s := newChatStore(t)
	start := streamWindow(t, s, "", "")
	say := streamWindow(t, s, KindSay, "")
	streamImport(t, s, "Master", 10, "[00:00:01]: [Say] (KU_EXAMPLE) Player: hello", false)
	streamImport(t, s, "Caves", 10, "[00:00:01]: [Say] (KU_EXAMPLE) Player: hello", false)
	streamImport(t, s, "Caves", 10, "[00:00:01]: [Say] (KU_EXAMPLE) Player: hello", false)
	streamImport(t, s, "Master", 20, "[00:00:02]: [Announcement] Mod: level up", false)
	streamImport(t, s, "Master", 30, "[00:00:03]: [Whisper] (KU_EXAMPLE) Player: private", false)
	w := streamWindow(t, s, "", start.Cursor)
	if len(w.Items) != 4 {
		t.Fatalf("changes: %#v", w)
	}
	if w.Items[0].Change.Operation != "created" || w.Items[1].Change.Operation != "updated" || w.Items[0].Change.ID != w.Items[1].Change.ID || len(w.Items[1].Change.Entry.Sources) != 2 {
		t.Fatal("cross-shard merge became a new message")
	}
	if w.Items[2].Change.Entry.Content != "Mod: level up" || w.Items[2].Change.Kind != KindAnnouncement {
		t.Fatal("mod message lost")
	}
	filtered := streamWindow(t, s, KindSay, say.Cursor)
	if len(filtered.Items) != 2 {
		t.Fatal("filter leaked announcement or whisper")
	}
	// New Store wrapper on the same database emulates controller restart.
	restarted := NewStore(s.db, "test_")
	if err := restarted.Migrate(); err != nil {
		t.Fatal(err)
	}
	replay := streamWindow(t, restarted, "", w.Items[1].Cursor)
	if len(replay.Items) != 2 || replay.Items[0].Change.Kind != KindAnnouncement {
		t.Fatal("resume lost persisted cursor")
	}
	if len(streamWindow(t, s, "", "").Items) != 0 {
		t.Fatal("new subscription replayed history")
	}
	if streamWindow(t, s, KindSay, filtered.Cursor).More {
		t.Fatal("filtered cursor did not advance")
	}
	for _, invalid := range []string{"bad", say.Cursor, strings.Repeat("x", 161), strings.Replace(start.Cursor, ".0", ".-1", 1)} {
		if _, err := s.StreamWindow("room", "", invalid); !errors.Is(err, ErrInvalidStreamCursor) {
			t.Fatalf("accepted invalid cursor %q: %v", invalid, err)
		}
	}
	if _, err := s.StreamWindow("other", "", start.Cursor); !errors.Is(err, ErrInvalidStreamCursor) {
		t.Fatal("cross-room cursor accepted")
	}
}
func TestChatJournalRollbackDoesNotPublish(t *testing.T) {
	s := newChatStore(t)
	start := streamWindow(t, s, "", "")
	wakes := 0
	s.notify = func(string) { wakes++ }
	if err := s.db.Exec("CREATE TRIGGER fail_stream_checkpoint BEFORE INSERT ON " + s.generationsTable + " BEGIN SELECT RAISE(FAIL, 'test failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	w := rooms.World{ID: "Master"}
	g := shared.RuntimeChatLogGeneration{ID: "g", Size: 10, StartedAt: started}
	e := parsedChatEntry(t, "room", w, started, 10, "[00:00:01]: [Announcement] test")
	if _, err := s.ImportGeneration("room", w, g, []parsedEntry{e}, 10, started); err == nil {
		t.Fatal("expected checkpoint error")
	}
	after := streamWindow(t, s, "", start.Cursor)
	if wakes != 0 || len(after.Items) != 0 || after.Cursor != start.Cursor {
		t.Fatal("rolled back data published")
	}
}
func TestChatJournalRetentionTruncationAndReset(t *testing.T) {
	for _, size := range []int{10, 60000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := newChatStore(t)
			start := streamWindow(t, s, "", "")
			// Imported in bounded batches; exercises real transactional retention.
			n := streamMaxRecords + 3
			if size > 10 {
				n = 80
			}
			started := time.Now()
			w := rooms.World{ID: "Master"}
			g := shared.RuntimeChatLogGeneration{ID: "g", Size: int64(n), StartedAt: started}
			for offset := 0; offset < n; offset += 100 {
				entries := []parsedEntry{}
				for i := offset; i < min(offset+100, n); i++ {
					entries = append(entries, parsedChatEntry(t, "room", w, started, int64(i+1), "[00:00:01]: [Announcement] "+strings.Repeat("a", size)+fmt.Sprint(i)))
				}
				if _, err := s.ImportGeneration("room", w, g, entries, int64(min(offset+100, n)), time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			state, err := s.streamState("room")
			if err != nil {
				t.Fatal(err)
			}
			if state.Head-state.Floor > streamMaxRecords || state.Bytes > streamMaxBytes || state.Floor == 0 {
				t.Fatalf("unbounded state %#v", state)
			}
			gap := streamWindow(t, s, "", start.Cursor)
			if gap.Signal != "chat.gap" || !gap.More {
				t.Fatal("missing retention gap")
			}
			kept := streamWindow(t, s, "", gap.Cursor)
			if len(kept.Items) == 0 {
				t.Fatal("retained replay missing")
			}
			streamImport(t, s, "Caves", 100000, "[00:00:02]: [Announcement] "+strings.Repeat("中", 30000), true)
			tail := streamWindow(t, s, "", encodeChatCursor(state, cursorScope("room", ""), state.Head))
			if len(tail.Items) != 1 || !tail.Items[0].Change.Truncated || !tail.Items[0].Change.Historical {
				t.Fatal("oversize or archive not explicit")
			}
			if err := s.db.Table(s.streamsTable).Where("room_id = ?", "room").UpdateColumn("epoch", uuid.NewString()).Error; err != nil {
				t.Fatal(err)
			}
			if streamWindow(t, s, "", kept.Cursor).Signal != "chat.reset" {
				t.Fatal("missing replacement reset")
			}
		})
	}
}
func TestChatCollectorsAreSharedBoundedAndStop(t *testing.T) {
	s := newChatStore(t)
	reader := &blockingGenerationReader{started: make(chan struct{}), release: make(chan struct{})}
	service, err := NewService(persistentChatCatalog{room: rooms.Room{ID: "room", Managed: true}, worlds: []rooms.World{{ID: "Master"}}}, chatSnapshots{}, WithPersistence(s, reader))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	subscriptions := []*Subscription{}
	for i := 0; i < streamMaxPerRoom; i++ {
		sub, err := service.Subscribe(context.Background(), "room", "", "")
		if err != nil {
			t.Fatal(err)
		}
		subscriptions = append(subscriptions, sub)
	}
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("collector did not start")
	}
	if _, err := service.Subscribe(context.Background(), "room", "", ""); !errors.Is(err, ErrStreamBusy) {
		t.Fatal("subscription cap not enforced")
	}
	service.feedsMu.Lock()
	count := len(service.feeds)
	service.feedsMu.Unlock()
	if count != 1 {
		t.Fatal("duplicate collector")
	}
	for _, sub := range subscriptions {
		sub.Close()
		sub.Close()
	}
	select {
	case <-subscriptions[0].Done:
	case <-time.After(time.Second):
		t.Fatal("last unsubscribe did not cancel")
	}
	service.Close()
	service.feedsMu.Lock()
	defer service.feedsMu.Unlock()
	if len(service.feeds) != 0 || service.feedListeners != 0 {
		t.Fatal("leaked feed")
	}
}

func TestChatJournalHistoryCorrectionAndDeletion(t *testing.T) {
	s := newChatStore(t)
	started := time.Now().Add(-time.Hour)
	world := rooms.World{ID: "Master", Name: "Master"}
	generation := shared.RuntimeChatLogGeneration{ID: "estimated", FileName: "archive.txt", Archived: true, StartedAtEstimated: true, StartedAt: started, Size: 10}
	entry := parsedChatEntry(t, "room", world, started, 10, "[00:00:01]: [Announcement] history")
	entry.TimeEstimated = true
	if _, err := s.ImportGeneration("room", world, generation, []parsedEntry{entry}, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	before := streamWindow(t, s, "", "")
	entry.TimeEstimated = false
	entry.timeVersion = 1
	corrected := started.Add(time.Minute)
	entry.OccurredAt = &corrected
	if _, err := s.ImportGeneration("room", world, generation, []parsedEntry{entry}, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	changed := streamWindow(t, s, "", before.Cursor)
	if len(changed.Items) != 1 || changed.Items[0].Change.Operation != "updated" || !changed.Items[0].Change.Historical || !changed.Items[0].Change.Entry.OccurredAt.Equal(corrected) {
		t.Fatalf("correction not published: %#v", changed)
	}
	generation.ID = "precise"
	generation.StartedAtEstimated = false
	if err := s.ReconcileGeneration("room", world, generation); err != nil {
		t.Fatal(err)
	}
	deleted := streamWindow(t, s, "", changed.Cursor)
	if len(deleted.Items) != 1 || deleted.Items[0].Change.Operation != "deleted" || deleted.Items[0].Change.Entry != nil {
		t.Fatalf("deletion not published: %#v", deleted)
	}
}

func TestChatStreamKeepsHealthyShardVisibleWithOfflineAgent(t *testing.T) {
	s := newChatStore(t)
	reader := &generationReaderFixture{worlds: map[string][]generationFixture{}, errors: map[string]error{"Caves": errors.New("synthetic offline Agent")}}
	service, err := NewService(persistentChatCatalog{room: rooms.Room{ID: "room", Managed: true}, worlds: []rooms.World{{ID: "Master"}, {ID: "Caves"}}}, chatSnapshots{}, WithPersistence(s, reader))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	result, err := service.SyncRoom(context.Background(), "room")
	if err != nil || result.AvailableWorlds != 1 || len(result.Problems) == 0 {
		t.Fatal("healthy shard incorrectly marked unavailable", result, err)
	}
	sub, err := service.Subscribe(context.Background(), "room", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		if status := service.StreamStatus("room"); status.State == "partial" && status.Partial {
			break
		}
		select {
		case <-sub.Wake:
		case <-timer.C:
			t.Fatal("offline Agent was not exposed as partial coverage")
		}
	}
	service.Close()
	select {
	case <-sub.Done:
	default:
		t.Fatal("shutdown did not end active subscription")
	}
}
