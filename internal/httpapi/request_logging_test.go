package httpapi

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRequestLogsKeepFailuresAndModActionsWithoutCountingSSELifetime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name, method, path, contentType string
		status                          int
		delay                           time.Duration
		wantLog                         bool
	}{
		{name: "fast read", method: http.MethodGet, path: "/api/v2/rooms", status: http.StatusOK},
		{name: "slow read", method: http.MethodGet, path: "/api/v2/rooms", status: http.StatusOK, delay: 1050 * time.Millisecond, wantLog: true},
		{name: "live subscription", method: http.MethodGet, path: "/api/v2/jobs/events", status: http.StatusOK, contentType: "text/event-stream; charset=utf-8", delay: 1050 * time.Millisecond},
		{name: "failed subscription handshake", method: http.MethodGet, path: "/api/v2/jobs/events", status: http.StatusServiceUnavailable, wantLog: true},
		{name: "failed stream response", method: http.MethodGet, path: "/api/v2/jobs/events", status: http.StatusServiceUnavailable, contentType: "text/event-stream", wantLog: true},
		{name: "fast server failure", method: http.MethodGet, path: "/api/v2/rooms", status: http.StatusInternalServerError, wantLog: true},
		{name: "permission failure", method: http.MethodGet, path: "/api/v2/rooms", status: http.StatusForbidden, wantLog: true},
		{name: "Mod mutation", method: http.MethodPost, path: "/api/v2/rooms/room/mods/actions/update", status: http.StatusAccepted, wantLog: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			t.Cleanup(func() { log.SetOutput(previous) })
			router := gin.New()
			router.Use(RequestContext())
			router.Handle(test.method, test.path, func(c *gin.Context) {
				if test.contentType != "" {
					c.Header("Content-Type", test.contentType)
				}
				c.Status(test.status)
				if test.delay > 0 {
					c.Writer.Flush()
					time.Sleep(test.delay)
				}
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d", response.Code)
			}
			if strings.Contains(output.String(), "[API]") != test.wantLog {
				t.Fatalf("request logging: want=%t output=%s", test.wantLog, output.String())
			}
			if test.wantLog && (!strings.Contains(output.String(), test.path) || !strings.Contains(output.String(), response.Header().Get("X-Request-ID"))) {
				t.Fatalf("request log lost correlation: %s", output.String())
			}
		})
	}
}
