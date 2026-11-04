package dstruntime

import (
	"fmt"
	lua "github.com/yuin/gopher-lua"
	"strings"
	"testing"
	"time"
)

func TestModNoticeValidationRejectsMetadataControlsAndInvalidUTF8(t *testing.T) {
	for _, text := range []string{"技能升级啦", "普通的中文通知"} {
		if !ValidModNotice(map[string]any{"source": "example_mod", "code": "level.up", "message": text}) {
			t.Fatal("valid notice rejected")
		}
	}
	cases := []map[string]any{
		{"source": "example_mod", "code": "level_up", "message": "hello", "risk": "low"},
		{"source": "example_mod", "code": "level_up", "message": "line\nbreak"},
		{"source": "example_mod", "code": "level_up", "message": strings.Repeat("中", 342)},
		{"source": "example_mod", "code": "level_up", "message": "\xff"},
		{"source": "example_mod", "code": "level_up", "message": 42},
		{"source": "evil()", "code": "level_up", "message": "hello"},
		{"source": "example_mod", "code": "", "message": "hello"},
	}
	for _, fields := range cases {
		if ValidModNotice(fields) {
			t.Fatal("unbounded/invalid metadata accepted", fields)
		}
	}
}

func TestModNoticeLuaTypedRateLimitAndEventIsolation(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	installStaticSchedulerStub(L)
	preloadStaticJSONHarness(L)
	now := int64(1800000000)
	L.SetField(L.GetGlobal("os"), "time", L.NewFunction(func(L *lua.LState) int { L.Push(lua.LNumber(now)); return 1 }))
	world := L.NewTable()
	L.SetField(world, "ismastersim", lua.LTrue)
	L.SetGlobal("TheWorld", world)
	sim := L.NewTable()
	L.SetField(sim, "SetPersistentString", L.NewFunction(func(*lua.LState) int { return 0 }))
	L.SetGlobal("TheSim", sim)
	mod := loadLuaModule(t, L, "events.lua")
	L.SetGlobal("events", mod)
	callLuaMethod(t, L, mod, "Start", true)
	code := `
 assert(events.Notify("example_mod", "level_up", "拓荒升级啦") == true)
 assert(events.Notify("example_mod", "level_up", function() end) == false)
 assert(events.Notify("example_mod", "level_up", string.rep("中", 342)) == false)
 assert(events.Notify("example_mod", "level_up", string.char(255)) == false)
 assert(events.Notify("example_mod", "level_up", string.char(194,128)) == false)
 assert(events.Notify("example_mod", "level_up", "a\nb") == false)
 assert(events.Notify("evil()", "level_up", "text") == false)
 assert(events.Emit("mod.notice", {source="example_mod", code="level_up", message="text", risk="low"}) == false)
 for i=1,4 do assert(events.Notify("example_mod", "level_up", "合规通知")) end
 assert(events.Notify("example_mod", "level_up", "超限消息") == false)
 for n=1,3 do for i=1,5 do assert(events.Notify("source_"..n, "level_up", "合规通知")) end end
 assert(events.Notify("another", "level_up", "总量超限") == false)
 assert(events.Emit("world.save", {}) == true)
 `
	if err := L.DoString(code); err != nil {
		t.Fatal(err)
	}
	now += 60
	if err := L.DoString(`assert(events.Notify("example_mod", "level_up", "新一分钟"))`); err != nil {
		t.Fatal(err)
	}
	callLuaMethod(t, L, mod, "Stop", true)
	if err := L.DoString(`assert(events.Notify("example_mod", "level_up", "停止后不可发送") == false)`); err != nil {
		t.Fatal(err)
	}
}

func TestModNoticeEventBatchUsesSameLocalAndAgentValidator(t *testing.T) {
	at := time.Now().UTC()
	value := fmt.Sprintf(`{"schemaVersion":1,"producerVersion":%q,"producerInstanceId":"example","sessionId":"session","shardId":"2","firstSequence":1,"lastSequence":1,"events":[{"sequence":1,"kind":"mod.notice","occurredAtUnix":%d,"fields":{"source":"example_mod","code":"level_up","message":"示例消息"}}]}`, RuntimeVersion, at.Unix())
	// decodeEventBatchData is the artifact decoder used by both transports.
	if _, err := decodeEventBatchData([]byte(value), "session", "2", at); err != nil {
		t.Fatal(err)
	}
	invalid := strings.Replace(value, `"message":"示例消息"`, `"message":"示例消息","readOnly":true`, 1)
	if _, err := decodeEventBatchData([]byte(invalid), "session", "2", at); err == nil {
		t.Fatal("forged fields accepted")
	}
}

func TestModNoticeActualLuaProducerRoundTripsToGoDecoder(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	events := loadLuaModule(t, L, "events.lua")
	L.SetGlobal("events", events)
	if err := L.DoString(`
 assert(events.Start())
 assert(events.Notify("example_mod", "skill.up", "O'Brien 的拓荒升到 2 级"))
 tick(2)
 assert(#writes==1)
 `); err != nil {
		t.Fatal(err)
	}
	raw := L.GetField(L.GetGlobal("files"), "mod_config_data/dst-admin/events-a.json").String()
	batch, err := decodeEventBatchData([]byte(raw), "SESSION", "1", time.Unix(1800000002, 0))
	if err != nil || len(batch.Events) != 1 {
		t.Fatalf("batch=%+v error=%v raw=%s", batch, err, raw)
	}
	if got := batch.Events[0].Fields["message"]; got != "O'Brien 的拓荒升到 2 级" {
		t.Fatal("text corrupted across Lua/Go boundary", got)
	}
	if err := L.DoString(`
 tick(60)
 for n=1,8 do assert(events.Notify("source_"..n,"code","独立来源")) end
 assert(events.Notify("source_9","code","来源数量超限")==false)
 `); err != nil {
		t.Fatal(err)
	}
}
