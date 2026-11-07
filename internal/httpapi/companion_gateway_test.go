package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apidocs "dont/docs"
	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"dont/internal/authn"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

func TestCompanionGatewayRealRouterIdentityScopeAndDedup(t *testing.T) {
	db, e := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "gateway.db"))
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
	session, _, e := auth.Setup("admin", "synthetic-password", "127.0.0.1", "test")
	if e != nil {
		t.Fatal(e)
	}
	store := aicontrol.NewStore(db, "generic_http_", t.TempDir())
	if e = store.Migrate(); e != nil {
		t.Fatal(e)
	}
	if _, e = store.SaveConfig(aicontrol.ConfigInput{Config: aicontrol.Config{Revision: "initial", Enabled: true}}); e != nil {
		t.Fatal(e)
	}
	_, secret, e := store.CreateToken(aicontrol.TokenInput{Name: "private plugin fixture", Days: 1, AdminID: session.Admin.ID})
	if e != nil {
		t.Fatal(e)
	}
	catalog, e := apicontract.Default()
	if e != nil {
		t.Fatal(e)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	scopeMiddleware := AITokenScope(store, catalog, nil)
	router.Use(Recovery(), RequestContext(), RequireSession(auth, func(secret string) (authn.Admin, error) {
		p, e := store.Authenticate(secret)
		if e != nil {
			return authn.Admin{}, e
		}
		return auth.AdministratorByID(p.AdminID)
	}), func(c *gin.Context) { scopeMiddleware(c) })
	v2 := router.Group("/api/v2", NewIdempotencyStore(time.Hour, 100).Middleware())
	backend := &AIBackend{}
	backend.ConfigureAPI(router, store)
	service := aicontrol.New(store, backend)
	service.ConfigureAPI(catalog)
	NewAIHandler(service).registerTasks(v2)
	executed := 0
	v2.POST("/rooms/:roomId/backups", func(c *gin.Context) { executed++; Success(c, 200, gin.H{"saved": true}) })
	actor := aicontrol.CompanionActor{Identity: []string{"test-platform", "test-bot", "test-group", "test-user"}, Profile: aicontrol.ProfileGameManage, Rooms: []string{"room-test"}, Categories: []string{"*"}, Writes: []string{"*"}}
	request := func(in aicontrol.GenericInput, model bool) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(in)
		req := httptest.NewRequest("POST", "/api/v2/bot/gateway", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		if model {
			req = req.WithContext(aicontrol.WithModelChannel(context.Background()))
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	input := aicontrol.GenericInput{Actor: actor, Mode: "run", Operation: "createRoomBackup", RequestID: "platform-message-001", Arguments: map[string]any{"path": map[string]any{"roomId": "room-test"}, "body": map[string]any{}}}
	if out := request(input, true); out.Code != 403 {
		t.Fatalf("model delegated: %d %s", out.Code, out.Body)
	}
	prepared := request(input, false)
	var envelope struct {
		Data struct {
			TaskRef string `json:"taskRef"`
			State   string `json:"state"`
		} `json:"data"`
	}
	if prepared.Code != 200 || json.Unmarshal(prepared.Body.Bytes(), &envelope) != nil || envelope.Data.State != "needs_confirmation" || executed != 0 {
		t.Fatalf("prepare: %d %s %d", prepared.Code, prepared.Body, executed)
	}
	input = aicontrol.GenericInput{Actor: actor, Mode: "execute", TaskRef: envelope.Data.TaskRef}
	for i := 0; i < 2; i++ {
		out := request(input, false)
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body)
		}
	}
	if executed != 1 {
		t.Fatal("duplicate dispatch", executed)
	}
	input.Actor.Writes = nil
	if out := request(input, false); out.Code != 403 {
		t.Fatal("HTTP cached response bypassed revocation", out.Code, out.Body)
	}
	input.Actor = actor
	input.Actor.Identity = append([]string{}, actor.Identity...)
	input.Actor.Identity[2] = "different-group"
	input.Mode = "result"
	if out := request(input, false); out.Code != 403 {
		t.Fatal("cross-group receipt", out.Code, out.Body)
	}
	t.Run("frozen-python-plugin-new-native-API", func(t *testing.T) {
		python := os.Getenv("DST_GENERIC_PYTHON")
		if python == "" {
			t.Skip("set DST_GENERIC_PYTHON for the real Go/HTTP/AstrBot integration")
		}
		// Only the server contract changes. There is no plugin registry entry,
		// keyword, category or operation-specific adapter for these endpoints.
		extra := `  /generic-fixture:
    get:
      tags: [System]
      operationId: readSprout
      summary: 合成植物读数
      responses:
        '200': {description: Synthetic array, content: {application/json: {schema: {type: object}}}}
    patch:
      tags: [System]
      operationId: changeSprout
      summary: 修改合成植物设置
      x-dst-access: {profile: panel-manage, access: write, confirmation: none}
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              properties:
                enabled: {type: boolean, default: true}
                omitted: {type: integer, default: 9}
                nullable: {type: [string, 'null']}
                composite: {oneOf: [{type: string}, {type: integer}]}
      responses:
        '204': {description: Updated}
`
		future, e := apicontract.Parse([]byte(strings.Replace(string(apidocs.OpenAPI), "paths:\n", "paths:\n"+extra, 1)))
		if e != nil {
			t.Fatal(e)
		}
		service.ConfigureAPI(future)
		scopeMiddleware = AITokenScope(store, future, nil)
		v2.GET("/generic-fixture", func(c *gin.Context) { Success(c, 200, []any{false, 0, nil, "synthetic"}) })
		writes := 0
		v2.PATCH("/generic-fixture", func(c *gin.Context) {
			var body map[string]any
			if c.ShouldBindJSON(&body) != nil {
				t.Error("bad body")
			}
			if len(body) != 3 || body["enabled"] != false || body["composite"] != float64(0) {
				t.Errorf("PATCH was changed: %#v", body)
			}
			writes++
			c.Status(204)
		})
		router.POST("/mcp", func(c *gin.Context) { service.MCP(c.Writer, c.Request) })
		server := httptest.NewServer(router)
		defer server.Close()
		command := exec.Command(python, "-m", "unittest", "test_generic_live_contract", "-q")
		command.Dir = "../../integrations/astrbot/tests"
		command.Env = append(os.Environ(), "DST_TEST_GENERIC_URL="+server.URL, "DST_TEST_GENERIC_TOKEN="+secret)
		if output, e := command.CombinedOutput(); e != nil {
			t.Fatalf("real plugin failed: %v\n%s", e, output)
		}
		if writes != 1 {
			t.Fatalf("expected one actual native write, got %d", writes)
		}
	})
}
