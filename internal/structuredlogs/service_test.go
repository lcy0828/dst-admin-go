package structuredlogs

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/logstream"
	"dont/internal/rooms"
)

type structuredLogCatalog struct{ managed bool }

func (c structuredLogCatalog) Room(string) (rooms.Room, error) {
	return rooms.Room{ID: "room", DirectoryName: "room", Name: "Room", Managed: c.managed}, nil
}
func (structuredLogCatalog) World(_, worldID string) (rooms.World, error) {
	name := "Master"
	if worldID == "caves" {
		name = "Caves"
	}
	return rooms.World{ID: worldID, RoomID: "room", DirectoryName: name, Name: name}, nil
}
func (structuredLogCatalog) Worlds(string) ([]rooms.World, error) {
	master, _ := (structuredLogCatalog{}).World("room", "master")
	caves, _ := (structuredLogCatalog{}).World("room", "caves")
	return []rooms.World{master, caves}, nil
}

type structuredRawLogs struct{ snapshots map[string]logstream.Snapshot }

func (l structuredRawLogs) Snapshot(_, worldID string, _ int, _ string) (logstream.Snapshot, error) {
	value, exists := l.snapshots[worldID]
	if !exists {
		return logstream.Snapshot{}, logstream.ErrLogNotFound
	}
	return value, nil
}

func TestServiceRefreshClassifiesAndQueriesStructuredLogs(t *testing.T) {
	store := newStructuredLogStore(t)
	service, err := NewService(structuredLogCatalog{managed: true}, structuredRawLogs{snapshots: map[string]logstream.Snapshot{
		"master": {UpdatedAt: time.Now(), Lines: []logstream.Line{
			{Cursor: 10, Text: "[2026-08-08T01:02:03Z] Player Willow joined the game"},
			{Cursor: 20, Text: "[00:01:02]: Warning: slow tick"},
			{Cursor: 30, Text: "[00:01:03]: Say(Wilson): hello"},
			{Cursor: 40, Text: "[00:01:04]: unmatched detail"},
		}},
	}}, store)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 8, 8, 2, 0, 0, 0, time.UTC) }
	result, err := service.RefreshWorld(context.Background(), "room", "master")
	if err != nil || result.Count != 4 {
		t.Fatalf("refresh = %#v, %v", result, err)
	}
	list, err := service.List("room", ListFilter{Limit: 50})
	if err != nil || list.Total != 4 || len(list.Counts) != 8 || list.SnapshotState != SnapshotStateReady || list.LastRefreshedAt == nil || list.Counts[TypePlayer] != 1 || list.Counts[TypeWarning] != 1 || list.Counts[TypeChat] != 1 || list.Counts[TypeUnknown] != 1 {
		t.Fatalf("list = %#v, %v", list, err)
	}
	players, err := service.List("room", ListFilter{Type: TypePlayer, Query: "Willow", Limit: 50})
	if err != nil || players.Total != 1 || players.Items[0].OccurredAt == nil || players.Items[0].SourceTimestamp != "2026-08-08T01:02:03Z" {
		t.Fatalf("player entries = %#v, %v", players, err)
	}
}

func TestServiceRuleValidationTestingAndCancellation(t *testing.T) {
	store := newStructuredLogStore(t)
	service, err := NewService(structuredLogCatalog{managed: true}, structuredRawLogs{snapshots: map[string]logstream.Snapshot{"master": {Lines: []logstream.Line{{Text: "spawned deerclops"}}}}}, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateRule("room", RuleInput{Name: "broken", LogType: TypeEntity, Pattern: "[", Regex: true, Enabled: true}); !errors.Is(err, ErrInvalidRule) {
		t.Fatalf("invalid regexp error = %v", err)
	}
	rule, err := service.CreateRule("room", RuleInput{Name: "Boss", LogType: TypeEntity, Pattern: "deerclops", Enabled: true, Priority: 1000})
	if err != nil || rule.BuiltIn {
		t.Fatalf("custom rule = %#v, %v", rule, err)
	}
	tested, err := service.TestRule("room", RuleTestInput{RuleInput: RuleInput{Name: "Boss", LogType: TypeEntity, Pattern: "deerclops", Priority: 1000}, Sample: "spawned deerclops"})
	if err != nil || !tested.Matched {
		t.Fatalf("rule test = %#v, %v", tested, err)
	}
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	list, err := service.List("room", ListFilter{Type: TypeEntity, Limit: 50})
	if err != nil || list.Total != 1 || list.Items[0].RuleID != rule.ID {
		t.Fatalf("custom classification = %#v, %v", list, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.RefreshWorld(canceled, "room", "master"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh error = %v", err)
	}
}

func TestServiceRequiresManagedRoom(t *testing.T) {
	service, err := NewService(structuredLogCatalog{managed: false}, structuredRawLogs{}, newStructuredLogStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.List("room", ListFilter{}); !errors.Is(err, ErrRoomNotManaged) {
		t.Fatalf("unmanaged list error = %v", err)
	}
}

func TestServicePreservesMultiLineHeadTailCustomTypesAndClear(t *testing.T) {
	store := newStructuredLogStore(t)
	service, err := NewService(structuredLogCatalog{managed: true}, structuredRawLogs{snapshots: map[string]logstream.Snapshot{
		"master": {Lines: []logstream.Line{
			{Cursor: 10, Text: "[00:00:01]: Error: first"},
			{Cursor: 20, Text: "[00:00:02]: Error: second"},
			{Cursor: 30, Text: "[00:00:03]: ordinary"},
			{Cursor: 40, Text: "[00:00:04]: BEGIN stack"},
			{Cursor: 50, Text: "[00:00:05]: detail"},
			{Cursor: 60, Text: "[00:00:06]: END"},
			{Cursor: 70, Text: "[00:00:07]: after"},
		}},
	}}, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateRule("room", RuleInput{Name: "连续错误", LogType: TypeError, Pattern: "Error:", Enabled: true, Priority: 100, MatchMode: MatchModeMultiLine}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateRule("room", RuleInput{Name: "堆栈", LogType: LogType("连接事件"), Pattern: "BEGIN", Enabled: true, Priority: 200, MatchMode: MatchModeHeadTail, TailPattern: "END$"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	errorsList, err := service.List("room", ListFilter{Type: TypeError, Limit: 50})
	if err != nil || errorsList.Total != 1 || errorsList.Items[0].Content != "Error: first\nError: second" {
		t.Fatalf("multi-line entries = %#v, %v", errorsList, err)
	}
	stack, err := service.List("room", ListFilter{Type: LogType("连接事件"), Limit: 50})
	if err != nil || stack.Total != 1 || stack.Items[0].RawContent != "[00:00:04]: BEGIN stack\n[00:00:05]: detail\n[00:00:06]: END" {
		t.Fatalf("head-tail entries = %#v, %v", stack, err)
	}
	tested, err := service.TestRule("room", RuleTestInput{RuleInput: RuleInput{Name: "堆栈", LogType: LogType("连接事件"), Pattern: "BEGIN", MatchMode: MatchModeHeadTail, TailPattern: "END$"}, Sample: "BEGIN stack\ndetail\nEND"})
	if err != nil || !tested.Matched {
		t.Fatalf("head-tail test = %#v, %v", tested, err)
	}
	cleared, err := service.ClearWorld("room", "master")
	if err != nil || cleared.Deleted != 4 {
		t.Fatalf("clear = %#v, %v", cleared, err)
	}
	list, err := service.List("room", ListFilter{Limit: 50})
	if err != nil || list.Total != 0 || list.SnapshotState != SnapshotStateCleared || list.SnapshotUpdatedAt == nil || list.LastRefreshedAt != nil {
		t.Fatalf("list after clear = %#v, %v", list, err)
	}
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	list, err = service.List("room", ListFilter{Limit: 50})
	if err != nil || list.SnapshotState != SnapshotStateReady || list.LastRefreshedAt == nil || list.Total != 4 {
		t.Fatalf("list after refresh = %#v, %v", list, err)
	}
}

func TestServicePreviewsAndMigratesLegacyRulesIdempotently(t *testing.T) {
	store := newStructuredLogStore(t)
	if err := store.db.Table(store.legacyRulesTable).CreateTable(&legacyRuleRecord{}).Error; err != nil {
		t.Fatal(err)
	}
	legacyRules := []legacyRuleRecord{
		{ID: 1, Name: "服务器启动", LogType: "system", Pattern: `Starting Up`, IsRegex: false, IsEnabled: true, Priority: 100, MatchMode: "single"},
		{ID: 2, Name: "匹配所有日志", LogType: "all", Pattern: `.*`, IsRegex: true, IsEnabled: true, Priority: 1, MatchMode: "single"},
		{ID: 3, Name: "错误正则", LogType: "error", Pattern: `[`, IsRegex: true, IsEnabled: true, Priority: 90, MatchMode: "single"},
		{ID: 4, Name: "固定行", LogType: "system", Pattern: `BEGIN`, IsRegex: false, IsEnabled: true, Priority: 80, MatchMode: "fixed_lines", LineCount: 3},
		{ID: 5, Name: "重复启动", LogType: "system", Pattern: `Starting Up`, IsRegex: false, IsEnabled: true, Priority: 100, MatchMode: "single"},
	}
	for _, rule := range legacyRules {
		if err := store.db.Table(store.legacyRulesTable).Create(&rule).Error; err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(structuredLogCatalog{managed: true}, structuredRawLogs{}, store)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewRuleMigration("room")
	if err != nil {
		t.Fatal(err)
	}
	if !preview.SourceAvailable || preview.Total != 5 || preview.Ready != 1 || preview.Skipped != 2 || preview.Incompatible != 2 || preview.Conflicts != 0 {
		t.Fatalf("unexpected migration preview: %#v", preview)
	}
	result, err := service.MigrateLegacyRules("room")
	if err != nil || result.Imported != 1 {
		t.Fatalf("migration result = %#v, err=%v", result, err)
	}
	migrated, err := store.Rule("room", "legacy-1")
	if err != nil || migrated.Name != "服务器启动" || migrated.Priority != 900 || migrated.BuiltIn {
		t.Fatalf("migrated rule = %#v, err=%v", migrated, err)
	}
	second, err := service.MigrateLegacyRules("room")
	if err != nil || second.Imported != 0 || second.Preview.Ready != 0 || second.Preview.Skipped != 3 {
		t.Fatalf("second migration = %#v, err=%v", second, err)
	}
	rules, err := service.Rules("room")
	if err != nil || len(rules) != len(defaultRules("room"))+1 {
		t.Fatalf("rules after migration = %d, err=%v", len(rules), err)
	}
}

func TestServiceMigrationReportsMatcherConflicts(t *testing.T) {
	store := newStructuredLogStore(t)
	if err := store.db.Table(store.legacyRulesTable).CreateTable(&legacyRuleRecord{}).Error; err != nil {
		t.Fatal(err)
	}
	legacy := legacyRuleRecord{ID: 9, Name: "冲突分类", LogType: "diagnostic", Pattern: `server`, IsEnabled: true, MatchMode: "single"}
	if err := store.db.Table(store.legacyRulesTable).Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	service, err := NewService(structuredLogCatalog{managed: true}, structuredRawLogs{}, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateRule("room", RuleInput{Name: "现有规则", LogType: TypeSystem, Pattern: "server", Enabled: true, Priority: 800}); err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewRuleMigration("room")
	if err != nil || preview.Conflicts != 1 || preview.Ready != 0 || preview.Items[0].ExistingRuleID == "" {
		t.Fatalf("conflict preview = %#v, err=%v", preview, err)
	}
}
