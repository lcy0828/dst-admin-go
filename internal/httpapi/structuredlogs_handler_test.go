package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/structuredlogs"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type structuredLogHandlerService struct{}

func (structuredLogHandlerService) List(string, structuredlogs.ListFilter) (structuredlogs.List, error) {
	return structuredlogs.List{Items: []structuredlogs.Entry{}, Counts: map[structuredlogs.LogType]int{}, Limit: 50}, nil
}
func (structuredLogHandlerService) WorldTargets(string) ([]rooms.World, error) {
	return []rooms.World{{ID: "master", Name: "Master"}}, nil
}
func (structuredLogHandlerService) RefreshWorld(context.Context, string, string) (structuredlogs.RefreshResult, error) {
	return structuredlogs.RefreshResult{Count: 2, Message: "已解析 2 行日志"}, nil
}
func (structuredLogHandlerService) ClearWorld(_, worldID string) (structuredlogs.ClearResult, error) {
	return structuredlogs.ClearResult{RoomID: "room", WorldID: worldID, Deleted: 2}, nil
}
func (structuredLogHandlerService) Rules(string) ([]structuredlogs.Rule, error) {
	return []structuredlogs.Rule{{ID: "rule", Name: "规则", LogType: structuredlogs.TypeSystem}}, nil
}
func (structuredLogHandlerService) CreateRule(_ string, input structuredlogs.RuleInput) (structuredlogs.Rule, error) {
	if input.Pattern == "" {
		return structuredlogs.Rule{}, &structuredlogs.FieldError{Fields: map[string]string{"pattern": "必填"}}
	}
	return structuredlogs.Rule{ID: "created", Name: input.Name, Pattern: input.Pattern}, nil
}
func (structuredLogHandlerService) UpdateRule(_ string, id string, input structuredlogs.RuleInput) (structuredlogs.Rule, error) {
	return structuredlogs.Rule{ID: id, Name: input.Name, Pattern: input.Pattern}, nil
}
func (structuredLogHandlerService) DeleteRule(_, id string) error {
	if id == "builtin" {
		return structuredlogs.ErrBuiltInRule
	}
	return nil
}
func (structuredLogHandlerService) TestRule(string, structuredlogs.RuleTestInput) (structuredlogs.RuleTestResult, error) {
	return structuredlogs.RuleTestResult{Matched: true}, nil
}
func (structuredLogHandlerService) PreviewRuleMigration(string) (structuredlogs.RuleMigrationPreview, error) {
	return structuredlogs.RuleMigrationPreview{SourceAvailable: true, Total: 2, Ready: 1, Skipped: 1}, nil
}
func (structuredLogHandlerService) MigrateLegacyRules(string) (structuredlogs.RuleMigrationResult, error) {
	return structuredlogs.RuleMigrationResult{Imported: 1}, nil
}

func newStructuredLogHandlerApp(t *testing.T) (*gin.Engine, *jobs.Service) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "structured_http_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	v2 := router.Group("/api/v2")
	NewStructuredLogHandler(structuredLogHandlerService{}, jobService).Register(v2)
	NewJobHandler(jobService).Register(v2)
	return router, jobService
}

func TestStructuredLogHTTPReadRulesAndRefreshJob(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, jobService := newStructuredLogHandlerApp(t)
	response := performJSON(router, http.MethodGet, "/api/v2/rooms/room/structured-logs", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/log-rules", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/log-rules/migration-preview", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["ready"].(float64) != 1 {
		t.Fatalf("unexpected migration preview: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/log-rules/actions/migrate-legacy", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["imported"].(float64) != 1 {
		t.Fatalf("unexpected migration result: %s", response.Body.String())
	}
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/log-rules", map[string]interface{}{"name": "missing"}, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_LOG_RULE")
	response = performJSON(router, http.MethodDelete, "/api/v2/rooms/room/log-rules/builtin", nil, nil, "")
	assertAPIError(t, response, http.StatusConflict, "BUILTIN_LOG_RULE")
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/structured-logs/actions/refresh", map[string]interface{}{}, nil, "")
	assertStatus(t, response, http.StatusAccepted)
	job := waitForStructuredLogJob(t, jobService, responseData(t, response)["id"].(string))
	if job.Kind != "log.structured.refresh" || job.Outcome != jobs.OutcomeFull {
		t.Fatalf("unexpected structured log job: %#v", job)
	}
	response = performJSON(router, http.MethodPost, "/api/v2/rooms/room/structured-logs/actions/clear", map[string]interface{}{"worldId": "master"}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if responseData(t, response)["deleted"].(float64) != 2 {
		t.Fatalf("unexpected clear response: %s", response.Body.String())
	}
}

func waitForStructuredLogJob(t *testing.T, service *jobs.Service, jobID string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(jobID)
		if err == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed || job.Status == jobs.StatusCanceled) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("structured log job %q did not finish", jobID)
	return jobs.Job{}
}
