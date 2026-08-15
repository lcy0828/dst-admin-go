package dstruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	onSend  func(string)
}

func (s *bridgeSender) Send(_ context.Context, _, _ string, script string) error {
	s.scripts = append(s.scripts, script)
	if s.onSend != nil {
		s.onSend(script)
	}
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

func TestBridgeActivatesAndReloadsOnlyTheManagedRuntime(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(filepath.Join(worldPath, "save", "session", "SESSION"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worldPath, "server.ini"), []byte("[SHARD]\nid = 1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(worldPath, "save", "mod_config_data", managedDirectory)
	if err := os.RemoveAll(output); err != nil {
		t.Fatal(err)
	}
	instance := 0
	outputReadyBeforeSend := false
	sender := &bridgeSender{onSend: func(script string) {
		if info, err := os.Stat(output); err == nil && info.IsDir() {
			outputReadyBeforeSend = true
		}
		instance++
		writeHealthyRuntime(t, worldPath, now.Add(time.Duration(instance)*time.Millisecond), fmt.Sprintf("instance-%d", instance))
	}}
	bridge, err := NewBridge(manager, bridgeProcess{running: true}, sender)
	if err != nil {
		t.Fatal(err)
	}
	bridge.now = func() time.Time { return now.Add(time.Duration(instance+1) * time.Millisecond) }
	bridge.pollInterval = time.Millisecond

	activated, err := bridge.Activate(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || activated.Mode != LifecycleModeActivate || activated.Health.ProducerInstanceID != "instance-1" {
		t.Fatalf("activated = %#v, error = %v", activated, err)
	}
	if len(sender.scripts) != 1 || sender.scripts[0] != managedActivationScript || strings.Contains(sender.scripts[0], "customcommands.lua") {
		t.Fatalf("activation scripts = %#v", sender.scripts)
	}
	if !outputReadyBeforeSend {
		t.Fatal("activation sent Lua before repairing the runtime output directory")
	}

	reloaded, err := bridge.Reload(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || reloaded.Mode != LifecycleModeReload || reloaded.Health.ProducerInstanceID != "instance-2" {
		t.Fatalf("reloaded = %#v, error = %v", reloaded, err)
	}
	if len(sender.scripts) != 2 || !strings.Contains(sender.scripts[1], `rawget(_G,"DSTAdmin")`) || strings.Contains(sender.scripts[1], "customcommands.lua") {
		t.Fatalf("reload scripts = %#v", sender.scripts)
	}
}

func TestBridgeLifecycleRefusesStoppedOrUninstalledWorlds(t *testing.T) {
	manager, catalog, _ := newRuntimeTestManager(t)
	sender := &bridgeSender{}
	bridge, _ := NewBridge(manager, bridgeProcess{running: false}, sender)
	if _, err := bridge.Activate(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("stopped activation error = %v", err)
	}
	if len(sender.scripts) != 0 {
		t.Fatalf("scripts sent to stopped shard = %#v", sender.scripts)
	}

	bridge, _ = NewBridge(manager, bridgeProcess{running: true}, sender)
	if _, err := bridge.Activate(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrRuntimeNotInstalled) {
		t.Fatalf("uninstalled activation error = %v", err)
	}
	if len(sender.scripts) != 0 {
		t.Fatalf("scripts sent without installation = %#v", sender.scripts)
	}
}

func writeHealthyRuntime(t *testing.T, worldPath string, modifiedAt time.Time, instanceID string) {
	t.Helper()
	health := Health{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: instanceID, SessionID: "SESSION", ShardID: "1",
		Running: true, Ready: true, Sequence: 1, Modules: map[string]ModuleHealth{
			"worldstate": {Running: true, Ready: true}, "commands": {Running: true, Ready: true},
			"events": {Running: true, Ready: true}, "diagnostics": {Running: true, Ready: true},
		},
	}
	output := filepath.Join(worldPath, "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(output, 0750); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(output, "health.json"), health)
	if err := os.Chtimes(filepath.Join(output, "health.json"), modifiedAt, modifiedAt); err != nil {
		t.Fatal(err)
	}
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

func TestBridgeIgnoresMatchingReceiptCreatedBeforeThisSend(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	writeRuntimeSessionAndHealth(t, root, now)
	request := CommandRequest{RequestID: "request-1234567890", Action: "player.kick", Arguments: map[string]interface{}{"userId": "KU_TEST"}}
	old := CommandReceipt{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION", ShardID: "1",
		Sequence: 7, RequestID: request.RequestID, Action: request.Action, OK: true, Code: "ACTION_COMPLETE", CompletedAtUnix: now.Add(-30 * time.Second).Unix(),
	}
	output := filepath.Join(root, "Cluster_1", "Master", "save", "mod_config_data", "dst-admin")
	writeJSONFile(t, filepath.Join(output, "command-receipt-a.json"), old)
	sender := &bridgeSender{}
	bridge, _ := NewBridge(manager, bridgeProcess{running: true}, sender)
	bridge.now = func() time.Time { return now }
	bridge.pollInterval = time.Millisecond
	bridge.timeout = 5 * time.Millisecond
	_, err := bridge.ExecuteCommand(context.Background(), catalog.room.ID, catalog.worlds[0].ID, request)
	if !errors.Is(err, ErrRuntimeResultAbsent) || len(sender.scripts) != 1 {
		t.Fatalf("old receipt error = %v, scripts = %#v", err, sender.scripts)
	}
}

func TestBridgeRefreshesStaleSnapshotsWithoutRequiringFreshHealth(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	now := time.Unix(1_786_500_100, 0).UTC()
	manager.now = func() time.Time { return now }
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	if err := os.WriteFile(filepath.Join(worldPath, "server.ini"), []byte("[SHARD]\nid = 1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worldPath, "save", "session", "SESSION"), 0750); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(worldPath, "save", "mod_config_data", "dst-admin")
	previous := Health{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION", ShardID: "1",
		Running: true, Ready: true, Sequence: 3, Modules: map[string]ModuleHealth{
			"worldstate": {Running: true, Ready: true, Sequence: 7}, "commands": {Running: true, Ready: true},
			"events": {Running: true, Ready: true}, "diagnostics": {Running: true, Ready: true},
		},
	}
	writeJSONFile(t, filepath.Join(output, "health.json"), previous)
	if err := os.Chtimes(filepath.Join(output, "health.json"), now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	sender := &bridgeSender{onSend: func(script string) {
		if script != managedRefreshScript {
			t.Fatalf("refresh script = %q", script)
		}
		players := Snapshot{
			SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION", ShardID: "1",
			Sequence: 4, CapturedAtUnix: now.Unix(), Complete: true, Players: []SnapshotPlayer{},
		}
		worldState := WorldStateSnapshot{
			SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "world-instance", SessionID: "SESSION", ShardID: "1",
			Sequence: 8, CapturedAtUnix: now.Unix(), Complete: true, Season: "autumn", Phase: "day", Precipitation: "none",
		}
		fresh := previous
		fresh.Sequence = 4
		fresh.Modules["worldstate"] = ModuleHealth{Running: true, Ready: true, Sequence: 8}
		writeJSONFile(t, filepath.Join(output, "players-a.json"), players)
		writeJSONFile(t, filepath.Join(output, "worldstate-a.json"), worldState)
		writeJSONFile(t, filepath.Join(output, "health.json"), fresh)
		if err := os.Chtimes(filepath.Join(output, "health.json"), now, now); err != nil {
			t.Fatal(err)
		}
	}}
	bridge, err := NewBridge(manager, bridgeProcess{running: true}, sender)
	if err != nil {
		t.Fatal(err)
	}
	bridge.now = func() time.Time { return now }
	bridge.pollInterval = time.Millisecond
	bridge.timeout = 100 * time.Millisecond
	result, err := bridge.RefreshSnapshots(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || result.Players.Sequence != 4 || result.WorldState.Sequence != 8 || result.Health.Sequence != 4 {
		t.Fatalf("refresh result = %#v, error = %v", result, err)
	}
	if len(sender.scripts) != 1 {
		t.Fatalf("refresh sent %d scripts, want 1", len(sender.scripts))
	}
}

func TestBridgeRefreshRequiresBothSequencesToAdvanceAndDoesNotRetry(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	if err := os.WriteFile(filepath.Join(worldPath, "server.ini"), []byte("[SHARD]\nid = 1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worldPath, "save", "session", "SESSION"), 0750); err != nil {
		t.Fatal(err)
	}
	previous := Health{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION", ShardID: "1",
		Running: true, Ready: true, Sequence: 2, Modules: map[string]ModuleHealth{"worldstate": {Running: true, Ready: true, Sequence: 5}},
	}
	output := filepath.Join(worldPath, "save", "mod_config_data", "dst-admin")
	writeJSONFile(t, filepath.Join(output, "health.json"), previous)
	sender := &bridgeSender{onSend: func(string) {
		unchangedWorld := previous
		unchangedWorld.Sequence = 3
		writeJSONFile(t, filepath.Join(output, "health.json"), unchangedWorld)
	}}
	bridge, _ := NewBridge(manager, bridgeProcess{running: true}, sender)
	bridge.now = func() time.Time { return now }
	bridge.pollInterval = time.Millisecond
	bridge.timeout = 5 * time.Millisecond
	_, err := bridge.RefreshSnapshots(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if !errors.Is(err, ErrRuntimeRefresh) || len(sender.scripts) != 1 {
		t.Fatalf("refresh error = %v, scripts = %#v", err, sender.scripts)
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
