package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dont/internal/authn"

	"github.com/gin-gonic/gin"
)

func TestFleetMemberPolicyKeepsDiagnosticsAndExitSettingsAvailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestContext(), FleetMemberPolicy(true))
	router.Any("/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	tests := []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodGet, path: "/api/v2/rooms", status: http.StatusNoContent},
		{method: http.MethodPost, path: "/api/v2/auth/logout", status: http.StatusNoContent},
		{method: http.MethodPost, path: "/api/v2/system/settings/actions/apply", status: http.StatusNoContent},
		{method: http.MethodPost, path: "/api/v2/rooms/room/actions/start", status: http.StatusLocked},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("%s %s status = %d, want %d", test.method, test.path, response.Code, test.status)
		}
	}
}

func TestFleetMemberPolicyPrecedesAuthenticatedWriteValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(
		RequestContext(),
		func(c *gin.Context) {
			c.Set(authn.ContextAdminKey, authn.Admin{ID: 1, Username: "admin"})
			c.Next()
		},
		FleetMemberPolicy(true),
		NewIdempotencyStore(time.Minute, 8).Middleware(),
	)
	router.POST("/api/v2/rooms", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodPost, "/api/v2/rooms", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusLocked {
		t.Fatalf("managed worker mutation status = %d, want %d: %s", response.Code, http.StatusLocked, response.Body.String())
	}
}
