package aicontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"dont/internal/apicontract"
)

type apiBackendFunc func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error)

func (f apiBackendFunc) Invoke(context.Context, string, Arguments, string) (json.RawMessage, error) {
	return nil, ErrUnavailable
}
func (f apiBackendFunc) InvokeAPI(ctx context.Context, p Principal, op *apicontract.Operation, input apicontract.Input) (json.RawMessage, error) {
	return f(ctx, p, op, input)
}

func TestChatDiscoversAndExecutesTypedBackupAction(t *testing.T) {
	var executions, rounds atomic.Int32
	s, store := fixture(t, apiBackendFunc(func(_ context.Context, p Principal, op *apicontract.Operation, input apicontract.Input) (json.RawMessage, error) {
		executions.Add(1)
		if p.ID != "page" || p.RoomID != "" || op.ID != "configureRoomBackupSchedule" || input.Path["roomId"] != "room-b" || !strings.HasPrefix(input.RequestID, "page-") {
			t.Errorf("wrong API execution: %+v %s %+v", p, op.ID, input)
		}
		return json.RawMessage(`{"data":{"effectiveEnabled":true,"nextRunLocal":"2026-10-21 04:00:00 +08:00"}}`), nil
	}))
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(catalog)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		switch rounds.Add(1) {
		case 1:
			fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"discover","type":"function","function":{"name":"find_api","arguments":"{\"query\":\"configureRoomBackupSchedule\",\"limit\":1}"}}]}}]}`)
		case 2:
			if !strings.Contains(string(data), `"name":"api_configureRoomBackupSchedule"`) {
				t.Error("discovered typed tool was not loaded")
			}
			fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"configure","type":"function","function":{"name":"api_configureRoomBackupSchedule","arguments":"{\"path\":{\"roomId\":\"room-b\"},\"body\":{\"enabled\":true},\"requestId\":\"model-placeholder\"}"}}]}}]}`)
		default:
			if !strings.Contains(string(data), "effectiveEnabled") {
				t.Error("actual effective result was not returned to model")
			}
			fmt.Fprint(w, `{"choices":[{"message":{"content":"房间 B 的自动备份已开启，每天 04:00 执行。"}}]}`)
		}
	}))
	defer model.Close()
	config, _ := store.Config()
	config.Endpoint = model.URL
	config.Model = "tools"
	if _, err = store.SaveConfig(ConfigInput{Config: config}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Chat(context.Background(), ChatInput{RoomID: "room-a", Messages: []Message{{Role: "user", Content: "帮房间 B 开启自动备份"}}}, func(ChatEvent) {})
	if err != nil || len(out.Calls) != 2 || executions.Load() != 1 || !strings.Contains(out.Content, "已开启") {
		t.Fatal(out, err, executions.Load())
	}
}

func TestGeneratedAPISchemaDedupAndTrueScope(t *testing.T) {
	var executions atomic.Int32
	s, _ := fixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		executions.Add(1)
		return json.RawMessage(`{"data":{"effectiveEnabled":true}}`), nil
	}))
	catalog, _ := apicontract.Default()
	s.ConfigureAPI(catalog)
	raw := json.RawMessage(`{"path":{"roomId":"room-a"},"body":{"enabled":true},"requestId":"same-backup-001"}`)
	first, err := s.Invoke(context.Background(), principal(), "api_configureRoomBackupSchedule", raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Invoke(context.Background(), principal(), "api_configureRoomBackupSchedule", raw)
	if err != nil || first.CallID != second.CallID || executions.Load() != 1 {
		t.Fatal(first, second, err)
	}
	for _, changed := range []string{strings.Replace(string(raw), "room-a", "room-b", 1), strings.Replace(string(raw), `true`, `"yes"`, 1), `{"path":{},"requestId":"different-001"}`} {
		if _, err = s.Invoke(context.Background(), principal(), "api_configureRoomBackupSchedule", []byte(changed)); err == nil {
			t.Fatal("invalid/scope argument accepted", changed)
		}
	}
	if _, err = s.Invoke(context.Background(), principal(), "api_listAgents", []byte(`{}`)); err == nil {
		t.Fatal("genuinely room-scoped credential escaped through global API")
	}
	if executions.Load() != 1 {
		t.Fatal("invalid call reached backend")
	}
}

func TestMCPGeneratedToolsUseSameBackend(t *testing.T) {
	var executed atomic.Int32
	s, store := fixture(t, apiBackendFunc(func(_ context.Context, p Principal, op *apicontract.Operation, input apicontract.Input) (json.RawMessage, error) {
		if p.ID == "page" || op.ID != "configureRoomBackupSchedule" || input.Path["roomId"] != "room-a" {
			t.Error("wrong MCP execution")
		}
		executed.Add(1)
		return json.RawMessage(`{"effectiveEnabled":true}`), nil
	}))
	catalog, _ := apicontract.Default()
	s.ConfigureAPI(catalog)
	_, secret, err := store.CreateToken(TokenInput{Name: "mcp", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"api_configureRoomBackupSchedule","arguments":{"path":{"roomId":"room-a"},"body":{"enabled":true},"requestId":"mcp-backup-001"}}}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	out := httptest.NewRecorder()
	s.MCP(out, req)
	if out.Code != 200 || executed.Load() != 1 || !strings.Contains(out.Body.String(), "effectiveEnabled") {
		t.Fatal(out.Code, out.Body, executed.Load())
	}
}

func TestFinalModelAndMCPToolsCarryDescriptionsAndLocalReferences(t *testing.T) {
	s, store := fixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, input apicontract.Input) (json.RawMessage, error) {
		if op.ID != "setModEnabled" || input.Path["modId"] != "local:fishdried_custom" {
			t.Errorf("wrong local operation %s %+v", op.ID, input)
		}
		return json.RawMessage(`{"data":{"publishedTargets":1,"worldIds":["Master"]}}`), nil
	}))
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(catalog)
	tools, err := s.AvailableTools()
	if err != nil {
		t.Fatal(err)
	}
	loaded := map[string]Tool{}
	for _, tool := range tools {
		if tool.Name == "api_listRoomCommands" {
			if !strings.Contains(tool.Description, "deprecated") || !strings.Contains(tool.Description, "api_listCommands") {
				t.Fatal("model lost replacement", tool.Description)
			}
		}
		if tool.Name == "api_getModConfiguration" {
			loaded[tool.Name] = tool
		}
	}
	definitions, _ := json.Marshal(s.modelTools(loaded))
	if !strings.Contains(string(definitions), "Local numeric directories") || !strings.Contains(string(definitions), "local:") {
		t.Fatal("parameter description lost in model tools")
	}
	_, secret, err := store.CreateToken(TokenInput{Name: "mcp metadata", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/mcp?view=legacy&tags=mod,console", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2025-06-18")
	response := httptest.NewRecorder()
	s.MCP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Local numeric directories") || !strings.Contains(response.Body.String(), "api_listCommands for the same behavior") {
		t.Fatalf("MCP metadata missing: status %d", response.Code)
	}
	raw := json.RawMessage(`{"path":{"roomId":"room-a","modId":"local:fishdried_custom"},"body":{"worldIds":["Master"],"enabled":false,"expectedRevision":"current"},"requestId":"local-mcp-toggle-001"}`)
	if _, err := s.Invoke(context.Background(), principal(), "api_setModEnabled", raw); err != nil {
		t.Fatal("local typed action rejected", err)
	}
}
