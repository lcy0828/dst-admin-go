package httpapi

import (
	"context"
	"dont/internal/playeractivity"
	"dont/internal/rooms"
	"dont/shared"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type activityCatalogStub struct{}

func (activityCatalogStub) List() ([]rooms.Room, error) {
	return []rooms.Room{{ID: "room", Managed: true}}, nil
}
func (activityCatalogStub) Room(id string) (rooms.Room, error) {
	if id != "room" {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return rooms.Room{ID: id, Managed: true}, nil
}
func (activityCatalogStub) Worlds(string) ([]rooms.World, error) {
	return []rooms.World{{ID: "world", Name: "Master"}}, nil
}

type activityNoLiveReads struct{ t *testing.T }

func (s activityNoLiveReads) ReadActivityArtifacts(context.Context, string, string, []string) (shared.RuntimeArtifactBundle, error) {
	s.t.Fatal("GET executed live collection")
	return shared.RuntimeArtifactBundle{}, nil
}
func (s activityNoLiveReads) ActivityScope(context.Context, string, string) (string, error) {
	s.t.Fatal("GET resolved runtime")
	return "", nil
}
func TestPlayerActivityHTTPPersistsSettingsAndKeepsReadsOffline(t *testing.T) {
	db, e := gorm.Open("sqlite3", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	store := playeractivity.NewStore(db, "activity_http_")
	if e = store.Migrate(); e != nil {
		t.Fatal(e)
	}
	service := playeractivity.New(store, activityCatalogStub{}, activityNoLiveReads{t}, nil)
	router := gin.New()
	NewPlayerActivityHandler(service).Register(router.Group("/api/v2"))
	for _, tc := range []struct {
		method, path, body string
		status             int
		contains           string
	}{
		{"GET", "/rooms/room/player-activity/settings", "", 200, `"enabled":true`},
		{"GET", "/rooms/room/player-activity/status", "", 200, `"pending":true`},
		{"GET", "/rooms/room/player-activity/events", "", 200, `"items":[]`},
		{"GET", "/rooms/room/player-activity/events?limit=999", "", 400, "PLAYER_ACTIVITY_INVALID"},
		{"GET", "/rooms/room/player-activity/events?from=nonsense", "", 400, "PLAYER_ACTIVITY_INVALID"},
		{"GET", "/rooms/room/player-activity/trajectory", "", 400, "PLAYER_ACTIVITY_INVALID"},
		{"GET", "/rooms/other/player-activity/settings", "", 404, ""},
		{"PUT", "/rooms/room/player-activity/settings", `{"revision":"default","enabled":false,"positionIntervalSeconds":30}`, 200, `"enabled":false`},
		{"PUT", "/rooms/room/player-activity/settings", `{"revision":"default","enabled":true}`, 409, "PLAYER_ACTIVITY_CONFLICT"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, "/api/v2"+tc.path, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, request)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if tc.method == http.MethodGet && w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("activity history was cacheable")
			}
		})
	}
}
