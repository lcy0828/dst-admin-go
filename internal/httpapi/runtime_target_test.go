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
	router.GET("/api/v2/rooms/room-one/topology", func(c *gin.Context) { Success(c, http.StatusOK, gin.H{"source": "topology"}) })
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

	request = httptest.NewRequest(http.MethodGet, "/api/v2/rooms/room-one/topology", nil)
	request.Header.Set(RuntimeTargetHeader, "agent:node-one")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assertStatus(t, response, http.StatusOK)
}

func TestRuntimeTargetBoundaryAllowsPlacementAwareRuntimeDomains(t *testing.T) {
	router := gin.New()
	router.Use(RequestContext(), RuntimeTargetBoundary())
	for _, path := range []string{
		"/api/v2/rooms/room-one/logs",
		"/api/v2/rooms/room-one/chat-logs",
		"/api/v2/rooms/room-one/players",
		"/api/v2/rooms/room-one/players/actions/refresh",
		"/api/v2/rooms/room-one/world-states",
		"/api/v2/rooms/room-one/world-states/actions/refresh",
		"/api/v2/rooms/room-one/worlds/master/logs",
		"/api/v2/rooms/room-one/worlds/master/logs/events",
		"/api/v2/rooms/room-one/worlds/master/commands",
		"/api/v2/rooms/room-one/structured-logs",
	} {
		router.GET(path, func(c *gin.Context) { c.Status(http.StatusNoContent) })
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set(RuntimeTargetHeader, "agent:node-one")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("path=%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}
