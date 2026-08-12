package dstruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type bridgeProcess struct{ running bool }

func (p bridgeProcess) IsRunning(context.Context, string, string) (bool, error) {
	return p.running, nil
}

type bridgeSender struct {
	manager *Manager
	root    string
	now     time.Time
	scripts []string
	reply   bool
}

func (s *bridgeSender) Send(_ context.Context, _, _ string, script string) error {
	s.scripts = append(s.scripts, script)
	if !s.reply {
		return nil
	}
	requestID := extractJSONString(script, `\"requestId\":\"`)
	action := extractJSONString(script, `\"action\":\"`)
	receipt := CommandReceipt{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION", ShardID: "1",
		Sequence: 1, RequestID: requestID, Action: action, OK: true, Code: "ACTION_COMPLETE", Message: "", CompletedAtUnix: s.now.Unix(),
	}
	data, _ := json.Marshal(receipt)
	output := filepath.Join(s.root, "Cluster_1", "Master", "save", "mod_config_data", "dst-admin")
	_ = os.MkdirAll(output, 0750)
	return os.WriteFile(filepath.Join(output, "command-receipt-a.json"), data, 0640)
}

func extractJSONString(source, marker string) string {
	start := strings.Index(source, marker)
	if start < 0 {
		return ""
	}
	value := source[start+len(marker):]
	if end := strings.Index(value, `\"`); end >= 0 {
		return value[:end]
	}
	return ""
}

func TestBridgeExecutesShortAllowedCommandAndReadsReceipt(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	now := time.Unix(1_786_500_100, 0).UTC()
	manager.now = func() time.Time { return now }
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	writeRuntimeSessionAndHealth(t, root, now)
	sender := &bridgeSender{manager: manager, root: root, now: now, reply: true}
	bridge, err := NewBridge(manager, bridgeProcess{running: true}, sender)
	if err != nil {
		t.Fatal(err)
	}
	bridge.now = func() time.Time { return now }
	bridge.pollInterval = time.Millisecond
	bridge.timeout = 100 * time.Millisecond
	receipt, err := bridge.ExecuteCommand(context.Background(), catalog.room.ID, catalog.worlds[0].ID, CommandRequest{
		RequestID: "request-1234567890", Action: "player.kick", Arguments: map[string]interface{}{"userId": "KU_TEST"},
	})
	if err != nil || !receipt.OK || receipt.RequestID != "request-1234567890" {
		t.Fatalf("receipt = %#v, error = %v", receipt, err)
	}
	if len(sender.scripts) != 1 || !strings.HasPrefix(sender.scripts[0], "DSTAdmin.Commands.ExecuteJSON(") || strings.Contains(sender.scripts[0], "TheNet:Kick") {
		t.Fatalf("unexpected command script: %#v", sender.scripts)
	}
}

func TestBridgeDoesNotRetryAfterReceiptTimeout(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	writeRuntimeSessionAndHealth(t, root, now)
	sender := &bridgeSender{manager: manager, root: root, now: now}
	bridge, _ := NewBridge(manager, bridgeProcess{running: true}, sender)
	bridge.now = func() time.Time { return now }
	bridge.pollInterval = time.Millisecond
	bridge.timeout = 5 * time.Millisecond
	_, err := bridge.ExecuteCommand(context.Background(), catalog.room.ID, catalog.worlds[0].ID, CommandRequest{
		RequestID: "request-1234567890", Action: "player.kick", Arguments: map[string]interface{}{"userId": "KU_TEST"},
	})
	if !errors.Is(err, ErrRuntimeResultAbsent) || len(sender.scripts) != 1 {
		t.Fatalf("error = %v, scripts = %#v", err, sender.scripts)
	}
}

func TestRuntimeResultReadersFallBackFromCorruptSlotAndValidateSequences(t *testing.T) {
	_, _, root := newRuntimeTestManager(t)
	now := time.Unix(1_786_500_100, 0).UTC()
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	output := filepath.Join(worldPath, "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(output, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "events-a.json"), []byte("{"), 0640); err != nil {
		t.Fatal(err)
	}
	batch := EventBatch{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION", ShardID: "1",
		FirstSequence: 4, LastSequence: 5, Events: []RuntimeEvent{
			{Sequence: 4, Kind: "world.phase", OccurredAtUnix: now.Add(-time.Second).Unix(), Fields: map[string]interface{}{"phase": "day"}},
			{Sequence: 5, Kind: "world.rain", OccurredAtUnix: now.Unix(), Fields: map[string]interface{}{"raining": true}},
		},
	}
	writeJSONFile(t, filepath.Join(output, "events-b.json"), batch)
	value, err := readEventBatch(worldPath, "SESSION", now)
	if err != nil || value.LastSequence != 5 || len(value.Events) != 2 {
		t.Fatalf("event batch = %#v, error = %v", value, err)
	}
	older := batch
	older.FirstSequence = 2
	older.LastSequence = 4
	older.Events = []RuntimeEvent{
		{Sequence: 2, Kind: "world.cycles", OccurredAtUnix: now.Add(-3 * time.Second).Unix(), Fields: map[string]interface{}{"cycles": 4}},
		{Sequence: 3, Kind: "world.season", OccurredAtUnix: now.Add(-2 * time.Second).Unix(), Fields: map[string]interface{}{"season": "autumn"}},
		{Sequence: 4, Kind: "world.phase", OccurredAtUnix: now.Add(-time.Second).Unix(), Fields: map[string]interface{}{"phase": "day"}},
	}
	writeJSONFile(t, filepath.Join(output, "events-a.json"), older)
	value, err = readEventBatch(worldPath, "SESSION", now)
	if err != nil || value.FirstSequence != 2 || value.LastSequence != 5 || len(value.Events) != 4 {
		t.Fatalf("merged event batch = %#v, error = %v", value, err)
	}
	batch.Events[1].Sequence = 7
	if err := os.WriteFile(filepath.Join(output, "events-a.json"), []byte("{"), 0640); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(output, "events-b.json"), batch)
	if _, err := readEventBatch(worldPath, "SESSION", now); !errors.Is(err, ErrRuntimeResultInvalid) {
		t.Fatalf("invalid sequence error = %v", err)
	}

	report := DiagnosticReport{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION", ShardID: "1", Sequence: 3,
		RequestID: "diagnostic-123456", Profile: "summary", OK: true, Code: "DIAGNOSTIC_COMPLETE", Message: "", Result: map[string]interface{}{"entityCount": 10}, CompletedAtUnix: now.Unix(),
	}
	writeJSONFile(t, filepath.Join(output, "diagnostic-a.json"), report)
	diagnostic, err := readLatestDiagnosticReport(worldPath, "SESSION", now)
	if err != nil || diagnostic.Profile != "summary" {
		t.Fatalf("diagnostic = %#v, error = %v", diagnostic, err)
	}
}

func TestEventReaderPrefersTheInstanceWithTheNewestEventOverFileModificationTime(t *testing.T) {
	_, _, root := newRuntimeTestManager(t)
	now := time.Unix(1_786_500_100, 0).UTC()
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	output := filepath.Join(worldPath, "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(output, 0750); err != nil {
		t.Fatal(err)
	}
	oldInstance := EventBatch{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "old-instance", SessionID: "SESSION", ShardID: "1",
		FirstSequence: 99, LastSequence: 99, Events: []RuntimeEvent{
			{Sequence: 99, Kind: "world.save", OccurredAtUnix: now.Add(-10 * time.Second).Unix(), Fields: map[string]interface{}{}},
		},
	}
	newInstance := EventBatch{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "new-instance", SessionID: "SESSION", ShardID: "1",
		FirstSequence: 1, LastSequence: 1, Events: []RuntimeEvent{
			{Sequence: 1, Kind: "world.phase", OccurredAtUnix: now.Add(-time.Second).Unix(), Fields: map[string]interface{}{"phase": "day"}},
		},
	}
	oldPath := filepath.Join(output, "events-a.json")
	newPath := filepath.Join(output, "events-b.json")
	writeJSONFile(t, oldPath, oldInstance)
	writeJSONFile(t, newPath, newInstance)
	if err := os.Chtimes(oldPath, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, now.Add(-5*time.Second), now.Add(-5*time.Second)); err != nil {
		t.Fatal(err)
	}

	value, err := readEventBatch(worldPath, "SESSION", now)
	if err != nil || value.ProducerInstanceID != "new-instance" || value.LastSequence != 1 {
		t.Fatalf("event batch = %#v, error = %v", value, err)
	}
}

func writeRuntimeSessionAndHealth(t *testing.T, root string, now time.Time) {
	t.Helper()
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(filepath.Join(worldPath, "save", "session", "SESSION"), 0750); err != nil {
		t.Fatal(err)
	}
	health := Health{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION", ShardID: "1", Running: true, Ready: true,
		Modules: map[string]ModuleHealth{"commands": {Running: true, Ready: true}, "diagnostics": {Running: true, Ready: true}},
	}
	output := filepath.Join(worldPath, "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(output, 0750); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(output, "health.json"), health)
	if err := os.Chtimes(filepath.Join(output, "health.json"), now, now); err != nil {
		t.Fatal(err)
	}
}

func writeJSONFile(t *testing.T, path string, value interface{}) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
}
