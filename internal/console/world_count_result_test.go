package console

import (
	"strings"
	"testing"
)

func TestFixedWorldCountRequiresCompleteBoundResult(t *testing.T) {
	run := Run{ID: "00000000-0000-4000-8000-000000000001", Mode: "builtin", CommandID: "count_world_entities", Status: RunSucceeded, ExecutionOutcome: "confirmed", Arguments: map[string]interface{}{"prefab": "twigs", "include_inventory": true}}
	text := `[DST-ADMIN-COUNT-V1] {"requestId":"00000000-0000-4000-8000-000000000001","prefab":"twigs","entities":0,"quantity":0,"includeInventory":true,"complete":true}`
	run.Output = &Output{Text: text}
	if r := describeWorldCount(run); r.Status != RunSucceeded || !strings.Contains(r.Message, "0 个") {
		t.Fatal(r)
	}
	for _, bad := range []string{"", strings.Replace(text, "000000000001", "000000000002", 1), strings.Replace(text, `"complete":true`, `"complete":false`, 1), text + "\n" + text, strings.Replace(text, `"quantity":0`, `"quantity":-1`, 1), text + " garbage", strings.Replace(text, `"entities":0,`, "", 1), strings.Replace(text, `"quantity":0,`, "", 1), strings.Replace(text, `"includeInventory":true,`, "", 1), strings.Replace(text, `"quantity":0`, `"quantity":null`, 1)} {
		run.Output = &Output{Text: bad}
		if r := describeWorldCount(run); r.Status != RunUncertain {
			t.Fatal(r)
		}
	}
}
