package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dont/internal/authn"
	consoleapi "dont/internal/console"
	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/worldquery"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type queryHandlerRuntime struct{ calls int }

func (*queryHandlerRuntime) QueryIdentity(context.Context, string, string) (string, error) {
	return "example-instance", nil
}
func (f *queryHandlerRuntime) ExecuteWorldQuery(_ context.Context, _, _ string, q dstruntime.WorldQueryRequest) (dstruntime.CommandReceipt, error) {
	f.calls++
	return dstruntime.CommandReceipt{}, dstruntime.ErrRuntimeCommandUnconfirmed
}
func (*queryHandlerRuntime) ReadCommandReceipt(context.Context, string, string, dstruntime.CommandRequest, time.Time) (dstruntime.CommandReceipt, error) {
	return dstruntime.CommandReceipt{}, errors.New("not found")
}
func (*queryHandlerRuntime) ReadCurrentWorldState(context.Context, string, string) (dstruntime.CurrentWorldState, error) {
	return dstruntime.CurrentWorldState{}, errors.New("missing")
}
func (*queryHandlerRuntime) ReadPlayers(context.Context, string, string) (dstruntime.Snapshot, error) {
	return dstruntime.Snapshot{}, errors.New("missing")
}

func newQueryHandlerTest(t *testing.T, authenticated bool) (*gin.Engine, *queryHandlerRuntime) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	store := worldquery.NewStore(db, "query_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	f := &queryHandlerRuntime{}
	s := worldquery.New(consoleHandlerRooms{}, f, store)
	router := gin.New()
	v2 := router.Group("/api/v2")
	if authenticated {
		v2.Use(func(c *gin.Context) { c.Set(authn.ContextAdminKey, authn.Admin{ID: 1}); c.Next() })
	}
	NewWorldQueryHandler(s, nil).Register(v2)
	return router, f
}
func TestWorldQueryHTTPRejectsUnknownParametersBodiesAndForgedFlags(t *testing.T) {
	root := "/api/v2/rooms/" + rooms.EncodeID("Example") + "/worlds/" + rooms.EncodeID("Master") + "/queries/"
	for _, tc := range []struct{ path, body string }{
		{"entity_count?prefab=spiderden&readOnly=true", ""},
		{"entity_count?prefab=spiderden&risk=low&category=query&isBuiltin=true", ""},
		{"entity_count?prefab=spiderden&commandId=save_world", ""},
		{"entity_count?prefab=spiderden&prefab=beefalo", ""},
		{"entity_count?prefab=spiderden%22%29%3BSpawnPrefab%28%22beefalo%22%29", ""},
		{"entity_count?prefab=%zz", ""},
		{"entity_count", ""},
		{"world_info?prefab=spiderden", ""},
		{"world_info?=ignored", ""},
		{"world_info", `{"script":"c_save()","readOnly":true}`},
		{"save_world", ""}, {"custom_query", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			router, f := newQueryHandlerTest(t, true)
			req := httptest.NewRequest("GET", root+tc.path, strings.NewReader(tc.body))
			out := httptest.NewRecorder()
			router.ServeHTTP(out, req)
			if out.Code != 422 || f.calls != 0 {
				t.Fatal(out.Code, out.Body.String(), f.calls)
			}
		})
	}
}
func TestWorldQueryHTTPRequiresAuthenticationAndDoesNotExecuteHEAD(t *testing.T) {
	root := "/api/v2/rooms/" + rooms.EncodeID("Example") + "/worlds/" + rooms.EncodeID("Master") + "/queries/entity_count?prefab=spiderden"
	router, f := newQueryHandlerTest(t, false)
	out := httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest("GET", root, nil))
	if out.Code != 403 || f.calls != 0 {
		t.Fatal(out.Code, f.calls)
	}
	router, f = newQueryHandlerTest(t, true)
	for _, method := range []string{"HEAD", "OPTIONS", "POST"} {
		out = httptest.NewRecorder()
		router.ServeHTTP(out, httptest.NewRequest(method, root, nil))
		if out.Code == 200 || f.calls != 0 {
			t.Fatal(method, out.Code, f.calls)
		}
	}
}
func TestWorldQueryHTTPReturnsUncertainRatherThanSuccessOrZero(t *testing.T) {
	router, f := newQueryHandlerTest(t, true)
	root := "/api/v2/rooms/" + rooms.EncodeID("Example") + "/worlds/" + rooms.EncodeID("Master") + "/queries/entity_count?prefab=spiderden"
	out := httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest(http.MethodGet, root, nil))
	if out.Code != 200 || f.calls != 1 || !strings.Contains(out.Body.String(), `"status":"uncertain"`) || strings.Contains(out.Body.String(), `"count":0`) || out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(out.Code, out.Body.String(), f.calls)
	}
}

func TestEditableCommandCannotOverrideReadOnlyQuery(t *testing.T) {
	router, f := newQueryHandlerTest(t, true)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := consoleapi.NewStore(db, "custom_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	s, err := consoleapi.NewService(consoleHandlerRooms{}, consoleHandlerSender{}, store)
	if err != nil {
		t.Fatal(err)
	}
	NewConsoleHandler(s).Register(router.Group("/api/v2"))
	out := performJSON(router, "POST", "/api/v2/commands", map[string]any{
		"name": "entity_count", "category": "query", "risk": "low", "readOnly": true, "isBuiltin": true,
		"script": `SpawnPrefab("beefalo");c_save()`, "parameters": []any{},
	}, nil, "")
	if out.Code != 201 {
		t.Fatal(out.Code, out.Body.String())
	}
	root := "/api/v2/rooms/" + rooms.EncodeID("Example") + "/worlds/" + rooms.EncodeID("Master") + "/queries/entity_count?prefab=spiderden"
	out = httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest("GET", root, nil))
	if out.Code != 200 || f.calls != 1 || strings.Contains(out.Body.String(), "SpawnPrefab") {
		t.Fatal(out.Code, out.Body.String())
	}
}

type unreadableQueryBody struct{}

func (unreadableQueryBody) Read([]byte) (int, error) {
	panic("GET query tried to read a potentially blocking body")
}
func (unreadableQueryBody) Close() error { return nil }
func TestWorldQueryRejectsChunkedBodyWithoutWaiting(t *testing.T) {
	router, f := newQueryHandlerTest(t, true)
	root := "/api/v2/rooms/" + rooms.EncodeID("Example") + "/worlds/" + rooms.EncodeID("Master") + "/queries/world_info"
	req := httptest.NewRequest("GET", root, nil)
	req.Body = unreadableQueryBody{}
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	out := httptest.NewRecorder()
	router.ServeHTTP(out, req)
	if out.Code != 422 || f.calls != 0 {
		t.Fatal(out.Code, f.calls)
	}
}
