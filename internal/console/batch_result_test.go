package console

import (
	"context"
	"strings"
	"testing"
)

func TestBatchEvidenceRejectsMissingMalformedAndUnconfirmedReceipts(t *testing.T) {
	valid := `[DST-ADMIN-BATCH-V1] {"state":"partial","matched":3,"attempted":2,"verified":1,"unconfirmed":1,"skipped":1,"failed":0}`
	base := Run{Mode: "builtin", CommandID: "act_nearby_entities", Script: "w.Nearby(", Status: RunSucceeded, ExecutionOutcome: "confirmed", Output: &Output{Text: valid}}
	if r := workbenchResult(base); r.Status != "partial" || !r.Complete || r.Batch.Verified != 1 {
		t.Fatal(r)
	}
	for _, output := range []string{"", valid + "\n" + valid, strings.Replace(valid, `"matched":3`, `"matched":2`, 1), strings.Replace(valid, `"failed":0`, `"failed":null`, 1), strings.Replace(valid, `"partial"`, `"completed"`, 1), strings.Replace(valid, `"failed":0`, `"failed":0,"forged":true`, 1)} {
		run := base
		run.Output = &Output{Text: output}
		if r := workbenchResult(run); r.Status != RunUncertain || r.Complete || r.Batch != nil {
			t.Fatalf("%s: %+v", output, r)
		}
		if r := describeResult(run); r.Status != RunUncertain {
			t.Fatal(r)
		}
	}
	for _, status := range []RunStatus{RunSending, RunFailed, RunUncertain} {
		run := base
		run.Status = status
		if batchResult(run) != nil {
			t.Fatal("accepted unconfirmed output", status)
		}
	}
	base.Output.Truncated = true
	if batchResult(base) != nil {
		t.Fatal("accepted truncated receipt")
	}
}

func TestNearbyRequiresCallerAndKeepsPreviewReadOnlyButNotPublic(t *testing.T) {
	args := map[string]interface{}{"player_id": "KU_TEST123", "action": "freeze", "radius": 10}
	if _, e := ValidateBuiltinWorkbench("act_nearby_entities", args, false); e != nil {
		t.Fatal(e)
	}
	args["phase"] = "execute"
	args["preview_id"] = "00000000-0000-4000-8000-000000000001"
	if _, e := ValidateBuiltinWorkbench("act_nearby_entities", args, false); e != ErrConfirmationNeeded {
		t.Fatal(e)
	}
	if _, e := bindNearbyCaller(context.Background(), "r", "w", "code"); e == nil {
		t.Fatal("missing caller accepted")
	}
	a, _ := bindNearbyCaller(WithWorkbenchCaller(context.Background(), "a"), "r", "w", "code")
	for _, scope := range [][3]string{{"b", "r", "w"}, {"a", "r2", "w"}, {"a", "r", "w2"}} {
		b, _ := bindNearbyCaller(WithWorkbenchCaller(context.Background(), scope[0]), scope[1], scope[2], "code")
		if a == b {
			t.Fatal("scope collision", scope)
		}
	}
	args["phase"] = "preview"
	args["action"] = "till"
	if _, e := ValidateBuiltinWorkbench("act_nearby_entities", args, true); e == nil {
		t.Fatal("legacy action silently executed a preview")
	}
}

func TestRecipeBudgetRejectsBeforeSpawningAndStopsPartialWork(t *testing.T) {
	script, e := builtinTemplates()["give_recipe_materials"].render(map[string]interface{}{"player_id": "KU_TEST123", "prefab": "axe", "count": 1})
	if e != nil {
		t.Fatal(e)
	}
	for _, ingredients := range []string{`{{type="twigs",amount=41}}`, `{{type="twigs",amount=21},{type="flint",amount=20}}`, `{{type="twigs",amount=0/0}}`} {
		_, e := auditLuaOutput(t, script, `AllPlayers={{userid="KU_TEST123"}};AllRecipes={axe={ingredients=`+ingredients+`}};SpawnPrefab=function()error("UNEXPECTED_SPAWN")end`)
		if e == nil || !strings.Contains(e.Error(), "MATERIAL_BUDGET_EXCEEDED") {
			t.Fatal(e)
		}
	}
	// CPU exhaustion is checked before each spawn, not just at HTTP timeout.
	_, e = auditLuaOutput(t, script, `AllPlayers={{userid="KU_TEST123"}};AllRecipes={axe={ingredients={{type="twigs",amount=2}}}};local now=0;os.clock=function()now=now+.01;return now end;SpawnPrefab=function()error("UNEXPECTED_SPAWN")end`)
	if e == nil || !strings.Contains(e.Error(), "MATERIAL_CPU_BUDGET") {
		t.Fatal(e)
	}
}
