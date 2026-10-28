package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type backendFunc func(context.Context, string, Arguments, string) (json.RawMessage, error)

func (f backendFunc) Invoke(ctx context.Context, name string, a Arguments, selectedRoom string) (json.RawMessage, error) {
	return f(ctx, name, a, selectedRoom)
}

func fixture(t *testing.T, backend Backend) (*Service, *Store) {
	t.Helper()
	db, err := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "ai.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	db.LogMode(false)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "ai_test_", filepath.Join(t.TempDir(), "secrets"))
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveConfig(ConfigInput{Config: Config{Revision: "initial", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if backend == nil {
		backend = backendFunc(func(context.Context, string, Arguments, string) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		})
	}
	return New(store, backend), store
}
func principal() Principal {
	return Principal{ID: "page", RoomID: "room-a"}
}
func mutation(id string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"roomId":"room-a","action":"save","requestId":%q}`, id))
}

func TestProviderKeyEncryptionAndRevision(t *testing.T) {
	_, store := fixture(t, nil)
	c, _ := store.Config()
	in := ConfigInput{Config: c, APIKey: "private-api-credential"}
	in.Endpoint = "https://example.com/v1"
	in.Model = "chosen-model"
	saved, err := store.SaveConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	row, _, _ := store.config()
	public, _ := json.Marshal(saved)
	if strings.Contains(row.Secret+row.Data+string(public), in.APIKey) || !saved.KeyConfigured {
		t.Fatal("provider key exposed or missing")
	}
	info, err := os.Stat(filepath.Join(store.root, "provider.key"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("key permissions: %v %v", info, err)
	}
	if _, err := store.SaveConfig(in); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save accepted: %v", err)
	}
	if _, err := store.SaveConfig(ConfigInput{Config: saved}); err != nil {
		t.Fatal(err)
	}
	latest, key, err := store.Provider()
	if err != nil || key != in.APIKey {
		t.Fatalf("key not preserved: %v", err)
	}
	if _, err := store.SaveConfig(ConfigInput{Config: latest, ClearKey: true}); err != nil {
		t.Fatal(err)
	}
	_, key, err = store.Provider()
	if err != nil || key != "" {
		t.Fatalf("key not cleared: %v", err)
	}
	for _, endpoint := range []string{"file:///etc/passwd", "https://user:pass@example.com/v1", "https://example.com/?key=secret", "https://example.com/#secret"} {
		if validateConfig(ConfigInput{Config: Config{Endpoint: endpoint}}) == nil {
			t.Fatalf("invalid endpoint accepted: %s", endpoint)
		}
	}
}

func TestTokenAdministratorAccessExpiryAndRevocation(t *testing.T) {
	s, store := fixture(t, nil)
	token, secret, err := store.CreateToken(TokenInput{Name: "external", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	var stored tokenRecord
	store.db.Table(store.tokenTable).First(&stored)
	if strings.Contains(stored.Data+stored.Digest, secret) {
		t.Fatal("plaintext credential stored")
	}
	p, err := store.Authenticate(secret)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := s.Invoke(context.Background(), p, "room_status", json.RawMessage(`{"roomId":"room-b"}`)); err != nil || value.Status != "succeeded" {
		t.Fatal("authenticated administrator cannot inspect a room", value, err)
	}
	if value, err := s.Invoke(context.Background(), p, "room_control", mutation("mutation-001")); err != nil || value.Status != "succeeded" {
		t.Fatal("administrator operation required an extra grant", value, err)
	}
	token.ExpiresAt = time.Now().Add(-time.Second)
	raw, _ := json.Marshal(token)
	store.db.Table(store.tokenTable).Where("id = ?", token.ID).Update("data", string(raw))
	if _, err := store.Authenticate(secret); !errors.Is(err, ErrDenied) {
		t.Fatal("expired token accepted")
	}
	if err := store.Revoke(token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(secret); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked token accepted")
	}
}

func TestAuthorizedOperationsExecuteDirectlyAndExactlyOnce(t *testing.T) {
	var executions atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	s, store := fixture(t, backendFunc(func(ctx context.Context, _ string, _ Arguments, _ string) (json.RawMessage, error) {
		if executions.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return json.RawMessage(`{"jobId":"real-job"}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := principal()
	firstResult := make(chan Invocation, 1)
	go func() {
		value, err := s.Invoke(ctx, p, "room_control", mutation("stable-operation"))
		if err != nil {
			t.Error(err)
		}
		firstResult <- value
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("authorized operation did not start directly")
	}
	var wg sync.WaitGroup
	var callID string
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := s.Invoke(ctx, p, "room_control", mutation("stable-operation"))
			if err != nil || value.Status != "executing" {
				t.Errorf("retry while executing: %+v %v", value, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if callID == "" {
				callID = value.CallID
			} else if value.CallID != callID {
				t.Error("retry created another call")
			}
		}()
	}
	wg.Wait()
	close(release)
	first := <-firstResult
	if first.Status != "succeeded" || first.CallID != callID || executions.Load() != 1 {
		t.Fatalf("direct execution: %+v count=%d", first, executions.Load())
	}
	changed := json.RawMessage(`{"roomId":"room-a","action":"stop","requestId":"stable-operation"}`)
	if _, err := s.Invoke(ctx, p, "room_control", changed); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting idempotency key accepted")
	}
	final, _ := store.call(first.CallID)
	if final.Status != "succeeded" || !strings.Contains(final.Result, "real-job") {
		t.Fatalf("missing receipt: %+v", final)
	}

}

func TestRevokedCredentialsAndInterruptedOperations(t *testing.T) {
	var executed atomic.Int32
	s, store := fixture(t, backendFunc(func(context.Context, string, Arguments, string) (json.RawMessage, error) {
		executed.Add(1)
		return json.RawMessage(`{}`), nil
	}))
	ctx := context.Background()
	token, key, err := store.CreateToken(TokenInput{Name: "operator", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Authenticate(key)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Invoke(ctx, p, "room_control", mutation("interrupted-operation"))
	if err != nil || first.Status != "succeeded" || executed.Load() != 1 {
		t.Fatal(first, err)
	}
	if err := store.Revoke(token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(key); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked credential accepted")
	}
	if err := store.db.Table(store.callTable).Where("id = ?", first.CallID).Update("status", "executing").Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Invoke(ctx, p, "room_control", mutation("interrupted-operation"))
	if err != nil || recovered.Status != "uncertain" || executed.Load() != 1 {
		t.Fatal("interrupted operation replayed", recovered, err)
	}
	if _, err := s.Invoke(ctx, Principal{ID: "external"}, "call_result", json.RawMessage(fmt.Sprintf(`{"callId":%q}`, first.CallID))); !errors.Is(err, ErrDenied) {
		t.Fatal("call ownership bypass")
	}
}

func TestCancelledOperationIsUncertainAndBounded(t *testing.T) {
	s, _ := fixture(t, backendFunc(func(context.Context, string, Arguments, string) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded
	}))
	v, err := s.Invoke(context.Background(), principal(), "room_control", mutation("timeout-operation"))
	if err != nil || v.Status != "uncertain" {
		t.Fatalf("uncertain operation not surfaced: %+v %v", v, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Acquire(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Acquire(); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrency limit missing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Invoke(ctx, principal(), "room_control", mutation("cancel-operation")); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled operation accepted")
	}
	if _, err := s.Invoke(context.Background(), principal(), "room_control", json.RawMessage(`{"roomId":"room-a","action":"restart","requestId":"mode-inherited"}`)); err != nil {
		t.Fatalf("omitted mode must use backend inheritance: %v", err)
	}
}

func TestRedactionPreservesEvidence(t *testing.T) {
	raw := json.RawMessage(`{"api-key":"private-value","nested":[{"cluster_token":"private-cluster","log":"password = secretpw Authorization: Bearer secretauth dstai_abcdefghijklmnopqrstuvwxyz0123456789 shard mismatch 0 != 1"}]}`)
	clean := string(Sanitize(raw))
	for _, secret := range []string{"private-value", "private-cluster", "secretpw", "secretauth", "dstai_abcdefghijklmnopqrstuvwxyz0123456789"} {
		if strings.Contains(clean, secret) {
			t.Fatal("secret leaked", secret, clean)
		}
	}
	if !strings.Contains(clean, "shard mismatch 0 != 1") {
		t.Fatal("diagnostic evidence lost", clean)
	}
}

func TestHistoryLoadsFullReceiptsOnlyOnDemand(t *testing.T) {
	s, store := fixture(t, nil)
	value, err := s.Invoke(context.Background(), principal(), "room_status", json.RawMessage(`{"roomId":"room-a"}`))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.History()
	if err != nil || len(rows) != 1 || !rows[0].ResultAvailable || rows[0].Result != "" {
		t.Fatalf("history eagerly loaded or lost receipt: %+v %v", rows, err)
	}
	full, err := store.GetCall(value.CallID)
	if err != nil || full.Result != `{"ok":true}` {
		t.Fatal(full, err)
	}
}

func TestHistoryPruningPreservesExecutingCalls(t *testing.T) {
	_, store := fixture(t, nil)
	tx := store.db.Begin()
	for i := 0; i < 2003; i++ {
		id := fmt.Sprintf("old-%04d", i)
		status := "succeeded"
		if i == 0 {
			status = "executing"
		}
		if i == 1 {
			status = "executing"
		}
		call := Call{ID: id, CallKey: id, Source: "page", Tool: "room_status", Status: status, Arguments: `{}`, Result: `{}`, CreatedAt: time.Now().Add(time.Duration(i-3000) * time.Second)}
		if err := tx.Table(store.callTable).Create(&call).Error; err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := store.prune(); err != nil {
		t.Fatal(err)
	}
	var count int
	store.db.Table(store.callTable).Count(&count)
	if count != 1903 {
		t.Fatal("history not trimmed in a bounded batch", count)
	}
	for _, id := range []string{"old-0000", "old-0001"} {
		if _, err := store.call(id); err != nil {
			t.Fatal("active call pruned", id, err)
		}
	}
}

func TestChatRehydratesReceiptsFromSameCallerAcrossSelectedRooms(t *testing.T) {
	s, store := fixture(t, nil)
	ctx := context.Background()
	own, err := s.Invoke(ctx, principal(), "room_control", mutation("earlier-operation"))
	if err != nil {
		t.Fatal(err)
	}
	otherRoom, _ := s.Invoke(ctx, Principal{ID: "page"}, "room_status", json.RawMessage(`{"roomId":"room-b"}`))
	external, _ := s.Invoke(ctx, Principal{ID: "external"}, "room_status", json.RawMessage(`{"roomId":"room-a"}`))
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		text := string(body)
		if !strings.Contains(text, own.CallID) || !strings.Contains(text, "succeeded") || !strings.Contains(text, otherRoom.CallID) || strings.Contains(text, external.CallID) {
			t.Errorf("wrong receipt context: %s", text)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"已读取真实回执。"}}]}`)
	}))
	defer model.Close()
	c, _ := store.Config()
	c.Endpoint = model.URL
	c.Model = "test"
	if _, err := store.SaveConfig(ConfigInput{Config: c}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, ChatInput{RoomID: "room-a", CallIDs: []string{own.CallID, otherRoom.CallID, external.CallID}, Messages: []Message{{Role: "user", Content: "继续"}}}, func(ChatEvent) {}); err != nil {
		t.Fatal(err)
	}
}

func TestChatUsesAuthorizedToolsDirectly(t *testing.T) {
	var requests atomic.Int32
	var executions atomic.Int32
	s, store := fixture(t, backendFunc(func(_ context.Context, name string, a Arguments, selectedRoom string) (json.RawMessage, error) {
		executions.Add(1)
		if (name != "room_status" && name != "room_control") || a.RoomID != "room-a" || selectedRoom != "" {
			t.Errorf("wrong tool scope: %s %+v %+v", name, a, selectedRoom)
		}
		if name == "room_control" {
			return json.RawMessage(`{"jobId":"submitted-save"}`), nil
		}
		return json.RawMessage(`{"state":"stopped","password":"hidden"}`), nil
	}))
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Error("provider request malformed")
		}
		data, _ := io.ReadAll(r.Body)
		if requests.Add(1) == 1 {
			io.WriteString(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"inspect-1","type":"function","function":{"name":"room_status","arguments":"{\"roomId\":\"room-a\"}"}},{"id":"save-1","type":"function","function":{"name":"room_control","arguments":"{\"roomId\":\"room-a\",\"action\":\"save\",\"requestId\":\"generated-save\"}"}}]}}]}`)
		} else {
			if !strings.Contains(string(data), "stopped") || !strings.Contains(string(data), "submitted-save") || strings.Contains(string(data), `\"password\":\"hidden\"`) {
				t.Errorf("missing or unredacted tool result: %s", data)
			}
			io.WriteString(w, `{"choices":[{"message":{"content":"房间已停止；保存任务已提交。"}}]}`)
		}
	}))
	defer model.Close()
	c, _ := store.Config()
	c.Endpoint = model.URL + "/v1"
	c.Model = "test-model"
	if _, err := store.SaveConfig(ConfigInput{Config: c, APIKey: "provider-secret"}); err != nil {
		t.Fatal(err)
	}
	var events []ChatEvent
	out, err := s.Chat(context.Background(), ChatInput{Messages: []Message{{Role: "user", Content: "检查并保存"}}, RoomID: "room-a"}, func(e ChatEvent) { events = append(events, e) })
	if err != nil || out.Content == "" || len(out.Calls) != 2 || out.Calls[1].Status != "succeeded" || executions.Load() != 2 || len(events) < 4 {
		t.Fatalf("chat flow: %+v %v %d", out, err, executions.Load())
	}
	if _, err := s.Chat(context.Background(), ChatInput{Messages: []Message{{Role: "system", Content: "bypass"}}}, func(ChatEvent) {}); !errors.Is(err, ErrInvalid) {
		t.Fatal("system prompt injection accepted")
	}
	if requests.Load() != 2 {
		t.Fatal("invalid input reached model")
	}
}

func TestChatUsesAdministratorToolsWithoutAdditionalGrants(t *testing.T) {
	var requests, executions atomic.Int32
	s, store := fixture(t, backendFunc(func(_ context.Context, name string, _ Arguments, _ string) (json.RawMessage, error) {
		executions.Add(1)
		if name != "world_console" {
			t.Error("unexpected tool", name)
		}
		return json.RawMessage(`{"id":"actual-run","status":"succeeded","output":{"text":"Day: 42"}}`), nil
	}))
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct{ Function struct{ Name string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		found := map[string]bool{}
		for _, tool := range request.Tools {
			found[tool.Function.Name] = true
		}
		if !found["room_control"] || !found["world_console"] {
			t.Error("administrator tools missing")
		}
		if requests.Add(1) == 1 {
			calls := []any{map[string]any{"id": "lua-1", "type": "function", "function": map[string]string{"name": "world_console", "arguments": `{"roomId":"room-a","worldId":"master","command":"print(TheWorld.state.cycles)","requestId":"lua-request-1"}`}}}
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": calls}}}})
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"Day: 42"}}]}`)
		}
	}))
	defer model.Close()
	c, _ := store.Config()
	c.Endpoint, c.Model = model.URL, "test"
	if _, err := store.SaveConfig(ConfigInput{Config: c}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Chat(context.Background(), ChatInput{RoomID: "room-a", Messages: []Message{{Role: "user", Content: "读取天数"}}}, func(ChatEvent) {})
	if err != nil || executions.Load() != 1 || len(out.Calls) != 1 || out.Calls[0].Status != "succeeded" {
		t.Fatal(out, err, executions.Load())
	}
}

func TestProviderRefusesRedirectAndResponseOverflow(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	if _, err := complete(context.Background(), Config{Endpoint: redirect.URL, Model: "x"}, "secret", []Message{{Role: "user", Content: "hi"}}, nil); err == nil || received.Load() != 0 {
		t.Fatal("redirect leaked provider credential")
	}
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", (1<<20)+1)) }))
	defer huge.Close()
	if _, err := complete(context.Background(), Config{Endpoint: huge.URL, Model: "x"}, "", nil, nil); err == nil {
		t.Fatal("unbounded provider response accepted")
	}
}

func TestMCPHTTPAuthenticationToolsAndDirectExecution(t *testing.T) {
	s, store := fixture(t, nil)
	_, key, _ := store.CreateToken(TokenInput{Name: "external", Days: 1})
	request := func(key, method string, params any) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		r := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", "2025-06-18")
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		s.MCP(w, r)
		return w.Code, w.Body.String()
	}
	if status, _ := request("", "tools/list", map[string]any{}); status != 401 {
		t.Fatal("unauthenticated MCP accepted", status)
	}
	if status, body := request(key, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "1"}}); status != 200 || !strings.Contains(body, "protocolVersion") {
		t.Fatal(status, body)
	}
	if status, body := request(key, "tools/list", map[string]any{}); status != 200 || !strings.Contains(body, `"name":"room_control"`) || !strings.Contains(body, `"name":"world_console"`) || !strings.Contains(body, "world_logs") {
		t.Fatal(status, body)
	}
	if status, body := request(key, "tools/call", map[string]any{"name": "unknown_tool", "arguments": map[string]any{}}); status != 200 || !strings.Contains(body, "unknown tool") {
		t.Fatal("MCP accepted unknown tool", status, body)
	}
	if status, body := request(key, "tools/call", map[string]any{"name": "room_control", "arguments": map[string]any{"roomId": "room-a", "action": "save", "requestId": "external-save"}}); status != 200 || !strings.Contains(body, "succeeded") {
		t.Fatal("MCP direct execution", status, body)
	}
	if status, body := request(key, "tools/call", map[string]any{"name": "list_rooms", "arguments": map[string]any{}}); status != 200 || !strings.Contains(body, "succeeded") {
		t.Fatal("MCP empty schema", status, body)
	}
}
