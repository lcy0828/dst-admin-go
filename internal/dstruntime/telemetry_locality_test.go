package dstruntime

import (
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

func TestTelemetrySeparatesClusterConnectionsFromLocalEntities(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	installStaticSchedulerStub(state)
	preloadGameJSONHarness(state)
	if err := state.DoString(`
local clients={
    {userid="KU_ONE",name="Player One",prefab="willow",netid="steam-one",admin=true},
    {userid="KU_TWO",name="Player Two",prefab="wendy",netid="steam-two"},
    {userid="KU_ONE",name="[Host]"},
}
TheNet={GetSessionIdentifier=function() return "SESSION" end,GetClientTable=function() return clients end}
TheShard={GetShardId=function() return "1" end}
TheWorld={ismastersim=true}
UserToPlayer=function() error("cluster identity lookup must not decide locality") end
local function player(id,prefab)
    return {userid=id,prefab=prefab,components={health={currenthealth=75,maxhealth=150,GetPercent=function() return .5 end,IsDead=function() return false end}},HasTag=function() return false end}
end
first=player("KU_ONE","woodie")
second=player("KU_TWO","wendy")
AllPlayers={first}
`); err != nil {
		t.Fatal(err)
	}
	writes := map[string][]byte{}
	sim := state.NewTable()
	state.SetField(sim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
		writes[L.CheckString(2)] = []byte(L.CheckString(3))
		if callback, ok := L.Get(5).(*lua.LFunction); ok {
			callLuaFunction(t, state, callback, lua.LTrue)
		}
		return 0
	}))
	state.SetGlobal("TheSim", sim)
	module := loadLuaModule(t, state, "telemetry.lua")
	callLuaMethod(t, state, module, "Start", true)
	for _, test := range []struct{ setup, slot, localID string }{
		{`AllPlayers={first}`, "a", "KU_ONE"},
		{`AllPlayers={second}`, "b", "KU_TWO"},
		{`AllPlayers={}`, "a", ""},
		{`AllPlayers={first}; first.migration={}`, "b", "KU_ONE"},
	} {
		if err := state.DoString(test.setup); err != nil {
			t.Fatal(err)
		}
		callLuaMethod(t, state, module, "EmitOnce", true)
		snapshot, err := decodeSnapshotData(writes["mod_config_data/dst-admin/players-"+test.slot+".json"], "SESSION", "1", time.Now())
		if err != nil || !snapshot.LocalityKnown || len(snapshot.Players) != 2 {
			t.Fatalf("snapshot=%#v err=%v", snapshot, err)
		}
		for _, p := range snapshot.Players {
			if p.LocalEntity == nil || *p.LocalEntity != (p.ID == test.localID) {
				t.Fatalf("incorrect local evidence: %#v", p)
			}
			if p.ID != test.localID && (p.HealthPercent != nil || p.GameplayState != "unknown") {
				t.Fatalf("remote client became local: %#v", p)
			}
			if p.ID == "KU_ONE" && test.localID == "KU_ONE" && (p.Prefab != "woodie" || p.HealthPercent == nil || *p.HealthPercent != 50 || !p.Admin || p.NetID != "steam-one") {
				t.Fatalf("local entity/account merge: %#v", p)
			}
		}
	}
	// A newly spawned entity is retained even before cluster metadata arrives.
	if err := state.DoString(`AllPlayers={{userid="KU_NEW",prefab="wes",name="New",components={},HasTag=function() return false end}}`); err != nil {
		t.Fatal(err)
	}
	callLuaMethod(t, state, module, "EmitOnce", true)
	snapshot, err := decodeSnapshotData(writes["mod_config_data/dst-admin/players-a.json"], "SESSION", "1", time.Now())
	if err != nil || len(snapshot.Players) != 3 || snapshot.Players[2].ID != "KU_NEW" || snapshot.Players[2].LocalEntity == nil || !*snapshot.Players[2].LocalEntity {
		t.Fatal(snapshot, err)
	}
	// An absent local-entity list is unknown, never proof of an empty shard.
	if err := state.DoString(`AllPlayers=nil`); err != nil {
		t.Fatal(err)
	}
	callLuaMethod(t, state, module, "EmitOnce", true)
	snapshot, err = decodeSnapshotData(writes["mod_config_data/dst-admin/players-b.json"], "SESSION", "1", time.Now())
	if err != nil || snapshot.LocalityKnown || snapshot.Players[0].LocalEntity != nil {
		t.Fatal(snapshot, err)
	}
}
