package aicontrol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dont/internal/apicontract"
)

func TestActivityExamplesValidateInDiscoveryAndBothMCPEntrypoints(t *testing.T) {
	s, _ := fixture(t, apiBackendFunc(func(_ context.Context, _ Principal, _ *apicontract.Operation, _ apicontract.Input) (json.RawMessage, error) {
		return json.RawMessage(`{"data":{"items":[]}}`), nil
	}))
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(catalog)
	for _, id := range []string{"getRoomPlayerActivitySettings", "updateRoomPlayerActivitySettings", "getRoomPlayerActivityStatus", "listRoomPlayerActivity", "getRoomPlayerTrajectory", "getRoomPlayerCombat", "resyncRoomPlayerActivity"} {
		t.Run(id, func(t *testing.T) {
			hint := activityArgumentHint(id)
			start := strings.Index(hint, "{")
			if start < 0 {
				t.Fatal("no complete example")
			}
			var example map[string]any
			decoder := json.NewDecoder(strings.NewReader(hint[start:]))
			if err := decoder.Decode(&example); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(example)
			op, _ := catalog.Operation(id)
			if _, err := op.Validate(raw); err != nil {
				t.Fatalf("invalid published example: %v", err)
			}
			result, err := s.discover(json.RawMessage(`{"query":"` + id + `","limit":1}`))
			if err != nil || !strings.Contains(string(result), "argumentHint") {
				t.Fatal("compact discovery omits guidance", err)
			}
			typed, err := s.Invoke(context.Background(), Principal{ID: "page"}, op.ToolName(), raw)
			if err != nil || typed.Status != "succeeded" {
				t.Fatal(typed, err)
			}
			dispatch, _ := json.Marshal(map[string]any{"operationId": id, "arguments": example})
			called, err := s.dispatchMCP(context.Background(), Principal{ID: "mcp"}, dispatch)
			if err != nil || called.Status != "succeeded" {
				t.Fatal(called, err)
			}
		})
	}
	for _, bad := range []string{`{"path":"private-secret"}`, `{"path":{"roomId":"room","userId":"KU_TEST"}}`, `{"path":{"roomId":"room"}}`} {
		_, err := s.Invoke(context.Background(), Principal{ID: "page"}, "api_getRoomPlayerTrajectory", json.RawMessage(bad))
		if err == nil || !strings.Contains(err.Error(), "userId is REQUIRED") || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("unhelpful/leaking error: %v", err)
		}
	}
}
