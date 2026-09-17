package console

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func assertLuaSyntax(t *testing.T, script string) {
	t.Helper()
	compiler, err := exec.LookPath("luac")
	if err != nil {
		t.Skip("luac is not available")
	}
	file, err := os.CreateTemp(t.TempDir(), "dst-admin-*.lua")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(script); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(compiler, "-p", file.Name()).CombinedOutput(); err != nil {
		t.Fatalf("invalid Lua: %v\n%s\n%s", err, output, script)
	}
}

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

type captureCommander struct {
	requests []dstruntime.CommandRequest
	replies  []dstruntime.CommandReceipt
	errors   []error
}

func (c *captureCommander) ExecuteCommand(_ context.Context, _, _ string, request dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	c.requests = append(c.requests, request)
	index := len(c.requests) - 1
	var receipt dstruntime.CommandReceipt
	if index < len(c.replies) {
		receipt = c.replies[index]
	}
	if receipt.RequestID == "" {
		receipt.RequestID = request.RequestID
	}
	if receipt.Action == "" {
		receipt.Action = request.Action
	}
	if receipt.CompletedAt.IsZero() {
		receipt.CompletedAt = time.Now().UTC()
	}
	if index < len(c.errors) {
		return receipt, c.errors[index]
	}
	return receipt, nil
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

func TestVerifiedCommandRequiresRuntimeReceiptForSuccess(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	commander := &captureCommander{replies: []dstruntime.CommandReceipt{{OK: true, Code: "COMMAND_EXECUTED"}}}
	service.commander = commander

	run, err := service.Execute(context.Background(), "room-id", "world-id", ExecuteRequest{
		CommandID: "announce", Arguments: map[string]interface{}{"message": "hello"},
	})
	if err != nil || run.Status != RunSucceeded || run.ExecutionOutcome != "confirmed" || run.TransportOutcome != "sent" {
		t.Fatalf("run = %#v, error = %v", run, err)
	}
	if len(commander.requests) != 1 || commander.requests[0].Action != "console.execute" || !strings.Contains(commander.requests[0].Arguments["script"].(string), `c_announce("hello")`) {
		t.Fatalf("runtime requests = %#v", commander.requests)
	}
	if sender.script != "" {
		t.Fatalf("verified command also used legacy sender: %q", sender.script)
	}
}

func TestVerifiedCommandPersistsRuntimeRejection(t *testing.T) {
	service := newConsoleService(t, &captureSender{})
	service.commander = &captureCommander{replies: []dstruntime.CommandReceipt{{
		OK: false, Code: "COMMAND_EXECUTION_FAILED", Message: "player is unavailable",
	}}}
	run, err := service.Execute(context.Background(), "room-id", "world-id", ExecuteRequest{
		CommandID: "announce", Arguments: map[string]interface{}{"message": "hello"},
	})
	if err != nil || run.Status != RunFailed || run.ExecutionOutcome != "rejected" || run.ErrorCode != "COMMAND_EXECUTION_FAILED" || run.ErrorMessage != "player is unavailable" {
		t.Fatalf("run = %#v, error = %v", run, err)
	}
}

func TestMissingReceiptRunsOneBeaconWithoutReplayingCommand(t *testing.T) {
	service := newConsoleService(t, &captureSender{})
	commander := &captureCommander{
		replies: []dstruntime.CommandReceipt{{}, {OK: true, Code: "CONSOLE_RESPONSIVE"}},
		errors:  []error{dstruntime.ErrRuntimeResultAbsent, nil},
	}
	service.commander = commander
	run, err := service.Execute(context.Background(), "room-id", "world-id", ExecuteRequest{
		CommandID: "announce", Arguments: map[string]interface{}{"message": "hello"},
	})
	if err != nil || run.Status != RunUncertain || run.ErrorCode != "COMMAND_OUTCOME_UNKNOWN" || !run.MayHaveExecuted || run.RecoveryOutcome != "succeeded" {
		t.Fatalf("run = %#v, error = %v", run, err)
	}
	if len(commander.requests) != 2 || commander.requests[0].Action != "console.execute" || commander.requests[1].Action != "system.ping" {
		t.Fatalf("runtime requests = %#v", commander.requests)
	}
}

func TestMissingReceiptAndBeaconMarksConsoleUnresponsive(t *testing.T) {
	service := newConsoleService(t, &captureSender{})
	commander := &captureCommander{errors: []error{dstruntime.ErrRuntimeResultAbsent, dstruntime.ErrRuntimeResultAbsent}}
	service.commander = commander
	run, err := service.Execute(context.Background(), "room-id", "world-id", ExecuteRequest{
		CommandID: "announce", Arguments: map[string]interface{}{"message": "hello"},
	})
	if err != nil || run.Status != RunUnresponsive || run.ErrorCode != "CONSOLE_UNRESPONSIVE" || run.ExecutionOutcome != "unresponsive" || run.RecoveryOutcome != "failed" {
		t.Fatalf("run = %#v, error = %v", run, err)
	}
	if !errors.Is(commander.errors[0], dstruntime.ErrRuntimeResultAbsent) || len(commander.requests) != 2 {
		t.Fatalf("runtime requests = %#v", commander.requests)
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
	if err != nil || len(definitions) != len(service.templates)+1 {
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
	if !strings.Contains(sender.script, `local mode="units"`) {
		t.Fatalf("default quantity mode is not units: %s", sender.script)
	}
	request.Arguments["count"] = float64(2)
	request.Arguments["quantity_mode"] = "stacks"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.script, `local mode="stacks"`) || !strings.Contains(sender.script, `stack:SetStackSize(stack.maxsize or 1)`) || !strings.Contains(sender.script, `for i=1,2`) {
		t.Fatalf("stack quantity mode was not rendered: %s", sender.script)
	}
	request.Arguments["quantity_mode"] = "crates"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unsafe quantity mode error = %v", err)
	}
	request.Arguments["quantity_mode"] = "units"
	request.Arguments["prefab"] = `flint");c_shutdown()`
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unsafe prefab error = %v", err)
	}
}

func TestRecipeMaterialsAndBlueprintsUseValidatedPrefabs(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{CommandID: "give_recipe_materials", Confirmation: "周末服", Arguments: map[string]interface{}{
		"player_id": "KU_SAFE_PLAYER", "prefab": "researchlab", "count": float64(2),
	}}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	assertLuaSyntax(t, sender.script)
	for _, fragment := range []string{`(AllRecipes or {})["researchlab"]`, `recipe.ingredients`, `for i=1,2`} {
		if !strings.Contains(sender.script, fragment) {
			t.Fatalf("material command missing %q: %s", fragment, sender.script)
		}
	}
	request.CommandID = "give_blueprint"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `SpawnPrefab("researchlab_blueprint")`) {
		t.Fatalf("blueprint script = %s, %v", sender.script, err)
	}
	assertLuaSyntax(t, sender.script)
	request.Arguments["prefab"] = `researchlab");c_shutdown()`
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unsafe blueprint prefab error = %v", err)
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

func TestSetPhaseOnlyAcceptsKnownClockPhases(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{CommandID: "set_phase", Arguments: map[string]interface{}{"phase": "dusk"}}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.script, `TheWorld:PushEvent("ms_setphase","dusk")`) {
		t.Fatalf("unexpected set phase script: %s", sender.script)
	}
	request.Arguments["phase"] = `night");c_shutdown()`
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unsafe phase error = %v", err)
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

func TestPlayerStatAndSpeedCommandsAreBounded(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{
		CommandID: "set_player_stat", Confirmation: "周末服",
		Arguments: map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "stat": "health", "value": float64(75)},
	}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`v.userid=="KU_SAFE_PLAYER"`, `components.health:SetPercent(0.75)`, `healthdelta`} {
		if !strings.Contains(sender.script, expected) {
			t.Fatalf("missing %q in player stat script: %s", expected, sender.script)
		}
	}
	request.Arguments["stat"] = `health);c_shutdown()`
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unsafe stat error = %v", err)
	}
	request.Arguments = map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "stat": "temperature", "value": float64(-51)}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("temperature boundary error = %v", err)
	}
	request.CommandID = "set_player_speed"
	request.Arguments = map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "multiplier": "2.5"}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `"dst_admin_go",2.5`) {
		t.Fatalf("speed script = %s, %v", sender.script, err)
	}
	request.Arguments["multiplier"] = "NaN"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("NaN speed error = %v", err)
	}
	request.Arguments["multiplier"] = float64(100.1)
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("speed boundary error = %v", err)
	}
}

func TestTMIRPlayerStateCommandsValidateParameters(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{CommandID: "set_player_lock", Confirmation: "周末服", Arguments: map[string]interface{}{
		"player_id": "KU_SAFE_PLAYER", "resource": "health", "value_mode": "percent", "enabled": true, "value": float64(50),
	}}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	assertLuaSyntax(t, sender.script)
	for _, fragment := range []string{`value_mode="percent"`, `GetMaxWithPenalty`, `*value/100`} {
		if !strings.Contains(sender.script, fragment) {
			t.Fatalf("health percent lock missing %q: %s", fragment, sender.script)
		}
	}
	request.Arguments["resource"] = "moisture"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("moisture accepted percent mode: %v", err)
	}

	request = ExecuteRequest{CommandID: "clear_player_naughtiness", Confirmation: "周末服", Arguments: map[string]interface{}{
		"player_id": "KU_SAFE_PLAYER", "mode": "spawn_random",
	}}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `math.random(2,4)`) {
		t.Fatalf("naughtiness script = %s, %v", sender.script, err)
	}
	assertLuaSyntax(t, sender.script)

	request = ExecuteRequest{CommandID: "set_player_attack_multiplier", Confirmation: "周末服", Arguments: map[string]interface{}{
		"player_id": "KU_SAFE_PLAYER", "multiplier": float64(1.25),
	}}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `damagemultiplier=1.25`) {
		t.Fatalf("attack multiplier script = %s, %v", sender.script, err)
	}
	assertLuaSyntax(t, sender.script)
}

func TestPlayerMaintenanceAndTeleportCommands(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{
		CommandID: "clear_player_debuffs", Confirmation: "周末服",
		Arguments: map[string]interface{}{"player_id": "KU_SAFE_PLAYER"},
	}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`Unfreeze()`, `firefx:Extinguish(true)`, `ComeTo()`, `Unstick()`} {
		if !strings.Contains(sender.script, expected) {
			t.Fatalf("missing %q in debuff script: %s", expected, sender.script)
		}
	}
	request.CommandID = "clear_player_inventory"
	request.Arguments["scope"] = "all"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`GetOverflowContainer()`, `RemoveCurse("MONKEY",curse)`, `inv.activeitem`} {
		if !strings.Contains(sender.script, expected) {
			t.Fatalf("missing %q in inventory script: %s", expected, sender.script)
		}
	}
	request.Arguments["scope"] = `all");c_shutdown()`
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unsafe inventory scope error = %v", err)
	}
	request.CommandID = "teleport_player"
	request.Arguments = map[string]interface{}{"player_id": "KU_SOURCE", "destination_id": "KU_TARGET"}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.script, `v.userid=="KU_SOURCE"`) || !strings.Contains(sender.script, `v.userid=="KU_TARGET"`) || !strings.Contains(sender.script, `p.Transform:SetPosition(x,y,z)`) {
		t.Fatalf("unexpected teleport script: %s", sender.script)
	}
	request.Arguments["destination_id"] = "KU_SOURCE"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("same-player teleport error = %v", err)
	}
}

func TestWorldToolCommandsAreBounded(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{CommandID: "skip_days", Confirmation: "周末服", Arguments: map[string]interface{}{"days": float64(200)}}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `local d=200`) {
		t.Fatalf("skip days script = %s, %v", sender.script, err)
	}
	request.Arguments["days"] = float64(201)
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("skip days boundary error = %v", err)
	}
	request.CommandID = "set_time_scale"
	request.Arguments = map[string]interface{}{"multiplier": float64(4)}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `TheSim:SetTimeScale(4)`) {
		t.Fatalf("time scale script = %s, %v", sender.script, err)
	}
	request.Arguments["multiplier"] = "Inf"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("infinite time scale error = %v", err)
	}
	request.CommandID = "set_precipitation"
	request.Arguments = map[string]interface{}{"mode": "dynamic"}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `"ms_setprecipitationmode","dynamic"`) {
		t.Fatalf("precipitation script = %s, %v", sender.script, err)
	}
	request.Arguments["mode"] = `dynamic");c_shutdown()`
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unsafe precipitation mode error = %v", err)
	}
	request.CommandID = "set_world_wetness"
	request.Arguments = map[string]interface{}{"value": float64(35.5)}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `local target=35.5`) {
		t.Fatalf("wetness script = %s, %v", sender.script, err)
	}
	request.Arguments["value"] = float64(100.1)
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("wetness boundary error = %v", err)
	}
	request.CommandID = "set_world_temperature"
	request.Arguments = map[string]interface{}{"mode": "fixed", "value": float64(-25)}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `c:SetTemperatureMod(0,target)`) || !strings.Contains(sender.script, `local target=-25`) {
		t.Fatalf("fixed world temperature script = %s, %v", sender.script, err)
	}
	request.Arguments["value"] = float64(95.1)
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("world temperature boundary error = %v", err)
	}
	request.Arguments = map[string]interface{}{"mode": "dynamic"}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `c:SetTemperatureMod(1,0)`) {
		t.Fatalf("dynamic world temperature script = %s, %v", sender.script, err)
	}
	request.Arguments["mode"] = `dynamic");c_shutdown()`
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unsafe world temperature mode error = %v", err)
	}
}

func TestRollbackCommandAcceptsConfiguredSnapshotRetention(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{
		CommandID: "rollback", Confirmation: "周末服",
		Arguments: map[string]interface{}{"days": float64(10)},
	}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, "c_rollback(10)") {
		t.Fatalf("rollback script = %s, %v", sender.script, err)
	}
	request.Arguments["days"] = float64(25)
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, "c_rollback(25)") {
		t.Fatalf("configured rollback script = %s, %v", sender.script, err)
	}
	request.Arguments["days"] = float64(0)
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("rollback minimum error = %v", err)
	}
}

func TestWorldEventsOnlyRequirePlayerForLightning(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{CommandID: "trigger_world_event", Confirmation: "周末服", Arguments: map[string]interface{}{"event": "earthquake"}}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `"ms_forcequake"`) {
		t.Fatalf("earthquake script = %s, %v", sender.script, err)
	}
	request.Arguments["event"] = "lunar_hail"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `"ms_startlunarhail"`) {
		t.Fatalf("lunar hail script = %s, %v", sender.script, err)
	}
	request.Arguments["event"] = "lightning"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("missing lightning player error = %v", err)
	}
	request.Arguments["player_id"] = "KU_SAFE_PLAYER"
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `Vector3(p.Transform:GetWorldPosition())`) {
		t.Fatalf("lightning script = %s, %v", sender.script, err)
	}
}

func TestExpandedWorldEventsAndIncidentsRenderValidLua(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	for _, event := range []string{"lightning", "earthquake", "lunar_hail", "meteor_shower", "acid_rain", "moon_storm", "nightmare_calm", "nightmare_warn", "nightmare_wild", "nightmare_dawn", "reset_ruins"} {
		arguments := map[string]interface{}{"event": event}
		if event == "lightning" || event == "meteor_shower" {
			arguments["player_id"] = "KU_SAFE_PLAYER"
		}
		request := ExecuteRequest{CommandID: "trigger_world_event", Confirmation: "周末服", Arguments: arguments}
		if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
			t.Fatalf("%s event error = %v", event, err)
		}
		assertLuaSyntax(t, sender.script)
	}
	for _, incident := range []string{"hounds", "worms", "worm_boss", "frog_rain", "brightshade", "pirates"} {
		request := ExecuteRequest{CommandID: "trigger_incident", Confirmation: "周末服", Arguments: map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "incident": incident}}
		if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
			t.Fatalf("%s incident error = %v", incident, err)
		}
		assertLuaSyntax(t, sender.script)
	}
}

func TestNearbyEntityActionsUseAllowlistedComponents(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	expectedByAction := map[string]string{
		"delete": `protected={FX=true`, "extinguish": `Extinguish(true)`, "ignite": `Ignite(nil,nil,p)`,
		"restore": `SpawnPrefab(name,skin,nil,p.userid)`, "repair": `visitplayeritems(repair)`, "repair_boat": `repaired_treegrowth`,
		"freshen": `visitplayeritems(fresh)`, "heat": `GetCurrent(),70`, "cool": `GetCurrent(),-20`,
		"salvage": `winchtarget:Salvage()`, "haunt": `DoHaunt(p)`, "fertilize": `compostwrap`,
		"grow": `DoMagicGrowth`, "harvest": `CanBeHarvested`, "pick": `CanAcceptCount`,
		"chop": `ACTIONS.CHOP`, "mine": `ACTIONS.MINE`, "hammer": `ACTIONS.HAMMER`, "dig": `ACTIONS.DIG`, "till": `CanTillSoilAtPoint`,
		"build_complete": `ForceCompletion(p)`, "chaos": `math.random(#candidates)`, "electrocute": `PushEventImmediate("electrocute")`,
		"kill": `c.health:Kill()`, "freeze": `AddColdness(1,60)`, "sleep": `AddSleepiness(10,20)`,
		"panic": `h:Panic(60)`, "pacify": `c.combat:GiveUp()`, "root": `AddComponent("rooted")`, "taunt": `c.combat:SetTarget(p)`,
	}
	for action, expected := range expectedByAction {
		request := ExecuteRequest{
			CommandID: "act_nearby_entities", Confirmation: "周末服",
			Arguments: map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "action": action, "radius": float64(30)},
		}
		if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
			t.Fatalf("%s action error = %v", action, err)
		}
		assertLuaSyntax(t, sender.script)
		for _, fragment := range []string{`local R=30`, `local filter=""`, `filter==""or e.prefab==filter`, expected} {
			if !strings.Contains(sender.script, fragment) {
				t.Fatalf("%s action missing %q: %s", action, fragment, sender.script)
			}
		}
	}
	request := ExecuteRequest{
		CommandID: "act_nearby_entities", Confirmation: "周末服",
		Arguments: map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "prefab": `pigman");c_shutdown()`, "action": "kill", "radius": float64(10)},
	}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("unsafe nearby prefab error = %v", err)
	}
	request.Arguments = map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "prefab": "pigman", "action": "repair", "radius": float64(64)}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `local filter="pigman"`) {
		t.Fatalf("optional prefab filter script = %s, %v", sender.script, err)
	}
}

func TestPlayerAbilitiesFollowersAndBeefaloAreAllowlisted(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{CommandID: "set_player_ability", Confirmation: "周末服"}
	for _, ability := range []string{"no_cooldown", "one_hit_kill", "aoe_attack", "critical_work", "water_walk", "knockback_immunity", "no_hate", "stealth", "unlock_recipes"} {
		request.Arguments = map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "ability": ability, "enabled": true}
		if ability == "unlock_recipes" {
			request.Arguments["recipe_mode"] = "permanent"
		}
		if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
			t.Fatalf("%s ability error = %v", ability, err)
		}
		assertLuaSyntax(t, sender.script)
	}
	request.Arguments = map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "ability": "god_mode", "enabled": true}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("duplicate god ability remains in console command: %v", err)
	}
	request.Arguments = map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "ability": "no_cooldown", "enabled": "true"}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != ErrInvalidArguments {
		t.Fatalf("non-boolean ability state error = %v", err)
	}
	request.CommandID = "manage_followers"
	request.Arguments = map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "action": "loyal", "radius": float64(25)}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `maxfollowtime or 6000`) {
		t.Fatalf("follower script = %s, %v", sender.script, err)
	}
	assertLuaSyntax(t, sender.script)
	request.CommandID = "spawn_domesticated_beefalo"
	request.Arguments = map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "tendency": "rider"}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil || !strings.Contains(sender.script, `TENDENCY.RIDER`) || !strings.Contains(sender.script, `"saddle_race"`) {
		t.Fatalf("beefalo script = %s, %v", sender.script, err)
	}
	assertLuaSyntax(t, sender.script)
	request.Arguments = map[string]interface{}{
		"player_id": "KU_SAFE_PLAYER", "tendency": "custom", "saddle": "wathgrithr", "bell": "shadow_beef_bell",
		"domestication": float64(75), "hunger": float64(40), "obedience": float64(80), "health": float64(90),
		"ornery": float64(10), "rider": float64(30), "pudgy": float64(20),
	}
	if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
		t.Fatalf("custom beefalo error = %v", err)
	}
	assertLuaSyntax(t, sender.script)
	for _, fragment := range []string{`saddle_wathgrithr`, `shadow_beef_bell`, `domestication=0.75`, `ORNERY=10`} {
		if !strings.Contains(sender.script, fragment) {
			t.Fatalf("custom beefalo missing %q: %s", fragment, sender.script)
		}
	}
}

func TestCharacterPowerActionsRenderValidLua(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	actions := []string{
		"beard_none", "beard_short", "beard_medium", "beard_long", "woby_empty", "woby_half", "woby_full",
		"abigail_bond_1", "abigail_bond_2", "abigail_bond_3", "abigail_health_5", "abigail_health_50", "abigail_health_100", "abigail_shadow", "abigail_lunar_toggle",
		"inspiration_0", "inspiration_50", "inspiration_100", "fitness_0", "fitness_50", "fitness_100",
		"woody_beaver", "woody_goose", "woody_moose", "wx_charge_add", "wx_charge_remove",
		"wormwood_bloom_0", "wormwood_bloom_1", "wormwood_bloom_2", "wormwood_bloom_3", "wormwood_bloom_progress_start", "wormwood_bloom_progress_half", "wormwood_bloom_progress_end", "wormwood_bloom_grow", "wormwood_bloom_decay",
		"merm_king_hunger_0", "merm_king_hunger_50", "merm_king_hunger_100", "merm_king_health_5", "merm_king_health_50", "merm_king_health_100", "merm_king_trident", "merm_king_crown", "merm_king_shoulder",
	}
	for _, action := range actions {
		request := ExecuteRequest{CommandID: "set_character_power", Confirmation: "周末服", Arguments: map[string]interface{}{"player_id": "KU_SAFE_PLAYER", "action": action}}
		if _, err := service.Execute(context.Background(), "room", "world", request); err != nil {
			t.Fatalf("%s character action error = %v", action, err)
		}
		assertLuaSyntax(t, sender.script)
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

func TestScheduledAdminCommandsRetainAuditAndInteractiveConfirmation(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	if _, err := service.Execute(context.Background(), "room", "world", ExecuteRequest{CommandID: "shutdown"}); !errors.Is(err, ErrConfirmationNeeded) {
		t.Fatalf("interactive confirmation changed: %v", err)
	}
	for _, raw := range []string{"", "print('scheduled check')"} {
		run, err := service.ExecuteScheduled(context.Background(), "room", "world", ExecuteRequest{CommandID: "shutdown"}, raw)
		if err != nil || run.ID == "" || run.RoomID != "room" || run.WorldID != "world" {
			t.Fatalf("run=%#v err=%v", run, err)
		}
		if raw != "" && (run.RawCommand != raw || !strings.Contains(sender.script, raw)) {
			t.Fatalf("raw command not recorded/sent: %#v", run)
		}
	}
}
