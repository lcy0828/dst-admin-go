package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"dont/internal/apicontract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGameReleaseMCPExamplesMatchPublishedSchemas(t *testing.T) {
	s, _ := fixture(t, nil)
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	for _, id := range []string{"previewGameRelease", "publishGameRelease", "getGameRelease", "retryGameRelease", "getJob", "getGameUpdateRun"} {
		t.Run(id, func(t *testing.T) {
			op, ok := c.Operation(id)
			if !ok {
				t.Fatal("missing operation")
			}
			var examples []string
			for line := range strings.SplitSeq(op.Description, "\n") {
				if line = strings.TrimSpace(line); strings.HasPrefix(line, "{") {
					examples = append(examples, line)
				}
			}
			if len(examples) != 1 {
				t.Fatalf("want one complete arguments example, got %d", len(examples))
			}
			example := examples[0]
			if _, err := op.Validate(json.RawMessage(example)); err != nil {
				t.Fatalf("published example fails real argument validation: %v", err)
			}
			tool, err := apiTool(op)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(s.mcpDefinition(tool).Description, example) {
				t.Fatal("MCP tool lost the complete arguments example")
			}
			raw, _ := json.Marshal(map[string]any{"query": id, "includeSchema": true, "limit": 1})
			result, err := s.discover(raw)
			if err != nil {
				t.Fatal(err)
			}
			var data struct {
				Items []struct {
					OperationID string `json:"operationId"`
					Description string `json:"description"`
				} `json:"items"`
			}
			if err := json.Unmarshal(result, &data); err != nil {
				t.Fatal(err)
			}
			if len(data.Items) != 1 || data.Items[0].OperationID != id || !strings.Contains(data.Items[0].Description, example) {
				t.Fatal("compact-client schema discovery lost the arguments example")
			}
		})
	}
	for _, raw := range []string{
		`{"path":"24a00000-0000-4000-8000-000000000001","requestId":"retry-test"}`,
		`{"path":{"id":"24a00000-0000-4000-8000-000000000001"},"requestId":"retry-test"}`,
	} {
		op, _ := c.Operation("retryGameRelease")
		if _, err := op.Validate(json.RawMessage(raw)); err == nil {
			t.Fatal("invalid issue #24 path shape accepted")
		}
	}
	op, _ := c.Operation("publishGameRelease")
	if _, err := op.Validate(json.RawMessage(`{"body":{"confirmation":true,"planHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"requestId":"publish-test"}`)); err == nil {
		t.Fatal("boolean confirmation accepted")
	}
}

func TestDiscoveryPreservesSchemaFieldNamesWithoutWeakeningResultRedaction(t *testing.T) {
	s, _ := fixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		return json.RawMessage(`{"data":{"properties":{"password":"example-credential-value"},"token":"example-token-value"}}`), nil
	}))
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	for _, id := range []string{"configureAI", "manageNodeSteamLogin", "applyRoomConfiguration", "createNotificationChannel"} {
		raw, _ := json.Marshal(map[string]any{"query": id, "includeResultSchema": true})
		expected, err := s.discover(raw)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := s.Invoke(context.Background(), Principal{ID: "page"}, discoverAPI, raw)
		if err != nil || actual.Status != "succeeded" {
			t.Fatal(actual, err)
		}
		var a, b any
		json.Unmarshal(expected, &a)
		json.Unmarshal(actual.Data, &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: discovery schema was redacted or truncated", id)
		}
		wire, _ := json.Marshal(actual)
		if got := modelToolResult(wire, discoverAPI, raw, actual.Status); !reflect.DeepEqual([]byte(got), wire) {
			t.Errorf("%s: page model received a broken schema", id)
		}
	}
	actual, err := s.Invoke(context.Background(), Principal{ID: "page"}, "api_listRooms", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(actual.Data), "example-credential-value") || strings.Contains(string(actual.Data), "example-token-value") {
		t.Fatal("business credential escaped redaction")
	}
	large, _ := json.Marshal(map[string]any{"output": strings.Repeat("x", 20<<10)})
	if len(modelToolResult(large, "api_getWorldLog", json.RawMessage(`{"includeSchema":true}`), "succeeded")) > 8<<10 {
		t.Fatal("non-discovery tool bypassed normal result budget")
	}
}

func TestDiscoveryProvidesCompleteResultContractOnDemand(t *testing.T) {
	s, _ := fixture(t, nil)
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	for _, op := range c.Operations() {
		if !op.Callable() {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"query": op.ID, "includeResultSchema": true})
		result, err := s.discover(raw)
		if err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		// A complete schema must survive the existing 64 KiB receipt bound.
		if len(result) > 60<<10 {
			t.Errorf("%s: schema exceeds receipt budget: %d", op.ID, len(result))
		}
		var data struct {
			Items []struct {
				OperationID string                      `json:"operationId"`
				Input       map[string]any              `json:"inputSchema"`
				Output      map[string]any              `json:"outputSchema"`
				Result      *apicontract.ResultContract `json:"resultContract"`
			} `json:"items"`
		}
		if err := json.Unmarshal(result, &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Items) != 1 || data.Items[0].OperationID != op.ID || data.Items[0].Input == nil || data.Items[0].Output == nil || data.Items[0].Result == nil {
			t.Fatalf("incomplete discovery for %s", op.ID)
		}
	}
	if _, err := s.discover(json.RawMessage(`{"query":"备份","includeResultSchema":true}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("broad schema expansion accepted", err)
	}
	result, err := s.discover(json.RawMessage(`{"query":"备份","limit":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), "dataSchema") || strings.Contains(string(result), "inputSchema\":{") {
		t.Fatal("ordinary discovery eagerly expands schemas")
	}
	for _, size := range []int{200, 257} {
		raw, _ := json.Marshal(map[string]any{"query": strings.Repeat("雨", size)})
		_, err := s.discover(raw)
		if size <= 256 && err != nil || size > 256 && !errors.Is(err, ErrInvalid) {
			t.Errorf("query length must count Unicode characters: %d %v", size, err)
		}
	}
}

func TestMCPChineseMetadataAndReceiptsStayCompatible(t *testing.T) {
	mode := "success"
	s, store := fixture(t, apiBackendFunc(func(_ context.Context, _ Principal, _ *apicontract.Operation, _ apicontract.Input) (json.RawMessage, error) {
		switch mode {
		case "failed":
			return nil, errors.New("sample failure")
		case "uncertain":
			return nil, ErrUncertain
		case "large":
			return json.Marshal(map[string]any{"operationId": "createRoomBackup", "httpStatus": 202, "data": map[string]any{"id": "sample-job", "status": "queued", "details": strings.Repeat("x", 100000)}})
		default:
			return json.RawMessage(`{"operationId":"createRoomBackup","httpStatus":202,"data":{"id":"sample-job","status":"queued"}}`), nil
		}
	}))
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	_, key, err := store.CreateToken(TokenInput{Name: "semantic-test", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method string, params any) json.RawMessage {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req := httptest.NewRequest("POST", "/mcp?tags=backup", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		out := httptest.NewRecorder()
		s.MCP(out, req)
		var response struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(out.Body.Bytes(), &response) != nil || len(response.Error) > 0 || out.Code != 200 {
			t.Fatalf("%d %s", out.Code, out.Body.String())
		}
		return response.Result
	}
	var list mcp.ListToolsResult
	if err := json.Unmarshal(request("tools/list", map[string]any{}), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range list.Tools {
		if tool.Name != "api_createRoomBackup" {
			continue
		}
		found = true
		meta := tool.Meta[mcpMetadataKey].(map[string]any)
		if meta["cn_purpose"] == nil || meta["symptoms"] == nil || meta["catalogRevision"] == nil || meta["domain_tier"] != nil || tool.OutputSchema == nil || !strings.Contains(tool.Description, "备份") {
			t.Fatal("missing or misleading metadata", tool)
		}
	}
	if !found {
		t.Fatal("missing canonical backup tool")
	}
	for _, value := range []string{"success", "failed", "uncertain", "large"} {
		mode = value
		raw := request("tools/call", map[string]any{"name": "api_createRoomBackup", "arguments": map[string]any{"path": map[string]any{"roomId": "room-a"}, "requestId": "receipt-schema-" + mode}})
		var response struct {
			Structured Invocation `json:"structuredContent"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		want := mode
		if mode == "success" || mode == "large" {
			want = "succeeded"
		}
		if response.Structured.Status != want || response.Structured.CallID == "" || len(response.Content) == 0 {
			t.Fatalf("%s: %s", mode, raw)
		}
		var fallback Invocation
		if json.Unmarshal([]byte(response.Content[0].Text), &fallback) != nil || fallback.CallID != response.Structured.CallID {
			t.Fatal("old text clients lost receipt")
		}
		if mode == "large" && (!strings.Contains(string(raw), "_aiTruncated") || !strings.Contains(string(raw), "sample-job")) {
			t.Fatal("truncation lost task evidence")
		}
	}
}
