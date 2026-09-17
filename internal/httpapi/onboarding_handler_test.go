package httpapi

import (
	"net/http"
	"testing"
	"time"

	"dont/internal/authn"
)

func TestOnboardingAuthenticationAndPersistence(t *testing.T) {
	app := newAuthTestApp(t)
	app.service.SetPolicyProvider(func() authn.PasswordPolicy {
		return authn.PasswordPolicy{MinimumLength: 10, RequireComplexity: true, SessionTTL: time.Hour}
	})
	response := performJSON(app.router, http.MethodGet, "/api/v2/auth/session", nil, nil, "")
	policy := responseData(t, response)["passwordPolicy"].(map[string]interface{})
	if policy["minimumLength"] != float64(10) || policy["maximumBytes"] != float64(72) || policy["requireComplexity"] != true {
		t.Fatalf("policy: %+v", policy)
	}
	response = performJSON(app.router, http.MethodPut, "/api/v2/auth/onboarding", map[string]string{"step": "complete"}, nil, "")
	assertStatus(t, response, http.StatusUnauthorized)
	response = performJSON(app.router, http.MethodPost, "/api/v2/auth/setup", map[string]string{"username": "owner", "password": "Test-password-1"}, nil, "")
	assertStatus(t, response, http.StatusCreated)
	cookie := response.Result().Cookies()[0]
	data := responseData(t, response)
	csrf := data["csrfToken"].(string)
	if state := data["onboarding"].(map[string]interface{}); state["required"] != true || state["step"] != "deployment" {
		t.Fatalf("initial state: %+v", state)
	}
	response = performJSON(app.router, http.MethodPut, "/api/v2/auth/onboarding", map[string]string{"step": "room"}, cookie, "")
	assertStatus(t, response, http.StatusForbidden)
	response = performJSON(app.router, http.MethodPut, "/api/v2/auth/onboarding", map[string]string{"step": "invalid"}, cookie, csrf)
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = performJSON(app.router, http.MethodPut, "/api/v2/auth/onboarding", map[string]string{"step": "room"}, cookie, csrf)
	assertStatus(t, response, http.StatusOK)
	response = performJSON(app.router, http.MethodGet, "/api/v2/auth/session", nil, cookie, "")
	if state := responseData(t, response)["onboarding"].(map[string]interface{}); state["step"] != "room" {
		t.Fatalf("session progress: %+v", state)
	}
	response = performJSON(app.router, http.MethodPut, "/api/v2/auth/onboarding", map[string]string{"step": "complete"}, cookie, csrf)
	assertStatus(t, response, http.StatusOK)
	response = performJSON(app.router, http.MethodPost, "/api/v2/auth/login", map[string]string{"username": "owner", "password": "Test-password-1"}, nil, "")
	assertStatus(t, response, http.StatusOK)
	if state := responseData(t, response)["onboarding"].(map[string]interface{}); state["required"] != false {
		t.Fatalf("completed login: %+v", state)
	}
}
