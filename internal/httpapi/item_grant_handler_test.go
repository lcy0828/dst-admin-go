package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	consoleapi "dont/internal/console"
	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

type grantLocator struct {
	calls int
	err   error
}

func (g *grantLocator) OnlineWorld(_ context.Context, room, _ string) (rooms.World, error) {
	g.calls++
	return rooms.World{RoomID: room, ID: "caves"}, g.err
}

type grantCommander struct {
	calls  int
	world  string
	output string
}

func (g *grantCommander) ExecuteCommand(_ context.Context, _, world string, request dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	g.calls++
	g.world = world
	return dstruntime.CommandReceipt{RequestID: request.RequestID, Action: request.Action, OK: true, Code: "COMMAND_EXECUTED", CompletedAt: time.Now(), Details: map[string]interface{}{"output": map[string]interface{}{"text": g.output, "truncated": false}}}, nil
}
func TestFixedItemGrantHTTPValidatesBeforeDispatchAndHidesScripts(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	store := consoleapi.NewStore(db, "grant_http_")
	if err = store.Migrate(); err != nil {
		t.Fatal(err)
	}
	commander := &grantCommander{output: "[DST-ADMIN-GIVE]\tKU_TEST123\tmeatballs\tunits\t3\n"}
	service, err := consoleapi.NewService(consoleHandlerRooms{}, consoleHandlerSender{}, store, commander)
	if err != nil {
		t.Fatal(err)
	}
	locator := &grantLocator{}
	h := NewConsoleHandler(service)
	h.ConfigurePlayerLocator(locator)
	router := gin.New()
	h.Register(router.Group("/api/v2"))
	path := "/api/v2/rooms/room/players/KU_TEST123/actions/give-item"
	for _, body := range []string{`{"item":"meatballs","count":41}`, `{"item":"meatballs","count":1.5}`, `{"item":"beefalo","count":3}`, `{"item":"meatballs\");c_shutdown()--","count":3}`, `{"item":"meatballs","count":3,"readOnly":true}`, `{"item":"meatballs","count":3,"commandId":"custom"}`, `{"item":"meatballs","count":3,"count":4}`} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != 422 || commander.calls != 0 || locator.calls != 0 {
			t.Fatal(w.Code, w.Body.String(), commander.calls, locator.calls)
		}
	}
	w := performJSON(router, http.MethodPost, path, map[string]interface{}{"item": "肉丸", "count": 3}, nil, "")
	var result struct {
		Data consoleapi.ItemGrant `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != 201 || result.Data.Status != "succeeded" || result.Data.Granted == nil || *result.Data.Granted != 3 || commander.calls != 1 || commander.world != "caves" {
		t.Fatal(w.Code, w.Body.String(), commander)
	}
	read := "/api/v2/rooms/room/players/KU_TEST123/item-grants/" + result.Data.RunID
	w = performJSON(router, http.MethodGet, read+"?refresh=true", nil, nil, "")
	if w.Code != 200 || commander.calls != 1 || strings.Contains(w.Body.String(), "script") || strings.Contains(w.Body.String(), "SpawnPrefab") {
		t.Fatal(w.Code, w.Body.String(), commander.calls)
	}
	w = performJSON(router, http.MethodGet, strings.Replace(read, "KU_TEST123", "KU_OTHER123", 1), nil, nil, "")
	if w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
	locator.err = errors.New("player migrating")
	w = performJSON(router, http.MethodPost, path, map[string]interface{}{"item": "肉丸", "count": 3}, nil, "")
	if w.Code != 409 || commander.calls != 1 {
		t.Fatal(w.Code, w.Body.String(), commander.calls)
	}
}
