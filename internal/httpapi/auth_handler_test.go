package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dont/internal/authn"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type authTestApp struct {
	router  *gin.Engine
	service *authn.Service
	handler *AuthHandler
}

func newAuthTestApp(t *testing.T) authTestApp {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	t.Cleanup(func() { _ = db.Close() })
	service := authn.NewService(db)
	if err := service.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	router := gin.New()
	router.Use(RequestContext(), Recovery(), SameOrigin())
	api := router.Group("/api", RequireSession(service))
	handler := NewAuthHandler(service)
	handler.Register(api.Group("/v2/auth"))
	api.GET("/v2/protected", func(c *gin.Context) { Success(c, http.StatusOK, gin.H{"ok": true}) })
	api.POST("/v2/protected", func(c *gin.Context) { Success(c, http.StatusOK, gin.H{"ok": true}) })
	return authTestApp{router: router, service: service, handler: handler}
}

func TestAuthLifecycleAndCSRF(t *testing.T) {
	app := newAuthTestApp(t)
	app.handler.SetUIPreferencesProvider(func() UIPreferences {
		return UIPreferences{SystemName: "林火管理台", Timezone: "UTC", DateFormat: "DD/MM/YYYY", ThemeColor: "#228844"}
	})

	response := performJSON(app.router, http.MethodGet, "/api/v2/auth/session", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	data := responseData(t, response)
	if data["setupRequired"] != true || data["authenticated"] != false {
		t.Fatalf("unexpected initial session: %#v", data)
	}
	preferences, ok := data["preferences"].(map[string]interface{})
	if !ok || preferences["systemName"] != "林火管理台" || preferences["timezone"] != "UTC" || preferences["dateFormat"] != "DD/MM/YYYY" || preferences["themeColor"] != "#228844" {
		t.Fatalf("session UI preferences = %#v", data["preferences"])
	}

	response = performJSON(app.router, http.MethodGet, "/api/v2/protected", nil, nil, "")
	assertStatus(t, response, http.StatusUnauthorized)

	response = performJSON(app.router, http.MethodPost, "/api/v2/auth/setup", map[string]string{
		"username": "admin", "password": "short",
	}, nil, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)

	response = performJSON(app.router, http.MethodPost, "/api/v2/auth/setup", map[string]string{
		"username": "admin", "password": "123456",
	}, nil, "")
	assertStatus(t, response, http.StatusCreated)
	cookie := response.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe session cookie: %#v", cookie)
	}
	csrfToken, _ := responseData(t, response)["csrfToken"].(string)
	if csrfToken == "" {
		t.Fatal("setup response did not contain a CSRF token")
	}
	if responseData(t, response)["preferences"].(map[string]interface{})["systemName"] != "林火管理台" {
		t.Fatalf("authenticated response lost UI preferences: %s", response.Body.String())
	}

	response = performJSON(app.router, http.MethodPost, "/api/v2/protected", nil, cookie, "")
	assertStatus(t, response, http.StatusForbidden)
	response = performJSON(app.router, http.MethodPost, "/api/v2/protected", nil, cookie, csrfToken)
	assertStatus(t, response, http.StatusOK)

	response = performJSON(app.router, http.MethodGet, "/api/v2/auth/session", nil, cookie, "")
	assertStatus(t, response, http.StatusOK)
	data = responseData(t, response)
	if data["authenticated"] != true || data["csrfToken"] != csrfToken {
		t.Fatalf("session refresh lost auth state: %#v", data)
	}

	response = performJSON(app.router, http.MethodPost, "/api/v2/auth/logout", nil, cookie, csrfToken)
	assertStatus(t, response, http.StatusOK)
	response = performJSON(app.router, http.MethodGet, "/api/v2/protected", nil, cookie, "")
	assertStatus(t, response, http.StatusUnauthorized)
}

func TestOriginAndLoginRateLimit(t *testing.T) {
	app := newAuthTestApp(t)
	response := performJSON(app.router, http.MethodPost, "/api/v2/auth/setup", map[string]string{
		"username": "admin", "password": "strong-password-one",
	}, nil, "")
	assertStatus(t, response, http.StatusCreated)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/auth/session", nil)
	request.Header.Set("Origin", "https://attacker.example")
	response = httptest.NewRecorder()
	app.router.ServeHTTP(response, request)
	assertStatus(t, response, http.StatusForbidden)

	for attempt := 0; attempt < 5; attempt++ {
		response = performJSON(app.router, http.MethodPost, "/api/v2/auth/login", map[string]string{
			"username": "admin", "password": "wrong-password",
		}, nil, "")
		assertStatus(t, response, http.StatusUnauthorized)
	}
	response = performJSON(app.router, http.MethodPost, "/api/v2/auth/login", map[string]string{
		"username": "admin", "password": "strong-password-one",
	}, nil, "")
	assertStatus(t, response, http.StatusTooManyRequests)
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("rate limited response is missing Retry-After")
	}
}

func TestConfiguredLoginAttemptLimit(t *testing.T) {
	app := newAuthTestApp(t)
	app.handler.SetSecurityPolicyProvider(func() LoginSecurityPolicy {
		return LoginSecurityPolicy{MaxAttempts: 3, BlockFor: time.Minute}
	})
	response := performJSON(app.router, http.MethodPost, "/api/v2/auth/setup", map[string]string{
		"username": "admin", "password": "strong-password-one",
	}, nil, "")
	assertStatus(t, response, http.StatusCreated)
	for attempt := 0; attempt < 3; attempt++ {
		response = performJSON(app.router, http.MethodPost, "/api/v2/auth/login", map[string]string{
			"username": "admin", "password": "wrong-password",
		}, nil, "")
		assertStatus(t, response, http.StatusUnauthorized)
	}
	response = performJSON(app.router, http.MethodPost, "/api/v2/auth/login", map[string]string{
		"username": "admin", "password": "strong-password-one",
	}, nil, "")
	assertStatus(t, response, http.StatusTooManyRequests)
}

func TestChangePasswordValidationAndSessionRevocation(t *testing.T) {
	app := newAuthTestApp(t)
	response := performJSON(app.router, http.MethodPost, "/api/v2/auth/setup", map[string]string{
		"username": "admin", "password": "strong-password-one",
	}, nil, "")
	assertStatus(t, response, http.StatusCreated)
	cookie := response.Result().Cookies()[0]
	csrfToken, _ := responseData(t, response)["csrfToken"].(string)

	response = performJSON(app.router, http.MethodPut, "/api/v2/auth/password", map[string]string{
		"currentPassword": "wrong-password", "newPassword": "strong-password-two",
	}, cookie, csrfToken)
	assertAPIError(t, response, http.StatusUnprocessableEntity, "CURRENT_PASSWORD_INVALID")

	response = performJSON(app.router, http.MethodPut, "/api/v2/auth/password", map[string]string{
		"currentPassword": "strong-password-one", "newPassword": "strong-password-one",
	}, cookie, csrfToken)
	assertAPIError(t, response, http.StatusUnprocessableEntity, "PASSWORD_UNCHANGED")

	response = performJSON(app.router, http.MethodPut, "/api/v2/auth/password", map[string]string{
		"currentPassword": "strong-password-one", "newPassword": "654321",
	}, cookie, csrfToken)
	assertStatus(t, response, http.StatusOK)
	response = performJSON(app.router, http.MethodGet, "/api/v2/protected", nil, cookie, "")
	assertStatus(t, response, http.StatusUnauthorized)
	response = performJSON(app.router, http.MethodPost, "/api/v2/auth/login", map[string]string{
		"username": "admin", "password": "654321",
	}, nil, "")
	assertStatus(t, response, http.StatusOK)
}

func performJSON(router http.Handler, method, path string, body interface{}, cookie *http.Cookie, csrfToken string) *httptest.ResponseRecorder {
	var encoded []byte
	if body != nil {
		encoded, _ = json.Marshal(body)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Host = "dst-admin.test"
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if csrfToken != "" {
		request.Header.Set("X-CSRF-Token", csrfToken)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func responseData(t *testing.T, response *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var envelope struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
	return envelope.Data
}

func assertStatus(t *testing.T, response *httptest.ResponseRecorder, expected int) {
	t.Helper()
	if response.Code != expected {
		t.Fatalf("expected status %d, got %d: %s", expected, response.Code, response.Body.String())
	}
}
