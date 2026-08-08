package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/players"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type playerHandlerService struct{}

func (playerHandlerService) List(string, players.ListFilter) (players.List, error) {
	return players.List{Items: []players.Player{{ID: "KU_ONE", Name: "Willow", Online: true}}, Total: 1, Online: 1, Limit: 25}, nil
}

func (playerHandlerService) Player(_ string, playerID string) (players.Player, error) {
	if playerID != "KU_ONE" {
		return players.Player{}, players.ErrPlayerNotFound
	}
	return players.Player{ID: playerID, Name: "Willow", Online: true, WorldID: "master"}, nil
}

func (playerHandlerService) WorldTargets(string) ([]players.WorldTarget, error) {
	return []players.WorldTarget{{ID: "master", Name: "地面"}, {ID: "caves", Name: "洞穴"}}, nil
}

func (playerHandlerService) RefreshWorld(_ context.Context, _, worldID string) (players.RefreshResult, error) {
	return players.RefreshResult{WorldID: worldID, Count: 1, Running: true, Message: "已读取 1 个在线玩家"}, nil
}

func (playerHandlerService) Act(_ context.Context, _ string, _, playerID string, action players.Action, _ players.ActionRequest) (players.ActionResult, error) {
	return players.ActionResult{PlayerID: playerID, Action: action, Message: "操作完成"}, nil
}

func newPlayerHandlerApp(t *testing.T) (*gin.Engine, *jobs.Service) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "players_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewPlayerHandler(playerHandlerService{}, jobService).Register(v2)
	NewJobHandler(jobService).Register(v2)
	return router, jobService
}

func TestPlayerHTTPListRefreshAndActions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, jobsService := newPlayerHandlerApp(t)

	response := performJSON(router, http.MethodGet, "/api/v2/rooms/room/players?status=online", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["online"] != float64(1) {
		t.Fatalf("unexpected player list: %s", response.Body.String())
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/players/actions/refresh", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForPlayerJob(t, jobsService, responseData(t, response)["id"].(string))
	if job.Kind != "player.refresh" || job.Outcome != jobs.OutcomeFull || len(job.Targets) != 2 {
		t.Fatalf("unexpected refresh job: %#v", job)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/players/actions/refresh", map[string]interface{}{
		"worldIds": []string{"caves"},
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job = waitForPlayerJob(t, jobsService, responseData(t, response)["id"].(string))
	if len(job.Targets) != 1 || job.Targets[0].TargetID != "caves" {
		t.Fatalf("selected world refresh was not respected: %#v", job)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/players/KU_ONE/actions/ban", map[string]interface{}{
		"worldId": "master", "confirmation": "room", "reason": "测试", "duration": "permanent",
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job = waitForPlayerJob(t, jobsService, responseData(t, response)["id"].(string))
	if job.Kind != "player.ban" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("unexpected action job: %#v", job)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/players/KU_ONE/actions/god-mode", map[string]interface{}{
		"worldId": "master", "enabled": true,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job = waitForPlayerJob(t, jobsService, responseData(t, response)["id"].(string))
	if job.Kind != "player.god-mode" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("unexpected god mode job: %#v", job)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/players/..%2Fbad/actions/kick", map[string]interface{}{}, nil, "")
	if response.Code == http.StatusAccepted {
		t.Fatalf("unsafe player ID was accepted: %s", response.Body.String())
	}
}

func waitForPlayerJob(t *testing.T, service *jobs.Service, jobID string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(jobID)
		if err == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed || job.Status == jobs.StatusCanceled) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("player job %q did not finish", jobID)
	return jobs.Job{}
}
