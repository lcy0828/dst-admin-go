package aicontrol

import (
	"bytes"
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

	var botView mcp.ListToolsResult
	if e := json.Unmarshal(request("?view=bot", "tools/list", map[string]any{}), &botView); e != nil {
		t.Fatal(e)
	}
	botNames := map[string]bool{}
	for _, tool := range botView.Tools {
		botNames[tool.Name] = true
		if !botMCPNames[tool.Name] {
			t.Fatal("unexpected bot tool", tool.Name)
		}
		if strings.Contains(tool.Name, "Bot") {
			read := tool.Name != "api_runBotCapability"
			if tool.Annotations.ReadOnlyHint != read || *tool.Annotations.DestructiveHint == read {
				t.Fatal("incorrect bot annotation", tool.Name)
			}
		}
	}
	for _, name := range []string{"api_listBotCapabilities", "api_queryBotCapability", "api_runBotCapability", "api_getBotCapabilityResult"} {
		if !botNames[name] {
			t.Fatal("missing bot tool", name)
		}
	}

	// Notification dependencies are available to permitted keys in the small
	// bot view, but adding them to the view must not grant new token scope.
	for _, name := range []string{"api_getRoom", "api_getRoomPlayer", "api_streamWorldRuntimeEvents", "api_listRoomRuntimeEvents", "api_listRoomChatLogs"} {
		if !botNames[name] {
			t.Fatal("missing unified notification dependency", name)
		}
		for _, tool := range botView.Tools {
			if tool.Name == name && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint) {
				t.Fatal("notification dependency must be read-only", name)
			}
		}
	}
	originalKey := key
	_, key, err = store.CreateToken(TokenInput{Name: "reader", Days: 1, Profile: ProfileGameRead, RoomIDs: []string{"room-a"}})
	if err != nil {
		t.Fatal(err)
	}
	var readerView mcp.ListToolsResult
	if e := json.Unmarshal(request("?view=bot", "tools/list", map[string]any{}), &readerView); e != nil {
		t.Fatal(e)
	}
	for _, tool := range readerView.Tools {
		if slices.Contains([]string{"api_getRoom", "api_getRoomPlayer", "api_streamWorldRuntimeEvents", "api_listRoomRuntimeEvents", "api_listRoomChatLogs"}, tool.Name) {
			t.Fatal("bot view expanded game-read scope", tool.Name)
		}
	}
	key = originalKey

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
	for _, query := range []string{"?view=bot", "?view=compact", "?view=all", "?view=legacy", "?tags=backup&access=read", "?tags=no-such-domain"} {
		var first mcp.ListToolsResult
		if err := json.Unmarshal(request(query, "tools/list", map[string]any{}), &first); err != nil {
			t.Fatal(err)
		}
		var explicitOn mcp.ListToolsResult
		if err := json.Unmarshal(request(query+"&pagination=on", "tools/list", map[string]any{}), &explicitOn); err != nil {
			t.Fatal(err)
		}
		firstJSON, _ := json.Marshal(first)
		onJSON, _ := json.Marshal(explicitOn)
		if !bytes.Equal(firstJSON, onJSON) {
			t.Fatalf("pagination=on changed the default listing for %s", query)
		}
		if (query == "?view=all" || query == "?view=legacy") && (len(first.Tools) != mcpPageSize || first.NextCursor == "") {
			t.Fatalf("default listing is not paginated for %s", query)
		}
		combined := append([]*mcp.Tool{}, first.Tools...)
		cursor := first.NextCursor
		for page := 0; cursor != ""; page++ {
			if page >= 20 {
				t.Fatalf("pagination did not terminate for %s", query)
			}
			var result mcp.ListToolsResult
			if err := json.Unmarshal(request(query, "tools/list", map[string]any{"cursor": cursor}), &result); err != nil {
				t.Fatal(err)
			}
			combined = append(combined, result.Tools...)
			cursor = result.NextCursor
		}
		var unpaginated mcp.ListToolsResult
		if err := json.Unmarshal(request(query+"&pagination=off", "tools/list", map[string]any{}), &unpaginated); err != nil {
			t.Fatal(err)
		}
		combinedJSON, _ := json.Marshal(combined)
		unpaginatedJSON, _ := json.Marshal(unpaginated.Tools)
		if unpaginated.NextCursor != "" || !bytes.Equal(combinedJSON, unpaginatedJSON) {
			t.Fatalf("unpaginated listing differs from all pages for %s: %d/%d tools", query, len(unpaginated.Tools), len(combined))
		}
		meta, ok := unpaginated.Meta["io.github.lcy0828.dst-admin/catalog"].(map[string]any)
		if !ok || meta["paginated"] != false || meta["total"] != float64(len(combined)) || meta["pageSize"] != float64(len(combined)) {
			t.Fatalf("incorrect unpaginated metadata for %s: %+v", query, meta)
		}
		if first.NextCursor != "" {
			var response struct {
				Code int `json:"code"`
			}
			if err := json.Unmarshal(request(query+"&pagination=off", "tools/list", map[string]any{"cursor": first.NextCursor}), &response); err != nil || response.Code != -32602 {
				t.Fatalf("unpaginated listing accepted a cursor for %s", query)
			}
		}
	}
	args := map[string]any{"operationId": "configureRoomBackupSchedule", "arguments": map[string]any{"path": map[string]any{"roomId": "room-a"}, "body": map[string]any{"enabled": true}, "requestId": "dispatch-backup-001"}}
	for i := 0; i < 2; i++ {
		query := ""
		if i == 1 {
			query = "?pagination=off"
		}
		if !strings.Contains(string(request(query, "tools/call", map[string]any{"name": dispatchAPI, "arguments": args})), "enabled") {
			t.Fatal("dispatch failed")
		}
	}
	if executed != 1 {
		t.Fatal("dispatcher bypassed idempotence", executed)
	}
	args["operationId"] = "setupAdmin"
	if !strings.Contains(string(request("?pagination=off", "tools/call", map[string]any{"name": dispatchAPI, "arguments": args})), "AI_DENIED") {
		t.Fatal("dispatcher exposed identity operation")
	}
	if executed != 1 {
		t.Fatal("invalid dispatch reached backend")
	}
}

func TestMCPFilterValidationAndScopedDispatch(t *testing.T) {
	for _, query := range []string{"view=bad", "access=bad", "risk=bad", "runtime=bad", "tags=backup,,room", "pagination=bad"} {
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
