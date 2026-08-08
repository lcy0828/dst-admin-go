package httpapi

import (
	"net/http"
	"net/url"
	"strings"

	"dont/internal/authn"
	"dont/internal/systemsettings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type publicRoute struct {
	method string
	path   string
}

var publicAPIRoutes = map[publicRoute]struct{}{
	{method: http.MethodGet, path: "/api/v2/auth/session"}: {},
	{method: http.MethodPost, path: "/api/v2/auth/login"}:  {},
	{method: http.MethodPost, path: "/api/v2/auth/setup"}:  {},
}

func RequestContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if requestID == "" || len(requestID) > 128 {
			requestID = uuid.NewString()
		}
		c.Set(requestIDKey, requestID)
		c.Header("X-Request-ID", requestID)
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "same-origin")
		if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		c.Next()
	}
}

func Recovery() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		Failure(c, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误", nil)
	})
}

func SameOrigin() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}
		parsed, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(parsed.Host, c.Request.Host) {
			Failure(c, http.StatusForbidden, "ORIGIN_DENIED", "不允许的请求来源", nil)
			return
		}
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Vary", "Origin")
		c.Next()
	}
}

func RequireSession(service *authn.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		if _, ok := publicAPIRoutes[publicRoute{method: c.Request.Method, path: c.Request.URL.Path}]; ok {
			c.Next()
			return
		}

		rawToken, err := c.Cookie(authn.SessionCookieName)
		if err != nil {
			Failure(c, http.StatusUnauthorized, "AUTH_REQUIRED", "请先登录", nil)
			return
		}
		authenticated, err := service.Authenticate(rawToken)
		if err != nil {
			Failure(c, http.StatusUnauthorized, "SESSION_INVALID", "登录状态已失效，请重新登录", nil)
			return
		}
		setSessionCookie(c, rawToken, authenticated.Session.ExpiresAt)
		c.Set(authn.ContextAdminKey, authenticated.Admin)
		c.Set(authn.ContextSessionKey, authenticated.Session)

		if isUnsafeMethod(c.Request.Method) && !service.CSRFMatches(authenticated.Session, c.GetHeader("X-CSRF-Token")) {
			Failure(c, http.StatusForbidden, "CSRF_INVALID", "请求校验失败，请刷新页面后重试", nil)
			return
		}
		c.Next()
	}
}

func AdminIPPolicy(provider func() string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions || provider == nil || systemsettings.IPAllowed(provider(), c.ClientIP()) {
			c.Next()
			return
		}
		Failure(c, http.StatusForbidden, "IP_NOT_ALLOWED", "当前 IP 不在管理系统白名单中", nil)
	}
}

func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}
