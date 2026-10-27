package dstruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/shared"
)

func TestDistributedLateReceiptReadCollectsOutputWithoutSending(t *testing.T) {
	b, f, room, world, now := newDistributedBridgeFixture(t)
	b.timeout = 10 * time.Millisecond
	req := CommandRequest{RequestID: "late-console-read-1234", Action: "console.execute", Arguments: map[string]interface{}{"script": "print(0)"}}
	if _, err := b.ExecuteCommand(context.Background(), room, world, req); !errors.Is(err, ErrRuntimeCommandUnconfirmed) {
		t.Fatal(err)
	}
	receipt := CommandReceipt{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-instance", SessionID: "REMOTE_SESSION", ShardID: "2", Sequence: 1, RequestID: req.RequestID, Action: req.Action, OK: true, Code: "COMMAND_EXECUTED", CompletedAtUnix: now.Unix(), Details: map[string]interface{}{"output": map[string]interface{}{"text": "0\n", "truncated": false}}}
	f.bundles[shared.ArtifactRuntimeCommand] = artifactBundle(shared.ArtifactRuntimeCommand, now, "command-receipt-a.json", receipt)
	got, err := b.ReadCommandReceipt(context.Background(), room, world, req, now)
	if err != nil || !got.OK || got.RequestID != req.RequestID || len(f.sent) != 1 {
		t.Fatalf("receipt=%+v sent=%d err=%v", got, len(f.sent), err)
	}
	text, _ := outputText(t, got)
	if text != "0\n" {
		t.Fatal(text)
	}
}

func TestDistributedReceiptReadRejectsForeignEvidenceWithoutRefresh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CommandReceipt)
	}{
		{"request", func(r *CommandReceipt) { r.RequestID = "another-request-1234" }},
		{"action", func(r *CommandReceipt) { r.Action = "system.ping" }},
		{"session", func(r *CommandReceipt) { r.SessionID = "OTHER_SESSION" }},
		{"shard", func(r *CommandReceipt) { r.ShardID = "1" }},
		{"module", func(r *CommandReceipt) { r.ProducerInstanceID = "previous-command-module" }},
		{"before dispatch", func(r *CommandReceipt) { r.CompletedAtUnix -= 10 }},
		{"expired", func(r *CommandReceipt) { r.CompletedAtUnix -= 180 }},
		{"future", func(r *CommandReceipt) { r.CompletedAtUnix += 60 }},
		{"invalid schema", func(r *CommandReceipt) { r.SchemaVersion = 9 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, f, room, world, now := newDistributedBridgeFixture(t)
			req := CommandRequest{RequestID: "exact-console-read-1234", Action: "console.execute"}
			var health Health
			if err := json.Unmarshal(f.bundles[shared.ArtifactRuntimeHealth].Artifacts[0].Data, &health); err != nil {
				t.Fatal(err)
			}
			module := health.Modules["commands"]
			module.ProducerInstanceID = "current-command-module"
			health.Modules["commands"] = module
			f.bundles[shared.ArtifactRuntimeHealth] = artifactBundle(shared.ArtifactRuntimeHealth, now, "health.json", health)
			receipt := CommandReceipt{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "current-command-module", SessionID: "REMOTE_SESSION", ShardID: "2", Sequence: 1, RequestID: req.RequestID, Action: req.Action, OK: true, Code: "COMMAND_EXECUTED", CompletedAtUnix: now.Unix()}
			tc.mutate(&receipt)
			f.bundles[shared.ArtifactRuntimeCommand] = artifactBundle(shared.ArtifactRuntimeCommand, now, "command-receipt-a.json", receipt)
			if _, err := b.ReadCommandReceipt(context.Background(), room, world, req, now); err == nil {
				t.Fatalf("accepted: %v", err)
			}
			if len(f.sent) != 0 {
				t.Fatal("lookup sent console input")
			}
		})
	}
}

func TestReceiptReadRejectsPreviousProcessAndOfflineNode(t *testing.T) {
	b, f, room, world, now := newDistributedBridgeFixture(t)
	req := CommandRequest{RequestID: "previous-boot-read-1234", Action: "console.execute"}
	f.current.StartedAt = now.Add(time.Second)
	if _, err := b.ReadCommandReceipt(context.Background(), room, world, req, now); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatal(err)
	}
	// No Controller fallback even though its local fixture is available.
	f.bundles = map[shared.ArtifactKind]shared.RuntimeArtifactBundle{}
	if _, err := b.ReadCommandReceipt(context.Background(), room, world, req, now); err == nil {
		t.Fatal("offline read succeeded")
	}
	if len(f.sent) != 0 {
		t.Fatal("lookup activated/refreshed runtime")
	}
}

func TestLocalReceiptReadUsesCurrentProcessWithoutSending(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	now := time.Now().UTC().Truncate(time.Second)
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	for _, name := range []string{"save/session/SESSION", "save/mod_config_data/dst-admin"} {
		if err := os.MkdirAll(filepath.Join(worldPath, name), 0750); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{
		"server.ini":     "[SHARD]\nid = 1\n",
		"server_log.txt": "[00:00:00]: Current time: " + now.Add(-time.Minute).In(time.Local).Format(time.ANSIC) + "\n",
	} {
		if err := os.WriteFile(filepath.Join(worldPath, path), []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	at := now.Unix()
	health := Health{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "local-instance", SessionID: "SESSION", ShardID: "1", Running: true, Ready: true, LastCapturedAtUnix: &at, LastWrittenAtUnix: &at, Modules: map[string]ModuleHealth{"commands": {ProducerInstanceID: "local-command"}}}
	output := filepath.Join(worldPath, "save/mod_config_data/dst-admin")
	writeJSONFile(t, filepath.Join(output, "health.json"), health)
	sender := &bridgeSender{}
	b, err := NewBridge(manager, bridgeProcess{running: true}, sender)
	if err != nil {
		t.Fatal(err)
	}
	b.now = func() time.Time { return now.Add(time.Second) }
	req := CommandRequest{RequestID: "local-read-receipt-1234", Action: "console.execute"}
	receipt := CommandReceipt{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "local-command", SessionID: "SESSION", ShardID: "1", Sequence: 1, RequestID: req.RequestID, Action: req.Action, OK: true, Code: "COMMAND_EXECUTED", CompletedAtUnix: now.Unix()}
	writeJSONFile(t, filepath.Join(output, "command-receipt-a.json"), receipt)
	if _, err := b.ReadCommandReceipt(context.Background(), catalog.room.ID, catalog.worlds[0].ID, req, now); err != nil {
		t.Fatal(err)
	}
	receipt.ShardID = "wrong"
	writeJSONFile(t, filepath.Join(output, "command-receipt-a.json"), receipt)
	if _, err := b.ReadCommandReceipt(context.Background(), catalog.room.ID, catalog.worlds[0].ID, req, now); err == nil {
		t.Fatal("accepted another shard")
	}
	if len(sender.scripts) != 0 {
		t.Fatal("lookup sent Lua")
	}
}
