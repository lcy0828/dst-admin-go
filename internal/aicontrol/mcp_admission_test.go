package aicontrol

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPAdmissionIsolatedFromPageConversations(t *testing.T) {
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
