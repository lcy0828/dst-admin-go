package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

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
	return worldstate.RefreshResult{WorldID: worldID, Message: "世界状态已采样"}, nil
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
	NewWorldStateHandler(worldStateHandlerService{}, jobService).Register(v2)
	NewJobHandler(jobService).Register(v2)

	response := performJSON(router, http.MethodGet, "/api/v2/rooms/room/world-states", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/world-states/history?worldId=master&limit=120", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/world-states/history?limit=bad", nil, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_WORLD_STATE_FILTER")
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/world-states/actions/refresh", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID := responseData(t, response)["id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, getErr := jobService.Get(jobID)
		if getErr == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed) {
			if job.Kind != "world.state.refresh" || job.Outcome != jobs.OutcomeFull || len(job.Targets) != 2 {
				t.Fatalf("unexpected world state job: %#v", job)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("world state job did not finish")
}
