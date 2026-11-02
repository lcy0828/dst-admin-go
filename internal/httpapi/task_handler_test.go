package httpapi

import (
	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"dont/internal/authn"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTaskHTTPPreparedExecutionScopeAndRevocation(t *testing.T) {
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
	NewAIHandler(service).registerTasks(v2)
	executions := 0
	v2.POST("/rooms/:roomId/backups", func(c *gin.Context) {
		executions++
		Success(c, 202, gin.H{"id": "job-a", "roomId": "room-a", "status": "queued"})
	})
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

	v2.GET("/rooms", func(c *gin.Context) {
		Success(c, 200, gin.H{"items": []gin.H{{"id": "room-a", "name": "示例房间"}}})
	})
	v2.GET("/jobs/:jobId", func(c *gin.Context) {
		Success(c, 200, gin.H{"id": "job-a", "roomId": "room-a", "status": "succeeded", "outcome": "full"})
	})
	protocol := request("GET", "/api/v2/bot/protocol", "", "", reader)
	if protocol.Code != 200 || !strings.Contains(protocol.Body.String(), "game-read") {
		t.Fatal(protocol.Code, protocol.Body.String())
	}
	prepPath := "/api/v2/bot/tasks/preparations/dst_server_action"
	if denied := request("POST", prepPath, `{"room":"room-a","action":"backup"}`, "", reader); denied.Code < 400 {
		t.Fatal(denied.Code)
	}
	prepared := request("POST", prepPath, `{"room":"room-a","action":"backup"}`, "prepare-test", secret)
	var envelope struct {
		Data aicontrol.TaskResponse `json:"data"`
	}
	if prepared.Code != 200 || json.Unmarshal(prepared.Body.Bytes(), &envelope) != nil || envelope.Data.State != "prepared" || executions != 0 {
		t.Fatal(prepared.Code, prepared.Body.String(), executions)
	}
	path := "/api/v2/bot/tasks/" + envelope.Data.TaskRef + "/execute"
	for _, invalid := range []string{`{"parameters":{"count":9}}`, `{"readOnly":true}`, `{"taskRef":"other"}`} {
		if response := request("POST", path, invalid, "execute-task", secret); response.Code < 400 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	first := request("POST", path, `{}`, "execute-task", secret)
	second := request("POST", path, `{}`, "execute-task", secret)
	if first.Code != 200 || second.Code != 200 || executions != 1 {
		t.Fatal(first.Code, first.Body.String(), second.Code, second.Body.String(), executions)
	}
	if response := request("GET", strings.TrimSuffix(path, "/execute"), "", "", reader); response.Code < 400 {
		t.Fatal("other principal read task", response.Code)
	}
	queryPath := "/api/v2/bot/tasks/queries/dst_read"
	if response := request("POST", queryPath, `{"room":"room-a","capabilityId":"room.backup.create"}`, "", secret); response.Code < 400 {
		t.Fatal("write through read", response.Code)
	}
	if e = store.Revoke(token.ID); e != nil {
		t.Fatal(e)
	}
	if response := request("POST", path, `{}`, "execute-task", secret); response.Code < 400 {
		t.Fatal("idempotency replay bypassed revocation")
	}
}
