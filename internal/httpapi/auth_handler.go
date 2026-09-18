package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/authn"

	"github.com/gin-gonic/gin"
)

type AuthHandler struct {
	service   *authn.Service
	mu        sync.Mutex
	failures  map[string]loginFailure
	now       func() time.Time
	nextSweep time.Time
	policy    func() LoginSecurityPolicy
	ui        func() UIPreferences
}

type LoginSecurityPolicy struct {
	MaxAttempts int
	BlockFor    time.Duration
}

type UIPreferences struct {
	SystemName string `json:"systemName"`
	Timezone   string `json:"timezone"`
	DateFormat string `json:"dateFormat"`
	ThemeColor string `json:"themeColor"`
}

type loginFailure struct {
	count        int
	blockedUntil time.Time
	expiresAt    time.Time
}

type credentialsRequest struct {
	Username string `json:"username" binding:"required,max=256"`
	Password string `json:"password" binding:"required,max=1024"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword" binding:"required"`
	NewPassword     string `json:"newPassword" binding:"required"`
}

func NewAuthHandler(service *authn.Service) *AuthHandler {
	return &AuthHandler{service: service, failures: make(map[string]loginFailure), now: time.Now, policy: func() LoginSecurityPolicy {
		return LoginSecurityPolicy{MaxAttempts: 5, BlockFor: 15 * time.Minute}
	}, ui: defaultUIPreferences}
}

func (h *AuthHandler) SetSecurityPolicyProvider(provider func() LoginSecurityPolicy) {
	if provider != nil {
		h.policy = provider
	}
}

func (h *AuthHandler) SetUIPreferencesProvider(provider func() UIPreferences) {
	if provider != nil {
		h.ui = provider
	}
}

func (h *AuthHandler) Register(group *gin.RouterGroup) {
	group.GET("/session", h.Session)
	group.POST("/setup", h.Setup)
	group.POST("/login", h.Login)
	group.POST("/logout", h.Logout)
	group.PUT("/password", h.ChangePassword)
	group.PUT("/onboarding", h.SaveOnboarding)
}

func (h *AuthHandler) Session(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	required, err := h.service.SetupRequired()
	if err != nil {
		Failure(c, http.StatusInternalServerError, "AUTH_STORAGE_ERROR", "无法读取认证状态", nil)
		return
	}
	result := gin.H{"authenticated": false, "setupRequired": required, "preferences": h.ui()}
	if required {
		policy := h.service.PasswordPolicy()
		result["passwordPolicy"] = gin.H{"minimumLength": policy.MinimumLength, "maximumBytes": 72, "requireComplexity": policy.RequireComplexity}
	}
	if rawToken, err := c.Cookie(authn.SessionCookieName); err == nil {
		if authenticated, err := h.service.Authenticate(rawToken); err == nil {
			result["authenticated"] = true
			result["user"] = gin.H{"id": authenticated.Admin.ID, "username": authenticated.Admin.Username}
			result["expiresAt"] = authenticated.Session.ExpiresAt
			result["csrfToken"] = authenticated.Session.CSRFToken
			result["onboarding"] = authn.OnboardingFor(authenticated.Admin)
		}
	}
	Success(c, http.StatusOK, result)
}

func (h *AuthHandler) Setup(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	var request credentialsRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_REQUEST", "用户名和密码不能为空", nil)
		return
	}
	authenticated, rawToken, err := h.service.Setup(request.Username, request.Password, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		h.writeAuthError(c, err)
		return
	}
	h.writeAuthenticated(c, authenticated, rawToken, http.StatusCreated)
}

func (h *AuthHandler) SaveOnboarding(c *gin.Context) {
	value, ok := c.Get(authn.ContextAdminKey)
	admin, valid := value.(authn.Admin)
	if !ok || !valid {
		Failure(c, http.StatusUnauthorized, "AUTH_REQUIRED", "请先登录", nil)
		return
	}
	var input struct {
		Step string `json:"step" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_REQUEST", "请指定初始化步骤", nil)
		return
	}
	state, err := h.service.SaveOnboarding(admin.ID, input.Step)
	if err != nil {
		if errors.Is(err, authn.ErrOnboardingStep) {
			Failure(c, http.StatusUnprocessableEntity, "INVALID_ONBOARDING_STEP", "初始化步骤无效", nil)
		} else {
			Failure(c, http.StatusInternalServerError, "AUTH_STORAGE_ERROR", "无法保存初始化进度", nil)
		}
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, state)
}

func (h *AuthHandler) Login(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	ipKey := "ip\x00" + c.ClientIP()
	if retryAfter, blocked := h.isBlocked(ipKey); blocked {
		c.Header("Retry-After", retryAfter)
		Failure(c, http.StatusTooManyRequests, "LOGIN_RATE_LIMITED", "登录尝试过于频繁，请稍后再试", nil)
		return
	}
	var request credentialsRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_REQUEST", "用户名和密码不能为空", nil)
		return
	}
	key := c.ClientIP() + "\x00" + strings.ToLower(strings.TrimSpace(request.Username))
	if retryAfter, blocked := h.isBlocked(key); blocked {
		c.Header("Retry-After", retryAfter)
		Failure(c, http.StatusTooManyRequests, "LOGIN_RATE_LIMITED", "登录尝试过于频繁，请稍后再试", nil)
		return
	}
	authenticated, rawToken, err := h.service.Login(request.Username, request.Password, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		if errors.Is(err, authn.ErrInvalidCredentials) {
			h.recordFailure(key)
			h.recordFailureLimit(ipKey, 25)
		}
		h.writeAuthError(c, err)
		return
	}
	h.clearFailure(key)
	h.writeAuthenticated(c, authenticated, rawToken, http.StatusOK)
}

func (h *AuthHandler) Logout(c *gin.Context) {
	if rawToken, err := c.Cookie(authn.SessionCookieName); err == nil {
		if err := h.service.Logout(rawToken); err != nil {
			Failure(c, http.StatusInternalServerError, "AUTH_STORAGE_ERROR", "退出登录失败", nil)
			return
		}
	}
	clearSessionCookie(c)
	Success(c, http.StatusOK, gin.H{"authenticated": false, "setupRequired": false, "preferences": h.ui()})
}

func (h *AuthHandler) ChangePassword(c *gin.Context) {
	var request changePasswordRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_REQUEST", "当前密码和新密码不能为空", nil)
		return
	}
	value, ok := c.Get(authn.ContextAdminKey)
	admin, valid := value.(authn.Admin)
	if !ok || !valid {
		Failure(c, http.StatusUnauthorized, "AUTH_REQUIRED", "请先登录", nil)
		return
	}
	if err := h.service.ChangePassword(admin.ID, request.CurrentPassword, request.NewPassword); err != nil {
		if errors.Is(err, authn.ErrInvalidCredentials) {
			Failure(c, http.StatusUnprocessableEntity, "CURRENT_PASSWORD_INVALID", "当前密码错误", nil)
			return
		}
		h.writeAuthError(c, err)
		return
	}
	clearSessionCookie(c)
	Success(c, http.StatusOK, gin.H{"authenticated": false, "setupRequired": false, "passwordChanged": true, "preferences": h.ui()})
}

func (h *AuthHandler) writeAuthenticated(c *gin.Context, authenticated *authn.AuthenticatedSession, rawToken string, status int) {
	setSessionCookie(c, rawToken, authenticated.Session.ExpiresAt)
	c.Header("Cache-Control", "no-store")
	Success(c, status, gin.H{
		"authenticated": true,
		"setupRequired": false,
		"user":          gin.H{"id": authenticated.Admin.ID, "username": authenticated.Admin.Username},
		"expiresAt":     authenticated.Session.ExpiresAt,
		"csrfToken":     authenticated.CSRFToken,
		"preferences":   h.ui(),
		"onboarding":    authn.OnboardingFor(authenticated.Admin),
	})
}

func defaultUIPreferences() UIPreferences {
	return UIPreferences{SystemName: "饥荒管理系统", Timezone: "Asia/Shanghai", DateFormat: "YYYY-MM-DD", ThemeColor: "#27272a"}
}

func (h *AuthHandler) writeAuthError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, authn.ErrInvalidCredentials):
		Failure(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "用户名或密码错误", nil)
	case errors.Is(err, authn.ErrSetupRequired):
		Failure(c, http.StatusConflict, "SETUP_REQUIRED", "请先完成管理员初始化", nil)
	case errors.Is(err, authn.ErrSetupComplete):
		Failure(c, http.StatusConflict, "SETUP_COMPLETE", "管理员初始化已经完成", nil)
	case errors.Is(err, authn.ErrWeakPassword):
		Failure(c, http.StatusUnprocessableEntity, "WEAK_PASSWORD", fmt.Sprintf("密码至少需要 %d 个字符", h.service.PasswordPolicy().MinimumLength), nil)
	case errors.Is(err, authn.ErrPasswordComplexity):
		Failure(c, http.StatusUnprocessableEntity, "PASSWORD_COMPLEXITY_REQUIRED", "密码必须包含大小写字母、数字和特殊字符", nil)
	case errors.Is(err, authn.ErrPasswordTooLong):
		Failure(c, http.StatusUnprocessableEntity, "PASSWORD_TOO_LONG", "密码不能超过 72 字节", nil)
	case errors.Is(err, authn.ErrPasswordUnchanged):
		Failure(c, http.StatusUnprocessableEntity, "PASSWORD_UNCHANGED", "新密码不能与当前密码相同", nil)
	default:
		Failure(c, http.StatusInternalServerError, "AUTH_STORAGE_ERROR", "认证服务暂时不可用", nil)
	}
}

const maxLoginFailureEntries = 4096

func (h *AuthHandler) sweepFailures(now time.Time) {
	if now.Before(h.nextSweep) && len(h.failures) < maxLoginFailureEntries {
		return
	}
	for key, failure := range h.failures {
		if !failure.expiresAt.After(now) {
			delete(h.failures, key)
		}
	}
	h.nextSweep = now.Add(time.Minute)
}

func (h *AuthHandler) isBlocked(key string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.sweepFailures(now)
	failure := h.failures[key]
	if failure.blockedUntil.After(now) {
		return strconv.Itoa(int(failure.blockedUntil.Sub(now).Seconds()) + 1), true
	}
	if !failure.expiresAt.After(now) || !failure.blockedUntil.IsZero() {
		delete(h.failures, key)
	}
	return "", false
}

func (h *AuthHandler) recordFailure(key string) {
	limit := h.policy().MaxAttempts
	if limit < 3 || limit > 10 {
		limit = 5
	}
	h.recordFailureLimit(key, limit)
}

func (h *AuthHandler) recordFailureLimit(key string, limit int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.sweepFailures(now)
	failure, exists := h.failures[key]
	if !exists && len(h.failures) >= maxLoginFailureEntries {
		// Keep memory bounded even when requests use a different source each time.
		var oldestKey string
		var oldest time.Time
		for candidate, item := range h.failures {
			if oldest.IsZero() || item.expiresAt.Before(oldest) {
				oldestKey, oldest = candidate, item.expiresAt
			}
		}
		delete(h.failures, oldestKey)
	}
	if !failure.expiresAt.After(now) {
		failure = loginFailure{}
	}
	blockFor := h.policy().BlockFor
	if blockFor <= 0 {
		blockFor = 15 * time.Minute
	}
	failure.count++
	failure.expiresAt = now.Add(blockFor)
	if failure.count >= limit {
		failure.blockedUntil = failure.expiresAt
	}
	h.failures[key] = failure
}

func (h *AuthHandler) clearFailure(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.failures, key)
}

func setSessionCookie(c *gin.Context, token string, expiresAt time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     authn.SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(c.Request),
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
	})
}

func clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     authn.SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(c.Request),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
	})
}

func requestIsHTTPS(request *http.Request) bool {
	return request.TLS != nil || strings.EqualFold(request.Header.Get("X-Forwarded-Proto"), "https")
}
