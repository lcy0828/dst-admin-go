package console

import (
	"os"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func TestLegacyMovementEffectsAndMissingEvidence(t *testing.T) {
	base := Run{Mode: "builtin", CommandID: "relocate_players", Status: RunSucceeded, ExecutionOutcome: "confirmed", Script: "w.MovePlayers(moves)"}
	good := `[DST-ADMIN-MOVE-V1] {"state":"confirmed","attempted":1,"verified":1,"observedAt":1,"items":[{"playerId":"KU_EXAMPLE","state":"confirmed","point":{"x":12,"z":24},"before":{"x":1,"z":2},"after":{"x":12,"z":24}}]}`
	for _, tc := range []struct {
		name, text string
		status     RunStatus
		complete   bool
	}{
		{"confirmed", good, RunSucceeded, true},
		{"partial", strings.ReplaceAll(strings.ReplaceAll(good, `"state":"confirmed","attempted"`, `"state":"partial","attempted"`), `"verified":1`, `"verified":0`), RunUncertain, false}, // inconsistent evidence rejected
		{"uncertain", strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(good, `"state":"confirmed","attempted"`, `"state":"partial","attempted"`), `"verified":1`, `"verified":0`), `"state":"confirmed","point"`, `"state":"uncertain","point"`), "partial", true},
		{"wrong position", strings.Replace(good, `"after":{"x":12`, `"after":{"x":50`, 1), RunUncertain, false},
		{"missing", "", RunUncertain, false},
		{"duplicate", good + "\n" + good, RunUncertain, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := base
			run.Output = &Output{Text: tc.text}
			r := workbenchResult(describeResult(run))
			if r.Status != tc.status || r.Complete != tc.complete {
				t.Fatalf("%+v", r)
			}
		})
	}
	base.Output = &Output{Text: good, Truncated: true}
	if movementResult(base) != nil {
		t.Fatal("accepted truncated output")
	}
	base.Output.Truncated = false
	base.Status = RunSent
	if movementResult(base) != nil {
		t.Fatal("accepted sent without receipt")
	}
	base.Status = RunSucceeded
	base.Output = &Output{Text: `[DST-ADMIN-MOVE-V1] {"state":"preview","previewId":"00000000-0000-4000-8000-000000000001","expiresAt":61,"observedAt":1,"attempted":0,"verified":0,"items":[{"playerId":"KU_EXAMPLE","state":"prepared","point":{"x":12,"z":24},"before":{"x":1,"z":2}}]}`}
	r := workbenchResult(describeResult(base))
	if r.Movement == nil || r.Movement.State != "preview" || !strings.Contains(r.Message, "尚未传送") {
		t.Fatalf("preview became success: %+v", r)
	}
}

func TestMovementPreviewTemplateKeepsTheFrozenSet(t *testing.T) {
	for _, command := range []string{"teleport_player", "relocate_players", "teleport_to_nearest_entity"} {
		args := map[string]interface{}{"player_id": "KU_ALPHA", "destination_id": "KU_BRAVO", "other_player_id": "KU_BRAVO", "action": "gather", "prefab": "spiderden", "phase": "execute", "preview_id": "00000000-0000-4000-8000-000000000001"}
		script, err := builtinTemplates()[command].render(args)
		if err != nil || strings.Contains(script, "AllPlayers") || strings.Contains(script, "Ents") || strings.Contains(script, "UserToPlayer") || !strings.Contains(script, "w.MovePlayers(nil,") {
			t.Fatalf("execution re-resolved targets: %s %v", script, err)
		}
		args["preview_id"] = `');c_shutdown();--`
		if _, err = builtinTemplates()[command].render(args); err == nil {
			t.Fatal("accepted forged preview")
		}
		for _, phase := range []interface{}{true, "", 12} {
			args["phase"] = phase
			if _, err = builtinTemplates()[command].render(args); err == nil {
				t.Fatal("invalid phase fell back to immediate movement")
			}
		}
	}
}

// Execute the real rendered legacy entry points with the shared Lua module.
// This checks command compatibility without touching an actual game world.
func TestLegacyRenderedMovementCompatibility(t *testing.T) {
	for _, action := range []string{"forward", "recall", "swap", "gather", "teleport", "nearest", "nearest-budget", "old-runtime", "partial", "blocked", "maintenance"} {
		t.Run(action, func(t *testing.T) {
			L := lua.NewState()
			defer L.Close()
			if e := L.DoString(`
Workbench={};mutations=0;blocked=false;partial=false
package.preload.json=function()return {encode_compliant=function(v)result=v;return "evidence" end}end
Vector3=function(x,y,z)return {x=x,y=y,z=z}end
TheWorld={Map={IsAboveGroundAtPoint=function()return not blocked end,GetPlatformAtPoint=function()return destination_boat end,IsPassableAtPoint=function(_,x,y,z,water,exclude_boats)assert(water==false and exclude_boats==true);return not blocked end,IsOceanAtPoint=function()return false end,IsGroundTargetBlocked=function()return false end}}
TheNet={GetSessionIdentifier=function()return "session"end}
TheSim={FindEntities=function()return AllPlayers end}
function mk(id,x,z)
local p={userid=id,x=x,z=z,components={}}
function p:IsValid()return true end
function p:HasTag(t)return t=="player"end
p.Transform={GetWorldPosition=function()return p.x,0,p.z end}
p.Physics={Teleport=function(_,x,y,z)mutations=mutations+1;if not partial or id=="KU_ALPHA" then p.x=x;p.z=z end end}
return p end
a=mk("KU_ALPHA",0,0);b=mk("KU_BRAVO",20,20);c=mk("KU_CHARLIE",40,40)
AllPlayers={a,b,c};UserToPlayer=function(id)for _,p in ipairs(AllPlayers)do if id==p.userid then return p end end end
Ents={target={prefab="spiderden",IsValid=function()return true end,HasTag=function()return false end,Transform={GetWorldPosition=function()return 60,0,60 end}}}
DSTAdmin={Commands={Workbench=Workbench}}
`); e != nil {
				t.Fatal(e)
			}
			src, e := os.ReadFile("../dstruntime/assets/teleport.lua")
			if e != nil {
				t.Fatal(e)
			}
			if e = L.DoString(string(src)); e != nil {
				t.Fatal(e)
			}
			args := map[string]interface{}{"player_id": "KU_ALPHA", "other_player_id": "KU_BRAVO", "action": action}
			command := "relocate_players"
			switch action {
			case "teleport":
				command = "teleport_player"
				args = map[string]interface{}{"player_id": "KU_ALPHA", "destination_id": "KU_BRAVO"}
			case "nearest", "nearest-budget":
				command = "teleport_to_nearest_entity"
				args = map[string]interface{}{"player_id": "KU_ALPHA", "prefab": "spiderden"}
				if action == "nearest-budget" {
					_ = L.DoString(`for i=1,4100 do Ents[i]=Ents.target end`)
				}
			case "old-runtime":
				args["action"] = "forward"
				_ = L.DoString(`Workbench.MovePlayers=nil`)
			case "blocked":
				args["action"] = "forward"
				_ = L.DoString(`blocked=true`)
			case "maintenance":
				args["action"] = "forward"
				_ = L.DoString(`DSTAdmin.Barriers={Status=function()return {holding=true}end}`)
			case "partial":
				args["action"] = "swap"
				_ = L.DoString(`partial=true`)
			}
			script, e := builtinTemplates()[command].render(args)
			if e != nil {
				t.Fatal(e)
			}
			e = L.DoString(script)
			if action == "old-runtime" || action == "blocked" || action == "maintenance" || action == "nearest-budget" {
				if e == nil || L.GetGlobal("mutations") != lua.LNumber(0) {
					t.Fatalf("unsafe execution %v", e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			check := `assert(result and result.state=="confirmed");assert(result.verified==result.attempted)`
			switch action {
			case "partial":
				check = `assert(result.state=="partial");assert(result.items[1].before and result.items[1].after);assert(result.items[2].state=="uncertain")`
			case "forward", "teleport":
				check += `;assert((a.x-20)^2+(a.z-20)^2<=36);assert(b.x==20)`
			case "recall":
				check += `;assert(b.x^2+b.z^2<=36);assert(a.x==0)`
			case "swap":
				check += `;assert((a.x-20)^2+(a.z-20)^2<=36);assert(b.x^2+b.z^2<=36)`
			case "gather":
				check += `;assert(result.verified==2);assert((b.x-c.x)^2+(b.z-c.z)^2>=2.25)`
			case "nearest":
				check += `;assert(a.x==60 and a.z==60)`
			}
			if e = L.DoString(check); e != nil {
				t.Fatal(e)
			}
		})
	}
}
