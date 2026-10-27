package httpapi

import (
	"context"
	"errors"
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

type installedVersionsHTTPFixture struct {
	targetIDs []string
}

func (f *installedVersionsHTTPFixture) Installed(_ context.Context, ids []string) ([]gameupdate.InstalledVersion, error) {
	f.targetIDs = ids
	return []gameupdate.InstalledVersion{{TargetID: "agent:node-a", InstallationID: "native", Online: true, Error: "读取游戏版本失败: context deadline exceeded"}}, nil
}

func TestInstalledVersionsHTTPReturnsScopedReadErrorsWithoutPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := &installedVersionsHTTPFixture{}
	handler := NewGameUpdateHandler(nil, nil)
	handler.ConfigureInstalledVersions(fixture)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	response := performJSON(router, http.MethodGet, "/api/v2/game/installed-versions?targetIds=agent:node-a,local", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if strings.Join(fixture.targetIDs, ",") != "agent:node-a,local" || !strings.Contains(response.Body.String(), "context deadline exceeded") {
		t.Fatalf("targets=%v response=%s", fixture.targetIDs, response.Body.String())
	}
}

type gameReleaseHTTPFixture struct {
	block            <-chan struct{}
	previewCalls     int
	retryRequest     gameupdate.ReleaseRetryRequest
	mu               sync.Mutex
	plan             gameupdate.ReleasePlan
	previewErr       error
	releases         map[string]gameupdate.Release
	retrySourceJobID string
	previewRequest   gameupdate.ReleasePreviewRequest
}

func (f *gameReleaseHTTPFixture) Preview(_ context.Context, request gameupdate.ReleasePreviewRequest) (gameupdate.ReleasePlan, error) {
	f.mu.Lock()
	f.previewCalls++
	f.previewRequest = request
	f.mu.Unlock()
	return f.plan, f.previewErr
}

func (f *gameReleaseHTTPFixture) Publish(ctx context.Context, request gameupdate.ReleasePublishRequest) (gameupdate.Release, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return gameupdate.Release{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	value := successfulHTTPGameRelease(request.ID, request.SourceJobID, request.Plan)
	f.releases[value.ID] = value
	return value, nil
}

func (f *gameReleaseHTTPFixture) Retry(ctx context.Context, id string, sourceJobIDs ...string) (gameupdate.Release, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return gameupdate.Release{}, ctx.Err()
		}
	}
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
		"targetIds": []string{"agent:node-a"},
		"policy":    map[string]interface{}{"restartRunning": true, "loadConfirmation": "logs", "timeoutSeconds": 300},
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
	fixture.mu.Lock()
	if len(fixture.previewRequest.TargetIDs) != 1 || fixture.previewRequest.TargetIDs[0] != "agent:node-a" {
		t.Fatalf("preview request=%#v", fixture.previewRequest)
	}
	fixture.mu.Unlock()
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

	fixture.previewErr = errors.Join(gameupdate.ErrLatestBuildUnavailable, context.DeadlineExceeded)
	response = performJSON(router, http.MethodPost, "/api/v2/game/releases/preview", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusGatewayTimeout)
	if body := response.Body.String(); !strings.Contains(body, `"code":"GAME_RELEASE_PREVIEW_TIMEOUT"`) || strings.Contains(body, "无法连接") {
		t.Fatalf("unexpected timeout response: %s", body)
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

func (f *gameReleaseHTTPFixture) RetryConfirmed(ctx context.Context, id string, request gameupdate.ReleaseRetryRequest, jobID string) (gameupdate.Release, error) {
	f.mu.Lock()
	f.retryRequest = request
	f.mu.Unlock()
	return f.Retry(ctx, id, jobID)
}

func TestGameReleaseHTTPActiveDuplicateReusesJobWithoutRepreview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	block := make(chan struct{})
	fixture := &gameReleaseHTTPFixture{plan: gameReleaseHTTPPlan(), releases: make(map[string]gameupdate.Release), block: block}
	router, jobsService := newGameReleaseHandlerApp(t, fixture)
	request := map[string]interface{}{"planHash": releaseHTTPPlanHash, "confirmation": releaseHTTPPlanHash}
	first := performJSON(router, http.MethodPost, "/api/v2/game/releases", request, nil, "")
	assertStatus(t, first, http.StatusAccepted)
	id := responseData(t, first)["id"].(string)
	defer func() { close(block); waitForJobStatus(t, jobsService, id, jobs.StatusSucceeded) }()
	second := performJSON(router, http.MethodPost, "/api/v2/game/releases", request, nil, "")
	assertStatus(t, second, http.StatusAccepted)
	if responseData(t, second)["id"] != id {
		t.Fatal("duplicate update enqueued a second job")
	}
	fixture.mu.Lock()
	calls := fixture.previewCalls
	fixture.mu.Unlock()
	if calls != 1 {
		t.Fatalf("duplicate repreviewed after execution began: %d", calls)
	}
	invalid := performJSON(router, http.MethodPost, "/api/v2/game/releases", map[string]interface{}{"planHash": releaseHTTPPlanHash, "confirmation": "wrong"}, nil, "")
	assertStatus(t, invalid, http.StatusUnprocessableEntity)
}

func TestGameReleaseHTTPDuplicateConfirmedRetryReusesJob(t *testing.T) {
	gin.SetMode(gin.TestMode)
	block := make(chan struct{})
	failed := successfulHTTPGameRelease("recover-one", "old-job", gameReleaseHTTPPlan())
	failed.Stage = gameupdate.ReleaseStageRecoveryRequired
	fixture := &gameReleaseHTTPFixture{plan: failed.Plan, releases: map[string]gameupdate.Release{failed.ID: failed}, block: block}
	router, jobsService := newGameReleaseHandlerApp(t, fixture)
	path := "/api/v2/game/releases/recover-one/actions/retry"
	request := map[string]interface{}{"planHash": releaseHTTPPlanHash, "confirmation": releaseHTTPPlanHash, "validateUnknown": true}
	first := performJSON(router, http.MethodPost, path, request, nil, "")
	assertStatus(t, first, http.StatusAccepted)
	id := responseData(t, first)["id"].(string)
	defer func() {
		close(block)
		waitForJobStatus(t, jobsService, id, jobs.StatusSucceeded)
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if !fixture.retryRequest.ValidateUnknown || fixture.retryRequest.PlanHash != releaseHTTPPlanHash {
			t.Fatalf("confirmation not forwarded: %+v", fixture.retryRequest)
		}
	}()
	second := performJSON(router, http.MethodPost, path, request, nil, "")
	assertStatus(t, second, http.StatusAccepted)
	if responseData(t, second)["id"] != id {
		t.Fatal("duplicate retry enqueued a second job")
	}
	different := performJSON(router, http.MethodPost, path, map[string]interface{}{}, nil, "")
	assertStatus(t, different, http.StatusConflict)
	invalid := performJSON(router, http.MethodPost, path, map[string]interface{}{"validateUnknown": true}, nil, "")
	assertStatus(t, invalid, http.StatusUnprocessableEntity)
}

func TestGameReleaseReportUsesActualValidatedBuildAfterRepreview(t *testing.T) {
	plan := gameReleaseHTTPPlan()
	value := successfulHTTPGameRelease("release", "job", plan)
	value.Installations[0].ExpectedVersion, value.Installations[0].AfterVersion = "702", "702"
	var results []jobs.TargetResult
	gameReleaseReport(func(result jobs.TargetResult) { results = append(results, result) }, plan, value, nil)
	if !strings.Contains(results[0].Message, "702") || strings.Contains(results[0].Message, "701") {
		t.Fatalf("reported old preview build: %+v", results[0])
	}
}
