package console

import (
	"context"
	"strings"
	"testing"

	"dont/internal/rooms"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type testRooms struct{ managed bool }

func (r testRooms) Room(id string) (rooms.Room, error) {
	return rooms.Room{ID: id, DirectoryName: "room", Name: "周末服", Managed: r.managed}, nil
}
func (testRooms) World(roomID, worldID string) (rooms.World, error) {
	return rooms.World{ID: worldID, RoomID: roomID, DirectoryName: "Master", Name: "Master"}, nil
}

type captureSender struct {
	script string
	err    error
}

func (s *captureSender) Send(_ context.Context, _, _, script string) error {
	s.script = script
	return s.err
}

func newConsoleService(t *testing.T, sender *captureSender) *Service {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(testRooms{managed: true}, sender, store)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestBuiltinArgumentsAreLuaQuotedAndRunIsPersisted(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	run, err := service.Execute(context.Background(), "room-id", "world-id", ExecuteRequest{
		CommandID: "announce", Arguments: map[string]interface{}{"message": `hello\"); c_shutdown()`},
	})
	if err != nil || run.Status != RunSent || run.LogQuery != run.ID {
		t.Fatalf("run = %#v, %v", run, err)
	}
	if !strings.Contains(sender.script, `c_announce("hello\\\"); c_shutdown()")`) {
		t.Fatalf("unsafe or unexpected script: %s", sender.script)
	}
	runs, total, err := service.Runs(ListFilter{RoomID: "room-id"})
	if err != nil || total != 1 || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("runs = %#v, %d, %v", runs, total, err)
	}
}

func TestCriticalAndRawCommandsRequireExactRoomConfirmation(t *testing.T) {
	service := newConsoleService(t, &captureSender{})
	if _, err := service.Execute(context.Background(), "room", "world", ExecuteRequest{CommandID: "rollback", Arguments: map[string]interface{}{"days": float64(1)}}); err != ErrConfirmationNeeded {
		t.Fatalf("rollback confirmation error = %v", err)
	}
	if _, err := service.ExecuteRaw(context.Background(), "room", "world", RawRequest{Command: "c_save()", Confirmation: "wrong"}); err != ErrConfirmationNeeded {
		t.Fatalf("raw confirmation error = %v", err)
	}
	if _, err := service.ExecuteRaw(context.Background(), "room", "world", RawRequest{Command: "c_save()", Confirmation: "周末服"}); err != nil {
		t.Fatalf("raw command: %v", err)
	}
}

func TestCustomDefinitionsPersistExecuteAndDelete(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	created, err := service.CreateDefinition(Definition{
		Name: "  自定义公告  ", Description: "测试自定义命令", Category: "自定义命令",
		Script:     `c_announce("{message}")`,
		Parameters: []Parameter{{Name: "message", Label: "公告内容", Type: "string", Required: true}},
	})
	if err != nil || created.ID == "" || created.Name != "自定义公告" || created.IsBuiltin || created.Risk != RiskCritical {
		t.Fatalf("created = %#v, %v", created, err)
	}
	definitions, err := service.DefinitionsWithError()
	if err != nil || len(definitions) != 14 {
		t.Fatalf("definitions = %#v, %v", definitions, err)
	}
	request := ExecuteRequest{CommandID: created.ID, Arguments: map[string]interface{}{"message": `hello\"); c_shutdown()`}}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrConfirmationNeeded {
		t.Fatalf("custom confirmation error = %v", err)
	}
	request.Confirmation = "周末服"
	run, err := service.Execute(context.Background(), "room", "world", request)
	if err != nil || run.Mode != "custom" || run.CommandID != created.ID {
		t.Fatalf("custom run = %#v, %v", run, err)
	}
	if !strings.Contains(sender.script, `c_announce("hello\\\"); c_shutdown()")`) {
		t.Fatalf("unsafe custom script: %s", sender.script)
	}
	created.Description = "已更新"
	updated, err := service.UpdateDefinition(created.ID, created)
	if err != nil || updated.Description != "已更新" {
		t.Fatalf("updated = %#v, %v", updated, err)
	}
	if err := service.DeleteDefinition("save_world"); err != ErrBuiltinDefinition {
		t.Fatalf("builtin delete error = %v", err)
	}
	if err := service.DeleteDefinition(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Definition(created.ID); err != ErrDefinitionNotFound {
		t.Fatalf("deleted definition error = %v", err)
	}
	deleted, err := service.DeleteRuns(ListFilter{RoomID: "room"})
	if err != nil || deleted != 1 {
		t.Fatalf("deleted runs = %d, %v", deleted, err)
	}
}

func TestGiveItemTargetsKUAndRejectsUnsafePrefab(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{
		CommandID: "give_item", Confirmation: "周末服",
		Arguments: map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "prefab": "goldnugget", "count": float64(3)},
	}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.script, `v.userid=="KU_SAFE_PLAYER"`) || !strings.Contains(sender.script, `SpawnPrefab("goldnugget")`) || !strings.Contains(sender.script, `for i=1,3`) {
		t.Fatalf("unexpected give script: %s", sender.script)
	}
	request.Arguments["prefab"] = `flint");c_shutdown()`
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unsafe prefab error = %v", err)
	}
}

func TestClockSegmentsRequireSixteenSegments(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{
		CommandID: "set_clock_segments", Confirmation: "周末服",
		Arguments: map[string]interface{}{"day": float64(10), "dusk": float64(4), "night": float64(1)},
	}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("invalid segment error = %v", err)
	}
	request.Arguments["night"] = float64(2)
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.script, `day=10,dusk=4,night=2`) {
		t.Fatalf("unexpected clock script: %s", sender.script)
	}
}

func TestSpawnEntityIsBoundedAndAnchoredToPlayer(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{
		CommandID: "spawn_entity", Confirmation: "周末服",
		Arguments: map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "prefab": "pigman", "count": float64(21)},
	}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unbounded spawn error = %v", err)
	}
	request.Arguments["count"] = float64(2)
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.script, `v.userid=="KU_SAFE_PLAYER"`) || !strings.Contains(sender.script, `SpawnPrefab("pigman")`) || !strings.Contains(sender.script, `GetWorldPosition()`) {
		t.Fatalf("unexpected spawn script: %s", sender.script)
	}
}

func TestWorldInfoAndNextPhaseCommandsAreBoundedTemplates(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	if _, err := service.Execute(context.Background(), "room", "world", ExecuteRequest{CommandID: "world_info"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.script, `[DST-ADMIN-WORLD]`) || !strings.Contains(sender.script, `remainingdaysinseason`) {
		t.Fatalf("unexpected world info script: %s", sender.script)
	}
	request := ExecuteRequest{CommandID: "next_phase"}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.script, `TheWorld:PushEvent("ms_nextphase")`) {
		t.Fatalf("unexpected phase script: %s", sender.script)
	}
	request.Arguments = map[string]interface{}{"unexpected": true}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unexpected phase arguments error = %v", err)
	}
}

func TestRemoveNearbyEntitiesRequiresConfirmationAndLimitsScope(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{
		CommandID: "remove_nearby_entities",
		Arguments: map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "prefab": "spoiled_food", "radius": float64(10), "maximum": float64(20)},
	}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrConfirmationNeeded {
		t.Fatalf("confirmation error = %v", err)
	}
	request.Confirmation = "周末服"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`v.userid=="KU_SAFE_PLAYER"`, `e.prefab=="spoiled_food"`, `n>=20`, `<=100`} {
		if !strings.Contains(sender.script, expected) {
			t.Fatalf("missing %q in cleanup script: %s", expected, sender.script)
		}
	}
	request.Arguments["maximum"] = float64(101)
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unbounded cleanup error = %v", err)
	}
}

func TestCustomDefinitionRejectsUnknownPlaceholder(t *testing.T) {
	service := newConsoleService(t, &captureSender{})
	_, err := service.CreateDefinition(Definition{
		Name: "无效命令", Category: "自定义命令", Script: `c_announce("{missing}")`,
	})
	if err != ErrInvalidDefinition {
		t.Fatalf("create error = %v", err)
	}
}
