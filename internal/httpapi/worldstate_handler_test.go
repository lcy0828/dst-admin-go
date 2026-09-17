package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"dont/internal/dstruntime"
	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/worldstate"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type worldStateHandlerService struct{}

func (worldStateHandlerService) List(context.Context, string) (worldstate.List, error) {
	return worldstate.List{Items: []worldstate.Snapshot{{WorldID: "master", Season: "autumn"}}, Total: 1}, nil
}

type worldStateHandlerFailureService struct {
	worldStateHandlerService
	refreshErr error
}

func (s worldStateHandlerFailureService) RefreshWorld(context.Context, string, string) (worldstate.RefreshResult, error) {
	return worldstate.RefreshResult{}, s.refreshErr
}
func (worldStateHandlerService) History(_ string, worldID string, limit int) (worldstate.History, error) {
	if worldID == "" || limit < 1 {
		return worldstate.History{}, worldstate.ErrInvalidFilter
	}
	return worldstate.History{WorldID: worldID, Limit: limit, Total: 1}, nil
}
func (worldStateHandlerService) WorldTargets(string) ([]rooms.World, error) {
	return []rooms.World{{ID: "master", Name: "Master"}, {ID: "caves", Name: "Caves"}}, nil
}
func (worldStateHandlerService) RefreshWorld(_ context.Context, _, worldID string) (worldstate.RefreshResult, error) {
	return worldstate.RefreshResult{
		WorldID: worldID, Message: "世界状态已采样",
		Snapshot: worldstate.Snapshot{ID: 12, RoomID: "room", WorldID: worldID, WorldName: "Master", Season: "winter"},
	}, nil
}

func TestWorldStateHTTPListHistoryAndRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "world_state_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewWorldStateHandler(worldStateHandlerService{}).Register(v2)
	NewJobHandler(jobService).Register(v2)

	response := performJSON(router, http.MethodGet, "/api/v2/rooms/room/world-states", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/world-states/history?worldId=master&limit=120", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/world-states/history?limit=bad", nil, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_WORLD_STATE_FILTER")
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/world-states/master/actions/refresh", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	data := responseData(t, response)
	snapshot, ok := data["snapshot"].(map[string]interface{})
	if !ok || snapshot["worldId"] != "master" || snapshot["season"] != "winter" {
		t.Fatalf("unexpected refreshed snapshot: %#v", data)
	}
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/world-states/actions/refresh", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if data := responseData(t, response); data["total"] != float64(1) || data["id"] != nil {
		t.Fatalf("room refresh did not return current data directly: %#v", data)
	}
	var jobCount int
	if err := db.Table("world_state_http_job").Count(&jobCount).Error; err != nil || jobCount != 0 {
		t.Fatalf("page refresh created jobs: count=%d, error=%v", jobCount, err)
	}
}

func TestWorldStateHTTPRefreshPreservesRuntimeFailureDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "refresh", err: fmt.Errorf("%w: target did not publish snapshots", dstruntime.ErrRuntimeRefresh), status: http.StatusBadGateway, code: "WORLD_STATE_REFRESH_FAILED"},
		{name: "unavailable", err: fmt.Errorf("%w: shard is offline", dstruntime.ErrRuntimeUnavailable), status: http.StatusServiceUnavailable, code: "WORLD_STATE_RUNTIME_UNAVAILABLE"},
		{name: "deferred", err: fmt.Errorf("%w: room operation in progress", dstruntime.ErrRuntimeRefreshDeferred), status: http.StatusConflict, code: "WORLD_STATE_REFRESH_DEFERRED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			v2 := router.Group("/api/v2")
			NewWorldStateHandler(worldStateHandlerFailureService{refreshErr: test.err}).Register(v2)
			response := performJSON(router, http.MethodPost, "/api/v2/rooms/room/world-states/master/actions/refresh", nil, nil, "")
			assertAPIError(t, response, test.status, test.code)

			var envelope struct {
				Error struct {
					Details struct {
						Reason string `json:"reason"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Details.Reason != test.err.Error() {
				t.Fatalf("reason=%q want %q", envelope.Error.Details.Reason, test.err.Error())
			}
		})
	}
}

func TestWorldStateHTTPUnknownFailurePreservesOperationReason(t *testing.T) {
	gin.SetMode(gin.TestMode)
	failure := errors.New("read worldstate-a.json: I/O Operation Failed")
	router := gin.New()
	router.Use(RequestContext())
	router.GET("/world-state", func(c *gin.Context) { worldStateFailure(c, failure) })

	response := performJSON(router, http.MethodGet, "/world-state", nil, nil, "")
	assertAPIError(t, response, http.StatusInternalServerError, "WORLD_STATE_FAILED")
	var envelope struct {
		Error struct {
			Details struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
		Meta responseMeta `json:"meta"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Details.Reason != failure.Error() {
		t.Fatalf("reason=%q want %q", envelope.Error.Details.Reason, failure.Error())
	}
	if envelope.Meta.RequestID == "" || response.Header().Get("X-Request-ID") != envelope.Meta.RequestID {
		t.Fatalf("request ID was not preserved: header=%q meta=%q", response.Header().Get("X-Request-ID"), envelope.Meta.RequestID)
	}
}
