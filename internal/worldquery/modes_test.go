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

func TestExtendedModesAcceptOldResponsesAndRejectForgedStates(t *testing.T) {
	output := `{"protocolVersion":1,"runId":"modes-example","queryId":"player_modes","complete":true,"data":{"kind":"player_modes","count":1,"players":[{"id":"KU_EXAMPLE","invincible":"disabled","creative":"unknown","panelGod":"disabled","panelInvincible":"unknown","panelModes":{"noclip":"enabled","invisible":"disabled"},"modeMemory":"disabled"}]}}`
	accept := func(s string) Result {
		return acceptReceipt(Result{QueryRunID: "modes-example"}, Request{QueryID: "player_modes"}, dstruntime.CommandReceipt{OK: true, CompletedAt: time.Now(), Details: map[string]interface{}{"output": map[string]interface{}{"text": s, "truncated": false}}})
	}
	if r := accept(output); r.Status != "succeeded" {
		t.Fatal(r)
	}
	for _, bad := range []string{strings.Replace(output, `"noclip":"enabled"`, `"god_from_custom_command":"enabled"`, 1), strings.Replace(output, `"noclip":"enabled"`, `"noclip":true`, 1), strings.Replace(output, `"modeMemory":"disabled"`, `"modeMemory":"persisted_probably"`, 1)} {
		if r := accept(bad); r.Data != nil || r.Status != "unavailable" {
			t.Fatal(r)
		}
	}
}
