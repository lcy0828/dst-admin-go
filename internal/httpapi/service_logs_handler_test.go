package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/jobs"
	"dont/internal/servicelogs"
	"dont/shared"
	"github.com/gin-gonic/gin"
)

type testServiceLogNodes struct {
	result shared.ServiceLogResult
	err    error
	calls  int
}

func (n *testServiceLogNodes) ServiceLogSources() ([]shared.ServiceLogSource, error) {
	return []shared.ServiceLogSource{{ID: "agent:node", Name: "Node", Online: true, Supported: true}}, nil
}
func (n *testServiceLogNodes) ReadServiceLogs(_ context.Context, _ string, _ shared.ServiceLogRequest) (shared.ServiceLogResult, error) {
	n.calls++
	return n.result, n.err
}

type testServiceLogJobs struct{ job jobs.Job }

func (s testServiceLogJobs) Get(string) (jobs.Job, error) { return s.job, nil }

func TestServiceLogHTTPQuerySettingsDiagnosticsAndRemoteErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	store, err := servicelogs.Open(servicelogs.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	store.Append(shared.ServiceLogEntry{Message: `password="do-not-return"`, JobID: "failed-job"})
	store.Close()
	nodes := &testServiceLogNodes{}
	router := gin.New()
	router.Use(RequestContext())
	NewServiceLogsHandler(root, nodes, testServiceLogJobs{jobs.Job{ID: "failed-job", Kind: "backup.create", Status: jobs.StatusFailed}}).Register(router.Group("/api/v2"))
	for _, test := range []struct {
		path   string
		status int
	}{{"", 200}, {"/sources", 200}, {"/settings", 200}, {"/export", 200}, {"?limit=501", 422}, {"/context?radius=90", 422}, {"/context?entryId=../outside", 422}, {"/diagnostics", 422}, {"/diagnostics?jobId=a&requestId=b", 422}, {"/diagnostics?requestId=unknown", 200}} {
		out := httptest.NewRecorder()
		router.ServeHTTP(out, httptest.NewRequest("GET", "/api/v2/system/service-logs"+test.path, nil))
		if out.Code != test.status {
			t.Fatalf("%s: %d %s", test.path, out.Code, out.Body.String())
		}
		if strings.Contains(out.Body.String(), "do-not-return") {
			t.Fatal("secret leaked")
		}
		if out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cache enabled")
		}
	}
	for _, test := range []struct {
		err    error
		status int
	}{{agents.ErrAgentOffline, 503}, {agents.ErrUnsupportedAction, 409}, {agents.ErrAgentNotFound, 404}, {servicelogs.ErrCursorExpired, 410}, {servicelogs.ErrConflict, 409}, {errors.New("internal"), 503}} {
		nodes.err = test.err
		out := httptest.NewRecorder()
		router.ServeHTTP(out, httptest.NewRequest("GET", "/api/v2/system/service-logs?sourceId=agent:node", nil))
		if out.Code != test.status {
			t.Fatal(test.err, out.Code, out.Body.String())
		}
	}
	nodes.err = nil
	nodes.result = shared.ServiceLogResult{Page: &shared.ServiceLogPage{Items: []shared.ServiceLogEntry{{Message: `{"Authorization":"Bearer remote-secret"}`}}}}
	out := httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest("GET", "/api/v2/system/service-logs?sourceId=agent:node", nil))
	if out.Code != 200 || strings.Contains(out.Body.String(), "remote-secret") {
		t.Fatal(out.Code, out.Body.String())
	}
}
func TestServiceLogsSettingsSaveRejectsStaleRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewServiceLogsHandler(t.TempDir(), nil, nil).Register(router.Group("/api/v2"))
	payload, _ := json.Marshal(servicelogs.DefaultSettings())
	for _, status := range []int{200, 409} {
		out := httptest.NewRecorder()
		router.ServeHTTP(out, httptest.NewRequest("PUT", "/api/v2/system/service-logs/settings", strings.NewReader(string(payload))))
		if out.Code != status {
			t.Fatal(out.Code, out.Body.String())
		}
	}
}

func TestTemporaryDebugAndPanicLogsPreserveRequestCorrelation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	settings := servicelogs.DefaultSettings()
	until := time.Now().Add(time.Minute)
	settings.DebugUntil = &until
	if _, err := servicelogs.SaveSettings(root, settings); err != nil {
		t.Fatal(err)
	}
	closeLogs := servicelogs.Start(servicelogs.Config{Root: root, Console: io.Discard}, false)
	router := gin.New()
	router.Use(RequestContext(), Recovery())
	router.GET("/api/v2/debug-check", func(c *gin.Context) { c.Status(200) })
	router.GET("/api/v2/crash-check", func(c *gin.Context) { panic(`panic api_key="private-panic-secret"`) })
	for _, path := range []string{"debug-check", "crash-check"} {
		request := httptest.NewRequest("GET", "/api/v2/"+path, nil)
		request.Header.Set("X-Request-ID", path)
		router.ServeHTTP(httptest.NewRecorder(), request)
	}
	closeLogs()
	page, err := servicelogs.Query(context.Background(), root, shared.ServiceLogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range page.Items {
		if strings.Contains(entry.Message+entry.Stack, "private-panic-secret") {
			t.Fatal("panic leaked secret")
		}
		if entry.RequestID == "debug-check" && entry.Level == "debug" {
			seen["debug"] = true
		}
		if entry.RequestID == "crash-check" && entry.Code == "INTERNAL_ERROR" && entry.Stack != "" {
			seen["crash"] = true
		}
	}
	if !seen["debug"] || !seen["crash"] {
		t.Fatal(seen)
	}
	if servicelogs.DebugEnabled() {
		t.Fatal("debug logging remained active after shutdown")
	}
}
