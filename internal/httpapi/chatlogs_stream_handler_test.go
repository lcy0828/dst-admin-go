package httpapi

import (
	"bufio"
	"context"
	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"encoding/json"
	"github.com/jinzhu/gorm"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/chatlogs"
	"github.com/gin-gonic/gin"
)

type chatStreamFixture struct {
	chatLogHandlerService
	cursor chan string
}

func (s chatStreamFixture) Subscribe(_ context.Context, _ string, _ chatlogs.Kind, cursor string) (*chatlogs.Subscription, error) {
	if cursor == "invalid" {
		return nil, chatlogs.ErrInvalidStreamCursor
	}
	if cursor == "busy" {
		return nil, chatlogs.ErrStreamBusy
	}
	s.cursor <- cursor
	return &chatlogs.Subscription{Initial: chatlogs.StreamWindow{Signal: "chat.cursor", Cursor: "checkpoint"}}, nil
}
func (s chatStreamFixture) StreamWindow(ctx context.Context, _ string, _ chatlogs.Kind, cursor string) (chatlogs.StreamWindow, error) {
	return chatlogs.StreamWindow{Cursor: cursor}, ctx.Err()
}
func (s chatStreamFixture) StreamStatus(string) chatlogs.StreamStatus {
	return chatlogs.StreamStatus{State: "ready"}
}
func TestChatStreamHTTPHeadersResumeAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	fixture := chatStreamFixture{cursor: make(chan string, 1)}
	NewChatLogHandler(fixture).Register(r.Group("/api/v2"))
	server := httptest.NewServer(r)
	defer server.Close()
	for _, test := range []struct{ query, header, want string }{{"", "header-cursor", "header-cursor"}, {"?cursor=query-cursor", "header-cursor", "query-cursor"}, {"", "", ""}} {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v2/rooms/room/chat-logs/stream"+test.query, nil)
		req.Header.Set("Last-Event-ID", test.header)
		response, err := server.Client().Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if response.StatusCode != 200 || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") || response.Header.Get("X-Accel-Buffering") != "no" {
			t.Fatal("missing stream headers")
		}
		if got := <-fixture.cursor; got != test.want {
			t.Fatalf("cursor %q != %q", got, test.want)
		}
		reader := bufio.NewReader(response.Body)
		frame := ""
		for !strings.Contains(frame, "\n\n") {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			frame += line
		}
		if !strings.Contains(frame, "event: chat.cursor") || !strings.Contains(frame, "id: checkpoint") {
			t.Fatal(frame)
		}
		cancel()
		response.Body.Close()
	}
	for _, test := range []struct {
		query   string
		code    int
		message string
	}{{"?cursor=", 422, "INVALID_CHAT_STREAM_CURSOR"}, {"?cursor=invalid", 422, "INVALID_CHAT_STREAM_CURSOR"}, {"?cursor=busy", 429, "CHAT_STREAM_BUSY"}} {
		response := performJSON(r, http.MethodGet, "/api/v2/rooms/room/chat-logs/stream"+test.query, nil, nil, "")
		assertAPIError(t, response, test.code, test.message)
		if test.code == 429 && response.Header().Get("Retry-After") != "5" {
			t.Fatal("missing Retry-After")
		}
	}
}
func TestChatStreamFramesDistinguishUpdatesAndGaps(t *testing.T) {
	out := httptest.NewRecorder()
	window := chatlogs.StreamWindow{Signal: "chat.gap", Cursor: "floor"}
	cursor, err := writeChatWindow(out, window, "old")
	if err != nil || cursor != "floor" {
		t.Fatal(err)
	}
	window = chatlogs.StreamWindow{Cursor: "after-filtered", Items: []chatlogs.StreamItem{{Cursor: "revision", Change: chatlogs.ChatChange{ID: "same-message", Operation: "updated", Kind: chatlogs.KindSay, RecordedAt: time.Now()}}}}
	cursor, err = writeChatWindow(out, window, cursor)
	if err != nil || cursor != "after-filtered" {
		t.Fatal(err)
	}
	body := out.Body.String()
	for _, want := range []string{"event: chat.gap", "RETENTION_EXCEEDED", "event: chat.record", "\"operation\":\"updated\"", "id: revision", "id: after-filtered", "event: chat.cursor"} {
		if !strings.Contains(body, want) {
			t.Fatal("missing " + want)
		}
	}
}

type failedChatWriter struct{ *httptest.ResponseRecorder }

func (failedChatWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestChatStreamWriteFailureKeepsPreviousCursor(t *testing.T) {
	cursor, err := writeChatWindow(failedChatWriter{httptest.NewRecorder()}, chatlogs.StreamWindow{Signal: "chat.cursor", Cursor: "new"}, "old")
	if err == nil || cursor != "old" {
		t.Fatal("failed write advanced resume point")
	}
}

func TestChatMCPResourceStreamsThroughRealScopedRouter(t *testing.T) {
	db, err := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	store := aicontrol.NewStore(db, "stream_", t.TempDir())
	if err = store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SaveConfig(aicontrol.ConfigInput{Config: aicontrol.Config{Revision: "initial", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	token, secret, err := store.CreateToken(aicontrol.TokenInput{Name: "synthetic stream reader", Days: 1, Profile: aicontrol.ProfilePanelManage, RoomIDs: []string{"room"}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatus(401)
			return
		}
		c.Next()
	}, AITokenScope(store, catalog, nil))
	fixture := chatStreamFixture{cursor: make(chan string, 1)}
	NewChatLogHandler(fixture).Register(router.Group("/api/v2"))
	backend := &AIBackend{}
	backend.ConfigureAPI(router, store)
	service := aicontrol.New(store, backend)
	service.ConfigureAPI(catalog)
	handler := NewAIHandler(service)
	handler.ConfigureResources(router)
	router.GET("/api/v2/system/ai/resources/:resourceId", handler.resource)
	request := httptest.NewRequest("POST", "/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	ctx := aicontrol.WithRequestMetadata(context.Background(), aicontrol.RequestAccess(request, "127.0.0.1"))
	principal, err := store.Authenticate(secret)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := catalog.Operation("streamRoomChatLogs")
	raw, err := backend.InvokeAPI(ctx, principal, op, apicontract.Input{Path: map[string]any{"roomId": "room"}, Query: map[string]any{"kind": "say"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.cursor) != 0 || !strings.Contains(string(raw), `"fetched":false`) {
		t.Fatal("resource creation consumed stream")
	}
	var link struct {
		Resource struct {
			URL string `json:"url"`
		} `json:"resource"`
	}
	if err = json.Unmarshal(raw, &link); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(streamCtx, "GET", server.URL+link.Resource.URL, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Last-Event-ID", "persisted-cursor")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("link did not stream", response.StatusCode)
	}
	if got := <-fixture.cursor; got != "persisted-cursor" {
		t.Fatal("resume header lost", got)
	}
	cancel()
	response.Body.Close()
	for _, path := range []string{link.Resource.URL, "/api/v2/rooms/room/chat-logs/stream"} {
		result := performJSON(router, "GET", path, nil, nil, "")
		if result.Code != 401 {
			t.Fatal("unauthenticated chat exposed")
		}
	}
	foreign := httptest.NewRequest("GET", "/api/v2/rooms/other/chat-logs/stream", nil)
	foreign.Header.Set("Authorization", "Bearer "+secret)
	denied := httptest.NewRecorder()
	router.ServeHTTP(denied, foreign)
	if denied.Code != 403 {
		t.Fatal("cross-room REST stream exposed", denied.Code)
	}
	if err = store.Revoke(token.ID); err != nil {
		t.Fatal(err)
	}
	expired := httptest.NewRequest("GET", link.Resource.URL, nil)
	expired.Header.Set("Authorization", "Bearer "+secret)
	denied = httptest.NewRecorder()
	router.ServeHTTP(denied, expired)
	if denied.Code != 401 {
		t.Fatal("revoked stream link still usable", denied.Code)
	}
}
