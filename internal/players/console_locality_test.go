package players

import (
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func TestConsoleLocalityUsesAllPlayersAndKeepsClusterMetadata(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	var output []string
	state.SetGlobal("print", state.NewFunction(func(L *lua.LState) int {
		output = append(output, L.CheckString(1))
		return 0
	}))
	if err := state.DoString(`
UserToPlayer=function() error("must not infer location from a cluster client") end
TheNet={GetClientTable=function() return {
    {userid="KU_ONE",name="玩家 一",prefab="willow",netid="steam-one",netscore=10,admin=true},
    {userid="KU_ONE",name="[Host]"},
    {userid="KU_TWO",name="Selecting"},
} end}
AllPlayers={
    {userid="KU_ONE",prefab="woodie",HasTag=function() return true end,components={}},
    {userid="KU_LOCAL",name="New local",prefab="wes",HasTag=function() return false end,components={}},
}
`); err != nil {
		t.Fatal(err)
	}
	if err := state.DoString(playerProbeScript("locality")); err != nil {
		t.Fatal(err)
	}
	values, complete, err := parseProbeOutput(strings.Join(output, "\n"), "locality")
	if err != nil || !complete || len(values) != 3 {
		t.Fatal(values, complete, err, output)
	}
	byID := map[string]Observation{}
	for _, value := range values {
		byID[value.ID] = value
	}
	local := byID["KU_ONE"]
	if local.LocalEntity == nil || !*local.LocalEntity || local.Name != "玩家 一" || local.NetID != "steam-one" || local.Prefab != "woodie" || local.GameplayState != GameplayStateGhost || !local.Admin {
		t.Fatal(local)
	}
	if selecting := byID["KU_TWO"]; selecting.LocalEntity == nil || *selecting.LocalEntity || selecting.GameplayState != GameplayStateSelectingCharacter {
		t.Fatal(selecting)
	}
	if justJoined := byID["KU_LOCAL"]; justJoined.LocalEntity == nil || !*justJoined.LocalEntity {
		t.Fatal(justJoined)
	}
	output = nil
	if err := state.DoString(`AllPlayers=nil`); err != nil {
		t.Fatal(err)
	}
	if err := state.DoString(playerProbeScript("missing")); err == nil {
		t.Fatal("missing AllPlayers was considered empty")
	}
	if _, complete, _ := parseProbeOutput(strings.Join(output, "\n"), "missing"); complete {
		t.Fatal("unknown locality was declared complete")
	}
}

func TestHistoricalConsoleBlocksCannotAttributeRemoteClusterClientsToWorld(t *testing.T) {
	line := "[DST-ADMIN-PLAYERS sample ITEM] " + strings.Join([]string{"KU_REMOTE", "Remote", "willow", "1", "0", "steam", "-1", "-1", "-1", "-1", "-999", "-1", "unknown", "0"}, "\t")
	values := parseProbeHistory(line + "\n[DST-ADMIN-PLAYERS sample DONE]\n")
	if len(values) != 0 {
		t.Fatal(values)
	}
	// The same output remains part of the live cluster roster, not history.
	live, complete, err := parseProbeOutput(line+"\n[DST-ADMIN-PLAYERS sample DONE]\n", "sample")
	if err != nil || !complete || len(live) != 1 || live[0].LocalEntity == nil || *live[0].LocalEntity {
		t.Fatal(live, complete, err)
	}
}
