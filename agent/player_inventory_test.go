package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/shared"
)

func TestPlayerInventorySavedReadNeedsNoLeaseOrRunningGame(t *testing.T) {
	runtime := &fakeShardRuntime{}
	a, installation := newShardOperationAgent(t, runtime)
	path := filepath.Join(installation.SavePath, "Cluster_1", "Master", "save", "session", "0123456789ABCDEF", "KU_01234567_")
	if e := os.MkdirAll(path, 0750); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(path, "0000000003"), []byte(`return {prefab="wilson",data={inventory={items={},equip={}}}}`), 0440); e != nil {
		t.Fatal(e)
	}
	q := runtimeOperationRequest(shared.RuntimeActionPlayerInventory)
	q.LeaseID = ""
	q.FencingToken = 0
	q.LeaseExpiresAt = nil
	q.OperationKey = ""
	q.PlayerInventory = &shared.PlayerInventoryRequest{PlayerID: "KU_01234567", Source: "save"}
	if shared.RuntimeActionMutates(q.Action) {
		t.Fatal("read classified as mutation")
	}
	v, e := a.executeRuntimeOperation(string(q.Action), &q, 15)
	if e != nil || v.PlayerInventory == nil || v.PlayerInventory.Code != "" || !v.PlayerInventory.Empty {
		t.Fatal(v, e)
	}
	if len(runtime.calls) != 0 {
		t.Fatal("saved read used console", runtime.calls)
	}
	q.PlayerInventory.PlayerID = "KU_76543210"
	v, e = a.executeRuntimeOperation(string(q.Action), &q, 15)
	if e != nil || v.PlayerInventory == nil || v.PlayerInventory.Code != "PLAYER_SNAPSHOT_NOT_FOUND" {
		t.Fatal(v, e)
	}
	q.Console = &shared.RuntimeConsoleRequest{Command: "unexpected"}
	if e := validateRuntimeOperationRequest(string(q.Action), q, 15, time.Now()); e == nil {
		t.Fatal("mixed payload accepted")
	}
}
