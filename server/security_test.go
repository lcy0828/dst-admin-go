package server

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

type testKeyManager struct{ key string }

func (manager *testKeyManager) GetKey() string                     { return manager.key }
func (manager *testKeyManager) SetKey(value string) error          { manager.key = value; return nil }
func (manager *testKeyManager) ValidateKey(value string) bool      { return value == manager.key }
func (manager *testKeyManager) GenerateNewKey() error              { return nil }
func (manager *testKeyManager) SetKeyChangedCallback(func(string)) {}
func (manager *testKeyManager) StopWatching()                      {}

func TestAgentAuthorizationPrefersBearerAndKeepsQueryCompatibility(t *testing.T) {
	request := httptest.NewRequest("GET", "http://dst.test/agent?key=legacy", nil)
	request.Header.Set("Authorization", "Bearer header-key")
	key, legacy, err := agentAuthorizationKey(request)
	if err != nil || key != "header-key" || legacy {
		t.Fatalf("header auth = %q, %v, %v", key, legacy, err)
	}

	request = httptest.NewRequest("GET", "http://dst.test/agent?key=legacy%2Bkey%3D", nil)
	key, legacy, err = agentAuthorizationKey(request)
	if err != nil || key != "legacy+key=" || !legacy {
		t.Fatalf("query auth = %q, %v, %v", key, legacy, err)
	}

	request.Header.Set("Authorization", "Basic ignored")
	if _, _, err := agentAuthorizationKey(request); err == nil {
		t.Fatal("malformed Authorization unexpectedly fell back to URL key")
	}
}

func TestAgentOriginAllowsNativeAndStrictSameOriginOnly(t *testing.T) {
	tests := []struct {
		name      string
		origin    string
		forwarded string
		want      bool
	}{
		{name: "native agent", want: true},
		{name: "same origin", origin: "http://dst.test", want: true},
		{name: "cross origin", origin: "http://evil.test", want: false},
		{name: "wrong scheme", origin: "https://dst.test", want: false},
		{name: "forwarded tls", origin: "https://dst.test", forwarded: "https", want: true},
		{name: "origin path", origin: "http://dst.test/attack", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "http://dst.test/agent", nil)
			request.Host = "dst.test"
			request.Header.Set("Origin", test.origin)
			request.Header.Set("X-Forwarded-Proto", test.forwarded)
			if got := allowAgentOrigin(request); got != test.want {
				t.Fatalf("allowAgentOrigin() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAllowedAgentExecIsExact(t *testing.T) {
	if !allowedAgentExec("df", []string{"-Pk"}) {
		t.Fatal("disk inspection command rejected")
	}
	for _, test := range []struct {
		program string
		args    []string
	}{
		{program: "sh", args: []string{"-c", "id"}},
		{program: "df", args: []string{"/etc/passwd"}},
		{program: "/bin/df", args: []string{"-Pk"}},
		{program: "powershell.exe", args: []string{"-Command", "whoami"}},
	} {
		if allowedAgentExec(test.program, test.args) {
			t.Fatalf("unexpectedly allowed %q %q", test.program, test.args)
		}
	}
}

func TestAgentHandlerMarksQueryAuthDeprecatedWithoutLoggingKey(t *testing.T) {
	secret := "legacy-secret-that-must-not-appear"
	server := &Server{
		keyManager: &testKeyManager{key: secret},
		upgrader:   websocket.Upgrader{CheckOrigin: allowAgentOrigin},
	}
	request := httptest.NewRequest("GET", "http://dst.test/agent?key="+secret, nil)
	request.Host = "dst.test"
	response := httptest.NewRecorder()
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	server.handleAgentConnection(response, request)
	log.SetOutput(previousWriter)
	if response.Header().Get("Deprecation") != "true" || !strings.Contains(response.Header().Get("Warning"), "Authorization: Bearer") {
		t.Fatalf("missing deprecation headers: %v", response.Header())
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("legacy Agent key leaked; logs=%q body=%q", logs.String(), response.Body.String())
	}
}
