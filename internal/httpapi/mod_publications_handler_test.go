package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/modcontrol"
	"dont/internal/modpublication"
	"dont/internal/operationprogress"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

const publicationTestHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type modPublicationHTTPFixture struct {
	mu            sync.Mutex
	plan          modpublication.Plan
	publications  []modpublication.Publication
	replicas      modpublication.RoomReplicaState
	replicaErr    error
	publishErr    error
	publishResult *modpublication.Publication
	retryErr      error
	retryResult   *modpublication.Publication
}

func (f *modPublicationHTTPFixture) Preview(context.Context, string, modcontrol.Request) (modpublication.Plan, error) {
	return f.plan, nil
}

func (f *modPublicationHTTPFixture) Publish(ctx context.Context, sourceJobID, roomID string, request modcontrol.Request) (modpublication.Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModCache, Percent: 50, Message: "正在同步房间模组"})
	if f.publishResult != nil {
		value := *f.publishResult
		value.SourceJobID, value.RoomID = sourceJobID, roomID
		f.publications = append([]modpublication.Publication{value}, f.publications...)
		return value, f.publishErr
	}
	value := modpublication.Publication{
		ID: "publication-created", SourceJobID: sourceJobID, RoomID: roomID,
		Status: modpublication.StatusSucceeded, Outcome: modpublication.OutcomeFull, Plan: f.plan,
		Targets: []modpublication.TargetResult{{
			TargetID: "local", InstallationID: "default", Status: modpublication.StatusSucceeded,
		}},
	}
	if request.Activation.Mode == modpublication.ActivationModeRestart {
		value.Activation = successfulHTTPActivation(f.plan, request.Activation)
	}
	if f.publishErr != nil {
		value.Status, value.Outcome = modpublication.StatusFailed, modpublication.OutcomeNone
		value.Targets[0].Status = modpublication.StatusFailed
		value.Targets[0].ErrorCode, value.Targets[0].ErrorMessage = "PUBLISH_FAILED", f.publishErr.Error()
	}
	f.publications = append([]modpublication.Publication{value}, f.publications...)
	return value, f.publishErr
}

func successfulHTTPActivation(plan modpublication.Plan, policy modpublication.ActivationPolicy) modpublication.Activation {
	now := time.Now().UTC()
	value := modpublication.Activation{Policy: policy, Status: modpublication.ActivationStatusSucceeded, RequestedAt: &now, FinishedAt: &now}
	for _, target := range plan.Targets {
		for _, world := range target.Worlds {
			value.Shards = append(value.Shards, modpublication.ShardActivationResult{
				RoomID: world.RoomID, WorldID: world.WorldID, TargetID: target.TargetID, InstallationID: target.InstallationID,
				Status: modpublication.ActivationStatusSucceeded, WasRunning: true, RuntimeState: "running",
				LoadMarker: "dst-admin-runtime-ready", RestartedAt: &now, LoadConfirmedAt: &now, UpdatedAt: now,
			})
		}
	}
	return value
}

func (f *modPublicationHTTPFixture) Retry(_ context.Context, sourceJobID, publicationID string) (modpublication.Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retryResult != nil {
		value := *f.retryResult
		value.SourceJobID = sourceJobID
		f.publications = append([]modpublication.Publication{value}, f.publications...)
		return value, f.retryErr
	}
	value := modpublication.Publication{
		ID: publicationID + "-retry", SourceJobID: sourceJobID, RoomID: "room-one",
		Status: modpublication.StatusSucceeded, Outcome: modpublication.OutcomeFull, Plan: f.plan,
		Targets: []modpublication.TargetResult{{TargetID: "local", InstallationID: "default", Status: modpublication.StatusSucceeded}},
	}
	if f.retryErr != nil {
		value.Status, value.Outcome = modpublication.StatusRecoveryRequired, modpublication.OutcomePartial
	}
	f.publications = append([]modpublication.Publication{value}, f.publications...)
	return value, f.retryErr
}

func (f *modPublicationHTTPFixture) Activate(_ context.Context, sourceJobID, publicationID string, policy modpublication.ActivationPolicy) (modpublication.Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, current := range f.publications {
		if current.ID != publicationID {
			continue
		}
		current.SourceJobID = sourceJobID
		current.Activation = modpublication.Activation{Policy: policy, Status: modpublication.ActivationStatusSucceeded}
		now := time.Now().UTC()
		for _, target := range current.Plan.Targets {
			for _, world := range target.Worlds {
				current.Activation.Shards = append(current.Activation.Shards, modpublication.ShardActivationResult{
					RoomID: world.RoomID, WorldID: world.WorldID, TargetID: target.TargetID, InstallationID: target.InstallationID,
					Status: modpublication.ActivationStatusSucceeded, WasRunning: true, UpdatedAt: now,
				})
			}
		}
		return current, nil
	}
	return modpublication.Publication{}, modpublication.ErrNotFound
}

func (f *modPublicationHTTPFixture) Get(publicationID string) (modpublication.Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, value := range f.publications {
		if value.ID == publicationID {
			return value, nil
		}
	}
	return modpublication.Publication{}, modpublication.ErrNotFound
}

func (f *modPublicationHTTPFixture) List(_ string, limit, offset int) (modcontrol.ListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := offset
	if start > len(f.publications) {
		start = len(f.publications)
	}
	end := start + limit
	if end > len(f.publications) {
		end = len(f.publications)
	}
	items := append([]modpublication.Publication(nil), f.publications[start:end]...)
	return modcontrol.ListResult{Items: items, Total: len(f.publications), Limit: limit, Offset: offset}, nil
}

func (f *modPublicationHTTPFixture) RoomReplicas(roomID string) (modpublication.RoomReplicaState, error) {
	if f.replicaErr != nil {
		return modpublication.RoomReplicaState{}, f.replicaErr
	}
	value := f.replicas
	if value.RoomID == "" {
		value.RoomID = roomID
	}
	if value.Items == nil {
		value.Items = []modpublication.ReplicaModState{}
	}
	return value, nil
}

func newModPublicationHandlerApp(t *testing.T, fixture *modPublicationHTTPFixture) (*gin.Engine, *jobs.Service) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "mod_publication_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewModPublicationHandler(fixture, jobService)
	if err != nil {
		t.Fatal(err)
	}
	handler.ConfigureReplicaReader(fixture)
	router := gin.New()
	handler.Register(router.Group("/api/v2"))
	return router, jobService
}

func TestModPublicationHTTPReturnsPlacementAwareReplicaCoverage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := &modPublicationHTTPFixture{
		plan: publicationHTTPPlan(),
		replicas: modpublication.RoomReplicaState{
			RoomID: "room-one", DesiredRevision: publicationTestHash,
			Coverage: modpublication.ReplicaCoverage{ReadyTargets: 1, TotalTargets: 2, ConfiguredWorlds: 1, TotalWorlds: 2},
			Items: []modpublication.ReplicaModState{{
				WorkshopID: "1392778117", ReadyTargets: 1, TotalTargets: 2, ConfiguredWorlds: 1, TotalWorlds: 2,
				Targets: []modpublication.ReplicaTargetState{{
					TargetID: "agent:node-one", InstallationID: "native", TreeSHA256: strings.Repeat("a", 64),
					Cached: true, Published: true, Ready: true,
					Worlds: []modpublication.ReplicaWorldState{{RoomID: "room-one", WorldID: "master", Configured: true}},
				}},
			}},
		},
	}
	router, _ := newModPublicationHandlerApp(t, fixture)

	response := performJSON(router, http.MethodGet, "/api/v2/rooms/room-one/mod-replicas", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	data := responseData(t, response)
	if data["roomId"] != "room-one" || data["desiredRevision"] != publicationTestHash {
		t.Fatalf("unexpected replica envelope: %s", response.Body.String())
	}
	coverage, _ := data["coverage"].(map[string]interface{})
	items, _ := data["items"].([]interface{})
	if coverage["readyTargets"] != float64(1) || coverage["totalTargets"] != float64(2) || len(items) != 1 {
		t.Fatalf("replica coverage was not preserved: %s", response.Body.String())
	}
}

func TestModPublicationHTTPReplicaReadFailureIsVisible(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := &modPublicationHTTPFixture{plan: publicationHTTPPlan(), replicaErr: modpublication.ErrInvalidInput}
	router, _ := newModPublicationHandlerApp(t, fixture)

	response := performJSON(router, http.MethodGet, "/api/v2/rooms/%20/mod-replicas", nil, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_MOD_PUBLICATION")
}

func publicationHTTPPlan() modpublication.Plan {
	return modpublication.Plan{
		Version: 1, RoomID: "room-one", TopologyRevision: "topology-one", PlanHash: publicationTestHash,
		Ready: true, RestartRequired: true, CreatedAt: time.Now().UTC(),
		Targets: []modpublication.TargetPlan{{
			TargetID: "local", NodeID: "local", InstallationID: "default",
			Worlds: []modpublication.WorldPlan{{RoomID: "room-one", RoomDirectory: "Cluster_1", WorldID: "master", WorldDirectory: "Master", IsMaster: true}},
		}},
	}
}

func TestModPublicationHTTPRestartActivationAndManualRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := &modPublicationHTTPFixture{plan: publicationHTTPPlan()}
	router, jobService := newModPublicationHandlerApp(t, fixture)

	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room-one/mod-publications", map[string]interface{}{
		"action": "reconcile", "planHash": publicationTestHash, "confirmation": publicationTestHash,
		"activation": map[string]interface{}{"mode": "restart", "loadConfirmation": "logs", "timeoutSeconds": 30},
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Status != jobs.StatusSucceeded || len(job.Targets) != 2 {
		t.Fatalf("restart activation targets were not reported: %#v", job)
	}
	for _, target := range job.Targets {
		if target.Status != jobs.StatusSucceeded {
			t.Fatalf("restart activation target failed: %#v", job.Targets)
		}
	}

	response = performJSON(router, http.MethodPost, "/api/v2/mod-publications/publication-created/actions/activate", map[string]interface{}{
		"mode": "restart", "loadConfirmation": "none", "timeoutSeconds": 30,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job = waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Kind != "mod.publication.activation" || job.Status != jobs.StatusSucceeded || len(job.Targets) != 1 {
		t.Fatalf("manual activation retry was not represented as a shard Job: %#v", job)
	}
}

func TestModPublicationHTTPPreviewConfirmationAndJobResolution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := &modPublicationHTTPFixture{plan: publicationHTTPPlan()}
	router, jobService := newModPublicationHandlerApp(t, fixture)

	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room-one/mod-publications/preview", map[string]interface{}{
		"action": "reconcile",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["planHash"] != publicationTestHash {
		t.Fatalf("unexpected preview: %s", response.Body.String())
	}

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room-one/mod-publications", map[string]interface{}{
		"action": "reconcile", "planHash": publicationTestHash,
	}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED")

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room-one/mod-publications", map[string]interface{}{
		"action": "reconcile", "planHash": publicationTestHash, "confirmation": "wrong-confirmation",
	}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED")

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room-one/mod-publications", map[string]interface{}{
		"action": "reconcile", "planHash": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "confirmation": publicationTestHash,
	}, nil, "")
	assertAPIError(t, response, http.StatusConflict, "PLAN_CHANGED")

	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room-one/mod-publications", map[string]interface{}{
		"action": "reconcile", "planHash": publicationTestHash, "confirmation": publicationTestHash,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	jobID, _ := responseData(t, response)["id"].(string)
	job := waitForModJob(t, jobService, jobID)
	if job.Kind != "mod.publication" || job.RoomID != "room-one" || job.Status != jobs.StatusSucceeded {
		t.Fatalf("unexpected publication job: %#v", job)
	}
	assertModJobProgressEvent(t, jobService, job.ID, 37, "正在同步房间模组")

	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room-one/mod-publications?limit=10&offset=0", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	data := responseData(t, response)
	items, _ := data["items"].([]interface{})
	if len(items) != 1 || items[0].(map[string]interface{})["sourceJobId"] != jobID {
		t.Fatalf("publication was not resolved by source job: %s", response.Body.String())
	}

	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room-one/mod-publications?limit=0", nil, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_PAGINATION")
}

func TestModPublicationHTTPRejectsBlockedPreview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plan := publicationHTTPPlan()
	plan.Ready = false
	plan.Blockers = []modpublication.Blocker{{Code: "AGENT_OFFLINE", Message: "Agent is offline"}}
	fixture := &modPublicationHTTPFixture{plan: plan}
	router, _ := newModPublicationHandlerApp(t, fixture)

	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room-one/mod-publications", map[string]interface{}{
		"action": "reconcile", "planHash": publicationTestHash, "confirmation": publicationTestHash,
	}, nil, "")
	assertAPIError(t, response, http.StatusConflict, "PREVIEW_BLOCKED")
}

func TestModPublicationHTTPFailureAndRetryJobs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := &modPublicationHTTPFixture{
		plan:       publicationHTTPPlan(),
		publishErr: errors.New("node disconnected"),
		publications: []modpublication.Publication{{
			ID: "publication-failed", RoomID: "room-one", Status: modpublication.StatusFailed,
			Outcome: modpublication.OutcomeNone, Plan: publicationHTTPPlan(),
		}},
	}
	router, jobService := newModPublicationHandlerApp(t, fixture)

	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room-one/mod-publications", map[string]interface{}{
		"action": "reconcile", "planHash": publicationTestHash, "confirmation": publicationTestHash,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Status != jobs.StatusFailed || job.Outcome != jobs.OutcomeNone || len(job.Targets) != 1 || job.Targets[0].Error == nil || job.Targets[0].Error.Code != "PUBLISH_FAILED" {
		t.Fatalf("unexpected failed publication job: %#v", job)
	}

	response = performJSON(router, http.MethodPost, "/api/v2/mod-publications/publication-failed/actions/retry-failed", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job = waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Kind != "mod.publication.retry" || job.Status != jobs.StatusSucceeded {
		t.Fatalf("unexpected retry job: %#v", job)
	}
}

func TestModPublicationHTTPInitialRollbackRemainsFailed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plan := publicationHTTPPlan()
	fixture := &modPublicationHTTPFixture{
		plan:       plan,
		publishErr: errors.New("node disconnected during publish"),
		publishResult: &modpublication.Publication{
			ID: "publication-rolled-back", Status: modpublication.StatusRolledBack,
			Outcome: modpublication.OutcomeNone, Plan: plan,
			ErrorCode: "TARGET_PUBLISH_FAILED", ErrorMessage: "node disconnected during publish",
			Targets: []modpublication.TargetResult{{
				TargetID: "local", InstallationID: "default", Status: modpublication.StatusRolledBack, RolledBack: true,
			}},
		},
	}
	router, jobService := newModPublicationHandlerApp(t, fixture)

	response := performJSON(router, http.MethodPost, "/api/v2/rooms/room-one/mod-publications", map[string]interface{}{
		"action": "reconcile", "planHash": publicationTestHash, "confirmation": publicationTestHash,
	}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Status != jobs.StatusFailed || job.Outcome != jobs.OutcomeNone || len(job.Targets) != 1 ||
		job.Targets[0].Status != jobs.StatusFailed || job.Targets[0].Error == nil ||
		job.Targets[0].Error.Code != "TARGET_PUBLISH_FAILED" {
		t.Fatalf("rolled-back initial publication was reported as successful: %#v", job)
	}
}

func TestModPublicationHTTPRetryReportsEachTargetResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plan := publicationHTTPPlan()
	plan.Targets = append(plan.Targets, modpublication.TargetPlan{TargetID: "node-two", NodeID: "node-two", InstallationID: "shared"})
	fixture := &modPublicationHTTPFixture{
		plan: plan,
		publications: []modpublication.Publication{{
			ID: "publication-partial", RoomID: "room-one", Status: modpublication.StatusRecoveryRequired,
			Outcome: modpublication.OutcomePartial, Plan: plan,
		}},
		retryErr: errors.New("node two remained offline"),
		retryResult: &modpublication.Publication{
			ID: "publication-partial", RoomID: "room-one", Status: modpublication.StatusRecoveryRequired,
			Outcome: modpublication.OutcomePartial, Plan: plan,
			Targets: []modpublication.TargetResult{
				{TargetID: "local", InstallationID: "default", Status: modpublication.StatusSucceeded},
				{TargetID: "node-two", InstallationID: "shared", Status: modpublication.StatusRecoveryRequired, ErrorCode: "AGENT_OFFLINE", ErrorMessage: "Agent is offline"},
			},
		},
	}
	router, jobService := newModPublicationHandlerApp(t, fixture)

	response := performJSON(router, http.MethodPost, "/api/v2/mod-publications/publication-partial/actions/retry-failed", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Status != jobs.StatusFailed || job.Outcome != jobs.OutcomePartial || len(job.Targets) != 2 {
		t.Fatalf("unexpected retry job: %#v", job)
	}
	if job.Targets[0].Status != jobs.StatusSucceeded || job.Targets[1].Status != jobs.StatusFailed ||
		job.Targets[1].Error == nil || job.Targets[1].Error.Code != "AGENT_OFFLINE" || job.Targets[1].Error.Message != "Agent is offline" {
		t.Fatalf("retry targets did not preserve publication results: %#v", job.Targets)
	}
}

func TestModPublicationHTTPRecoveryReportsSuccessfulRollback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plan := publicationHTTPPlan()
	fixture := &modPublicationHTTPFixture{
		plan: plan,
		publications: []modpublication.Publication{{
			ID: "publication-recovery", RoomID: "room-one", Status: modpublication.StatusRecoveryRequired,
			Outcome: modpublication.OutcomePartial, Plan: plan,
		}},
		retryResult: &modpublication.Publication{
			ID: "publication-recovery", RoomID: "room-one", Status: modpublication.StatusRolledBack,
			Outcome: modpublication.OutcomeNone, Plan: plan,
			Targets: []modpublication.TargetResult{{
				TargetID: "local", InstallationID: "default", Status: modpublication.StatusRolledBack, RolledBack: true,
			}},
		},
	}
	router, jobService := newModPublicationHandlerApp(t, fixture)

	response := performJSON(router, http.MethodPost, "/api/v2/mod-publications/publication-recovery/actions/retry-failed", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Status != jobs.StatusSucceeded || job.Outcome != jobs.OutcomeFull || len(job.Targets) != 1 || job.Targets[0].Status != jobs.StatusSucceeded {
		t.Fatalf("successful rollback was reported as a failed recovery: %#v", job)
	}
}

func TestPublicationJobErrorPreservesActionableCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "topology", err: modcontrol.ErrTopologyChanged, code: "TOPOLOGY_CHANGED"},
		{name: "plan", err: modpublication.ErrPlanChanged, code: "PLAN_CHANGED"},
		{name: "blocked", err: modpublication.ErrPreviewBlocked, code: "PREVIEW_BLOCKED"},
		{name: "recovery", err: modpublication.ErrRecoveryRequired, code: "MOD_PUBLICATION_RECOVERY_REQUIRED"},
		{name: "idempotency", err: modpublication.ErrIdempotencyConflict, code: "MOD_PUBLICATION_IDEMPOTENCY_CONFLICT"},
		{name: "unknown", err: errors.New("node disconnected"), code: "MOD_PUBLICATION_FAILED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := publicationJobError(test.err)
			if result.Code != test.code || result.Message != test.err.Error() {
				t.Fatalf("unexpected job error: %#v", result)
			}
		})
	}
}

func TestPublicationJobTargetCannotCollideOnDelimiters(t *testing.T) {
	first := publicationJobTarget("agent:a:b", "c")
	second := publicationJobTarget("agent:a", "b:c")
	if first == second || len(first) != len("mod-target-")+64 || len(second) != len("mod-target-")+64 {
		t.Fatalf("publication Job target identity is ambiguous: %q %q", first, second)
	}
}

func TestModPublicationHTTPDoesNotHideGlobalRetryFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	plan := publicationHTTPPlan()
	plan.Targets = append(plan.Targets, modpublication.TargetPlan{TargetID: "node-two", NodeID: "node-two", InstallationID: "shared"})
	fixture := &modPublicationHTTPFixture{
		plan: plan,
		publications: []modpublication.Publication{{
			ID: "publication-global-failure", RoomID: "room-one", Status: modpublication.StatusRecoveryRequired,
			Outcome: modpublication.OutcomeFull, Plan: plan,
		}},
		retryErr: modcontrol.ErrTopologyChanged,
		retryResult: &modpublication.Publication{
			ID: "publication-global-failure", RoomID: "room-one", Status: modpublication.StatusRecoveryRequired,
			Outcome: modpublication.OutcomeFull, Plan: plan,
			Targets: []modpublication.TargetResult{
				{TargetID: "local", InstallationID: "default", Status: modpublication.StatusSucceeded},
				{TargetID: "node-two", InstallationID: "shared", Status: modpublication.StatusSucceeded},
			},
		},
	}
	router, jobService := newModPublicationHandlerApp(t, fixture)

	response := performJSON(router, http.MethodPost, "/api/v2/mod-publications/publication-global-failure/actions/retry-failed", nil, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForModJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Status != jobs.StatusFailed || job.Outcome != jobs.OutcomeNone || len(job.Targets) != 2 {
		t.Fatalf("global retry failure was hidden: %#v", job)
	}
	for _, target := range job.Targets {
		if target.Status != jobs.StatusFailed || target.Error == nil || target.Error.Code != "TOPOLOGY_CHANGED" {
			t.Fatalf("global retry error was not preserved: %#v", job.Targets)
		}
	}
}
