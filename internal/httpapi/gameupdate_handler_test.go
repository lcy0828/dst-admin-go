package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/gameupdate"
	"dont/internal/jobs"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

const releaseHTTPPlanHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type gameReleaseHTTPFixture struct {
	mu               sync.Mutex
	plan             gameupdate.ReleasePlan
	previewErr       error
	releases         map[string]gameupdate.Release
	retrySourceJobID string
}

func (f *gameReleaseHTTPFixture) Preview(context.Context, gameupdate.ReleasePreviewRequest) (gameupdate.ReleasePlan, error) {
	return f.plan, f.previewErr
}

func (f *gameReleaseHTTPFixture) Publish(_ context.Context, request gameupdate.ReleasePublishRequest) (gameupdate.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value := successfulHTTPGameRelease(request.ID, request.SourceJobID, request.Plan)
	f.releases[value.ID] = value
	return value, nil
}

func (f *gameReleaseHTTPFixture) Retry(_ context.Context, id string, sourceJobIDs ...string) (gameupdate.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, found := f.releases[id]
	if !found {
		return gameupdate.Release{}, gameupdate.ErrReleaseNotFound
	}
	if len(sourceJobIDs) > 0 {
		f.retrySourceJobID = sourceJobIDs[0]
	}
	value = successfulHTTPGameRelease(value.ID, value.SourceJobID, value.Plan)
	f.releases[id] = value
	return value, nil
}

func (f *gameReleaseHTTPFixture) Get(id string) (gameupdate.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, found := f.releases[id]
	if !found {
		return gameupdate.Release{}, gameupdate.ErrReleaseNotFound
	}
	return value, nil
}

func (f *gameReleaseHTTPFixture) List(limit, offset int) ([]gameupdate.Release, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	values := make([]gameupdate.Release, 0, len(f.releases))
	for _, value := range f.releases {
		values = append(values, value)
	}
	total := len(values)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return values[offset:end], total, nil
}

func gameReleaseHTTPPlan() gameupdate.ReleasePlan {
	return gameupdate.ReleasePlan{
		Version: 1, DesiredVersion: "701", TopologyRevision: releaseHTTPPlanHash, PlanHash: releaseHTTPPlanHash,
		Policy:          gameupdate.ReleasePolicy{RestartRunning: true, LoadConfirmation: gameupdate.ReleaseLoadConfirmationLogs, TimeoutSeconds: 300},
		AffectedRoomIDs: []string{"room-one"}, Ready: true, UpdateRequired: true, CreatedAt: time.Now().UTC(),
		Installations: []gameupdate.ReleaseInstallationPlan{{
			TargetID: "agent:node-a", TargetName: "Node A", InstallationID: "primary", CurrentVersion: "700", DesiredVersion: "701",
			Shards: []gameupdate.ReleaseShardPlan{{
				RoomID: "room-one", RoomName: "Room One", WorldID: "Master", WorldName: "Master",
				TargetID: "agent:node-a", InstallationID: "primary", WasRunning: true,
			}},
		}},
	}
}

func successfulHTTPGameRelease(id, sourceJobID string, plan gameupdate.ReleasePlan) gameupdate.Release {
	now := time.Now().UTC()
	return gameupdate.Release{
		ID: id, SourceJobID: sourceJobID, Stage: gameupdate.ReleaseStageSucceeded, Plan: plan,
		Installations: []gameupdate.ReleaseInstallationResult{{
			TargetID: "agent:node-a", InstallationID: "primary", Stage: gameupdate.ReleaseStageSucceeded,
			BeforeVersion: "700", AfterVersion: "701", FinishedAt: &now, UpdatedAt: now,
		}},
		Shards: []gameupdate.ReleaseShardResult{{
			RoomID: "room-one", WorldID: "Master", TargetID: "agent:node-a", InstallationID: "primary",
			IsMaster: true, WasRunning: true, Stage: gameupdate.ReleaseStageSucceeded, RuntimeState: "running", UpdatedAt: now,
		}},
		CreatedAt: now, UpdatedAt: now, FinishedAt: &now,
	}
}

func newGameReleaseHandlerApp(t *testing.T, fixture *gameReleaseHTTPFixture) (*gin.Engine, *jobs.Service) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "game_release_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewGameUpdateHandler(nil, jobService)
	if err := handler.ConfigureReleases(fixture); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	return router, jobService
}

func TestGameReleaseHTTPRequiresCurrentPlanHashConfirmationAndReportsJob(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := &gameReleaseHTTPFixture{plan: gameReleaseHTTPPlan(), releases: make(map[string]gameupdate.Release)}
	router, jobService := newGameReleaseHandlerApp(t, fixture)

	response := performJSON(router, http.MethodPost, "/api/v2/game/releases", map[string]interface{}{
		"planHash": releaseHTTPPlanHash, "confirmation": "wrong",
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)

	response = performJSON(router, http.MethodPost, "/api/v2/game/releases", map[string]interface{}{
		"planHash":     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"confirmation": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, nil, "")
	assertStatus(t, response, http.StatusConflict)

	response = performJSON(router, http.MethodPost, "/api/v2/game/releases", map[string]interface{}{
		"desiredVersion": "701", "planHash": releaseHTTPPlanHash, "confirmation": releaseHTTPPlanHash,
		"policy": map[string]interface{}{"restartRunning": true, "loadConfirmation": "logs", "timeoutSeconds": 300},
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID := responseData(t, response)["id"].(string)
	waitForJobStatus(t, jobService, jobID, jobs.StatusSucceeded)
	job, err := jobService.Get(jobID)
	if err != nil || len(job.Targets) != 2 {
		t.Fatalf("job=%#v error=%v", job, err)
	}
	for _, target := range job.Targets {
		if target.Status != jobs.StatusSucceeded {
			t.Fatalf("target=%#v", target)
		}
	}
	response = performJSON(router, http.MethodGet, "/api/v2/game/releases/"+jobID, nil, nil, "")
	assertStatus(t, response, http.StatusOK)
}

func TestGameReleaseHTTPUsesUpdateTerminologyAndClassifiesSteamLookupFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := &gameReleaseHTTPFixture{previewErr: gameupdate.ErrLatestBuildUnavailable, releases: make(map[string]gameupdate.Release)}
	router, _ := newGameReleaseHandlerApp(t, fixture)
	response := performJSON(router, http.MethodPost, "/api/v2/game/releases/preview", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusServiceUnavailable)
	if body := response.Body.String(); !strings.Contains(body, `"code":"STEAM_BUILD_UNAVAILABLE"`) || strings.Contains(body, "发布") {
		t.Fatalf("unexpected response: %s", body)
	}

	fixture.previewErr = gameupdate.ErrReleaseInvalid
	response = performJSON(router, http.MethodPost, "/api/v2/game/releases/preview", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	if body := response.Body.String(); !strings.Contains(body, "游戏更新请求无效") || strings.Contains(body, "发布") {
		t.Fatalf("unexpected response: %s", body)
	}
}

func TestGameReleaseHTTPRetryRejectsTerminalSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plan := gameReleaseHTTPPlan()
	fixture := &gameReleaseHTTPFixture{plan: plan, releases: map[string]gameupdate.Release{
		"release-success": successfulHTTPGameRelease("release-success", "", plan),
	}}
	router, _ := newGameReleaseHandlerApp(t, fixture)
	response := performJSON(router, http.MethodPost, "/api/v2/game/releases/release-success/actions/retry", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusConflict)
}

func TestGameReleaseHTTPRetryPassesCurrentJobID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plan := gameReleaseHTTPPlan()
	failed := successfulHTTPGameRelease("release-failed", "original-job", plan)
	failed.Stage = gameupdate.ReleaseStageFailed
	fixture := &gameReleaseHTTPFixture{plan: plan, releases: map[string]gameupdate.Release{failed.ID: failed}}
	router, jobService := newGameReleaseHandlerApp(t, fixture)

	response := performJSON(router, http.MethodPost, "/api/v2/game/releases/release-failed/actions/retry", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID := responseData(t, response)["id"].(string)
	waitForJobStatus(t, jobService, jobID, jobs.StatusSucceeded)

	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.retrySourceJobID != jobID {
		t.Fatalf("retry source job=%q want %q", fixture.retrySourceJobID, jobID)
	}
}

func TestGameUpdateRunFallsBackToJobBeforeSteamCMDRunExists(t *testing.T) {
	now := time.Now().UTC()
	job := jobs.Job{ID: "update-job", Kind: "game.update", Status: jobs.StatusQueued, CreatedAt: now}
	value, ok := gameUpdateRunFromJob(job)
	if !ok || value.JobID != job.ID || value.Status != string(jobs.StatusQueued) || !value.StartedAt.Equal(now) {
		t.Fatalf("run=%#v ok=%v", value, ok)
	}
	if _, ok := gameUpdateRunFromJob(jobs.Job{ID: "other", Kind: "room.start"}); ok {
		t.Fatal("non-update job must not be exposed as an update run")
	}
}
