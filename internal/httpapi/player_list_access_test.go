package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"dont/internal/authn"
	"dont/internal/players"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type accessPolicyPlayers struct {
	playerHandlerService
	calls, syncs int
	last         players.ListFilter
	unavailable  bool
}

func (p *accessPolicyPlayers) SyncPassivePlayers(context.Context, string) error {
	p.syncs++
	return nil
}
func (p *accessPolicyPlayers) List(_ string, filter players.ListFilter) (players.List, error) {
	p.calls++
	p.last = filter
	value := players.List{Items: []players.Player{{ID: "KU_EXAMPLE", Name: "Example", Online: true}}, Total: 1, Online: 1, Limit: 25}
	if !filter.SkipAccessLists {
		value.AccessListsAvailable = !p.unavailable
		value.Items[0].Banned = true
		value.Items[0].BanReason = "synthetic-management-detail"
		value.Banned = 1
	}
	return value, nil
}

func TestPlayerListAccessThroughAuthenticatedRESTAndMCP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "players.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.DB().SetMaxOpenConns(1)
	db.LogMode(false)
	auth := authn.NewService(db)
	if err := auth.Migrate(); err != nil {
		t.Fatal(err)
	}
	_, cookie, err := auth.Setup("testadmin", "synthetic-test-password", "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	store := aicontrol.NewStore(db, "policy_", t.TempDir())
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveConfig(aicontrol.ConfigInput{Config: aicontrol.Config{Revision: "initial", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(RequestContext(), RequireSession(auth, func(secret string) (authn.Admin, error) {
		_, err := store.Authenticate(secret)
		return authn.Admin{ID: 1}, err
	}), AITokenScope(store, catalog, nil))
	service := &accessPolicyPlayers{}
	NewPlayerHandler(service, nil).Register(router.Group("/api/v2"))
	backend := &AIBackend{}
	backend.ConfigureAPI(router, store)
	ai := aicontrol.New(store, backend)
	ai.ConfigureAPI(catalog)

	rest := func(key, query, room string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v2/rooms/"+room+"/players"+query, nil)
		if key == "" {
			req.AddCookie(&http.Cookie{Name: authn.SessionCookieName, Value: cookie})
		} else {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	mcpRequest := func(key, view, method string, params json.RawMessage) json.RawMessage {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req := httptest.NewRequest(http.MethodPost, "/mcp"+view, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		out := httptest.NewRecorder()
		ai.MCP(out, req)
		if out.Code != 200 {
			t.Fatalf("MCP status %d", out.Code)
		}
		var response struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(out.Body.Bytes(), &response) != nil || len(response.Error) > 0 {
			t.Fatalf("MCP protocol error: %s", out.Body.String())
		}
		return response.Result
	}
	for _, profile := range []string{aicontrol.ProfileGameRead, aicontrol.ProfilePanelManage, aicontrol.ProfileGameManage, "legacy", "cookie"} {
		admin := profile != aicontrol.ProfileGameRead && profile != aicontrol.ProfileGameManage
		var key string
		var token aicontrol.Token
		if profile != "cookie" {
			input := aicontrol.TokenInput{Name: "synthetic", Days: 1, Profile: profile, RoomIDs: []string{"allowed"}}
			if profile == "legacy" {
				input.Profile = ""
				input.RoomIDs = nil
			}
			token, key, err = store.CreateToken(input)
			if err != nil {
				t.Fatal(err)
			}
			if profile == "legacy" {
				// Exercise persisted pre-profile tokens, not just a current admin key.
				token.Profile = ""
				data, _ := json.Marshal(token)
				if err := db.Table("policy_ai_token").Where("id = ?", token.ID).UpdateColumn("data", string(data)).Error; err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, option := range []string{"omitted", "empty-query", "false", "true"} {
			for _, entry := range []string{"REST", "native", "call_api"} {
				if profile == "cookie" && entry != "REST" {
					continue
				} // MCP authenticates bearer keys only.
				t.Run(profile+"/"+option+"/"+entry, func(t *testing.T) {
					before, synced := service.calls, service.syncs
					denied := !admin && option == "true"
					include := option == "true" || (option == "omitted" || option == "empty-query") && admin
					state := "available"
					if option == "false" {
						state = "not_requested"
					} else if !include {
						state = "not_permitted"
					}
					var value players.List
					if entry == "REST" {
						query := ""
						if option == "false" || option == "true" {
							query = "?includeAccessLists=" + option
						}
						out := rest(key, query, "allowed")
						if denied {
							if out.Code != 403 || !strings.Contains(out.Body.String(), "ACCESS_LISTS_PERMISSION_REQUIRED") {
								t.Fatalf("expected explicit denial: %s", out.Body.String())
							}
						} else {
							var envelope struct {
								Data players.List `json:"data"`
							}
							if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &envelope) != nil {
								t.Fatalf("REST query failed: %s", out.Body.String())
							}
							value = envelope.Data
						}
					} else {
						arguments := `{"path":{"roomId":"allowed"}`
						if option == "empty-query" {
							arguments += `,"query":{}`
						}
						if option == "false" || option == "true" {
							arguments += `,"query":{"includeAccessLists":` + option + `}`
						}
						arguments += `}`
						name := "api_listRoomPlayers"
						if entry == "call_api" {
							name = "call_api"
							arguments = `{"operationId":"listRoomPlayers","arguments":` + arguments + `}`
						}
						result := mcpRequest(key, "", "tools/call", []byte(`{"name":"`+name+`","arguments":`+arguments+`}`))
						var receipt struct {
							IsError    bool                 `json:"isError"`
							Structured aicontrol.Invocation `json:"structuredContent"`
						}
						if err := json.Unmarshal(result, &receipt); err != nil {
							t.Fatal(err)
						}
						if denied {
							if !receipt.IsError || !strings.Contains(string(result), "ACCESS_LISTS_PERMISSION_REQUIRED") {
								t.Fatalf("expected explicit denial: %s", result)
							}
						} else {
							if receipt.IsError || receipt.Structured.Status != "succeeded" {
								t.Fatalf("MCP query failed: %s", result)
							}
							var response struct {
								Data players.List `json:"data"`
							}
							if err := json.Unmarshal(receipt.Structured.Data, &response); err != nil {
								t.Fatal(err)
							}
							value = response.Data
						}
					}
					if denied {
						if service.calls != before || service.syncs != synced {
							t.Fatal("denied call reached business reads")
						}
						return
					}
					if service.calls != before+1 || service.syncs != synced+1 || service.last.SkipAccessLists == include {
						t.Fatalf("wrong read scope/repeated read: %+v", service.last)
					}
					if value.AccessListsState != state || value.AccessListsAvailable != include || len(value.Items) != 1 {
						t.Fatalf("wrong result: %+v", value)
					}
					if !include && (value.Banned != 0 || value.Items[0].Banned || value.Items[0].BanReason != "") {
						t.Fatal("management data leaked")
					}
				})
			}
		}
		if key != "" {
			// Exercise actual shared MCP definitions across alternating profiles/views.
			for _, view := range []string{"", "?view=compact", "?view=all&pagination=off", "?view=bot", "?view=legacy&pagination=off"} {
				var listed mcp.ListToolsResult
				if err := json.Unmarshal(mcpRequest(key, view, "tools/list", json.RawMessage(`{}`)), &listed); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, tool := range listed.Tools {
					if tool.Name != "api_listRoomPlayers" {
						continue
					}
					found = true
					param := tool.InputSchema.(map[string]any)["properties"].(map[string]any)["query"].(map[string]any)["properties"].(map[string]any)["includeAccessLists"].(map[string]any)
					if param["default"] != admin || (!admin && fmt.Sprint(param["enum"]) != "[false]") {
						t.Fatal("incorrect discovery", profile, param)
					}
					if !tool.Annotations.ReadOnlyHint || *tool.Annotations.DestructiveHint {
						t.Fatal("query annotations changed")
					}
				}
				if !found {
					t.Fatal("player query missing", view)
				}
			}
			for _, entry := range []string{"api_listRoomPlayers", "call_api"} {
				for _, arguments := range []string{
					`{"path":{"roomId":"allowed"},"query":{"includeAccessLists":null}}`,
					`{"path":{"roomId":"allowed"},"query":{"includeAccessLists":"false"}}`,
					`{"path":{"roomId":"allowed"},"query":{"includeAccessLists":true,"includeAccessLists":false}}`,
					`{"path":{"roomId":"allowed"},"query":{},"query":{}}`,
					`{"path":{"roomId":"allowed"},"readOnly":true,"profile":"panel-manage"}`,
				} {
					before := service.calls
					if entry == "call_api" {
						arguments = `{"operationId":"listRoomPlayers","arguments":` + arguments + `}`
					}
					result := mcpRequest(key, "", "tools/call", []byte(`{"name":"`+entry+`","arguments":`+arguments+`}`))
					var receipt struct {
						IsError bool `json:"isError"`
					}
					if json.Unmarshal(result, &receipt) != nil || !receipt.IsError || service.calls != before {
						t.Fatalf("ambiguous MCP input accepted: %s", result)
					}
				}
			}
		}
		for _, query := range []string{"?includeAccessLists=", "?includeAccessLists", "?includeAccessLists=no", "?includeAccessLists=false&includeAccessLists=true", "?includeAccessLists=false&includeAccessLists=false", "?includeAccessLists=%zz", "?includeAccessLists=false;includeAccessLists=true"} {
			before := service.calls
			if out := rest(key, query, "allowed"); out.Code != 422 || service.calls != before {
				t.Fatalf("invalid REST input accepted: %s status=%d", query, out.Code)
			}
		}
		for _, query := range []string{"?includeAccessLists=FALSE", "?includeAccessLists=0", "?includeAccessLists=False"} {
			if out := rest(key, query, "allowed"); out.Code != 200 || !service.last.SkipAccessLists {
				t.Fatalf("valid false alias rejected: %s status=%d", query, out.Code)
			}
		}
		for _, query := range []string{"?includeAccessLists=TRUE", "?includeAccessLists=1", "?includeAccessLists=true&profile=panel-manage&readOnly=true"} {
			before := service.calls
			out := rest(key, query, "allowed")
			if admin {
				if out.Code != 200 || service.last.SkipAccessLists {
					t.Fatal("admin true alias rejected")
				}
			} else if out.Code != 403 || service.calls != before {
				t.Fatal("true alias or spoofed role bypassed scope")
			}
		}
		if profile != "legacy" && profile != "cookie" {
			before := service.calls
			if out := rest(key, "", "other"); out.Code != 403 || service.calls != before {
				t.Fatal("room scope bypass")
			}
			for _, entry := range []string{"api_listRoomPlayers", "call_api"} {
				args := `{"path":{"roomId":"other"},"query":{"includeAccessLists":true}}`
				if entry == "call_api" {
					args = `{"operationId":"listRoomPlayers","arguments":` + args + `}`
				}
				result := mcpRequest(key, "", "tools/call", []byte(`{"name":"`+entry+`","arguments":`+args+`}`))
				if !strings.Contains(string(result), "AI_DENIED") || strings.Contains(string(result), "ACCESS_LISTS_PERMISSION_REQUIRED") || service.calls != before {
					t.Fatalf("room denial lost priority: %s", result)
				}
			}
		}
		if admin {
			service.unavailable = true
			out := rest(key, "", "allowed")
			if out.Code != 200 || !strings.Contains(out.Body.String(), `"accessListsState":"unavailable"`) {
				t.Fatal("unavailable list confused with omitted", out.Body.String())
			}
			service.unavailable = false
		}
		if key != "" {
			if err := store.Revoke(token.ID); err != nil {
				t.Fatal(err)
			}
			before := service.calls
			if out := rest(key, "", "allowed"); out.Code != 401 || service.calls != before {
				t.Fatal("revoked token accepted")
			}
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			req.Header.Set("Authorization", "Bearer "+key)
			out := httptest.NewRecorder()
			ai.MCP(out, req)
			if out.Code != 401 || service.calls != before {
				t.Fatal("revoked token accepted by MCP")
			}
		}
	}
	if err := auth.Logout(cookie); err != nil {
		t.Fatal(err)
	}
	if out := rest("", "", "allowed"); out.Code != 401 {
		t.Fatal("expired cookie accepted")
	}
}
