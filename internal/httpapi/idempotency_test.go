package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/authn"

	"github.com/gin-gonic/gin"
)

func TestIdempotencyReplaysCompletedWriteAndRejectsChangedPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewIdempotencyStore(time.Minute, 8)
	var executions atomic.Int32
	router := gin.New()
	router.Use(RequestContext(), func(c *gin.Context) {
		c.Set(authn.ContextAdminKey, authn.Admin{ID: 7, Username: "admin"})
	}, store.Middleware())
	router.POST("/resource", func(c *gin.Context) {
		executions.Add(1)
		Success(c, http.StatusCreated, gin.H{"value": 1})
	})

	first := idempotencyRequest(router, `{"value":1}`, "write-12345678")
	second := idempotencyRequest(router, `{"value":1}`, "write-12345678")
	conflict := idempotencyRequest(router, `{"value":2}`, "write-12345678")

	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("write statuses = %d, %d; want 201, 201", first.Code, second.Code)
	}
	if second.Header().Get(idempotencyReplayHeader) != "true" {
		t.Fatalf("replay header = %q, want true", second.Header().Get(idempotencyReplayHeader))
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replayed body differs:\nfirst: %s\nsecond: %s", first.Body.String(), second.Body.String())
	}
	firstRequestID := responseRequestID(t, first)
	secondRequestID := responseRequestID(t, second)
	if firstRequestID == "" || secondRequestID != firstRequestID {
		t.Fatalf("response request IDs = %q, %q; want the original non-empty ID", firstRequestID, secondRequestID)
	}
	if first.Header().Get("X-Request-ID") != firstRequestID || second.Header().Get("X-Request-ID") != secondRequestID {
		t.Fatalf(
			"request ID headers = %q, %q; body IDs = %q, %q",
			first.Header().Get("X-Request-ID"), second.Header().Get("X-Request-ID"), firstRequestID, secondRequestID,
		)
	}
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), idempotencyConflictErrorCode) {
		t.Fatalf("changed payload response = %d %s, want 409 conflict", conflict.Code, conflict.Body.String())
	}
	if executions.Load() != 1 {
		t.Fatalf("handler executions = %d, want 1", executions.Load())
	}
}

func TestIdempotencyCoalescesConcurrentWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewIdempotencyStore(time.Minute, 8)
	started := make(chan struct{})
	release := make(chan struct{})
	var executions atomic.Int32
	router := gin.New()
	router.Use(RequestContext(), func(c *gin.Context) {
		c.Set(authn.ContextAdminKey, authn.Admin{ID: 9})
	}, store.Middleware())
	router.POST("/resource", func(c *gin.Context) {
		executions.Add(1)
		close(started)
		<-release
		Success(c, http.StatusAccepted, gin.H{"accepted": true})
	})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- idempotencyRequest(router, `{"value":1}`, "write-87654321") }()
	<-started
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- idempotencyRequest(router, `{"value":1}`, "write-87654321") }()
	close(release)
	first, second := <-firstDone, <-secondDone

	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("concurrent statuses = %d, %d; want 202, 202", first.Code, second.Code)
	}
	if executions.Load() != 1 {
		t.Fatalf("handler executions = %d, want 1", executions.Load())
	}
	if first.Header().Get(idempotencyReplayHeader) != "true" && second.Header().Get(idempotencyReplayHeader) != "true" {
		t.Fatal("neither concurrent response was marked as replayed")
	}
}

func TestIdempotencyRejectsMalformedKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestContext(), func(c *gin.Context) {
		c.Set(authn.ContextAdminKey, authn.Admin{ID: 1})
	}, NewIdempotencyStore(time.Minute, 8).Middleware())
	router.POST("/resource", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	response := idempotencyRequest(router, `{}`, "short")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), idempotencyInvalidKeyErrorCode) {
		t.Fatalf("malformed key response = %d %s, want 400", response.Code, response.Body.String())
	}
}

func TestIdempotencyRequiresKeyForAuthenticatedWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestContext(), func(c *gin.Context) {
		c.Set(authn.ContextAdminKey, authn.Admin{ID: 1})
	}, NewIdempotencyStore(time.Minute, 8).Middleware())
	router.POST("/resource", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodPost, "/resource", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), idempotencyRequiredErrorCode) {
		t.Fatalf("missing key response = %d %s, want 400", response.Code, response.Body.String())
	}
}

func idempotencyRequest(handler http.Handler, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/resource", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(idempotencyRequestHeader, key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func responseRequestID(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Meta struct {
			RequestID string `json:"requestId"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response envelope: %v", err)
	}
	return envelope.Meta.RequestID
}
