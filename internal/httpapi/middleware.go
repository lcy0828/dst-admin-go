package httpapi

import (
	"dont/internal/servicelogs"
	"dont/shared"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"dont/internal/aicontrol"
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
		started := time.Now()
		requestID := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if requestID == "" || len(requestID) > 128 || strings.ContainsAny(requestID, "\r\n\t\x00 ") {
			requestID = uuid.NewString()
		}
		c.Set(requestIDKey, requestID)
		if aicontrol.Metadata(c.Request.Context()) != nil {
			c.Set("dst_admin.ai_action", true)
		}
		c.Header("X-Request-ID", requestID)
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "same-origin")
		if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		c.Next()
		duration := time.Since(started)
		contentType, _, _ := strings.Cut(c.Writer.Header().Get("Content-Type"), ";")
		eventStream := strings.EqualFold(strings.TrimSpace(contentType), "text/event-stream")
		// A subscription's lifetime is not request latency. Failed subscriptions
		// and ordinary failures must still be visible, even when they fail fast.
		important := c.Writer.Status() >= http.StatusBadRequest || duration >= time.Second && !eventStream || isModMutationRequest(c.Request.Method, c.Request.URL.Path)
		debugRequest := !eventStream && !important && servicelogs.DebugEnabled()
		if strings.HasPrefix(c.Request.URL.Path, "/api/v2/") && (important || debugRequest) {
			code, _ := c.Get("dst_admin.error_code")
			message, _ := c.Get("dst_admin.error_message")
			stack, _ := c.Get("dst_admin.error_stack")
			level := "info"
			if debugRequest {
				level = "debug"
			} else if c.Writer.Status() >= 500 {
				level = "error"
			} else if c.Writer.Status() >= 400 {
				level = "warn"
			}
			text := fmt.Sprintf("[API] request_id=%s method=%s path=%s status=%d duration_ms=%d", requestID, c.Request.Method, c.Request.URL.Path, c.Writer.Status(), duration.Milliseconds())
			if value, ok := message.(string); ok && value != "" {
				text += " " + value
			}
			entry := shared.ServiceLogEntry{Level: level, Message: text, RequestID: requestID, Method: c.Request.Method, Path: c.Request.URL.Path, Status: c.Writer.Status(), DurationMS: duration.Milliseconds()}
			entry.Code, _ = code.(string)
			entry.Stack, _ = stack.(string)
			servicelogs.Emit(entry)
		}
	}
}

func isModMutationRequest(method, path string) bool {
	return method != http.MethodGet && method != http.MethodHead && strings.Contains(path, "/mods")
}

func Recovery() gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered interface{}) {
		c.Set("dst_admin.error_stack", fmt.Sprintf("%v\n%s", recovered, debug.Stack()))
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

type BearerAuthenticator func(string) (authn.Admin, error)

func RequireSession(service *authn.Service, bearer ...BearerAuthenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		if _, ok := publicAPIRoutes[publicRoute{method: c.Request.Method, path: c.Request.URL.Path}]; ok {
			c.Next()
			return
		}
		if header := c.GetHeader("Authorization"); header != "" && len(bearer) > 0 {
			scheme, token, ok := strings.Cut(header, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				Failure(c, 401, "API_TOKEN_INVALID", "接入密钥无效", nil)
				return
			}
			admin, err := bearer[0](token)
			if err != nil || admin.ID < 1 {
				Failure(c, 401, "API_TOKEN_INVALID", "接入密钥无效或已撤销", nil)
				return
			}
			c.Set(authn.ContextAdminKey, admin)
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
