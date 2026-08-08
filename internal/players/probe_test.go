package players

import (
	"strings"
	"testing"
)

func TestParseProbeOutputDecodesIdentityAndVitals(t *testing.T) {
	nonce := "probe-1"
	output := "[12:00:00] [DST-ADMIN-PLAYERS probe-1 ITEM] " + strings.Join([]string{
		"KU_ONE", "Willow%20The%20Brave", "willow", "42", "1", "7656119", "5", "92.5", "61", "74", "31.2", "8",
	}, "\t") + "\n[12:00:00] [DST-ADMIN-PLAYERS probe-1 DONE]\n"
	items, complete, err := parseProbeOutput(output, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !complete || len(items) != 1 || items[0].Name != "Willow The Brave" || items[0].HealthPercent == nil || *items[0].HealthPercent != 92.5 {
		t.Fatalf("unexpected probe result: complete=%v items=%#v", complete, items)
	}
}

func TestProbeParserIgnoresOtherNonceAndRejectsMalformedOutput(t *testing.T) {
	items, complete, err := parseProbeOutput("[DST-ADMIN-PLAYERS other DONE]\n", "wanted")
	if err != nil || complete || len(items) != 0 {
		t.Fatalf("foreign probe leaked into result: complete=%v items=%#v err=%v", complete, items, err)
	}
	if _, _, err := parseProbeOutput("[DST-ADMIN-PLAYERS wanted ITEM] too-few-fields\n", "wanted"); err == nil {
		t.Fatal("malformed probe output was accepted")
	}
}

func TestProbeScriptDoesNotExposeCompleteMarkersInEchoedCommand(t *testing.T) {
	nonce := "nonce-echo-test"
	script := playerProbeScript(nonce)
	if strings.Contains(script, "[DST-ADMIN-PLAYERS "+nonce+" DONE]") || strings.Contains(script, "[DST-ADMIN-PLAYERS "+nonce+" ITEM]") {
		t.Fatalf("echoed command can be mistaken for probe output: %s", script)
	}
}
