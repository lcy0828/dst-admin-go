package dstruntime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// DST's save-game encoder escapes apostrophes as \\', which is not JSON.
// Model that difference here so the test exercises the actual Lua producer
// and Go reader instead of replacing both encoders with an always-valid stub.
func preloadGameJSONHarness(state *lua.LState) {
	state.PreloadModule("json", func(L *lua.LState) int {
		module := L.NewTable()
		for _, name := range []string{"encode", "encode_compliant"} {
			name := name
			L.SetField(module, name, L.NewFunction(func(L *lua.LState) int {
				data, err := json.Marshal(luaJSONValue(L.Get(1)))
				if err != nil {
					L.RaiseError("encode: %v", err)
					return 0
				}
				encoded := string(data)
				if name == "encode" {
					encoded = strings.ReplaceAll(encoded, "'", `\'`)
				}
				L.Push(lua.LString(encoded))
				return 1
			}))
		}
		L.Push(module)
		return 1
	})
}

func luaJSONValue(value lua.LValue) interface{} {
	switch value := value.(type) {
	case *lua.LTable:
		if value.Len() > 0 {
			items := make([]interface{}, value.Len())
			for i := range items {
				items[i] = luaJSONValue(value.RawGetInt(i + 1))
			}
			return items
		}
		fields := make(map[string]interface{})
		value.ForEach(func(key, item lua.LValue) { fields[key.String()] = luaJSONValue(item) })
		return fields
	case lua.LString:
		return string(value)
	case lua.LNumber:
		return float64(value)
	case lua.LBool:
		return bool(value)
	default:
		return nil
	}
}

func TestTelemetryJSONRoundTripsPlayerNamesAndErrors(t *testing.T) {
	for _, name := range []string{"Don't you cry", `中文玩家 "quoted" \\ path`, "名字\n换行\t制表\r回车 🎮"} {
		t.Run(name, func(t *testing.T) {
			state := lua.NewState()
			defer state.Close()
			preloadGameJSONHarness(state)
			state.SetGlobal("player_name", lua.LString(name))
			if err := state.DoString(`
TheNet = {
    GetSessionIdentifier = function() return "SESSION" end,
    GetClientTable = function() return {
        { userid = "KU_QUOTE", name = player_name, prefab = "wendy", playerage = 10 },
        { userid = "KU_OTHER", name = "lcy", prefab = "wathgrithr", playerage = 277 },
    } end,
}
TheShard = { GetShardId = function() return "1" end }
TheWorld = { ismastersim = true, DoPeriodicTask = function() return { Cancel = function() end } end }
DSTAdmin = { WorldState = { Status = function() return { running = true, ready = true } end } }
GetTime = function() return 1 end
`); err != nil {
				t.Fatal(err)
			}
			writes := make(map[string][]byte)
			theSim := state.NewTable()
			state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
				writes[L.CheckString(2)] = []byte(L.CheckString(3))
				if callback, ok := L.Get(5).(*lua.LFunction); ok {
					callLuaFunction(t, state, callback, lua.LTrue)
				}
				return 0
			}))
			state.SetGlobal("TheSim", theSim)
			module := loadLuaModule(t, state, "telemetry.lua")
			callLuaMethod(t, state, module, "Start", true)
			for _, slot := range []string{"a", "b"} {
				callLuaMethod(t, state, module, "EmitOnce", true)
				data := writes["mod_config_data/dst-admin/players-"+slot+".json"]
				snapshot, err := decodeSnapshotData(data, "SESSION", "1", time.Now())
				if err != nil {
					t.Fatalf("player name %q broke shard snapshot: %v", name, err)
				}
				if len(snapshot.Players) != 2 || snapshot.Players[0].Name != name || snapshot.Players[1].Name != "lcy" {
					t.Fatalf("player identities changed: %#v", snapshot.Players)
				}
			}
			if err := state.DoString(`TheNet.GetClientTable = function() error("can't sample player's state") end`); err != nil {
				t.Fatal(err)
			}
			callLuaMethod(t, state, module, "EmitOnce", false)
			health, err := decodeHealthData(writes["mod_config_data/dst-admin/health.json"], "SESSION", "1", time.Now())
			if err != nil || health.LastError == nil || !strings.Contains(*health.LastError, "can't sample player's state") || health.ConsecutiveFailures != 1 {
				t.Fatalf("health error lost: %#v, %v", health, err)
			}
		})
	}
}
