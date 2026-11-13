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

func TestRepairRecoversMultilineContinuationsAndClearsFalseParseErrors(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Managed: true}
	world := rooms.World{ID: "master", Name: "地面", Role: rooms.WorldRoleMaster}
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	g := shared.RuntimeChatLogGeneration{
		ID: "boot", FileName: "server_chat_log.txt", StartedAt: start,
		UpdatedAt: start.Add(time.Hour), Size: 260, TimeVersion: shared.ChatTimeVersion,
	}
	truncatedOld := parsedChatEntry(t, room.ID, world, start, 40, "[00:01:00]: [Announcement] 今日播报：")
	if _, err := store.importGeneration(room.ID, world, g, []parsedEntry{truncatedOld}, 260, start, batchProgress{
		version: parserVersion, parseErrors: 2, lastParseProblem: "字节 80：无法识别聊天行",
	}); err != nil {
		t.Fatal(err)
	}

	lines := []shared.RuntimeLogLine{
		{Cursor: 40, Text: "[00:01:00]: [Announcement] 今日播报："},
		{Cursor: 80, Text: "地表将在 14 分钟后降雨"},
		{Cursor: 120, Text: "青蛙雨预警解除"},
		{Cursor: 180, Text: "[00:02:00]: [BadTag] malformed timestamped line"},
		{Cursor: 220, Text: "orphan after malformed line"},
		{Cursor: 260, Text: "[00:03:00]: [Say] () Server: 自动存档已完成"},
	}
	reader := timedGenerationFixture{
		generationReaderFixture: &generationReaderFixture{
			worlds: map[string][]generationFixture{world.ID: {{generation: g, lines: lines}}},
		},
		at: start.Add(time.Minute),
	}
	svc, err := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{world}}, chatSnapshots{}, WithPersistence(store, reader))
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.RepairRoom(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.ParseErrors != 2 {
		t.Fatalf("expected only the 2 genuinely malformed lines to count as parseErrors, got %+v", result.HistoryHealth)
	}
	list, err := store.List(room.ID, Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 2 {
		t.Fatalf("expected 2 valid events after repair, got %+v", list)
	}
	var foundMultiline, foundServerSay bool
	for _, item := range list.Items {
		if item.Kind == KindAnnouncement && item.Content == "今日播报：\n地表将在 14 分钟后降雨\n青蛙雨预警解除" {
			foundMultiline = true
		}
		if item.Kind == KindSay && item.PlayerName == "Server" && item.Content == "自动存档已完成" {
			foundServerSay = true
		}
	}
	if !foundMultiline || !foundServerSay {
		t.Fatalf("expected recovered multiline announcement and server say, got %#v", list.Items)
	}
}

type oneLinePerChunkReader struct {
	timedGenerationFixture
}

func (f oneLinePerChunkReader) ReadChatLogGeneration(ctx context.Context, r, w string, q shared.RuntimeChatLogRequest) (shared.RuntimeChatLogResult, error) {
	res, err := f.timedGenerationFixture.ReadChatLogGeneration(ctx, r, w, q)
	if len(res.Lines) > 1 {
		res.Lines = res.Lines[:1]
		if len(res.Times) > 1 {
			res.Times = res.Times[:1]
		}
		res.Cursor = res.Lines[0].Cursor
		res.Complete = res.Cursor == res.Generation.Size
	}
	return res, err
}

func TestSyncAndRepairAcrossChunkBoundariesEmptyFirstLineAndModWarnings(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Managed: true}
	world := rooms.World{ID: "master", Name: "地面", Role: rooms.WorldRoleMaster}
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	lines := []shared.RuntimeLogLine{
		{Cursor: 35, Text: "[00:00:10]: [Announcement] "},
		{Cursor: 70, Text: "春季第 5 天"},
		{Cursor: 72, Text: ""},
		{Cursor: 110, Text: "鹿鹅巢已刷新"},
		{Cursor: 150, Text: "[00:00:20]: [Say] (KU_ONE) Willow: 前面这句"},
		{Cursor: 190, Text: "[00:00:21]: [Mod Warning] something broke"},
		{Cursor: 230, Text: "stack traceback: mods/foo.lua:12"},
		{Cursor: 270, Text: "[00:00:25]: [BadTag] malformed line"},
		{Cursor: 310, Text: "orphan tail line"},
		{Cursor: 360, Text: "[00:00:30]: [System Message] 保存完成"},
	}
	g := shared.RuntimeChatLogGeneration{
		ID: "boot", FileName: "server_chat_log.txt", StartedAt: start,
		UpdatedAt: start.Add(time.Hour), Size: 360, TimeVersion: shared.ChatTimeVersion,
	}
	reader := oneLinePerChunkReader{
		timedGenerationFixture: timedGenerationFixture{
			generationReaderFixture: &generationReaderFixture{
				worlds: map[string][]generationFixture{world.ID: {{generation: g, lines: lines}}},
			},
			at: start.Add(time.Minute),
		},
	}
	svc, err := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{world}}, chatSnapshots{}, WithPersistence(store, reader))
	if err != nil {
		t.Fatal(err)
	}
	// First sync runs live across 1-line chunks (each boundary saved & resumed via CarryState).
	syncRes, err := svc.SyncRoom(context.Background(), room.ID)
	if err != nil {
		t.Fatalf("chunked sync failed: %v", err)
	}
	if syncRes.ParseErrors != 3 {
		t.Fatalf("expected 3 parseErrors (traceback after Mod Warning, BadTag, orphan after BadTag), got %d", syncRes.ParseErrors)
	}
	// Repair pass across 1-line chunks when DB already contains full 2-line empty-first-line announcement.
	repairRes, err := svc.RepairRoom(context.Background(), room.ID)
	if err != nil {
		t.Fatalf("chunked repair failed: %v", err)
	}
	if repairRes.ParseErrors != syncRes.ParseErrors {
		t.Fatalf("repair parseErrors=%d want=%d", repairRes.ParseErrors, syncRes.ParseErrors)
	}
	list, err := store.List(room.ID, Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 3 {
		t.Fatalf("expected 3 events, got %+v", list)
	}
	contents := map[string]bool{}
	for _, item := range list.Items {
		contents[item.Content] = true
	}
	if !contents["春季第 5 天\n鹿鹅巢已刷新"] || !contents["前面这句"] || !contents["保存完成"] {
		t.Fatalf("unexpected items across 1-line chunk boundaries: %#v", list.Items)
	}
}

func TestSyncAutomaticallyRepairsV1MultilineHistory(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Managed: true}
	world := rooms.World{ID: "master", Name: "地面", Role: rooms.WorldRoleMaster}
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	g := shared.RuntimeChatLogGeneration{
		ID: "boot", FileName: "server_chat_log.txt", StartedAt: start,
		UpdatedAt: start.Add(time.Hour), Size: 120, TimeVersion: shared.ChatTimeVersion,
	}
	old := parsedChatEntry(t, room.ID, world, start, 40, "[00:01:00]: [Announcement] 今日播报：")
	if _, err := store.importGeneration(room.ID, world, g, []parsedEntry{old}, 120, start, batchProgress{
		version: 1, parseErrors: 1, lastParseProblem: "字节 120：无法识别聊天行",
	}); err != nil {
		t.Fatal(err)
	}
	lines := []shared.RuntimeLogLine{
		{Cursor: 40, Text: "[00:01:00]: [Announcement] 今日播报："},
		{Cursor: 120, Text: "夏季高温预警"},
	}
	reader := timedGenerationFixture{
		generationReaderFixture: &generationReaderFixture{
			worlds: map[string][]generationFixture{world.ID: {{generation: g, lines: lines}}},
		},
		at: start.Add(time.Minute),
	}
	svc, err := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{world}}, chatSnapshots{}, WithPersistence(store, reader))
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.SyncRoom(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.ParseErrors != 0 {
		t.Fatalf("expected v1 false parse errors to auto-heal on sync, got %+v", result.HistoryHealth)
	}
	list, err := store.List(room.ID, Filter{Limit: 10})
	if err != nil || list.Total != 1 || list.Items[0].Content != "今日播报：\n夏季高温预警" || list.SyncState != "ready" {
		t.Fatalf("expected auto-healed v1 history, got %+v err=%v", list, err)
	}
}

func TestRepairCancellationMidMultilineResumesWithoutConflict(t *testing.T) {
	store := newChatStore(t)
	room := rooms.Room{ID: "room", Managed: true}
	world := rooms.World{ID: "master", Name: "地面", Role: rooms.WorldRoleMaster}
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	lines := []shared.RuntimeLogLine{
		{Cursor: 40, Text: "[00:00:10]: [Announcement] 今日播报："},
		{Cursor: 80, Text: "第一行"},
		{Cursor: 120, Text: "第二行"},
	}
	g := shared.RuntimeChatLogGeneration{
		ID: "boot", FileName: "server_chat_log.txt", StartedAt: start,
		UpdatedAt: start.Add(time.Hour), Size: 120, TimeVersion: shared.ChatTimeVersion,
	}
	fullReader := timedGenerationFixture{
		generationReaderFixture: &generationReaderFixture{
			worlds: map[string][]generationFixture{world.ID: {{generation: g, lines: lines}}},
		},
		at: start.Add(time.Minute),
	}
	svc, err := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{world}}, chatSnapshots{}, WithPersistence(store, fullReader))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}

	// Simulate canceling RepairRoom after chunk 1 (stops right after Line 1).
	ctx, cancel := context.WithCancel(context.Background())
	canceling := &cancelingChatReader{
		timedGenerationFixture: fullReader,
		cancel:                 cancel,
	}
	resumingSvc, err := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{world}}, chatSnapshots{}, WithPersistence(store, canceling))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumingSvc.RepairRoom(ctx, room.ID); err == nil {
		t.Fatal("expected cancellation error")
	}
	state, err := store.Generation(room.ID, world.ID, g.ID)
	if err != nil || state.RepairCursor != 40 || state.RepairCarryCursor != 40 || state.RepairCarryState == "" {
		t.Fatalf("expected saved repair carry checkpoint after line 1, got %+v err=%v", state, err)
	}

	// Cancel again right after Line 2 (mid-multiline between Line 2 and Line 3).
	ctx2, cancel2 := context.WithCancel(context.Background())
	canceling2 := &cancelingChatReader{
		timedGenerationFixture: fullReader,
		cancel:                 cancel2,
	}
	resumingSvc2, _ := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{world}}, chatSnapshots{}, WithPersistence(store, canceling2))
	if _, err := resumingSvc2.RepairRoom(ctx2, room.ID); err == nil {
		t.Fatal("expected second cancellation error")
	}
	state2, err := store.Generation(room.ID, world.ID, g.ID)
	if err != nil || state2.RepairCursor != 80 || state2.RepairCarryCursor != 80 || state2.RepairCarryState == "" {
		t.Fatalf("expected saved repair carry checkpoint after line 2, got %+v err=%v", state2, err)
	}

	// Final resume finishes Line 3 without conflict and clears repair checkpoint.
	finalSvc, _ := NewService(persistentChatCatalog{room: room, worlds: []rooms.World{world}}, chatSnapshots{}, WithPersistence(store, fullReader))
	if _, err := finalSvc.RepairRoom(context.Background(), room.ID); err != nil {
		t.Fatalf("final repair resume failed: %v", err)
	}
	finalState, _ := store.Generation(room.ID, world.ID, g.ID)
	if finalState.RepairCursor != 0 || finalState.RepairCarryCursor != 0 || finalState.RepairCarryState != "" || finalState.ParserVersion != parserVersion {
		t.Fatalf("expected clean finished repair state, got %+v", finalState)
	}
	list, err := store.List(room.ID, Filter{Limit: 10})
	if err != nil || list.Total != 1 || list.Items[0].Content != "今日播报：\n第一行\n第二行" {
		t.Fatalf("unexpected repaired item: %+v err=%v", list, err)
	}
}
