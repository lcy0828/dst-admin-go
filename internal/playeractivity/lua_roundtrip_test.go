package playeractivity

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Execute the deployed Lua producer, then run its exact output through strict
// envelope/event validation and SQLite persistence (including KLEI headers).
func TestLuaProducerToStrictReaderAndSQLite(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	var jsonValue func(lua.LValue) any
	jsonValue = func(v lua.LValue) any {
		switch x := v.(type) {
		case *lua.LTable:
			if x.Len() > 0 {
				a := []any{}
				for i := 1; i <= x.Len(); i++ {
					a = append(a, jsonValue(x.RawGetInt(i)))
				}
				return a
			}
			m := map[string]any{}
			x.ForEach(func(k, v lua.LValue) { m[k.String()] = jsonValue(v) })
			return m
		case lua.LNumber:
			return float64(x)
		case lua.LString:
			return string(x)
		case lua.LBool:
			return bool(x)
		default:
			return nil
		}
	}
	L.PreloadModule("json", func(L *lua.LState) int {
		m := L.NewTable()
		for _, name := range []string{"encode", "encode_compliant"} {
			name := name
			L.SetField(m, name, L.NewFunction(func(L *lua.LState) int {
				b, err := json.Marshal(jsonValue(L.Get(1)))
				if err != nil {
					L.RaiseError("encode failed")
					return 0
				}
				encoded := string(b)
				if name == "encode" {
					encoded = strings.ReplaceAll(encoded, "'", `\'`)
				}
				L.Push(lua.LString(encoded))
				return 1
			}))
		}
		L.Push(m)
		return 1
	})
	L.SetGlobal("activityNow", lua.LNumber(time.Now().Unix()))
	L.SetGlobal("playerName", lua.LString(`中文 O'Brian "引号"`))
	if err := L.DoString(`
now=activityNow;os.time=function()return now end
files={};TheSim={GetPersistentString=function(self,name,cb)cb(false,"")end,SetPersistentString=function(self,name,data,encoded,cb)files[name]=data;if cb then cb(true)end end}
TheWorld={ismastersim=true,listeners={},Map={GetTileAtPoint=function()return 6 end}}
function TheWorld:ListenForEvent(name,cb)self.listeners[name]=cb end
function TheWorld:RemoveEventCallback(name)self.listeners[name]=nil end
function TheWorld:DoStaticTaskInTime()return {Cancel=function()end}end
TheNet={GetSessionIdentifier=function()return "SESSION" end};TheShard={GetShardId=function()return "0" end}
p={userid="KU_TEST",name=playerName,prefab="wilson",listeners={},Transform={GetWorldPosition=function()return 0,0,-20 end}}
function p:ListenForEvent(name,cb)self.listeners[name]=cb end
function p:RemoveEventCallback(name)self.listeners[name]=nil end
AllPlayers={}
`); err != nil {
		t.Fatal(err)
	}
	clock, err := os.ReadFile("../dstruntime/assets/clock.lua")
	if err != nil {
		t.Fatal(err)
	}
	activity, err := os.ReadFile("../dstruntime/assets/activity.lua")
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := L.LoadString(string(clock) + "\n" + string(activity))
	if err != nil {
		t.Fatal(err)
	}
	if err = L.CallByParam(lua.P{Fn: chunk, NRet: 1, Protect: true}); err != nil {
		t.Fatal(err)
	}
	L.SetGlobal("activity", L.Get(-1))
	L.Pop(1)
	if err = L.DoString(`
activity.Start();TheWorld.listeners.ms_playerjoined(TheWorld,p)
p.listeners.death(p,{});p.listeners.ms_respawnedfromghost(p,{})
p.listeners.onpickupitem(p,{item={prefab="log",GUID=10}})
p.listeners.onhitother(p,{target={prefab="spider",GUID=11},damageresolved=20})
now=now+1
assert(activity.Drain(function(ok)assert(ok)end))
`); err != nil {
		t.Fatal(err)
	}
	s, src := testService(t)
	files := L.GetGlobal("files").(*lua.LTable)
	files.ForEach(func(k, v lua.LValue) {
		name := strings.TrimPrefix(k.String(), "mod_config_data/dst-admin/")
		src.files[name] = []byte("KLEI 1\n" + v.String())
	})
	s.collect(context.Background(), "room", "world", Defaults())
	row, _ := s.store.cursor("room", "world")
	if row.Error != "" || row.Generation != 1 {
		t.Fatalf("real producer rejected: %+v", row)
	}
	page, err := s.Events("room", Query{UserID: "KU_TEST"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, e := range page.Items {
		if strings.HasPrefix(e.Kind, "collection.") {
			continue
		}
		kinds[e.Kind] = true
		if e.Name != `中文 O'Brian "引号"` || e.Position == nil || e.Position.X != 0 || e.Position.Z != -20 {
			t.Fatalf("encoding/coordinates changed: %+v", e)
		}
	}
	for _, kind := range []string{"player.joined", "player.death", "player.respawned", "action.pickup", "combat.attack"} {
		if !kinds[kind] {
			t.Fatalf("event missing: %s", kind)
		}
	}
}
