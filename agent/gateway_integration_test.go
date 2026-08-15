package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/shards"
	legacyserver "dont/server"
	"dont/shared"
)

func TestProductionAgentGatewayRoundTrip(t *testing.T) {
	root := t.TempDir()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	configPath := filepath.Join(root, "agent.conf")
	if err := os.WriteFile(configPath, []byte("[server]\nSECURITY_KEY = "+key+"\n[agent]\nSECURITY_KEY = "+key+"\nSERVER_URL = ws://127.0.0.1/agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gateway, err := legacyserver.NewServer(&legacyserver.Config{KeyFile: configPath, SecurityKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Stop()
	httpServer := httptest.NewServer(gateway.Handler())
	defer httpServer.Close()
	t.Setenv("DST_ADMIN_AGENT_SERVER_URL", "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/agent")

	saveRoot, serverRoot := filepath.Join(root, "saves"), filepath.Join(root, "server")
	worldRoot := filepath.Join(saveRoot, "Cluster_1", "Master")
	for _, directory := range []string{worldRoot, serverRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worldRoot, "server.ini"), []byte("[NETWORK]\nserver_port=10999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saveRoot, "Cluster_1", "cluster.ini"), []byte("[NETWORK]\ncluster_name=integration\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, err := NewAgent(&Config{
		ServerURL: "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/agent", AgentID: "integration-agent",
		SecurityKey: key, KeyFile: configPath, OperationStateFile: filepath.Join(root, "operations.json"),
		RuntimeInstallations: []RuntimeInstallation{{ID: "default", SavePath: saveRoot, ServerPath: serverRoot, ServerMode: "64"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.shardRuntime = func(RuntimeInstallation) (shardRuntimeControl, error) { return runtimeControl, nil }
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()

	agentID := waitForRegisteredAgent(t, gateway)
	agent.sendHeartbeat()
	if err := gateway.RequestPassiveReport(agentID, "dst_runtime_inventory", map[string]interface{}{
		"installation_id": "default", "display_name": "integration", "save_path": saveRoot, "server_path": serverRoot, "server_mode": "64",
	}); err != nil {
		t.Fatal(err)
	}
	waitForGatewayInfo(t, gateway, agentID, func(info map[string]interface{}) bool { return info["inventory"] != nil })

	request := shared.ShardOperationRequest{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "integration-status", InstallationID: "default",
		Action: shared.ShardActionStatus, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
	}
	commandID, err := gateway.SendShardOperation(agentID, request, 10)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		result, resultErr := gateway.GetCommandResult(commandID)
		if resultErr == nil && result.Status == "failed" {
			t.Fatalf("typed action failed: %#v", result)
		}
		if resultErr == nil && result.Status == "completed" {
			var operation shared.ShardOperationResult
			if err := json.Unmarshal([]byte(result.Output), &operation); err != nil {
				t.Fatal(err)
			}
			if operation.OperationID != request.OperationID || operation.Status.State != string(shards.RuntimeRunning) {
				t.Fatalf("operation=%#v", operation)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("typed action did not complete: result=%#v err=%v", result, resultErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForRegisteredAgent(t *testing.T, gateway *legacyserver.Server) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for id := range gateway.GetAllAgentInfo() {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatal("Agent did not register")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForGatewayInfo(t *testing.T, gateway *legacyserver.Server, agentID string, ready func(map[string]interface{}) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if info := gateway.GetAllAgentInfo()[agentID]; ready(info) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("Agent report did not arrive")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
