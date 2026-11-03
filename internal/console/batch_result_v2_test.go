package console

import (
	"encoding/json"
	"strings"
	"testing"
)

func v2TestRun() Run {
	return Run{ID: "00000000-0000-4000-8000-000000000001", Mode: "builtin", CommandID: "act_nearby_entities", Arguments: map[string]interface{}{"action": "pick"}, Script: "w.NearbyV2(", Status: RunSucceeded, ExecutionOutcome: "confirmed", Output: &Output{Text: `[DST-ADMIN-BATCH-V2] {"version":2,"requestId":"00000000-0000-4000-8000-000000000001","action":"pick","worldId":"100","state":"partial","matched":2,"attempted":1,"verified":1,"unconfirmed":0,"skipped":1,"failed":0,"units":3,"detailsComplete":true,"items":[{"prefab":"twigs","state":"verified","destination":"inventory","units":3},{"prefab":"flint","state":"skipped","destination":"inventory","units":0,"reason":"INVENTORY_FULL"}]}`}}
}
func TestBatchV2RequiresBoundedCorrelatedConservedEvidence(t *testing.T) {
	base := v2TestRun()
	r := batchResult(base)
	if r == nil || r.Units != 3 || len(r.Items) != 2 || workbenchResult(base).Status != "partial" {
		t.Fatal(r)
	}
	var fields map[string]any
	json.Unmarshal([]byte(strings.TrimPrefix(base.Output.Text, "[DST-ADMIN-BATCH-V2] ")), &fields)
	for name, mutation := range map[string]func(map[string]any){
		"wrong request":           func(v map[string]any) { v["requestId"] = "00000000-0000-4000-8000-000000000099" },
		"wrong action":            func(v map[string]any) { v["action"] = "delete" },
		"future version":          func(v map[string]any) { v["version"] = 3 },
		"summary mismatch":        func(v map[string]any) { v["verified"] = 2 },
		"unit mismatch":           func(v map[string]any) { v["units"] = 4 },
		"detail mismatch":         func(v map[string]any) { v["items"] = []any{} },
		"incomplete":              func(v map[string]any) { v["detailsComplete"] = false },
		"unknown field":           func(v map[string]any) { v["confirmed"] = true },
		"null required":           func(v map[string]any) { v["failed"] = nil },
		"missing required":        func(v map[string]any) { delete(v, "skipped") },
		"oversize count":          func(v map[string]any) { v["matched"] = 41 },
		"negative count":          func(v map[string]any) { v["attempted"] = -1 },
		"fake completion":         func(v map[string]any) { v["state"] = "completed" },
		"arbitrary reason":        func(v map[string]any) { v["reason"] = "trusted" },
		"verified pending reason": func(v map[string]any) { v["items"].([]any)[0].(map[string]any)["reason"] = "TRANSITION_PENDING" },
	} {
		t.Run(name, func(t *testing.T) {
			b, _ := json.Marshal(fields)
			var v map[string]any
			json.Unmarshal(b, &v)
			mutation(v)
			out, _ := json.Marshal(v)
			run := base
			run.Output = &Output{Text: "[DST-ADMIN-BATCH-V2] " + string(out)}
			if batchResult(run) != nil {
				t.Fatal(string(out))
			}
			if describeResult(run).Status != RunUncertain {
				t.Fatal("missing evidence treated as success")
			}
		})
	}
	for _, run := range []Run{
		func() Run { r := base; r.Mode = "custom"; return r }(),
		func() Run { r := base; r.Status = RunSent; return r }(),
		func() Run { r := base; r.ExecutionOutcome = "unknown"; return r }(),
		func() Run { r := base; r.Output = &Output{Text: base.Output.Text, Truncated: true}; return r }(),
		func() Run { r := base; r.Output = &Output{Text: base.Output.Text + "\n" + base.Output.Text}; return r }(),
	} {
		if batchResult(run) != nil {
			t.Fatal("accepted non authoritative result")
		}
	}
	// V2-only fields must still be rejected by the original strict V1 decoder.
	legacy := base
	legacy.Script = "w.Nearby("
	legacy.Output = &Output{Text: `[DST-ADMIN-BATCH-V1] {"state":"completed","matched":0,"attempted":0,"verified":0,"unconfirmed":0,"skipped":0,"failed":0,"version":2}`}
	if batchResult(legacy) != nil {
		t.Fatal("V1 unknown field accepted")
	}
}

func TestStageBSettingsHaveFixedInputsAndHumanApproval(t *testing.T) {
	for id, args := range map[string]map[string]interface{}{
		"set_player_hunger_rate": {"player_id": "KU_TEST123", "mode": "set", "multiplier": .5},
		"set_wanda_age":          {"player_id": "KU_TEST123", "age": "old"},
		"restore_woodie_human":   {"player_id": "KU_TEST123"},
	} {
		t.Run(id, func(t *testing.T) {
			d, e := ValidateBuiltinWorkbench(id, args, false)
			if e != nil || d.Interaction == nil {
				t.Fatal(d, e)
			}
			script, e := builtinTemplates()[id].render(args)
			if e != nil || !strings.Contains(script, "WORKBENCH_UPDATE_REQUIRED") {
				t.Fatal(script, e)
			}
			assertLuaSyntax(t, script)
			args["phase"] = "execute"
			args["preview_id"] = "00000000-0000-4000-8000-000000000002"
			if _, e = ValidateBuiltinWorkbench(id, args, false); e != ErrConfirmationNeeded {
				t.Fatal(e)
			}
			if _, e = ValidateBuiltinWorkbench(id, args, true); e != nil {
				t.Fatal(e)
			}
			args["readOnly"] = true
			if _, e = ValidateBuiltinWorkbench(id, args, true); e != ErrInvalidArguments {
				t.Fatal(e)
			}
		})
	}
	for _, value := range []any{-.1, 5.1, ".5);os.execute('bad')", true} {
		if _, e := ValidateBuiltinWorkbench("set_player_hunger_rate", map[string]interface{}{"player_id": "KU_TEST123", "mode": "set", "multiplier": value}, false); e == nil {
			t.Fatal(value)
		}
	}
}

func TestBatchV2NativeWireShapesAndPreviewBudgets(t *testing.T) {
	// Synthetic identities and fixed values, matching the native Lua encoder's
	// sparse optional fields and empty arrays. No saved player data is included.
	samples := []struct{ command, action, payload string }{
		{"set_wanda_age", "", `{"version":2,"requestId":"00000000-0000-4000-8000-000000000001","action":"set_wanda_age","worldId":"100","state":"preview","previewId":"00000000-0000-4000-8000-000000000002","observedAt":100,"expiresAt":130,"expiresIn":30,"x":0,"z":0,"matched":1,"attempted":0,"verified":0,"unconfirmed":0,"skipped":0,"failed":0,"units":0,"detailsComplete":true,"items":[{"prefab":"wanda","state":"prepared","before":38,"after":74,"destination":"none","units":0}]}`},
		{"act_nearby_entities", "pick", `{"version":2,"requestId":"00000000-0000-4000-8000-000000000001","action":"pick","worldId":"100","state":"completed","matched":0,"attempted":0,"verified":0,"unconfirmed":0,"skipped":0,"failed":0,"units":0,"detailsComplete":true,"items":[]}`},
	}
	for _, sample := range samples {
		run := v2TestRun()
		run.CommandID = sample.command
		run.Arguments["action"] = sample.action
		if sample.command != "act_nearby_entities" {
			run.Script = "w.Setting("
		}
		run.Output = &Output{Text: "[DST-ADMIN-BATCH-V2] " + sample.payload + "\t"}
		if batchResult(run) == nil {
			t.Fatal("native receipt shape rejected", sample.command)
		}
		if sample.command != "act_nearby_entities" {
			run.Output.Text = strings.Replace(run.Output.Text, `"matched":1`, `"matched":0`, 1)
			if batchResult(run) != nil {
				t.Fatal("single-player setting accepted a mismatched target count")
			}
		}
	}
	run := v2TestRun()
	run.Output = &Output{Text: `[DST-ADMIN-BATCH-V2] {"version":2,"requestId":"00000000-0000-4000-8000-000000000001","action":"pick","worldId":"100","state":"preview","previewId":"00000000-0000-4000-8000-000000000002","observedAt":100,"expiresAt":130,"expiresIn":30,"x":0,"z":0,"matched":2,"attempted":0,"verified":0,"unconfirmed":0,"skipped":0,"failed":0,"units":0,"detailsComplete":true,"items":[{"prefab":"twigs","state":"prepared","destination":"inventory","units":150},{"prefab":"flint","state":"prepared","destination":"inventory","units":150}]}`}
	if batchResult(run) != nil {
		t.Fatal("preview accepted an aggregate output exceeding the unit budget")
	}
}
