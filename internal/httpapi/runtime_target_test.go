package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRuntimeTargetBoundaryKeepsLocalDefaultAndBlocksRemoteFallthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestContext(), RuntimeTargetBoundary())
	router.GET("/api/v2/rooms", func(c *gin.Context) { Success(c, http.StatusOK, gin.H{"source": "local"}) })
	router.GET("/api/v2/runtime-targets", func(c *gin.Context) { Success(c, http.StatusOK, gin.H{"source": "targets"}) })

	request := httptest.NewRequest(http.MethodGet, "/api/v2/rooms", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assertStatus(t, response, http.StatusOK)

	request = httptest.NewRequest(http.MethodGet, "/api/v2/rooms", nil)
	request.Header.Set(RuntimeTargetHeader, "agent:node-one")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assertStatus(t, response, http.StatusConflict)
	if !strings.Contains(response.Body.String(), "REMOTE_RUNTIME_ACTION_UNAVAILABLE") {
		t.Fatalf("unexpected boundary response: %s", response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v2/runtime-targets", nil)
	request.Header.Set(RuntimeTargetHeader, "agent:node-one")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assertStatus(t, response, http.StatusOK)
}
