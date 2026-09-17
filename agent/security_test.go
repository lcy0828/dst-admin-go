package agent

import (
	"bytes"
	"encoding/base64"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-ini/ini"
	"github.com/gorilla/websocket"
)

func agentTestKey() string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'k'}, 32))
}

func TestNormalizedAgentURLRemovesLegacyCredentials(t *testing.T) {
	value, err := normalizedAgentURL("wss://dst.test/agent?room=one&key=secret&token=other#fragment")
	if err != nil {
		t.Fatal(err)
	}
	if value != "wss://dst.test/agent?room=one" {
		t.Fatalf("normalized URL = %q", value)
	}
	if got := displayAgentURL(value); got != "wss://dst.test/agent" {
		t.Fatalf("display URL = %q", got)
	}
}

func TestAgentCommandWhitelistRejectsArbitraryExecution(t *testing.T) {
	if !allowedAgentCommand("df", []string{"-Pk"}) {
		t.Fatal("disk inspection command rejected")
	}
	for _, item := range []struct {
		program string
		args    []string
	}{
		{program: "sh", args: []string{"-c", "touch /tmp/owned"}},
		{program: "bash", args: []string{"-c", "id"}},
		{program: "df", args: []string{"-h"}},
	} {
		if allowedAgentCommand(item.program, item.args) {
			t.Fatalf("unexpectedly allowed %q %q", item.program, item.args)
		}
	}
}

func TestAgentConnectUsesBearerWithoutLeakingKey(t *testing.T) {
	key := agentTestKey()
	var authorization, queryKey string
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		queryKey = request.URL.Query().Get("key")
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_, _, _ = connection.ReadMessage()
	}))
	defer server.Close()
	websocketURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/agent?key=" + key
	configPath := filepath.Join(t.TempDir(), "agent.conf")
	agent, err := NewAgent(&Config{ServerURL: websocketURL, AgentID: "test", SecurityKey: key, KeyFile: configPath})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	err = agent.Connect()
	log.SetOutput(previousWriter)
	if err == nil {
		t.Fatal("Connect unexpectedly completed without a registration response")
	}
	if authorization != "Bearer "+key || queryKey != "" {
		t.Fatalf("authorization = %q, query key = %q", authorization, queryKey)
	}
	if strings.Contains(logs.String(), key) || strings.Contains(err.Error(), key) {
		t.Fatalf("Agent leaked its key; logs=%q err=%q", logs.String(), err)
	}
	data, readErr := os.ReadFile(configPath)
	if readErr != nil || strings.Contains(string(data), "?key=") || strings.Contains(string(data), key+"&") {
		t.Fatalf("Agent persisted a credential-bearing URL: %q, err=%v", data, readErr)
	}
}

func TestConfiguredAgentIDIsPersistedOnlyOnFirstRegistration(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "agent.conf")
	key := agentTestKey()
	if err := os.WriteFile(configPath, []byte("[agent]\nSECURITY_KEY = "+key+"\nSERVER_URL = ws://127.0.0.1/agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := NewAgent(&Config{AgentID: "debian42-container", SecurityKey: key, KeyFile: configPath})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := first.getOrCreateAgentUUID()
	if err != nil || identity != "debian42-container" {
		t.Fatalf("first identity = %q, err=%v", identity, err)
	}
	second, err := NewAgent(&Config{AgentID: "replacement-id", SecurityKey: key, KeyFile: configPath})
	if err != nil {
		t.Fatal(err)
	}
	identity, err = second.getOrCreateAgentUUID()
	if err != nil || identity != "debian42-container" {
		t.Fatalf("persisted identity = %q, err=%v", identity, err)
	}
}

func TestAgentUUIDIsReadOncePerProcess(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.conf")
	key := agentTestKey()
	if err := os.WriteFile(configPath, []byte("[agent]\nAGENT_UUID = stable-agent\nSECURITY_KEY = "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(&Config{AgentID: "configured-agent", SecurityKey: key, KeyFile: configPath})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := agent.getOrCreateAgentUUID()
	if err != nil || identity != "stable-agent" {
		t.Fatalf("initial identity = %q, err=%v", identity, err)
	}
	if err := os.WriteFile(configPath, []byte("[agent]\nAGENT_UUID = changed-on-disk\nSECURITY_KEY = "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err = agent.getOrCreateAgentUUID()
	if err != nil || identity != "stable-agent" {
		t.Fatalf("cached identity = %q, err=%v", identity, err)
	}
}

func TestAgentRejectsUnsafeIdentity(t *testing.T) {
	_, err := NewAgent(&Config{AgentID: "../../node", KeyFile: filepath.Join(t.TempDir(), "agent.conf")})
	if err == nil {
		t.Fatal("unsafe Agent ID was accepted")
	}
}

func TestAgentConfigWritesArePrivateAndPreserveOtherSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.conf")
	key := agentTestKey()
	if err := os.WriteFile(path, []byte("[agent]\nSECURITY_KEY = "+key+"\nSERVER_URL = ws://old.test/agent\n[paths]\nDST_SAVE_PATH = /srv/dst\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{Config: &Config{ServerURL: "wss://dst.test/agent?key=old", SecurityKey: key, KeyFile: path}}
	if err := agent.saveConfig(path, agent.Config.ServerURL, key); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	config, err := ini.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := config.Section("agent").Key("SERVER_URL").String(); got != "wss://dst.test/agent" {
		t.Fatalf("saved URL = %q", got)
	}
	if got := config.Section("paths").Key("DST_SAVE_PATH").String(); got != "/srv/dst" {
		t.Fatalf("unrelated config lost: %q", got)
	}
}
