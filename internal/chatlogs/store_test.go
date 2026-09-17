package chatlogs

import (
	"testing"
	"time"

	"dont/internal/logstream"
	"dont/internal/rooms"
	"dont/shared"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func TestStorePersistsIdempotentCrossShardChatHistory(t *testing.T) {
	store := newChatStore(t)
	roomID := "room"
	master := rooms.World{ID: "master", Name: "Master", Role: rooms.WorldRoleMaster}
	caves := rooms.World{ID: "caves", Name: "Caves", Role: rooms.WorldRoleCaves}
	masterStarted := time.Date(2026, time.August, 23, 19, 8, 35, 0, time.UTC)
	cavesStarted := time.Date(2026, time.August, 23, 19, 10, 24, 0, time.UTC)
	masterEntry := parsedChatEntry(t, roomID, master, masterStarted, 50, "[00:06:03]: [Say] (KU_ONE) lcy: 123")
	cavesEntry := parsedChatEntry(t, roomID, caves, cavesStarted, 48, "[00:04:15]: [Say] (KU_ONE) lcy: 123")
	masterGeneration := shared.RuntimeChatLogGeneration{ID: "master-generation", FileName: "server_chat_log.txt", Size: 50, StartedAt: masterStarted, UpdatedAt: masterStarted.Add(time.Minute)}
	cavesGeneration := shared.RuntimeChatLogGeneration{ID: "caves-generation", FileName: "server_chat_log.txt", Size: 48, StartedAt: cavesStarted, UpdatedAt: cavesStarted.Add(time.Minute)}

	if count, err := store.ImportGeneration(roomID, master, masterGeneration, []parsedEntry{masterEntry}, 50, time.Now()); err != nil || count != 1 {
		t.Fatalf("master import count=%d err=%v", count, err)
	}
	if count, err := store.ImportGeneration(roomID, master, masterGeneration, []parsedEntry{masterEntry}, 50, time.Now()); err != nil || count != 0 {
		t.Fatalf("master replay count=%d err=%v", count, err)
	}
	if count, err := store.ImportGeneration(roomID, caves, cavesGeneration, []parsedEntry{cavesEntry}, 48, time.Now()); err != nil || count != 1 {
		t.Fatalf("caves import count=%d err=%v", count, err)
	}
	syncedAt := time.Now().UTC()
	if err := store.MarkSync(roomID, "ready", "", &syncedAt); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(roomID, Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || len(list.Items) != 1 || len(list.Items[0].Sources) != 2 || list.Counts[KindSay] != 1 {
		t.Fatalf("persisted list=%#v", list)
	}
	if list.Items[0].Sources[0].WorldRole != rooms.WorldRoleMaster || list.SyncState != "pending" || !list.HistoryAvailable {
		t.Fatalf("persisted metadata=%#v", list)
	}
	if err := store.MarkSync(roomID, "partial", "agent offline", nil); err != nil {
		t.Fatal(err)
	}
	partial, err := store.SyncState(roomID)
	if err != nil || partial.LastSyncedAt == nil || partial.State != "partial" {
		t.Fatalf("partial sync state=%#v err=%v", partial, err)
	}
	filtered, err := store.List(roomID, Filter{WorldID: caves.ID, Query: "123", Kind: KindSay, Limit: 10})
	if err != nil || filtered.Total != 1 {
		t.Fatalf("filtered=%#v err=%v", filtered, err)
	}
	state, err := store.Generation(roomID, master.ID, masterGeneration.ID)
	if err != nil || state.Cursor != 50 || !state.CaughtUp {
		t.Fatalf("generation state=%#v err=%v", state, err)
	}
}

func TestStoreDoesNotMergeAmbiguousRepeatedMessages(t *testing.T) {
	store := newChatStore(t)
	roomID := "room"
	master := rooms.World{ID: "master", Name: "Master", Role: rooms.WorldRoleMaster}
	caves := rooms.World{ID: "caves", Name: "Caves", Role: rooms.WorldRoleCaves}
	started := time.Date(2026, time.August, 23, 19, 0, 0, 0, time.UTC)
	masterGeneration := shared.RuntimeChatLogGeneration{ID: "m", FileName: "server_chat_log.txt", Size: 100, StartedAt: started, UpdatedAt: started}
	cavesGeneration := shared.RuntimeChatLogGeneration{ID: "c", FileName: "server_chat_log.txt", Size: 100, StartedAt: started, UpdatedAt: started}
	masterEntries := []parsedEntry{
		parsedChatEntry(t, roomID, master, started, 20, "[00:00:10]: [Say] (KU_ONE) lcy: same"),
		parsedChatEntry(t, roomID, master, started, 40, "[00:00:12]: [Say] (KU_ONE) lcy: same"),
	}
	if _, err := store.ImportGeneration(roomID, master, masterGeneration, masterEntries, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	// This source is exactly between both messages. Keeping a separate event is
	// safer than dropping a genuine repeated message.
	caveEntry := parsedChatEntry(t, roomID, caves, started, 20, "[00:00:11]: [Say] (KU_ONE) lcy: same")
	if _, err := store.ImportGeneration(roomID, caves, cavesGeneration, []parsedEntry{caveEntry}, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(roomID, Filter{Limit: 10})
	if err != nil || list.Total != 3 {
		t.Fatalf("ambiguous list=%#v err=%v", list, err)
	}
}

func TestStoreGenerationCursorNeverMovesBackward(t *testing.T) {
	store := newChatStore(t)
	world := rooms.World{ID: "master", Name: "Master", Role: rooms.WorldRoleMaster}
	started := time.Date(2026, time.August, 23, 19, 0, 0, 0, time.UTC)
	generation := shared.RuntimeChatLogGeneration{
		ID: "generation", FileName: "server_chat_log.txt", Size: 100,
		StartedAt: started, UpdatedAt: started.Add(time.Minute),
	}
	if _, err := store.ImportGeneration("room", world, generation, nil, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	stale := generation
	stale.Size = 50
	stale.UpdatedAt = started.Add(30 * time.Second)
	if _, err := store.ImportGeneration("room", world, stale, nil, 50, time.Now()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Generation("room", world.ID, generation.ID)
	if err != nil || state.Cursor != 100 || state.Size != 100 || !state.CaughtUp {
		t.Fatalf("generation state regressed: %#v err=%v", state, err)
	}
}

func TestStoreRollsBackEntriesWhenGenerationCheckpointFails(t *testing.T) {
	store := newChatStore(t)
	world := rooms.World{ID: "master", Name: "Master", Role: rooms.WorldRoleMaster}
	started := time.Date(2026, time.August, 23, 19, 0, 0, 0, time.UTC)
	generation := shared.RuntimeChatLogGeneration{
		ID: "generation", FileName: "server_chat_log.txt", Size: 50,
		StartedAt: started, UpdatedAt: started.Add(time.Minute),
	}
	entry := parsedChatEntry(t, "room", world, started, 50, "[00:00:10]: [Say] (KU_ONE) lcy: durable")
	trigger := "CREATE TRIGGER test_fail_chat_checkpoint BEFORE INSERT ON " + store.generationsTable + " BEGIN SELECT RAISE(FAIL, 'checkpoint failed'); END"
	if err := store.db.Exec(trigger).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportGeneration("room", world, generation, []parsedEntry{entry}, 50, time.Now()); err == nil {
		t.Fatal("checkpoint failure did not abort the import")
	}
	if err := store.db.Exec("DROP TRIGGER test_fail_chat_checkpoint").Error; err != nil {
		t.Fatal(err)
	}
	if count, err := store.ImportGeneration("room", world, generation, []parsedEntry{entry}, 50, time.Now()); err != nil || count != 1 {
		t.Fatalf("replay after rollback count=%d err=%v", count, err)
	}
	list, err := store.List("room", Filter{Limit: 10})
	if err != nil || list.Total != 1 || list.Items[0].Content != "durable" {
		t.Fatalf("replayed list=%#v err=%v", list, err)
	}
}

func TestStoreReplacesEstimatedArchiveIdentityWithPreciseGeneration(t *testing.T) {
	store := newChatStore(t)
	world := rooms.World{ID: "master", Name: "Master", Role: rooms.WorldRoleMaster}
	estimatedStart := time.Date(2026, time.August, 23, 19, 8, 36, 0, time.UTC)
	preciseStart := time.Date(2026, time.August, 22, 2, 26, 32, 0, time.UTC)
	fileName := "server_chat_log_2026-08-23-19-08-36.txt"
	estimated := shared.RuntimeChatLogGeneration{
		ID: "estimated", FileName: fileName, Archived: true, Size: 50,
		StartedAt: estimatedStart, StartedAtEstimated: true, UpdatedAt: estimatedStart,
	}
	precise := shared.RuntimeChatLogGeneration{
		ID: "precise", FileName: fileName, Archived: true, Size: 50,
		StartedAt: preciseStart, UpdatedAt: estimatedStart,
	}
	estimatedEntry := parsedChatEntry(t, "room", world, estimatedStart, 50, "[14:34:06]: [Say] (KU_ONE) lcy: corrected")
	estimatedEntry.TimeEstimated = true
	if _, err := store.ImportGeneration("room", world, estimated, []parsedEntry{estimatedEntry}, 50, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileGeneration("room", world, precise); err != nil {
		t.Fatal(err)
	}
	preciseEntry := parsedChatEntry(t, "room", world, preciseStart, 50, "[14:34:06]: [Say] (KU_ONE) lcy: corrected")
	if _, err := store.ImportGeneration("room", world, precise, []parsedEntry{preciseEntry}, 50, time.Now()); err != nil {
		t.Fatal(err)
	}
	list, err := store.List("room", Filter{Limit: 10})
	if err != nil || list.Total != 1 || list.Items[0].TimeEstimated || list.Items[0].OccurredAt == nil || !list.Items[0].OccurredAt.Equal(preciseStart.Add(14*time.Hour+34*time.Minute+6*time.Second)) {
		t.Fatalf("reconciled list=%#v err=%v", list, err)
	}
	old, err := store.Generation("room", world.ID, estimated.ID)
	if err != nil || old != (GenerationState{}) {
		t.Fatalf("estimated generation remains: %#v err=%v", old, err)
	}
}

func newChatStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.DB().SetMaxOpenConns(1)
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	return store
}

func parsedChatEntry(t *testing.T, roomID string, world rooms.World, startedAt time.Time, cursor int64, text string) parsedEntry {
	t.Helper()
	entry, ok := parseLine(roomID, logstream.WorldSnapshot{
		WorldID: world.ID, WorldName: world.Name, WorldRole: world.Role,
		Snapshot: &logstream.Snapshot{StartedAt: startedAt},
	}, logstream.Line{Cursor: cursor, Text: text})
	if !ok {
		t.Fatalf("failed to parse chat line %q", text)
	}
	return entry
}
