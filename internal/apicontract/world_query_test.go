package apicontract

import (
	"encoding/json"
	"testing"
)

func TestControlledQueryContractRejectsLuaFlagsAndUnknownQueries(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	op, ok := c.Operation("queryRoomWorld")
	if !ok || op.Mutation() || op.ToolMetadata().Access != "read" || op.ToolMetadata().Destructive {
		t.Fatal("unsafe metadata")
	}
	for _, raw := range []string{
		`{"path":{"roomId":"RXhhbXBsZQ","worldId":"TWFzdGVy","queryId":"entity_count"},"query":{"prefab":"spiderden"}}`,
		`{"path":{"roomId":"RXhhbXBsZQ","worldId":"TWFzdGVy","queryId":"world_info"}}`,
		`{"path":{"roomId":"RXhhbXBsZQ","worldId":"TWFzdGVy","queryId":"online_players"}}`,
	} {
		if _, err := op.Validate(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{
		`{"path":{"roomId":"r","worldId":"w","queryId":"save_world"}}`,
		`{"path":{"roomId":"r","worldId":"w","queryId":"entity_count"},"query":{"prefab":"spiderden\n"}}`,
		`{"path":{"roomId":"r","worldId":"w","queryId":"world_info"},"readOnly":true}`,
		`{"path":{"roomId":"r","worldId":"w","queryId":"world_info"},"query":{"risk":"low","isBuiltin":true}}`,
		`{"path":{"roomId":"r","worldId":"w","queryId":"world_info"},"body":{"script":"c_save()"}}`,
		`{"path":{"roomId":"r","worldId":"w","queryId":"world_info"},"headers":{"X-DST-Runtime-Target":"local"}}`,
	} {
		if _, err := op.Validate(json.RawMessage(raw)); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	for _, id := range []string{"executeRoomCommand", "executeRawRoomCommand"} {
		write, _ := c.Operation(id)
		if !write.Mutation() || write.ToolMetadata().Access != "write" || !write.ToolMetadata().Destructive {
			t.Fatal("administrator tool downgraded", id)
		}
	}
	if found := c.Search("普通用户只读查询实体数量", 12); len(found) == 0 {
		t.Fatal("query not discoverable")
	}
}

func TestControlledQueryResultContractIncludesUncertainAndPartialEvidence(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	op, _ := c.Operation("queryRoomWorld")
	response := op.Responses["200"].Content["application/json"].Schema
	fake := &Operation{ID: "queryResponse", Method: "GET", catalog: c, RequestBody: Body{Required: true, Content: map[string]Media{"application/json": {Schema: response}}}}
	for _, tc := range []struct {
		status   string
		data     any
		complete bool
	}{
		{"succeeded", map[string]any{"prefab": "spiderden", "count": 0}, true},
		{"partial", map[string]any{"prefab": "spiderden", "count": nil, "lowerBound": 5}, false},
		{"uncertain", nil, false},
		{"unavailable", nil, false},
		{"succeeded", map[string]any{"players": []any{}, "count": 0}, true},
		{"succeeded", map[string]any{"cycles": 0, "season": "autumn", "phase": "day", "temperature": nil, "israining": false, "issnowing": false}, true},
	} {
		data := map[string]any{"queryId": "entity_count", "roomId": "RXhhbXBsZQ", "worldId": "TWFzdGVy", "status": tc.status, "source": "runtime", "cached": false, "complete": tc.complete, "transportOutcome": "sent", "executionOutcome": "unknown", "resultState": "unconfirmed", "mayHaveExecuted": true, "data": tc.data, "error": nil}
		raw, _ := json.Marshal(map[string]any{"body": map[string]any{"data": data, "error": nil, "meta": map[string]any{"requestId": "query-example"}}})
		if _, err := fake.Validate(raw); err != nil {
			t.Fatal(tc.status, err, string(raw))
		}
	}
}
