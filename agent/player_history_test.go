package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/runtimefiles"
	"dont/internal/shards"
	"dont/shared"
)

func TestAgentReadsPlayerHistoryThroughArtifactTransportWithoutMutationLease(t *testing.T) {
	control := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning}}
	agent, installation := newShardOperationAgent(t, control)
	root := filepath.Join(installation.SavePath, "Cluster_1", "Master")
	if err := os.WriteFile(filepath.Join(root, "server_log.txt"), []byte("[00:00:00]: Current time: Fri Sep 11 10:00:00 2026\n[00:47:32]: Client authenticated: (KU_SHORT) 短连接\n[00:47:37]: [Shard] (KU_SHORT) disconnected from Master(1)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	request := runtimeOperationRequest(shared.RuntimeActionReadArtifacts)
	request.OperationKey, request.LeaseID, request.FencingToken, request.LeaseExpiresAt = "", "", 0, nil
	request.Artifacts = &shared.RuntimeArtifactRequest{Kind: shared.ArtifactRuntimePlayerHistory}
	result, err := agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Artifacts == nil {
		t.Fatal("history artifact missing")
	}
	if err := runtimefiles.ValidateArtifactBundle(shared.ArtifactRuntimePlayerHistory, *result.Artifacts); err != nil {
		t.Fatal(err)
	}
	var history runtimefiles.PlayerHistoryResult
	if err := json.Unmarshal(result.Artifacts.Artifacts[0].Data, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Players) != 1 || !history.Complete || history.Players[0].LastSeenWorld != "Master" || history.Players[0].LastDisconnectedAt.Sub(history.Players[0].LastConnectedAt).Seconds() != 5 {
		t.Fatalf("history=%#v", history)
	}
	if len(control.calls) != 0 {
		t.Fatalf("history read changed game state: %v", control.calls)
	}
}
