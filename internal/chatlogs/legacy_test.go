package chatlogs

import (
	"context"
	"testing"
	"time"

	"dont/internal/rooms"
	"dont/shared"
)

const legacyTestID = "0123456789abcdef0123456789abcdef"

func TestSyncRecoversSameBootCurrentFileWithoutDuplicatingLegacyArchive(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Managed: true}
	world := rooms.World{ID: "master", Role: rooms.WorldRoleMaster}
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	legacy := shared.RuntimeChatLogGeneration{
		ID: legacyTestID, FileName: "server_chat_log_2026-09-07-00-00-00.txt", Archived: true,
		StartedAt: start, UpdatedAt: start.Add(24 * time.Hour), Size: 100,
	}
	oldText := "[00:00:01]: [Announcement] saved archive"
	entry := parsedChatEntry(t, room.ID, world, start, 100, oldText)
	if _, err := store.ImportGeneration(room.ID, world, legacy, []parsedEntry{entry}, 100, start); err != nil {
		t.Fatal(err)
	}
	oldList, err := store.List(room.ID, Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	oldEventID := oldList.Items[0].ID
	archive := legacy
	archive.ID = "chat-v2:" + legacyTestID + ":11111111111111111111111111111111"
	current := legacy
	current.ID, current.FileName, current.Archived = "chat-v2:"+legacyTestID+":22222222222222222222222222222222", "server_chat_log.txt", false
	current.Size = 200
	reader := timedGenerationFixture{generationReaderFixture: &generationReaderFixture{worlds: map[string][]generationFixture{
		world.ID: {
			{generation: archive, lines: []shared.RuntimeLogLine{{Cursor: 100, Text: oldText}}},
			{generation: current, lines: []shared.RuntimeLogLine{
				{Cursor: 100, Text: "[00:00:02]: [Announcement] current first"},
				{Cursor: 200, Text: "[00:00:03]: [Announcement] current next"},
			}},
		},
	}}, at: start.Add(time.Minute)}
	for pass := 0; pass < 2; pass++ {
		service, err := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{world}}, chatSnapshots{}, WithPersistence(store, reader))
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.SyncRoom(context.Background(), room.ID)
		if err != nil {
			t.Fatal(err)
		}
		list, err := store.List(room.ID, Filter{Limit: 10})
		if err != nil || list.Total != 3 || list.SyncState != "ready" || list.UnavailableGenerations != 0 {
			t.Fatalf("pass=%d list=%+v err=%v", pass, list, err)
		}
		wantImported := 2
		if pass == 1 {
			wantImported = 0
		}
		if result.Imported != wantImported {
			t.Fatalf("pass=%d result=%+v", pass, result)
		}
		found := false
		for _, item := range list.Items {
			if item.Content == "saved archive" {
				found = item.ID == oldEventID
			}
		}
		if !found {
			t.Fatal("upgrading changed the saved event identity")
		}
	}
	state, err := store.Generation(room.ID, world.ID, current.ID)
	if err != nil || state.Cursor != 200 || !state.CaughtUp {
		t.Fatalf("current checkpoint=%+v err=%v", state, err)
	}
}

func TestLegacySourceMigrationResumesBatchesAndRollsBackWithCheckpoint(t *testing.T) {
	store := newChatStore(t)
	world := rooms.World{ID: "master", Role: rooms.WorldRoleMaster}
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	legacy := shared.RuntimeChatLogGeneration{ID: legacyTestID, FileName: "server_chat_log.txt", StartedAt: start, UpdatedAt: start.Add(time.Minute), Size: 200}
	first := parsedChatEntry(t, "room", world, start, 100, "[00:00:01]: [Announcement] first")
	second := parsedChatEntry(t, "room", world, start, 200, "[00:00:02]: [Announcement] second")
	if _, err := store.ImportGeneration("room", world, legacy, []parsedEntry{first, second}, 200, start); err != nil {
		t.Fatal(err)
	}
	modern := legacy
	modern.ID = "chat-v2:" + legacyTestID + ":11111111111111111111111111111111"
	modern.Size = 300
	trigger := "CREATE TRIGGER legacy_checkpoint_fail BEFORE INSERT ON " + store.generationsTable + " BEGIN SELECT RAISE(FAIL, 'checkpoint failed'); END"
	if err := store.db.Exec(trigger).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportGeneration("room", world, modern, []parsedEntry{first}, 100, start); err == nil {
		t.Fatal("failed checkpoint did not roll back migration")
	}
	var remaining int
	if err := store.db.Table(store.sourcesTable).Where("generation_id = ?", legacy.ID).Count(&remaining).Error; err != nil || remaining != 2 {
		t.Fatalf("legacy sources lost during rollback: count=%d err=%v", remaining, err)
	}
	if err := store.db.Exec("DROP TRIGGER legacy_checkpoint_fail").Error; err != nil {
		t.Fatal(err)
	}
	if imported, err := store.ImportGeneration("room", world, modern, []parsedEntry{first}, 100, start); err != nil || imported != 0 {
		t.Fatalf("first migration batch=%d err=%v", imported, err)
	}
	// A new Store has no in-memory migration state; the binding must survive.
	restarted := NewStore(store.db, "test_")
	if imported, err := restarted.ImportGeneration("room", world, modern, []parsedEntry{second}, 200, start); err != nil || imported != 0 {
		t.Fatalf("resumed migration batch=%d err=%v", imported, err)
	}
	third := parsedChatEntry(t, "room", world, start, 300, "[00:00:03]: [Announcement] third")
	if imported, err := restarted.ImportGeneration("room", world, modern, []parsedEntry{third}, 300, start); err != nil || imported != 1 {
		t.Fatalf("new append=%d err=%v", imported, err)
	}
	list, err := store.List("room", Filter{Limit: 10})
	if err != nil || list.Total != 3 {
		t.Fatalf("migration duplicated or erased messages: %+v err=%v", list, err)
	}
	state, err := store.Generation("room", world.ID, legacy.ID)
	if err != nil || state != (GenerationState{}) {
		t.Fatalf("obsolete checkpoint remains: %+v err=%v", state, err)
	}
}

func TestLegacyMigrationKeepsConflictingSavedBody(t *testing.T) {
	store := newChatStore(t)
	world := rooms.World{ID: "master"}
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	legacy := shared.RuntimeChatLogGeneration{ID: legacyTestID, FileName: "server_chat_log.txt", StartedAt: start, UpdatedAt: start.Add(time.Minute), Size: 200}
	first := parsedChatEntry(t, "room", world, start, 100, "[00:00:01]: [Announcement] first")
	second := parsedChatEntry(t, "room", world, start, 200, "[00:00:02]: [Announcement] original")
	if _, err := store.ImportGeneration("room", world, legacy, []parsedEntry{first, second}, 200, start); err != nil {
		t.Fatal(err)
	}
	modern := legacy
	modern.ID = "chat-v2:" + legacyTestID + ":11111111111111111111111111111111"
	if _, err := store.ImportGeneration("room", world, modern, []parsedEntry{first}, 100, start); err != nil {
		t.Fatal(err)
	}
	second.Content = "changed"
	if _, err := store.ImportGeneration("room", world, modern, []parsedEntry{second}, 200, start); err == nil {
		t.Fatal("different persisted text was silently replaced")
	}
	list, err := store.List("room", Filter{Limit: 10})
	if err != nil || list.Total != 2 {
		t.Fatalf("saved history changed: %+v err=%v", list, err)
	}
	state, err := store.Generation("room", world.ID, modern.ID)
	if err != nil || state.Cursor != 100 {
		t.Fatalf("failed verification advanced cursor: %+v err=%v", state, err)
	}
}

func TestSyncRetainsEventIdentityWhenLiveLogIsCopiedToArchive(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Managed: true}
	world := rooms.World{ID: "master", Role: rooms.WorldRoleMaster}
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	live := shared.RuntimeChatLogGeneration{
		ID: "chat-v2:" + legacyTestID + ":11111111111111111111111111111111", FileName: "server_chat_log.txt",
		StartedAt: start, UpdatedAt: start.Add(time.Hour), Size: 200,
	}
	lines := []shared.RuntimeLogLine{
		{Cursor: 100, Text: "[00:00:01]: [Announcement] first"},
		{Cursor: 200, Text: "[00:00:02]: [Announcement] second"},
	}
	reader := timedGenerationFixture{generationReaderFixture: &generationReaderFixture{worlds: map[string][]generationFixture{world.ID: {{generation: live, lines: lines}}}}, at: start.Add(time.Minute)}
	service, err := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{world}}, chatSnapshots{}, WithPersistence(store, reader))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SyncRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	before, err := store.List(room.ID, Filter{Limit: 10})
	if err != nil || before.Total != 2 {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	archive := live
	archive.ID, archive.Archived, archive.FileName = "chat-v2:"+legacyTestID+":22222222222222222222222222222222", true, "server_chat_log_2026-09-06-01-00-00.txt"
	current := live
	current.ID, current.Size = "chat-v2:"+legacyTestID+":33333333333333333333333333333333", 100
	// A separate live file with the same first record must remain independent.
	reader.worlds[world.ID] = []generationFixture{{generation: archive, lines: lines}, {generation: current, lines: lines[:1]}}
	for i := 0; i < 2; i++ {
		if _, err := service.SyncRoom(context.Background(), room.ID); err != nil {
			t.Fatal(err)
		}
		after, err := store.List(room.ID, Filter{Limit: 10})
		if err != nil || after.Total != 3 || after.SyncState != "ready" || after.UnavailableGenerations != 0 {
			t.Fatalf("after=%+v err=%v", after, err)
		}
		for _, original := range before.Items {
			found := false
			for _, item := range after.Items {
				found = found || original.ID == item.ID
			}
			if !found {
				t.Fatal("copying the archive replaced an existing event ID")
			}
		}
	}
	// A subsequent rename preserves identity and refreshes its stored location.
	current.Archived, current.FileName = true, "server_chat_log_2026-09-06-02-00-00.txt"
	reader.worlds[world.ID][1].generation = current
	if _, err := service.SyncRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	var record generationRecord
	if err := store.db.Table(store.generationsTable).Where("generation_id = ?", current.ID).First(&record).Error; err != nil || !record.Archived || record.FileName != current.FileName {
		t.Fatalf("renamed checkpoint=%+v err=%v", record, err)
	}
}

func TestEmptyAndUnparsedLiveIdentityChangesDoNotInventHistoryGaps(t *testing.T) {
	store := newChatStore(t)
	world := rooms.World{ID: "master"}
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		id          string
		size        int64
		archived    bool
		parseErrors int
	}{
		{"empty", 0, false, 0},
		{"unfinished", 20, false, 0},
		{"parse-error", 20, false, 1},
		{"missing-archive", 20, true, 0},
	} {
		name := "server_chat_log.txt"
		if fixture.archived {
			name = "server_chat_log_2026-09-06-01-00-00.txt"
		}
		generation := shared.RuntimeChatLogGeneration{ID: fixture.id, FileName: name, Size: fixture.size, Archived: fixture.archived, UpdatedAt: start}
		if _, err := store.importGeneration("room", world, generation, nil, 0, start, batchProgress{parseErrors: fixture.parseErrors}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.observeGenerations("room", world.ID, []string{"new"}); err != nil {
		t.Fatal(err)
	}
	health, err := store.historyHealth("room")
	if err != nil || health.UnavailableGenerations != 2 || health.ParseErrors != 1 {
		t.Fatalf("health=%+v err=%v", health, err)
	}
}

func TestUnrelatedLegacyArchiveDoesNotBlockModernCopyReconciliation(t *testing.T) {
	store := newChatStore(t)
	world := rooms.World{ID: "master"}
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	legacy := shared.RuntimeChatLogGeneration{ID: legacyTestID, FileName: "server_chat_log_2026-09-06-01-00-00.txt", Archived: true, StartedAt: start, UpdatedAt: start.Add(time.Hour), Size: 100}
	older := parsedChatEntry(t, "room", world, start, 100, "[00:00:01]: [Announcement] unrelated archive")
	if _, err := store.ImportGeneration("room", world, legacy, []parsedEntry{older}, 100, start); err != nil {
		t.Fatal(err)
	}
	live := legacy
	live.ID, live.FileName, live.Archived = "chat-v2:"+legacyTestID+":11111111111111111111111111111111", "server_chat_log.txt", false
	entry := parsedChatEntry(t, "room", world, start, 100, "[00:00:02]: [Announcement] copied message")
	if _, err := store.ImportGeneration("room", world, live, []parsedEntry{entry}, 100, start); err != nil {
		t.Fatal(err)
	}
	copy := live
	copy.ID, copy.FileName, copy.Archived = "chat-v2:"+legacyTestID+":22222222222222222222222222222222", "server_chat_log_2026-09-06-02-00-00.txt", true
	if err := store.observeGenerations("room", world.ID, []string{legacy.ID, copy.ID}); err != nil {
		t.Fatal(err)
	}
	if imported, err := store.ImportGeneration("room", world, copy, []parsedEntry{entry}, 100, start); err != nil || imported != 0 {
		t.Fatalf("copy duplicated a modern source because a different legacy archive existed: imported=%d err=%v", imported, err)
	}
	list, err := store.List("room", Filter{Limit: 10})
	if err != nil || list.Total != 2 || list.UnavailableGenerations != 0 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
}
