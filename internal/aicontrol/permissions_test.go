package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dont/internal/apicontract"
)

func TestTokenScopePersistsAndRejectsUnknownPresets(t *testing.T) {
	_, store := fixture(t, nil)
	for _, in := range []TokenInput{{Name: "x", Days: 1, Profile: "unknown"}, {Name: "x", Days: 1, Profile: ProfileGameRead}, {Name: "x", Days: 1, Profile: ProfileGameManage, RoomIDs: []string{""}}} {
		if _, _, err := store.CreateToken(in); err == nil {
			t.Fatal("invalid token scope accepted")
		}
	}
	token, secret, err := store.CreateToken(TokenInput{Name: "reader", Days: 1, Profile: ProfileGameRead, RoomIDs: []string{"a", "a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Authenticate(secret)
	if err != nil || p.Profile != ProfileGameRead || len(p.RoomIDs) != 2 || p.AllowsRoom("c") {
		t.Fatalf("scope lost: %+v %v", p, err)
	}
	if err = store.Revoke(token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Authenticate(secret); err == nil {
		t.Fatal("revoked token accepted")
	}
	_, legacy, err := store.CreateToken(TokenInput{Name: "admin", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	p, _ = store.Authenticate(legacy)
	if p.Profile != ProfilePanelManage {
		t.Fatal("legacy compatibility lost")
	}
}
func TestRestrictedTypedLegacyDispatcherAndDiscoverySharePolicy(t *testing.T) {
	count := 0
	s, _ := fixture(t, apiBackendFunc(func(_ context.Context, _ Principal, op *apicontract.Operation, _ apicontract.Input) (json.RawMessage, error) {
		count++
		if op.ID == "listRooms" {
			return json.RawMessage(`{"httpStatus":200,"data":{"items":[{"id":"a","name":"A"},{"id":"b","name":"B"}],"total":2}}`), nil
		}
		return json.RawMessage(`{"httpStatus":200,"data":{"items":[]}}`), nil
	}))
	c, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.ConfigureAPI(c)
	p := Principal{ID: "reader", Profile: ProfileGameRead, RoomIDs: []string{"a"}}
	for _, tc := range []struct{ name, raw string }{
		{"api_executeRawRoomCommand", `{"path":{"roomId":"a","worldId":"m"},"body":{"command":"print(1)","confirmation":"A"},"requestId":"test-reader-001"}`},
		{"api_listRoomPlayers", `{"path":{"roomId":"b"},"query":{"includeAccessLists":false}}`},
		{"api_listRoomPlayers", `{"path":{"roomId":"a"}}`},
		{"world_console", `{"roomId":"a","worldId":"m","command":"print(1)","requestId":"test-reader-002"}`},
		{"api_createRoomBackup", `{"path":{"roomId":"a"},"body":{},"requestId":"test-reader-003"}`},
	} {
		if _, err := s.Invoke(context.Background(), p, tc.name, json.RawMessage(tc.raw)); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if count != 0 {
		t.Fatal("denied operation reached backend")
	}
	if _, err = s.dispatchMCP(context.Background(), p, json.RawMessage(`{"operationId":"createRoomBackup","arguments":{"path":{"roomId":"a"},"requestId":"test-reader-004"}}`)); !errors.Is(err, ErrDenied) {
		t.Fatalf("dispatcher bypass: %v", err)
	}
	out, err := s.Invoke(context.Background(), p, "api_listRooms", json.RawMessage(`{}`))
	if err != nil || strings.Contains(string(out.Data), `"id":"b"`) {
		t.Fatalf("list leaked scope: %s %v", out.Data, err)
	}
	out, err = s.Invoke(context.Background(), p, "find_api", json.RawMessage(`{"query":"executeRawRoomCommand","includeSchema":true}`))
	if err != nil || strings.Contains(string(out.Data), `"operationId":"executeRawRoomCommand"`) {
		t.Fatalf("discovery leaked forbidden API: %s %v", out.Data, err)
	}
	if _, err = s.Invoke(context.Background(), p, "api_listRoomPlayers", json.RawMessage(`{"path":{"roomId":"a"},"query":{"includeAccessLists":false}}`)); err != nil {
		t.Fatal(err)
	}
}
func TestMCPAdmissionIndependentOfLongConversations(t *testing.T) {
	s, store := fixture(t, nil)
	_, secret, err := store.CreateToken(TokenInput{Name: "test", Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.Acquire()
	defer first()
	second, _ := s.Acquire()
	defer second()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	s.MCP(w, req)
	if w.Code != 200 {
		t.Fatalf("MCP blocked by chats: %d %s", w.Code, w.Body.String())
	}
}
