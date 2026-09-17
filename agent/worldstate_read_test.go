package agent

import (
	"os"
	"path/filepath"
	"testing"

	"dont/internal/shards"
	"dont/shared"
)

func TestAgentWorldStateReadDoesNotSendCommandsOrWriteOperationState(t *testing.T) {
	paused := true
	control := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true, Paused: &paused}}
	agent, installation := newShardOperationAgent(t, control)
	world := filepath.Join(installation.SavePath, "Cluster_1", "Master")
	if err := os.MkdirAll(filepath.Join(world, "save/mod_config_data/dst-admin"), 0750); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"server.ini":     "[SHARD]\nid=1\n",
		"server_log.txt": "[00:00:00]: Current time: Sun Sep 6 12:00:00 2026\n",
		"save/mod_config_data/dst-admin/worldstate-a.json": `{"sequence":1}`,
		"save/mod_config_data/dst-admin/worldstate-b.json": `{"sequence":2}`,
	} {
		if err := os.WriteFile(filepath.Join(world, name), []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	request := runtimeOperationRequest(shared.RuntimeActionWorldStateRead)
	request.OperationKey, request.LeaseID, request.FencingToken, request.LeaseExpiresAt = "", "", 0, nil
	before, beforeErr := os.ReadFile(agent.Config.OperationStateFile)
	value, err := agent.executeRuntimeOperation(string(request.Action), &request, 10)
	if err != nil || value.WorldState == nil || value.WorldState.Runtime.State != "running" || value.WorldState.ReadError != "" || len(value.WorldState.Artifacts.Artifacts) != 2 || len(control.calls) != 0 {
		t.Fatalf("read=%#v commands=%#v error=%v", value, control.calls, err)
	}
	if value.WorldState.Runtime.Paused == nil || !*value.WorldState.Runtime.Paused {
		t.Fatal("pause observation was lost in the Agent response")
	}
	after, afterErr := os.ReadFile(agent.Config.OperationStateFile)
	if string(before) != string(after) || os.IsNotExist(beforeErr) != os.IsNotExist(afterErr) {
		t.Fatalf("read wrote operation state: %v/%v", beforeErr, afterErr)
	}
	request.Console = &shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: "return true"}
	if _, err := agent.executeRuntimeOperation(string(request.Action), &request, 10); err == nil || len(control.calls) != 0 {
		t.Fatalf("accepted unrelated command payload: %v", err)
	}
}
