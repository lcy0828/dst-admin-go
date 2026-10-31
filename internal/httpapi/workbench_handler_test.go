package httpapi

import (
	consoleapi "dont/internal/console"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	"net/http"
	"strings"
	"testing"
)

func TestWorkbenchHTTPValidatesThenLocatesActualPlayerShard(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	store := consoleapi.NewStore(db, "workbench_http_")
	if err = store.Migrate(); err != nil {
		t.Fatal(err)
	}
	commander := &grantCommander{output: "[DST-ADMIN-RESURRECT] REQUESTED"}
	service, err := consoleapi.NewService(consoleHandlerRooms{}, consoleHandlerSender{}, store, commander)
	if err != nil {
		t.Fatal(err)
	}
	locator := &grantLocator{}
	h := NewConsoleHandler(service)
	h.ConfigurePlayerLocator(locator)
	app := gin.New()
	h.Register(app.Group("/api/v2"))
	path := "/api/v2/rooms/room/workbench/actions/execute"
	for _, body := range []string{
		`{"commandId":"custom","arguments":{}}`,
		`{"commandId":"resurrect_player","arguments":{"player_id":"KU_TEST123"},"readOnly":true}`,
		`{"commandId":"resurrect_player","arguments":{"player_id":"KU_TEST123","risk":"low"}}`,
		`{"commandId":"resurrect_player","commandId":"announce","arguments":{"player_id":"KU_TEST123"}}`,
		`{"commandId":"resurrect_player","arguments":{"player_id":"KU_X\");c_shutdown()--"}}`,
		`{"commandId":"rollback","arguments":{"days":1}}`,
	} {
		w := performJSON(app, http.MethodPost, path, json.RawMessage(body), nil, "")
		if w.Code < 400 || commander.calls != 0 || locator.calls != 0 {
			t.Fatal(w.Code, w.Body.String(), commander.calls, locator.calls)
		}
	}
	w := performJSON(app, http.MethodPost, path, map[string]any{"commandId": "resurrect_player", "arguments": map[string]any{"player_id": "KU_TEST123"}}, nil, "")
	var env struct {
		Data consoleapi.WorkbenchResult `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &env) != nil || w.Code != 201 || !env.Data.Complete || commander.world != "caves" {
		t.Fatal(w.Code, w.Body.String(), commander.world)
	}
	w = performJSON(app, http.MethodGet, "/api/v2/rooms/room/workbench/runs/"+env.Data.RunID+"?refresh=true", nil, nil, "")
	if w.Code != 200 || commander.calls != 1 || strings.Contains(w.Body.String(), `"script"`) {
		t.Fatal(w.Code, w.Body.String(), commander.calls)
	}
	w = performJSON(app, http.MethodGet, "/api/v2/rooms/other/workbench/runs/"+env.Data.RunID, nil, nil, "")
	if w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
}
