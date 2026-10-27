package console

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"dont/internal/dstruntime"

	lua "github.com/yuin/gopher-lua"
)

func auditLuaOutput(t *testing.T, script, setup string) (string, error) {
	t.Helper()
	state := lua.NewState()
	defer state.Close()
	var output strings.Builder
	state.SetGlobal("print", state.NewFunction(func(l *lua.LState) int {
		for index := 1; index <= l.GetTop(); index++ {
			if index > 1 {
				output.WriteByte('\t')
			}
			output.WriteString(l.Get(index).String())
		}
		output.WriteByte('\n')
		return 0
	}))
	if err := state.DoString(setup); err != nil {
		t.Fatal(err)
	}
	err := state.DoString(script)
	return output.String(), err
}

func auditRun(command, output string) Run {
	return Run{Mode: "builtin", CommandID: command, Status: RunSucceeded, Script: withSpawnAudit(""),
		Arguments: map[string]interface{}{"player_id": "KU_TEST123"}, Output: &Output{Text: output}}
}

func TestSpawnAuditReadsActualEntityPositionAndSurvivesDatabaseReload(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	request := ExecuteRequest{CommandID: "spawn_entity", Confirmation: "周末服", Arguments: map[string]interface{}{"player_id": "KU_TEST123", "prefab": "beefalo", "count": float64(2)}}
	script, err := service.templates[request.CommandID].render(request.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	output, err := auditLuaOutput(t, script, `
 AllPlayers={{userid="KU_TEST123",Transform={GetWorldPosition=function() return 10,0,-20 end}}}
 local guid=100
 SpawnPrefab=function(name)
   guid=guid+1
   local e={prefab=name,GUID=guid,Transform={}}
   local x,y,z=0,0,0
   e.Transform.SetPosition=function(_,nx,ny,nz) x,y,z=nx+.25,ny,nz end
   e.Transform.GetWorldPosition=function() return x,y,z end
   return e
 end
 `)
	if err != nil {
		t.Fatal(err)
	}
	service.commander = &captureCommander{replies: []dstruntime.CommandReceipt{{OK: true, Details: map[string]interface{}{"output": map[string]interface{}{"text": output, "truncated": false}}}}}
	run, err := service.Execute(context.Background(), "room", "world", request)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.store.Get(run.ID)
	if err != nil || stored.SpawnAudit == nil || stored.SpawnAudit.Status != "complete" {
		t.Fatalf("reload = %#v, %v", stored.SpawnAudit, err)
	}
	audit := stored.SpawnAudit
	if *audit.Total != 2 || audit.Origin.X != 10 || audit.Origin.Z != -20 || len(audit.Records) != 2 || audit.Records[0].EntityGUID != "101" {
		t.Fatalf("audit = %#v", audit)
	}
	if audit.Records[0].Position.X != 12.25 || math.Abs(audit.Records[1].Position.X-8.25) > .00001 {
		t.Fatalf("reported intended instead of actual landing: %#v", audit.Records)
	}
}

func TestGiveAuditDistinguishesInventoryDropsAndConsumedEntities(t *testing.T) {
	script, err := builtinTemplates()["give_item"].render(map[string]interface{}{"player_id": "KU_TEST123", "prefab": "goldnugget", "count": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{"inventory", "world", "unresolved"} {
		t.Run(destination, func(t *testing.T) {
			setup := fmt.Sprintf(`
 local mode=%q
 local inv={GiveItem=function(_,e) if mode=="inventory" then e.components.inventoryitem.owner={} elseif mode=="unresolved" then e.valid=false end end}
 AllPlayers={{userid="KU_TEST123",Transform={GetWorldPosition=function() return 11,0,12 end},components={inventory=inv}}}
 SpawnPrefab=function(name) return {prefab=name,GUID=42,valid=true,IsValid=function(self) return self.valid end,components={inventoryitem={}},Transform={GetWorldPosition=function() return 9,0,8 end}} end
 `, destination)
			output, err := auditLuaOutput(t, script, setup)
			if err != nil {
				t.Fatal(err)
			}
			run := auditRun("give_item", output)
			run.Script = script
			audit := spawnAudit(run)
			if audit.Status != "complete" || len(audit.Records) != 1 || audit.Records[0].Destination != destination {
				t.Fatalf("audit = %#v, output=%s", audit, output)
			}
			if destination == "world" {
				if audit.Records[0].Position.X != 9 || audit.Records[0].Position.Z != 8 {
					t.Fatal("did not capture actual dropped item position")
				}
			} else if audit.Records[0].Position != nil {
				t.Fatal("invented a ground position for an inventory/consumed item")
			}
		})
	}
}

func TestSpawnAuditIsBoundedAndReportsPartialCapture(t *testing.T) {
	output, err := auditLuaOutput(t, withSpawnAudit(`for i=1,100 do __dst_record({prefab="flint",GUID=i,Transform={GetWorldPosition=function() return i,0,0 end}},nil,"world",1) end`), "")
	if err != nil {
		t.Fatal(err)
	}
	audit := spawnAudit(auditRun("give_recipe_materials", output))
	if audit.Status != "partial" || len(audit.Records) != 40 || *audit.Total != 100 || audit.Omitted != 60 || audit.Origin != nil || len(output) > 16<<10 {
		t.Fatalf("audit=%#v, output=%d bytes", audit, len(output))
	}
	output, err = auditLuaOutput(t, withSpawnAudit(`__dst_record({prefab="beefalo",GUID=1,Transform={GetWorldPosition=function() return 0,0,0 end}},nil,"world",1);error("later mutation failed")`), "")
	if err == nil {
		t.Fatal("expected script failure")
	}
	run := auditRun("spawn_domesticated_beefalo", output)
	run.Status = RunFailed
	audit = spawnAudit(run)
	if audit.Status != "partial" || len(audit.Records) != 1 || audit.Total != nil || audit.Records[0].Position.X != 0 {
		t.Fatalf("lost valid partial evidence: %#v", audit)
	}
}

func TestSpawnAuditDoesNotInventEvidenceFromLegacyRawOrMalformedOutput(t *testing.T) {
	valid := spawnAuditPrefix + "ORIGIN\t0\t0\t0\n" + spawnAuditPrefix + "ITEM\tbeefalo\t1\tworld\t1\t0\t0\t0\n" + spawnAuditPrefix + "END\t1\n"
	run := auditRun("spawn_entity", valid)
	if spawnAudit(run).Status != "complete" {
		t.Fatal("zero positions must be accepted")
	}
	for _, mode := range []string{"raw", "custom"} {
		run.Mode = mode
		if spawnAudit(run) != nil {
			t.Fatal("raw/custom output was interpreted as an instrumented builtin")
		}
	}
	run = auditRun("spawn_entity", valid)
	run.Script = "SpawnPrefab('beefalo')"
	if spawnAudit(run).Status != "unavailable" {
		t.Fatal("legacy command gained invented audit evidence")
	}
	for _, output := range []string{
		strings.ReplaceAll(valid, "world\t1\t0", "world\t1\tnan"),
		strings.ReplaceAll(valid, "END\t1", "END\t0"),
		strings.ReplaceAll(valid, "world\t1", "arbitrary\t1"),
		strings.ReplaceAll(valid, "END\t1", "END\t99999999999"),
		valid + spawnAuditPrefix + "END\t1\n",
	} {
		if audit := spawnAudit(auditRun("spawn_entity", output)); audit.Status == "complete" {
			t.Fatalf("accepted malformed evidence: %s", output)
		}
	}
	run = auditRun("spawn_entity", valid)
	run.Output.Truncated = true
	if spawnAudit(run).Status != "partial" {
		t.Fatal("truncated receipt was presented as complete")
	}
}

func TestSpawnAuditCannotTurnRecordingErrorsIntoActionErrors(t *testing.T) {
	output, err := auditLuaOutput(t, withSpawnAudit(`__dst_record({prefab="beefalo",Transform={GetWorldPosition=function() error("broken mod transform") end}},nil,"world",1);print("action completed")`), "")
	if err != nil || !strings.Contains(output, "action completed") || spawnAudit(auditRun("spawn_entity", output)).Status != "partial" {
		t.Fatalf("output=%s, error=%v", output, err)
	}
}

func TestInstrumentedBuiltinScriptsFitExistingRuntimeLimit(t *testing.T) {
	for _, id := range []string{"spawn_entity", "spawn_domesticated_beefalo", "give_item", "give_blueprint", "give_recipe_materials"} {
		arguments := map[string]interface{}{"player_id": "KU_" + strings.Repeat("x", 64), "prefab": strings.Repeat("a", 80), "count": float64(1), "tendency": "custom", "saddle": "wathgrithr", "bell": "shadow_beef_bell"}
		script, err := builtinTemplates()[id].render(arguments)
		if err != nil || len(script) > 4096 {
			t.Fatalf("%s = %d bytes, %v", id, len(script), err)
		}
		state := lua.NewState()
		_, err = state.LoadString(script)
		state.Close()
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}
