package worldstate

import (
	"strings"
	"testing"
)

func TestParseProbeOutputDecodesWorldMetrics(t *testing.T) {
	nonce := "probe-one"
	output := "[DST-ADMIN-WORLDSTATE other DONE]\n" +
		"[DST-ADMIN-WORLDSTATE " + nonce + " ITEM] autumn\tday\t48\t6\t14\t0.3\t0.34\t0.57\tacid_rain\tnew\t18.5\t0.18\t18\t100\t0.25\twarn\t0.46\t1\n" +
		"[DST-ADMIN-WORLDSTATE " + nonce + " DONE]\n"
	observation, complete, err := parseProbeOutput(output, nonce)
	if err != nil || !complete {
		t.Fatalf("parse failed: complete=%v err=%v", complete, err)
	}
	if observation.Season != "autumn" || observation.Precipitation != "acid_rain" || observation.Cycles == nil || *observation.Cycles != 48 || observation.NightmareProgress == nil || *observation.NightmareProgress != .46 || observation.HostPerformance == nil || *observation.HostPerformance != 1 {
		t.Fatalf("unexpected observation: %#v", observation)
	}
}

func TestProbeParserPreservesMissingFieldsAndRejectsMalformedData(t *testing.T) {
	nonce := "probe-two"
	output := "[DST-ADMIN-WORLDSTATE " + nonce + " ITEM] \tday\t1\t\t\t\t0.1\t0.2\tnone\tfull\t\t\t\t\t\t\t\t\n" +
		"[DST-ADMIN-WORLDSTATE " + nonce + " DONE]\n"
	observation, complete, err := parseProbeOutput(output, nonce)
	if err != nil || !complete || observation.Season != "" || observation.Temperature != nil {
		t.Fatalf("missing fields were not preserved: %#v complete=%v err=%v", observation, complete, err)
	}
	if _, _, err := parseProbeOutput("[DST-ADMIN-WORLDSTATE "+nonce+" ITEM] too-few\n", nonce); err == nil {
		t.Fatal("malformed output was accepted")
	}
	if _, _, err := parseProbeOutput("[DST-ADMIN-WORLDSTATE "+nonce+" ITEM] autumn\tday\tNaN\t\t\t\t\t\tnone\t\t\t\t\t\t\t\t\t\n", nonce); err == nil {
		t.Fatal("invalid number was accepted")
	}
	if _, _, err := parseProbeOutput("[DST-ADMIN-WORLDSTATE "+nonce+" ITEM] autumn\tday\t1\t\t\t\t\t\tnone\t\t\t\t\t\t\t\t\t3\n", nonce); err == nil {
		t.Fatal("invalid host performance was accepted")
	}
}

func TestProbeScriptDoesNotExposeMarkersInEchoedCommand(t *testing.T) {
	nonce := "echo-check"
	script := worldStateProbeScript(nonce)
	if strings.Contains(script, "[DST-ADMIN-WORLDSTATE "+nonce+" ITEM]") || strings.Contains(script, "[DST-ADMIN-WORLDSTATE "+nonce+" DONE]") {
		t.Fatalf("probe marker is visible in command echo: %q", script)
	}
}
