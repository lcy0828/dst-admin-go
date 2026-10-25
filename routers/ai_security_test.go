package routers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"dont/models"
	"github.com/google/uuid"
)

func TestAIRoutesKeepSessionCSRFAndBearerBoundaries(t *testing.T) {
	configureRouterTestEnvironment(t)
	router, err := InitRouter()
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"dont_admin_session", "dont_auth"} {
		if err := models.DB().Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/api/v2/system/ai", "/api/v2/system/ai/calls", "/api/v2/system/ai/tokens"} {
		if out := request(router, "GET", path, nil, nil, ""); out.Code != 401 {
			t.Fatal("AI data exposed", path, out.Code)
		}
	}
	setup := request(router, "POST", "/api/v2/auth/setup", map[string]string{"username": "ai-security", "password": "AiTest123!"}, nil, "")
	if setup.Code != 201 {
		t.Fatal(setup.Code, setup.Body)
	}
	cookie := setup.Result().Cookies()[0]
	var session struct {
		Data struct {
			CSRF string `json:"csrfToken"`
		}
	}
	json.Unmarshal(setup.Body.Bytes(), &session)
	input := map[string]any{"enabled": true, "endpoint": "", "model": ""}
	status := request(router, "GET", "/api/v2/system/ai", nil, cookie, "")
	var state struct {
		Data struct {
			Config struct {
				Revision string `json:"revision"`
			}
		}
	}
	json.Unmarshal(status.Body.Bytes(), &state)
	input["revision"] = state.Data.Config.Revision
	if out := request(router, "PUT", "/api/v2/system/ai", input, cookie, ""); out.Code != 403 {
		t.Fatal("AI CSRF bypass", out.Code, out.Body)
	}
	if out := request(router, "PUT", "/api/v2/system/ai", input, cookie, session.Data.CSRF); out.Code != 400 {
		t.Fatal("AI idempotency bypass", out.Code, out.Body)
	}
	write := func(method, path string, input any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(input)
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Host = "dst-admin.test"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", session.Data.CSRF)
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req.AddCookie(cookie)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	if out := write("PUT", "/api/v2/system/ai", input); out.Code != 200 {
		t.Fatal(out.Code, out.Body)
	}
	created := write("POST", "/api/v2/system/ai/tokens", map[string]any{"name": "external test", "days": 1})
	if created.Code != 201 {
		t.Fatal(created.Code, created.Body)
	}
	var token struct {
		Data struct {
			Secret string
			Token  struct{ ID string }
		}
	}
	json.Unmarshal(created.Body.Bytes(), &token)
	if token.Data.Secret == "" || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("one-time credential missing")
	}
	mcp := func(secret, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		req.Host = "dst-admin.test"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		req.AddCookie(cookie)
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	if out := mcp("", ""); out.Code != 401 {
		t.Fatal("MCP accepted browser session without Bearer", out.Code, out.Body)
	}
	if out := mcp(token.Data.Secret, ""); out.Code != 200 || !strings.Contains(out.Body.String(), `"name":"call_api"`) || !strings.Contains(out.Body.String(), `"name":"find_api"`) {
		t.Fatal("MCP administrator tool catalog", out.Code, out.Body)
	}
	if out := mcp(token.Data.Secret, "https://untrusted.example"); out.Code != 403 {
		t.Fatal("MCP Origin guard bypass", out.Code, out.Body)
	}
	req := httptest.NewRequest("GET", "/api/v2/system/ai", nil)
	req.Host = "dst-admin.test"
	req.Header.Set("Authorization", "Bearer "+token.Data.Secret)
	out := httptest.NewRecorder()
	router.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatal("administrator Bearer could not use shared REST", out.Code, out.Body)
	}
	if out := write("DELETE", "/api/v2/system/ai/tokens/"+token.Data.Token.ID, nil); out.Code != 200 {
		t.Fatal(out.Code, out.Body)
	}
	if out := mcp(token.Data.Secret, ""); out.Code != 401 {
		t.Fatal("revoked MCP credential accepted", out.Code, out.Body)
	}
	out = httptest.NewRecorder()
	router.ServeHTTP(out, req)
	if out.Code != 401 {
		t.Fatal("revoked Bearer accepted by REST", out.Code, out.Body)
	}
}
