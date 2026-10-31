package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	consoleapi "dont/internal/console"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

func TestControlledSpawnHTTPValidatesBeforeLocatingOrWriting(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	store := consoleapi.NewStore(db, "spawn_http_")
	if err = store.Migrate(); err != nil {
		t.Fatal(err)
	}
	commander := &grantCommander{output: "[DST-ADMIN-CONTROLLED-SPAWN]\tKU_TEST123\twarg\t2\t2\tOK\n"}
	service, err := consoleapi.NewService(consoleHandlerRooms{}, consoleHandlerSender{}, store, commander)
	if err != nil {
		t.Fatal(err)
	}
	locator := &grantLocator{}
	handler := NewConsoleHandler(service)
	handler.ConfigurePlayerLocator(locator)
	app := gin.New()
	handler.Register(app.Group("/api/v2"))
	path := "/api/v2/rooms/room/players/KU_TEST123/actions/spawn-entity"
	for _, body := range []string{`{"entity":"warg","count":6}`, `{"entity":"beefalo","count":1}`, `{"entity":"warg","count":2,"readOnly":true}`, `{"entity":"warg","count":2,"commandId":"custom"}`, `{"entity":"warg","count":2,"x":0}`, `{"entity":"warg","count":2,"count":1}`, `{"entity":"warg\");c_shutdown()--","count":1}`} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		app.ServeHTTP(w, req)
		if w.Code != 422 || commander.calls != 0 || locator.calls != 0 {
			t.Fatal(w.Code, w.Body.String(), commander.calls, locator.calls)
		}
	}
	w := performJSON(app, http.MethodPost, path, map[string]interface{}{"entity": "狗王", "count": 2}, nil, "")
	var env struct {
		Data consoleapi.SpawnEntityResult `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &env) != nil || w.Code != 201 || env.Data.Status != "succeeded" || env.Data.Spawned == nil || *env.Data.Spawned != 2 || commander.world != "caves" {
		t.Fatal(w.Code, w.Body.String())
	}
	read := "/api/v2/rooms/room/players/KU_TEST123/entity-spawns/" + env.Data.RunID
	for _, path := range []string{read + "?refresh=true", read} {
		w = performJSON(app, http.MethodGet, path, nil, nil, "")
		if w.Code != 200 || commander.calls != 1 || strings.Contains(w.Body.String(), "script") {
			t.Fatal(w.Code, w.Body.String(), commander.calls)
		}
	}
	w = performJSON(app, http.MethodGet, strings.Replace(read, "KU_TEST123", "KU_OTHER123", 1), nil, nil, "")
	if w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
}
