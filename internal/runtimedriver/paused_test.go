package runtimedriver

import (
	"encoding/json"
	"testing"

	"dont/internal/shards"
	"dont/shared"
)

func TestNativePauseTransportKeepsUnknownDistinctFromFalse(t *testing.T) {
	paused, unpaused := true, false
	for _, value := range []*bool{nil, &paused, &unpaused} {
		status := nativeStatus(shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true, Paused: value})
		encoded, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		var decoded shared.ShardRuntimeStatus
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if (value == nil) != (decoded.Paused == nil) || value != nil && *value != *decoded.Paused || decoded.State != "running" {
			t.Fatalf("pause value changed across transport: %s", encoded)
		}
	}
	var oldAgent shared.ShardRuntimeStatus
	if err := json.Unmarshal([]byte(`{"state":"running","session_exists":true}`), &oldAgent); err != nil || oldAgent.Paused != nil {
		t.Fatalf("old Agent pause must stay unknown: %+v error=%v", oldAgent, err)
	}
}
