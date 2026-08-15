package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/shards"
	"dont/shared"
)

func runtimeOperationRequest(action shared.RuntimeAction) shared.RuntimeOperationRequest {
	expires := time.Now().UTC().Add(5 * time.Minute)
	return shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: "runtime-operation-1", OperationKey: "runtime-key-1",
		InstallationID: "default", Action: action, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
		LeaseID: "lease-1", FencingToken: 1, LeaseExpiresAt: &expires,
	}
}

func TestRuntimeConsoleSendIsIdempotent(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	request := runtimeOperationRequest(shared.RuntimeActionConsoleSend)
	request.Console = &shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: "c_announce(\"hello\")"}
	first, err := agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err != nil || first.Outcome != shared.RuntimeOutcomeSent || first.Idempotent {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err != nil || !second.Idempotent || len(runtimeControl.calls) != 1 {
		t.Fatalf("second=%#v calls=%v err=%v", second, runtimeControl.calls, err)
	}
}

func TestRuntimeReadsLogsAndFixedArtifacts(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, installation := newShardOperationAgent(t, runtimeControl)
	worldRoot := filepath.Join(installation.SavePath, "Cluster_1", "Master")
	if err := os.WriteFile(filepath.Join(worldRoot, "server_log.txt"), []byte("first\nsecond needle\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(worldRoot, "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "health.json"), []byte(`{"ready":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := runtimeOperationRequest(shared.RuntimeActionReadLogs)
	logs.OperationKey, logs.LeaseID, logs.FencingToken, logs.LeaseExpiresAt = "", "", 0, nil
	logs.Logs = &shared.RuntimeLogRequest{Cursor: -1, MaxBytes: 1024, MaxLines: 10, Query: "needle"}
	logResult, err := agent.executeRuntimeOperation(string(logs.Action), &logs, 10)
	if err != nil || logResult.Logs == nil || len(logResult.Logs.Lines) != 1 || !strings.Contains(logResult.Logs.Lines[0].Text, "needle") {
		t.Fatalf("logs=%#v err=%v", logResult.Logs, err)
	}

	artifacts := runtimeOperationRequest(shared.RuntimeActionReadArtifacts)
	artifacts.OperationID = "runtime-operation-2"
	artifacts.OperationKey, artifacts.LeaseID, artifacts.FencingToken, artifacts.LeaseExpiresAt = "", "", 0, nil
	artifacts.Artifacts = &shared.RuntimeArtifactRequest{Kind: shared.ArtifactRuntimeHealth}
	artifactResult, err := agent.executeRuntimeOperation(string(artifacts.Action), &artifacts, 10)
	if err != nil || artifactResult.Artifacts == nil || len(artifactResult.Artifacts.Artifacts) != 1 || string(artifactResult.Artifacts.Artifacts[0].Data) != `{"ready":true}` {
		t.Fatalf("artifacts=%#v err=%v", artifactResult.Artifacts, err)
	}
}

func TestRuntimeConsoleRequestRequiresLeaseAndRejectsNewlines(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	request := runtimeOperationRequest(shared.RuntimeActionConsoleSend)
	request.Console = &shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeRaw, Command: "print(1)\nprint(2)"}
	if _, err := agent.executeRuntimeOperation(string(request.Action), &request, 10); err == nil || !strings.Contains(err.Error(), "内容无效") {
		t.Fatalf("newline error=%v", err)
	}
	request.Console.Command = "print(1)"
	request.LeaseID, request.LeaseExpiresAt = "", nil
	if _, err := agent.executeRuntimeOperation(string(request.Action), &request, 10); err == nil || !strings.Contains(err.Error(), "租约") {
		t.Fatalf("lease error=%v", err)
	}
}

func TestRuntimeConsoleHealth(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	request := runtimeOperationRequest(shared.RuntimeActionConsoleHealth)
	request.OperationKey, request.LeaseID, request.FencingToken, request.LeaseExpiresAt = "", "", 0, nil
	result, err := agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err != nil || result.ConsoleHealth == nil || !result.ConsoleHealth.Available || result.ConsoleHealth.Runtime.State != "running" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
