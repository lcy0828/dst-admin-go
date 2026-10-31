package console

import (
	"strings"
	"testing"
)

func TestControlledSpawnOnlyUsesFixedCreatureAllowlist(t *testing.T) {
	for _, name := range []string{"座狼", "狗王", "warg", "Varg", "蜘蛛", "rabbit"} {
		if _, err := ResolveSpawnEntity(name, 2); err != nil {
			t.Fatal(name, err)
		}
	}
	for _, name := range []string{"beefalo", "deerclops", "meatballs", "spiderden", "example_mod", `warg");c_shutdown()--`, " 座狼"} {
		if _, err := ResolveSpawnEntity(name, 1); err == nil {
			t.Fatal("accepted", name)
		}
	}
	for _, n := range []int{-1, 0, 6, 1000} {
		if _, err := ResolveSpawnEntity("warg", n); err == nil {
			t.Fatal("accepted count", n)
		}
	}
}

func TestControlledSpawnPreflightsMapAndUsesActualReceipt(t *testing.T) {
	base := `AllPlayers={{userid="KU_TEST123",Transform={GetWorldPosition=function()return 10,0,20 end}}};TheWorld={Map={IsPassableAtPoint=function()return true end,IsOceanAtPoint=function()return false end}};local guid=0;SpawnPrefab=function(name)guid=guid+1;local e={GUID=guid,prefab=name,Transform={}};local x,y,z;e.Transform.SetPosition=function(_,a,b,c)x,y,z=a,b,c end;e.Transform.GetWorldPosition=function()return x,y,z end;return e end;`
	for _, tc := range []struct {
		name, extra, status string
		n                   *int
		may                 bool
	}{
		{"success", "", "succeeded", ptrGrant(3), true},
		{"no land", `TheWorld.Map.IsPassableAtPoint=function()return false end;SpawnPrefab=function()error("must not spawn")end;`, "failed", ptrGrant(0), false},
		{"ocean", `TheWorld.Map.IsOceanAtPoint=function()return true end;SpawnPrefab=function()error("must not spawn")end;`, "failed", ptrGrant(0), false},
		{"migration", `AllPlayers[1].migration=true;SpawnPrefab=function()error("must not spawn")end;`, "failed", ptrGrant(0), false},
		{"missing map", `TheWorld.Map=nil;SpawnPrefab=function()error("must not spawn")end;`, "failed", ptrGrant(0), false},
		{"partial", `local original=SpawnPrefab;local n=0;SpawnPrefab=function(name)n=n+1;if n>1 then error("constructor failed")end;return original(name)end;`, "failed", ptrGrant(1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]interface{}{"player_id": "KU_TEST123", "prefab": "warg", "count": 3}
			script, err := spawnPlayerEntityTemplate().render(args)
			if err != nil {
				t.Fatal(err)
			}
			out, err := auditLuaOutput(t, script, base+tc.extra)
			if err != nil {
				t.Fatal(err)
			}
			run := Run{RoomID: "room", Mode: "builtin", CommandID: "spawn_player_entity", Arguments: args, Script: script, Status: RunSucceeded, ExecutionOutcome: "confirmed", Output: &Output{Text: out}}
			run.SpawnAudit = spawnAudit(run)
			result := projectEntitySpawn(run)
			if result.Status != tc.status || result.Spawned == nil || *result.Spawned != *tc.n || result.MayHaveExecuted != tc.may {
				t.Fatalf("%+v output=%s", result, out)
			}
			if tc.name == "success" && (result.SpawnAudit == nil || len(result.SpawnAudit.Records) != 3) {
				t.Fatal(result)
			}
		})
	}
}

func TestControlledSpawnUnknownDuplicateOrTruncatedOutputIsNeverSuccess(t *testing.T) {
	valid := "[DST-ADMIN-CONTROLLED-SPAWN]\tKU_TEST123\twarg\t3\t3\tOK\n"
	for _, output := range []string{"", valid + valid, strings.Replace(valid, "KU_TEST123", "KU_OTHER123", 1), strings.Replace(valid, "\t3\tOK", "\t2\tOK", 1)} {
		r := projectEntitySpawn(Run{Arguments: map[string]interface{}{"player_id": "KU_TEST123", "prefab": "warg", "count": 3}, Status: RunSucceeded, ExecutionOutcome: "confirmed", Output: &Output{Text: output}})
		if r.Status != "uncertain" || r.Complete || !r.MayHaveExecuted {
			t.Fatal(r)
		}
	}
	for _, status := range []RunStatus{RunSent, RunSucceeded} {
		r := projectEntitySpawn(Run{Arguments: map[string]interface{}{"player_id": "KU_TEST123", "prefab": "warg", "count": 3}, Status: status, ExecutionOutcome: "confirmed", Output: &Output{Text: valid, Truncated: true}})
		if r.Status != "uncertain" {
			t.Fatal(r)
		}
	}
}
