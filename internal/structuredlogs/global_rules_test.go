package structuredlogs

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/logstream"
	"dont/internal/rooms"
)

func prepareRoomRuleUpgrade(t *testing.T) *Store {
	t.Helper()
	store := newStructuredLogStore(t)
	// A pre-upgrade database has the rules table but no global migration marker.
	if err := store.db.Table(store.ruleMigrationsTable).DropTable(&ruleMigrationRecord{}).Error; err != nil {
		t.Fatal(err)
	}
	return store
}

func TestGlobalRuleUpgradePreservesVariantsAndDoesNotResurrectDeletedRules(t *testing.T) {
	store := prepareRoomRuleUpgrade(t)
	old := defaultRules("room-a")[0]
	newer := old
	newer.RoomID, newer.Enabled = "room-b", false
	custom := Rule{ID: "custom", RoomID: "room-a", Name: "Custom", LogType: TypeEntity, Pattern: "boss", Enabled: true, Priority: 999}
	duplicate := custom
	duplicate.RoomID, duplicate.ID = "room-b", "another-id"
	variant := custom
	variant.RoomID, variant.Priority = "room-c", 998
	for i, rule := range []Rule{old, newer, custom, duplicate, variant} {
		store.now = func() time.Time { return time.Unix(int64(i+1), 0) }
		if _, err := store.CreateRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	rules, err := store.Rules("")
	if err != nil || len(rules) != 4 {
		t.Fatalf("merged rules=%+v err=%v", rules, err)
	}
	current, err := store.Rule("", "builtin-error")
	if err != nil || current.Enabled || !current.BuiltIn || !current.UpdatedAt.Equal(time.Unix(2, 0)) {
		t.Fatalf("latest built-in choice lost: %+v %v", current, err)
	}
	for _, rule := range rules {
		if rule.RoomID != "" {
			t.Fatalf("global rule has room scope: %+v", rule)
		}
		if rule.ID != "builtin-error" {
			if rule.BuiltIn {
				t.Fatal("retained variants must be editable/deletable")
			}
			if rule.Pattern == old.Pattern && rule.Enabled {
				t.Fatal("an old variant must not reactivate a disabled built-in")
			}
			if err := store.DeleteRule("", rule.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	rules, err = store.Rules("")
	if err != nil || len(rules) != 1 || rules[0].Enabled {
		t.Fatalf("archived rules were reimported: %+v %v", rules, err)
	}
	archived, err := store.Rule("room-a", "builtin-error")
	if err != nil || !archived.Enabled {
		t.Fatalf("original room rule was changed: %+v %v", archived, err)
	}
}

func TestGlobalRuleUpgradeComparesEveryEditableField(t *testing.T) {
	store := prepareRoomRuleUpgrade(t)
	base := Rule{ID: "same", Name: "Rule", Description: "Description", Pattern: "hello", LogType: TypeChat, Enabled: true, Priority: 500, MatchMode: MatchModeSingle}
	changes := []func(*Rule){
		func(r *Rule) {},
		func(r *Rule) { r.Name = "Renamed" },
		func(r *Rule) { r.Description = "Edited" },
		func(r *Rule) { r.Pattern = "bye" },
		func(r *Rule) { r.LogType = TypePlayer },
		func(r *Rule) { r.Enabled = false },
		func(r *Rule) { r.Regex = true },
		func(r *Rule) { r.Priority = 501 },
		func(r *Rule) { r.MatchMode = MatchModeMultiLine },
		func(r *Rule) { r.MatchMode, r.TailPattern = MatchModeHeadTail, "end" },
	}
	for i, change := range changes {
		rule := base
		rule.RoomID = string(rune('a' + i))
		change(&rule)
		if _, err := store.CreateRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	rules, err := store.Rules("")
	if err != nil || len(rules) != len(changes) {
		t.Fatalf("distinct edits were lost: %+v %v", rules, err)
	}
}

func TestGlobalRuleUpgradeRollsBackBeforeMarkingComplete(t *testing.T) {
	store := prepareRoomRuleUpgrade(t)
	for _, id := range []string{"a", "b"} {
		if _, err := store.CreateRule(Rule{ID: id, RoomID: "room", Name: id, Pattern: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.db.Exec("CREATE TRIGGER fail_global_rule BEFORE INSERT ON " + store.rulesTable + " WHEN NEW.room_id = '' AND NEW.id = 'b' BEGIN SELECT RAISE(ABORT, 'test failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err == nil {
		t.Fatal("expected the interrupted migration to fail")
	}
	rules, err := store.Rules("")
	if err != nil || len(rules) != 0 {
		t.Fatalf("partial migration committed: %+v %v", rules, err)
	}
	if err := store.db.Exec("DROP TRIGGER fail_global_rule").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	rules, err = store.Rules("")
	if err != nil || len(rules) != 2 {
		t.Fatalf("migration did not recover: %+v %v", rules, err)
	}
}

type globalRuleCatalog struct{ structuredLogCatalog }

func (globalRuleCatalog) Room(id string) (rooms.Room, error) {
	return rooms.Room{ID: id, Managed: true}, nil
}

func TestGlobalRulesWorkBeforeRoomsExistAndApplyAcrossRooms(t *testing.T) {
	store := newStructuredLogStore(t)
	service, err := NewService(structuredLogCatalog{managed: false}, structuredRawLogs{}, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Rules(); err != nil {
		t.Fatalf("rule management must not need a room: %v", err)
	}
	input := RuleInput{Name: "Boss", Pattern: "boss-event", LogType: TypeEntity, Enabled: true, Priority: 1000}
	rule, err := service.CreateRule(input)
	if err != nil || rule.RoomID != "" {
		t.Fatalf("create without rooms: %+v %v", rule, err)
	}
	if result, err := service.TestRule(RuleTestInput{RuleInput: input, Sample: "boss-event"}); err != nil || !result.Matched {
		t.Fatalf("test without rooms: %+v %v", result, err)
	}
	service.rooms = globalRuleCatalog{}
	service.logs = structuredRawLogs{snapshots: map[string]logstream.Snapshot{"master": {Lines: []logstream.Line{{Cursor: 1, Text: "boss-event"}}}}}
	for _, room := range []string{"a", "b"} {
		if _, err := service.RefreshWorld(context.Background(), room, "master"); err != nil {
			t.Fatal(err)
		}
		entries, err := service.List(room, ListFilter{})
		if err != nil || entries.Total != 1 || entries.Items[0].RuleID != rule.ID || entries.Items[0].RoomID != room {
			t.Fatalf("shared classification in room %s: %+v %v", room, entries, err)
		}
	}
	input.Enabled = false
	if _, err := service.UpdateRule(rule.ID, input); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefreshWorld(context.Background(), "a", "master"); err != nil {
		t.Fatal(err)
	}
	for _, room := range []string{"a", "b"} {
		entries, err := service.List(room, ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if (entries.Items[0].RuleID == rule.ID) != (room == "b") {
			t.Fatal("changes must apply on refresh without rewriting another room's snapshot")
		}
	}
	if err := service.DeleteRule(rule.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Rule("", rule.ID); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("global rule not deleted: %v", err)
	}
}
