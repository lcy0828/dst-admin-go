package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"dont/internal/apicontract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPViewsPaginationMetadataAndDispatcher(t *testing.T) {
	executed := 0
	s, store := fixture(t, apiBackendFunc(func(_ context.Context, p Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
		if op.ID != "configureRoomBackupSchedule" || in.Path["roomId"] != "room-a" {
			t.Errorf("unexpected operation %s %+v", op.ID, in)
		}
		executed++
		return json.RawMessage(`{"enabled":true}`), nil
	}))
	catalog, _ := apicontract.Default()
	s.ConfigureAPI(catalog)
	_, key, err := store.CreateToken(TokenInput{Name: "catalog", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	request := func(query, method string, params any) json.RawMessage {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req := httptest.NewRequest("POST", "/mcp"+query, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		out := httptest.NewRecorder()
		s.MCP(out, req)
		if out.Code != 200 {
			t.Fatalf("status=%d %s", out.Code, out.Body.String())
		}
		var response struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(out.Body.Bytes(), &response) != nil {
			t.Fatal(out.Body.String())
		}
		if len(response.Error) > 0 {
			return response.Error
		}
		return response.Result
	}
	var compact mcp.ListToolsResult
	if err := json.Unmarshal(request("", "tools/list", map[string]any{}), &compact); err != nil {
		t.Fatal(err)
	}
	if len(compact.Tools) < 3 || len(compact.Tools) > 20 || compact.NextCursor != "" {
		t.Fatalf("compact tool count=%d", len(compact.Tools))
	}
	names := []string{}
	for _, tool := range compact.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, dispatchAPI) || !slices.Contains(names, discoverAPI) || slices.Contains(names, "room_control") {
		t.Fatal(names)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 20; page++ {
		var result mcp.ListToolsResult
		if err := json.Unmarshal(request("?view=legacy", "tools/list", map[string]any{"cursor": cursor}), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) > mcpPageSize || len(result.Tools) == 0 {
			t.Fatal("unbounded or empty page")
		}
		for _, tool := range result.Tools {
			if seen[tool.Name] {
				t.Fatal("duplicate", tool.Name)
			}
			seen[tool.Name] = true
			if tool.Annotations == nil || tool.Annotations.Title == "" || tool.Annotations.DestructiveHint == nil || tool.Annotations.OpenWorldHint == nil || tool.Meta[mcpMetadataKey] == nil {
				t.Fatal("incomplete metadata", tool.Name)
			}
		}
		if page == 0 {
			if !strings.Contains(string(request("?tags=backup", "tools/list", map[string]any{"cursor": result.NextCursor})), "cursor") {
				t.Fatal("cross-view cursor accepted")
			}
		}
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
	}
	all, _ := s.AvailableTools()
	if len(seen) != len(all)+1 || !seen["world_console"] {
		t.Fatalf("legacy missing tools: %d/%d", len(seen), len(all)+1)
	}
	var filtered mcp.ListToolsResult
	json.Unmarshal(request("?tags=backup&access=read", "tools/list", map[string]any{}), &filtered)
	if len(filtered.Tools) == 0 {
		t.Fatal("no backup tools")
	}
	for _, tool := range filtered.Tools {
		if !tool.Annotations.ReadOnlyHint || !strings.HasPrefix(tool.Name, "api_") {
			t.Fatal(tool.Name)
		}
	}
	args := map[string]any{"operationId": "configureRoomBackupSchedule", "arguments": map[string]any{"path": map[string]any{"roomId": "room-a"}, "body": map[string]any{"enabled": true}, "requestId": "dispatch-backup-001"}}
	for i := 0; i < 2; i++ {
		if !strings.Contains(string(request("", "tools/call", map[string]any{"name": dispatchAPI, "arguments": args})), "enabled") {
			t.Fatal("dispatch failed")
		}
	}
	if executed != 1 {
		t.Fatal("dispatcher bypassed idempotence", executed)
	}
	args["operationId"] = "setupAdmin"
	if !strings.Contains(string(request("", "tools/call", map[string]any{"name": dispatchAPI, "arguments": args})), "AI_DENIED") {
		t.Fatal("dispatcher exposed identity operation")
	}
	if executed != 1 {
		t.Fatal("invalid dispatch reached backend")
	}
}

func TestMCPFilterValidationAndScopedDispatch(t *testing.T) {
	for _, query := range []string{"view=bad", "access=bad", "risk=bad", "runtime=bad", "tags=backup,,room"} {
		values, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseMCPView(values); err == nil {
			t.Fatalf("accepted %s", query)
		}
	}
	s, _ := fixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		t.Fatal("scope bypass")
		return nil, nil
	}))
	c, _ := apicontract.Default()
	s.ConfigureAPI(c)
	raw := json.RawMessage(`{"operationId":"configureRoomBackupSchedule","arguments":{"path":{"roomId":"room-other"},"body":{"enabled":true},"requestId":"scope-dispatch-001"}}`)
	if _, err := s.dispatchMCP(context.Background(), Principal{ID: "scoped", RoomID: "room-a"}, raw); !errors.Is(err, ErrDenied) {
		t.Fatalf("scope result=%v", err)
	}
}
func TestDiscoveryReturnsFilteredSchemaAndSharedMetadata(t *testing.T) {
	s, _ := fixture(t, nil)
	c, _ := apicontract.Default()
	s.ConfigureAPI(c)
	result, err := s.discover(json.RawMessage(`{"query":"configureRoomBackupSchedule","tags":["backup"],"access":"write","includeSchema":true,"limit":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Items []struct {
			OperationID string         `json:"operationId"`
			Tags        []string       `json:"tags"`
			Access      string         `json:"access"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"items"`
	}
	if json.Unmarshal(result, &value) != nil || len(value.Items) != 1 {
		t.Fatal(string(result))
	}
	item := value.Items[0]
	if item.OperationID != "configureRoomBackupSchedule" || !slices.Contains(item.Tags, "backup") || !slices.Contains(item.Tags, "room") || item.Access != "write" || item.InputSchema["properties"] == nil {
		t.Fatal(string(result))
	}
}
