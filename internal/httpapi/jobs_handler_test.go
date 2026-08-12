package httpapi

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"dont/internal/jobs"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func TestJobEventsStartAtCurrentWatermarkAndResumeFromLastEventID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	store := jobs.NewStore(db, "sse_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	service, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Submit("system.refresh", "", "", []jobs.TargetSpec{{ID: "local", Name: "当前节点"}}, func(_ context.Context, report func(jobs.TargetResult)) error {
		report(jobs.TargetResult{TargetID: "local", Status: jobs.StatusSucceeded, Message: "刷新完成"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForJobStatus(t, service, job.ID, jobs.StatusSucceeded)

	events, err := service.EventsAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("event count = %d, expected at least 2", len(events))
	}
	resumeAfter := events[len(events)-2].ID
	expectedID := events[len(events)-1].ID

	router := gin.New()
	NewJobHandler(service).Register(router.Group("/api/v2"))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/jobs/events", nil)
	requestContext, cancel := context.WithCancel(request.Context())
	cancel()
	request = request.WithContext(requestContext)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if ids := streamEventIDs(t, recorder.Body.String()); len(ids) != 1 || ids[0] != expectedID {
		t.Fatalf("fresh stream ids = %v, expected watermark [%d]", ids, expectedID)
	}
	if !strings.Contains(recorder.Body.String(), "event: job.cursor") || !strings.Contains(recorder.Body.String(), `"reset":false`) {
		t.Fatalf("fresh stream did not expose cursor: %s", recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v2/jobs/events", nil)
	request.Header.Set("Last-Event-ID", strconv.FormatInt(resumeAfter, 10))
	requestContext, cancel = context.WithCancel(request.Context())
	cancel()
	request = request.WithContext(requestContext)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if recorder.Header().Get("Cache-Control") != "no-cache, no-transform" || recorder.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("unexpected stream headers: %#v", recorder.Header())
	}
	if ids := streamEventIDs(t, recorder.Body.String()); len(ids) != 2 || ids[0] != resumeAfter || ids[1] != expectedID {
		t.Fatalf("resumed event ids = %v, expected cursor %d then event %d", ids, resumeAfter, expectedID)
	}
}

func TestJobEventsResetCursorOutsideRetentionWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := jobs.NewStore(db, "sse_reset_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	service, err := jobs.NewService(store, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := store.Create("system.refresh", "", "", []jobs.TargetSpec{{ID: "local", Name: "当前节点"}})
	if err != nil {
		t.Fatal(err)
	}
	window, err := store.EventWindow()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sse_reset_job_event (job_id, type, data, created_at) VALUES (?, ?, ?, ?)`,
		job.ID, "job.created", "{}", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`DELETE FROM sse_reset_job_event WHERE id = ?`, window.FirstID).Error; err != nil {
		t.Fatal(err)
	}
	window, err = store.EventWindow()
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	NewJobHandler(service).Register(router.Group("/api/v2"))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/jobs/events?after=0", nil)
	requestContext, cancel := context.WithCancel(request.Context())
	cancel()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request.WithContext(requestContext))
	if ids := streamEventIDs(t, recorder.Body.String()); len(ids) != 1 || ids[0] != window.LastID {
		t.Fatalf("reset stream ids = %v, expected [%d]", ids, window.LastID)
	}
	if !strings.Contains(recorder.Body.String(), `"reset":true`) {
		t.Fatalf("reset stream did not mark reset: %s", recorder.Body.String())
	}
}

func waitForJobStatus(t *testing.T, service *jobs.Service, jobID string, status jobs.Status) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(jobID)
		if err == nil && job.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s", jobID, status)
}

func streamEventIDs(t *testing.T, body string) []int64 {
	t.Helper()
	ids := make([]int64, 0)
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		if err != nil {
			t.Fatalf("invalid SSE id line %q: %v", line, err)
		}
		ids = append(ids, id)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}
