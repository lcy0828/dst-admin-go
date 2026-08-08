package controller

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dont/server"
	"dont/shared"

	"github.com/gin-gonic/gin"
)

func TestLegacyAgentMutationEndpointsAreRetired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		path    string
		handler gin.HandlerFunc
	}{
		{path: "/command", handler: SendCommand},
		{path: "/report", handler: RequestReport},
		{path: "/security/key/generate", handler: GenerateNewKey},
		{path: "/security/key/update", handler: UpdateSecurityKey},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			router := gin.New()
			router.POST(test.path, test.handler)
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"type":"shell","content":"id","key":"attacker"}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), "/api/v2/agents/") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestLegacyAgentKeyReadReturnsMaskAndFingerprintOnly(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'s'}, 32))
	configPath := filepath.Join(t.TempDir(), "app.conf")
	if err := shared.WritePrivateFile(configPath, []byte("[server]\nSECURITY_KEY = "+key+"\n")); err != nil {
		t.Fatal(err)
	}
	instance, err := server.NewServer(&server.Config{KeyFile: configPath})
	if err != nil {
		t.Fatal(err)
	}
	previous := AgentServer
	AgentServer = instance
	t.Cleanup(func() {
		instance.Stop()
		AgentServer = previous
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/security/key", GetSecurityKey)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/security/key", nil))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if strings.Contains(response.Body.String(), key) || strings.Contains(response.Body.String(), `"key"`) {
		t.Fatalf("legacy response leaked full key: %s", response.Body.String())
	}
	var envelope struct {
		Data struct {
			MaskedKey   string `json:"masked_key"`
			Fingerprint string `json:"fingerprint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.MaskedKey == "" || len(envelope.Data.Fingerprint) != 64 {
		t.Fatalf("unexpected security metadata: %+v", envelope.Data)
	}
}
