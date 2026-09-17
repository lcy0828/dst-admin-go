package runtimedriver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/shared"
)

func TestNativeSendConsolePublishesCommandDocumentBeforeTrigger(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "Cluster_1", "Master", "dst-admin")
	if err := os.MkdirAll(directory, 0750); err != nil {
		t.Fatal(err)
	}
	driver, err := NewNative(root, &nativeLifecycleControl{})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"requestId":"native-document-1234","action":"console.execute","arguments":{"script":"return true"}}`)
	sum := sha256.Sum256(data)
	request := shared.RuntimeConsoleRequest{
		Mode: shared.ConsoleModeManaged, Command: `DSTAdmin.Commands.ExecuteFile("native-document-1234","console.execute")`,
		CommandDocument: &shared.RuntimeCommandDocument{RequestID: "native-document-1234", SHA256: hex.EncodeToString(sum[:]), Data: data},
	}
	target := Target{
		TargetID: "local", InstallationID: "default", RoomID: "room-1", WorldID: "world-1",
		Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
	}
	result, err := driver.SendConsole(context.Background(), target, Operation{ID: "native-command-operation"}, request, time.Second)
	if err != nil || result.Outcome != shared.RuntimeOutcomeSent {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	written, err := os.ReadFile(filepath.Join(directory, "command-requests", "native-document-1234.json"))
	if err != nil || string(written) != string(data) {
		t.Fatalf("written=%q err=%v", written, err)
	}
}
