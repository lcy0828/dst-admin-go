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
	"time"
	"unicode/utf8"
)

func TestOversizedSuccessPreservesReceiptAndIdempotency(t *testing.T) {
	var count int
	s, store := fixture(t, backendFunc(func(context.Context, string, Arguments, string) (json.RawMessage, error) {
		count++
		return json.Marshal(map[string]any{"id": "actual-run", "jobId": "actual-job", "status": "succeeded", "output": map[string]any{"text": "BEGIN " + strings.Repeat("<测试>", 16000) + " FINAL_ERROR_EVIDENCE"}})
	}))
	first, err := s.Invoke(context.Background(), principal(), "world_console", json.RawMessage(`{"roomId":"room-a","worldId":"master","command":"c_save()","requestId":"large-receipt-001"}`))
	if err != nil || first.Status != "succeeded" || len(first.Data) > 64<<10 || !json.Valid(first.Data) {
		t.Fatal(first, err)
	}
	for _, evidence := range []string{"actual-run", "actual-job", "succeeded", "BEGIN", "FINAL_ERROR_EVIDENCE", "_aiTruncated"} {
		if !strings.Contains(string(first.Data), evidence) {
			t.Fatal("lost evidence", evidence)
		}
	}
	stored, err := store.GetCall(first.CallID)
	if err != nil || stored.Result != string(first.Data) || stored.Status != "succeeded" {
		t.Fatal("wrong stored receipt", stored, err)
	}
	second, err := s.Invoke(context.Background(), principal(), "world_console", json.RawMessage(`{"roomId":"room-a","worldId":"master","command":"c_save()","requestId":"large-receipt-001"}`))
	if err != nil || second.CallID != first.CallID || count != 1 || second.Status != "succeeded" {
		t.Fatal("truncation caused execution retry", second, err, count)
	}
}

func TestResultExcerptPreservesStructureAndNumbers(t *testing.T) {
	items := []any{}
	for i := 0; i < 200; i++ {
		items = append(items, map[string]any{"id": fmt.Sprint(i), "text": strings.Repeat("中文\x00<", 1000)})
	}
	raw, _ := json.Marshal(map[string]any{"callId": "real-call", "status": "succeeded", "items": items, "sequence": json.Number("9007199254740993")})
	for _, budget := range []int{8 << 10, 64 << 10} {
		bounded := boundResult(Sanitize(raw), budget)
		if len(bounded) > budget || !json.Valid(bounded) || !utf8.Valid(bounded) || !strings.Contains(string(bounded), "real-call") {
			t.Fatal("invalid bounded result", len(bounded), string(bounded))
		}
		var result struct {
			Sequence json.Number `json:"sequence"`
			Items    []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(bounded, &result); err != nil || result.Sequence.String() != "9007199254740993" || len(result.Items) == 0 || result.Items[0].ID != "0" || result.Items[len(result.Items)-1].ID != "199" {
			t.Fatal("excerpts lost structure, recent rows or exact number", result, err)
		}
	}
}

func TestModelReceivesUsefulExcerptOfLargeCommandReceipt(t *testing.T) {
	var requests int
	s, store := fixture(t, backendFunc(func(context.Context, string, Arguments, string) (json.RawMessage, error) {
		return json.Marshal(map[string]any{"id": "actual-run", "status": "succeeded", "output": map[string]any{"text": "World: forest " + strings.Repeat("x", 20<<10) + " Season: winter"}})
	}))
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"id":"receipt-1","type":"function","function":{"name":"command_receipt","arguments":"{\"roomId\":\"room-a\",\"runId\":\"actual-run\"}"}}]}}]}`)
			return
		}
		var body struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		last := body.Messages[len(body.Messages)-1]
		if last.Role != "tool" || len(last.Content) > 8<<10 {
			t.Error("unbounded tool context", len(last.Content))
		}
		for _, evidence := range []string{"actual-run", "succeeded", "World: forest", "Season: winter", "_aiTruncated", "callId"} {
			if !strings.Contains(last.Content, evidence) {
				t.Error("model lost evidence", evidence)
			}
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"World: forest, Season: winter"}}]}`)
	}))
	defer model.Close()
	config, _ := store.Config()
	config.Endpoint, config.Model = model.URL, "test"
	if _, err := store.SaveConfig(ConfigInput{Config: config}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Chat(context.Background(), ChatInput{RoomID: "room-a", Messages: []Message{{Role: "user", Content: "查询命令结果"}}}, func(ChatEvent) {})
	if err != nil || out.Limited || len(out.Calls) != 1 || requests != 2 {
		t.Fatal(out, err, requests)
	}
}

func TestMCPBatchBoundsActualToolExecution(t *testing.T) {
	var active, peak atomic.Int32
	started, release := make(chan struct{}, 20), make(chan struct{})
	s, store := fixture(t, backendFunc(func(ctx context.Context, _ string, _ Arguments, _ string) (json.RawMessage, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		started <- struct{}{}
		select {
		case <-release:
			return json.RawMessage(`{"ok":true}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	_, key, err := store.CreateToken(TokenInput{Name: "batch-client", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	batch := []any{}
	for i := 0; i < 20; i++ {
		batch = append(batch, map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": "tools/call", "params": map[string]any{"name": "room_status", "arguments": map[string]string{"roomId": "room-a"}}})
	}
	body, _ := json.Marshal(batch)
	r := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-03-26")
	r.Header.Set("Authorization", "Bearer "+key)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.MCP(w, r) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			close(release)
			<-done
			t.Fatal("batch did not start", w.Body.String())
		}
	}
	close(release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("batch did not complete")
	}
	if w.Code != 200 || peak.Load() != 2 {
		t.Fatal("MCP bypassed execution limit", peak.Load(), w.Code, w.Body.String())
	}
}

func TestSelectedRoomContextAppliesToReceiptsAndTaskQueries(t *testing.T) {
	s, _ := fixture(t, nil)
	ctx := context.Background()
	p := principal()
	if _, err := s.Invoke(ctx, p, "get_job", json.RawMessage(`{"jobId":"task-a"}`)); err == nil {
		t.Fatal("task query omitted its room")
	}
	if _, err := s.Invoke(ctx, p, "room_status", json.RawMessage(`{"roomId":"room-b"}`)); err == nil {
		t.Fatal("tool ignored selected room")
	}
	first, err := s.Invoke(ctx, p, "get_job", json.RawMessage(`{"roomId":"room-a","jobId":"task-a"}`))
	if err != nil {
		t.Fatal(err)
	}
	p.RoomID = "room-b"
	if _, err := s.Invoke(ctx, p, "call_result", json.RawMessage(fmt.Sprintf(`{"callId":%q}`, first.CallID))); err == nil {
		t.Fatal("cached task ignored selected room")
	}
}

func TestModelExcerptKeepsConsoleEvidenceAndLookupState(t *testing.T) {
	value := map[string]any{"id": "run-123", "status": "uncertain", "resultState": "unconfirmed", "receiptRefreshUntil": "2026-10-27T10:00:00Z", "receiptCheck": map[string]any{"status": "not_found", "checkedAt": "2026-10-27T09:59:00Z"}, "output": map[string]any{"text": "0\n", "truncated": false}}
	for i := 0; i < 100; i++ {
		value[fmt.Sprintf("extra_%03d", i)] = strings.Repeat("noise", 500)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	bounded := boundResult(raw, 2048)
	if err = json.Unmarshal(bounded, &got); err != nil {
		t.Fatal(err)
	}
	if got["resultState"] != "unconfirmed" || got["receiptRefreshUntil"] != value["receiptRefreshUntil"] || got["receiptCheck"].(map[string]any)["status"] != "not_found" || got["output"].(map[string]any)["text"] != "0\n" {
		t.Fatalf("lost evidence: %s", bounded)
	}
}

func TestModelExcerptRetainsDiagnosticLimitationsAndCausalUncertainty(t *testing.T) {
	value := map[string]any{"status": "suspected", "logCorrelation": "unknown", "limitations": []string{"STACK_REFERENCE_IS_NOT_CAUSATION", "START_CORRELATION_UNKNOWN"}, "findings": []map[string]any{{"code": "MOD_IN_ERROR_STACK", "kind": "suspected"}}, "evidence": []map[string]any{{"errorCursor": 12, "lines": []string{"LUA ERROR"}}}}
	for i := 0; i < 100; i++ {
		value[fmt.Sprintf("long-extra-%d", i)] = strings.Repeat("x", 1000)
	}
	raw, _ := json.Marshal(value)
	bounded := boundResult(raw, 8192)
	var result map[string]any
	if err := json.Unmarshal(bounded, &result); err != nil || result["logCorrelation"] != "unknown" || result["limitations"] == nil || result["findings"] == nil {
		t.Fatalf("diagnostic uncertainty lost: %s %v", bounded, err)
	}
}
