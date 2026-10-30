package aicontrol

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"dont/internal/apicontract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPControlledQueryMetadataValidationAndTypedDispatch(t *testing.T) {
	calls := 0
	s, store := fixture(t, apiBackendFunc(func(_ context.Context, p Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		calls++
		if op.ID != "queryRoomWorld" || in.Path["queryId"] != "entity_count" || in.Query["prefab"] != "spiderden" {
			t.Errorf("unexpected dispatch %s %+v", op.ID, in)
		}
		return json.RawMessage(`{"status":"uncertain","complete":false,"data":null}`), nil
	}))
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(catalog)
	_, key, err := store.CreateToken(TokenInput{Name: "query-test", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method string, params any) json.RawMessage {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		out := httptest.NewRecorder()
		s.MCP(out, req)
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body.String())
		}
		var envelope struct {
			Result json.RawMessage
			Error  json.RawMessage
		}
		if json.Unmarshal(out.Body.Bytes(), &envelope) != nil || len(envelope.Error) > 0 {
			t.Fatal(out.Body.String())
		}
		return envelope.Result
	}
	var list mcp.ListToolsResult
	if json.Unmarshal(request("tools/list", map[string]any{}), &list) != nil {
		t.Fatal("list failed")
	}
	found := 0
	for _, tool := range list.Tools {
		if tool.Name == "api_queryRoomWorld" || tool.Name == "api_getRoomWorldQueryRun" {
			found++
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
				t.Fatal(tool)
			}
		}
	}
	if found != 2 {
		t.Fatal("query tools missing in compact view")
	}
	bad := map[string]any{"path": map[string]any{"roomId": "RXhhbXBsZQ", "worldId": "TWFzdGVy", "queryId": "entity_count"}, "query": map[string]any{"prefab": "spiderden", "readOnly": true, "script": "c_save()"}}
	var result mcp.CallToolResult
	if json.Unmarshal(request("tools/call", map[string]any{"name": "api_queryRoomWorld", "arguments": bad}), &result) != nil || !result.IsError || calls != 0 {
		t.Fatal("bad parameters reached backend", calls)
	}
	delete(bad["query"].(map[string]any), "readOnly")
	delete(bad["query"].(map[string]any), "script")
	if json.Unmarshal(request("tools/call", map[string]any{"name": "api_queryRoomWorld", "arguments": bad}), &result) != nil || result.IsError || calls != 1 {
		t.Fatal(result, calls)
	}
}
