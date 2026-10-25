package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/dstruntime"
	"dont/internal/shards"
	"dont/internal/shardtransfer"
	"dont/shared"
)

func TestAgentRestoreRetainsEnabledCollectorAndOriginalUntilPublish(t *testing.T) {
	a, installation := newShardOperationAgent(t, &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}})
	current := filepath.Join(installation.SavePath, "Cluster_1/Master")
	user := []byte("print('agent user hook')\n")
	if err := os.WriteFile(filepath.Join(current, "customcommands.lua"), user, 0600); err != nil {
		t.Fatal(err)
	}
	install := runtimeOperationRequest(shared.RuntimeActionLuaEnsure)
	install.LuaRuntime = &shared.RuntimeLuaRequest{Version: dstruntime.RuntimeVersion, InstallIfMissing: true}
	if _, err := a.executeRuntimeOperation(string(install.Action), &install, 30); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(current, "customcommands.lua"))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	for name, data := range map[string]string{"cluster.ini": "[NETWORK]\ncluster_name=Imported\n", "Master/server.ini": "[SHARD]\nis_master=true\nid=1\n", "Master/save/shardindex": `return {session_id="NEW"}`, "Master/save/session/NEW/0000000042": "imported-world"} {
		path := filepath.Join(source, "Cluster_1", name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sender, err := shardtransfer.New(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "restore-agent-runtime-0001"
	descriptor, err := sender.PrepareBackup(context.Background(), id, "Cluster_1", "Master")
	if err != nil {
		t.Fatal(err)
	}
	sequence := 0
	mutate := func(action shared.RuntimeAction, payload shared.RuntimeBackupRequest) {
		t.Helper()
		sequence++
		request := runtimeOperationRequest(action)
		request.OperationID, request.OperationKey = fmt.Sprintf("restore-runtime-%03d", sequence), fmt.Sprintf("restore-runtime-key-%03d", sequence)
		request.Backup = &payload
		if _, err := a.executeRuntimeOperation(string(action), &request, 30); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	mutate(shared.RuntimeActionRestoreBegin, shared.RuntimeBackupRequest{BackupID: id, Size: descriptor.Size, ContentSize: descriptor.ContentSize, FileCount: descriptor.FileCount, SHA256: descriptor.SHA256, SharedSHA256: descriptor.SharedSHA256})
	for offset := int64(0); offset < descriptor.Size; {
		chunk, err := sender.ReadBackup(context.Background(), id, offset)
		if err != nil {
			t.Fatal(err)
		}
		mutate(shared.RuntimeActionRestoreWrite, shared.RuntimeBackupRequest{BackupID: id, Offset: offset, Size: descriptor.Size, SHA256: descriptor.SHA256, Data: chunk.Data})
		offset = chunk.NextOffset
	}
	mutate(shared.RuntimeActionRestorePrepare, shared.RuntimeBackupRequest{BackupID: id})
	got, err := os.ReadFile(filepath.Join(current, "customcommands.lua"))
	if err != nil || !bytes.Equal(got, before) {
		t.Fatalf("prepare changed original: %q %v", got, err)
	}
	mutate(shared.RuntimeActionRestorePublish, shared.RuntimeBackupRequest{BackupID: id, PublishShared: true})
	mutate(shared.RuntimeActionRestoreComplete, shared.RuntimeBackupRequest{BackupID: id})
	got, err = os.ReadFile(filepath.Join(current, "customcommands.lua"))
	if err != nil || !bytes.Contains(got, user) || !bytes.Contains(got, []byte("DST-ADMIN MANAGED BLOCK BEGIN")) {
		t.Fatalf("collector or user script lost: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(current, "save/mod_config_data/dst-admin/runtime/bootstrap.lua")); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(current, "save/session/NEW/0000000042"))
	if err != nil || string(got) != "imported-world" {
		t.Fatalf("save changed: %q %v", got, err)
	}
}
