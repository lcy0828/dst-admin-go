package httpapi

import (
	"context"
	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"dont/internal/authn"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBotRESTMCPShareDurableRequestAndCurrentAuthorization(t *testing.T) {
	db, e := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "bot.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	db.LogMode(false)
	auth := authn.NewService(db)
	if e = auth.Migrate(); e != nil {
		t.Fatal(e)
	}
	session, _, e := auth.Setup("admin", "testpassword", "127.0.0.1", "test")
	if e != nil {
		t.Fatal(e)
	}
	store := aicontrol.NewStore(db, "bot_http_", t.TempDir())
	if e = store.Migrate(); e != nil {
		t.Fatal(e)
	}
	if _, e = store.SaveConfig(aicontrol.ConfigInput{Config: aicontrol.Config{Revision: "initial", Enabled: true}}); e != nil {
		t.Fatal(e)
	}
	token, secret, e := store.CreateToken(aicontrol.TokenInput{Name: "manager", AdminID: session.Admin.ID, Days: 1, Profile: aicontrol.ProfileGameManage, RoomIDs: []string{"room-a"}})
	if e != nil {
		t.Fatal(e)
	}
	_, reader, e := store.CreateToken(aicontrol.TokenInput{Name: "reader", AdminID: session.Admin.ID, Days: 1, Profile: aicontrol.ProfileGameRead, RoomIDs: []string{"room-a"}})
	if e != nil {
		t.Fatal(e)
	}
	catalog, e := apicontract.Default()
	if e != nil {
		t.Fatal(e)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Recovery(), RequestContext(), RequireSession(auth, func(secret string) (authn.Admin, error) {
		p, e := store.Authenticate(secret)
		if e != nil {
			return authn.Admin{}, e
		}
		return auth.AdministratorByID(p.AdminID)
	}), AITokenScope(store, catalog, nil))
	v2 := router.Group("/api/v2", NewIdempotencyStore(time.Hour, 100).Middleware())
	backend := &AIBackend{}
	backend.ConfigureAPI(router, store)
	service := aicontrol.New(store, backend)
	service.ConfigureAPI(catalog)
	NewAIHandler(service).registerBot(v2)
	executions := 0
	v2.POST("/rooms/:roomId/backups", func(c *gin.Context) {
		executions++
		Success(c, 202, gin.H{"id": "job-a", "roomId": "room-a", "status": "queued"})
	})
	p, _ := store.Authenticate(secret)
	directory, e := service.BotDirectory(p, 1, 64, "", "")
	if e != nil {
		t.Fatal(e)
	}
	revision := ""
	for _, c := range directory["items"].([]aicontrol.BotCapability) {
		if c.ID == "room.backup.create" {
			revision = c.SchemaRevision
		}
	}
	if revision == "" {
		t.Fatal("missing write capability")
	}
	body := `{"contractVersion":1,"schemaRevision":"` + revision + `","parameters":{}}`
	request := func(method, path, content, key, credential string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(content))
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	path := "/api/v2/rooms/room-a/bot/actions/room.backup.create"
	first := request("POST", path, body, "request-shared-001", secret)
	if first.Code != 200 || !strings.Contains(first.Body.String(), "job-a") {
		t.Fatal(first.Code, first.Body.String())
	}
	// MCP reaches exactly the same durable reservation, no second backing request.
	args, _ := json.Marshal(map[string]any{"path": map[string]any{"roomId": "room-a", "capabilityId": "room.backup.create"}, "body": json.RawMessage(body), "requestId": "request-shared-001"})
	original := httptest.NewRequest("POST", path, nil)
	original.Header.Set("Authorization", "Bearer "+secret)
	ctx := aicontrol.WithRequestMetadata(context.Background(), aicontrol.RequestAccess(original, "127.0.0.1"))
	invocation, e := service.Invoke(ctx, p, "api_runBotCapability", args)
	if e != nil || invocation.Status != "succeeded" || executions != 1 {
		t.Fatal(invocation, e, executions)
	}
	if e = store.Migrate(); e != nil {
		t.Fatal(e)
	}
	again := request("POST", path, body, "request-shared-001", secret)
	if again.Code != 200 || executions != 1 {
		t.Fatal(again.Code, again.Body.String(), executions)
	}
	for _, tc := range []struct {
		method, path, body, key, credential string
		status                              int
	}{
		{"POST", path, body, "request-shared-001", reader, 403},
		{"POST", path, strings.Replace(body, `"parameters":{}`, `"parameters":{"readOnly":true}`, 1), "request-other-001", secret, 422},
		{"POST", path, strings.Replace(body, `"parameters":{}`, `"parameters":{"x":1,"x":2}`, 1), "request-other-002", secret, 422},
		{"GET", "/api/v2/bot/capabilities?protocolVersion=1&protocolVersion=1", "", "", secret, 422},
		{"GET", "/api/v2/bot/capabilities?protocolVersion=1", "{}", "", secret, 422},
		{"HEAD", "/api/v2/rooms/room-a/bot/queries/world.entity_count", "", "", secret, 403},
		{"GET", "/api/v2/rooms/other/bot/requests/request-shared-001", "", "", secret, 403},
	} {
		got := request(tc.method, tc.path, tc.body, tc.key, tc.credential)
		if got.Code != tc.status {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, got.Code, got.Body.String())
		}
	}
	if executions != 1 {
		t.Fatal("invalid request executed")
	}
	if e = store.Revoke(token.ID); e != nil {
		t.Fatal(e)
	}
	revoked := request(http.MethodPost, path, body, "request-shared-001", secret)
	if revoked.Code != 401 {
		t.Fatal("replayed revoked authority", revoked.Code, revoked.Body.String())
	}
}
