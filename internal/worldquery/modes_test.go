package worldquery

import (
	"dont/internal/dstruntime"
	"strings"
	"testing"
	"time"
)

func TestModeEvidenceNeverTurnsUnknownOrMissingIntoDisabled(t *testing.T) {
	query := Request{QueryID: "player_modes"}
	output := `{"protocolVersion":1,"runId":"modes-example","queryId":"player_modes","complete":true,"data":{"kind":"player_modes","count":1,"players":[{"id":"KU_EXAMPLE","invincible":"enabled","creative":"unknown","panelGod":"disabled","panelInvincible":"unknown"}]}}`
	accept := func(text string) Result {
		return acceptReceipt(Result{QueryRunID: "modes-example"}, query, dstruntime.CommandReceipt{OK: true, CompletedAt: time.Now(), Details: map[string]interface{}{"output": map[string]interface{}{"text": text, "truncated": false}}})
	}
	if r := accept(output); r.Status != "succeeded" || !strings.Contains(string(r.Data), `"creative":"unknown"`) {
		t.Fatal(r)
	}
	for _, bad := range []string{
		strings.Replace(output, `"kind":"player_modes",`, "", 1),
		strings.Replace(output, `"creative":"unknown"`, `"creative":false`, 1),
		strings.Replace(output, `"count":1`, `"count":0`, 1),
		strings.Replace(output, `"panelInvincible":"unknown"`, `"panelInvincible":null`, 1),
	} {
		if r := accept(bad); r.Status != "unavailable" || r.Data != nil {
			t.Fatal(r)
		}
	}
}
