package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestIPPolicyAcceptsForwardedAddressOnlyFromConfiguredProxy(t *testing.T) {
	for _, tc := range []struct {
		proxies, remote, forwarded string
		status                     int
	}{
		{"", "127.0.0.1:1234", "192.0.2.10", http.StatusForbidden},
		{"127.0.0.1,::1", "127.0.0.1:1234", "192.0.2.10", http.StatusNoContent},
		{"127.0.0.1", "127.0.0.1:1234", "192.0.2.20", http.StatusForbidden},
		{"127.0.0.1", "192.0.2.20:1234", "192.0.2.10", http.StatusForbidden},
		{"", "192.0.2.10:1234", "192.0.2.20", http.StatusNoContent},
	} {
		router := gin.New()
		if err := ConfigureTrustedProxies(router, tc.proxies); err != nil {
			t.Fatal(err)
		}
		router.Use(AdminIPPolicy(func() string { return "192.0.2.10" }))
		router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = tc.remote
		request.Header.Set("X-Forwarded-For", tc.forwarded)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("%#v: %d", tc, response.Code)
		}
	}
	for _, invalid := range []string{"0.0.0.0/0", "::/0", "proxy.example", "bad"} {
		if err := ConfigureTrustedProxies(gin.New(), invalid); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}
