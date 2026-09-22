package agent

import (
	"dont/shared"
	"os"
	"testing"
)

func TestAgentArtworkReadsWithoutConsoleOrPersistentOperationState(t *testing.T) {
	control := &fakeShardRuntime{}
	agent, _ := newShardOperationAgent(t, control)
	req := runtimeOperationRequest(shared.RuntimeActionEntityArtwork)
	req.OperationKey, req.LeaseID, req.FencingToken, req.LeaseExpiresAt = "", "", 0, nil
	req.EntityArtwork = &shared.RuntimeEntityArtworkRequest{Prefab: "no_image", ModID: "workshop-123"}
	before, beforeErr := os.ReadFile(agent.Config.OperationStateFile)
	result, err := agent.executeRuntimeOperation(string(req.Action), &req, 10)
	if err != nil || result.EntityArtwork == nil || len(result.EntityArtwork.Data) != 0 || len(control.calls) != 0 || shared.RuntimeOperationRequiresLease(req) {
		t.Fatalf("%+v %v calls=%v", result, err, control.calls)
	}
	after, afterErr := os.ReadFile(agent.Config.OperationStateFile)
	if string(before) != string(after) || os.IsNotExist(beforeErr) != os.IsNotExist(afterErr) {
		t.Fatal("artwork read wrote operation state")
	}
	req.Console = &shared.RuntimeConsoleRequest{Command: "print(1)"}
	if _, err := agent.executeRuntimeOperation(string(req.Action), &req, 10); err == nil {
		t.Fatal("unrelated payload accepted")
	}
	req.Console = nil
	req.EntityArtwork.ModID = "../secret"
	if _, err := agent.executeRuntimeOperation(string(req.Action), &req, 10); err == nil {
		t.Fatal("unsafe mod ID accepted")
	}
}
