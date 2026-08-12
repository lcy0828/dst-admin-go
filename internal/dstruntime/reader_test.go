package dstruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadPlayersChoosesNewestValidSlotAndFallsBackFromCorruption(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	manager.now = func() time.Time { return time.Unix(1_786_500_010, 0).UTC() }
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	sessionRoot := filepath.Join(root, "Cluster_1", "Master", "save", "session", "SESSION_ONE")
	if err := os.MkdirAll(sessionRoot, 0750); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "Cluster_1", "Master", "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(output, 0750); err != nil {
		t.Fatal(err)
	}
	a := Snapshot{
		SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION_ONE", ShardID: "1",
		Sequence: 4, CapturedAtUnix: 1_786_500_004, Complete: true, Players: []SnapshotPlayer{{ID: "KU_ONE", Name: "Willow", Age: 4}},
	}
	b := a
	b.Sequence = 5
	b.CapturedAtUnix = 1_786_500_005
	b.Players = []SnapshotPlayer{{ID: "KU_TWO", Name: "Wendy", Age: 5}}
	writeSnapshot(t, filepath.Join(output, "players-a.json"), a)
	writeSnapshot(t, filepath.Join(output, "players-b.json"), b)
	value, err := manager.ReadPlayers(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || value.Sequence != 5 || len(value.Players) != 1 || value.Players[0].ID != "KU_TWO" {
		t.Fatalf("selected snapshot = %#v, error = %v", value, err)
	}
	if err := os.WriteFile(filepath.Join(output, "players-b.json"), []byte("{"), 0640); err != nil {
		t.Fatal(err)
	}
	value, err = manager.ReadPlayers(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || value.Sequence != 4 || value.Players[0].ID != "KU_ONE" {
		t.Fatalf("fallback snapshot = %#v, error = %v", value, err)
	}
}

func TestReadPlayersRejectsStaleWrongSessionAndInvalidEmptySnapshot(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	manager.now = func() time.Time { return time.Unix(1_786_500_100, 0).UTC() }
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Cluster_1", "Master", "save", "session", "CURRENT"), 0750); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "Cluster_1", "Master", "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(output, 0750); err != nil {
		t.Fatal(err)
	}
	value := Snapshot{
		SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "OLD", ShardID: "1",
		Sequence: 1, CapturedAtUnix: 1_786_500_099, Complete: true, Players: []SnapshotPlayer{},
	}
	writeSnapshot(t, filepath.Join(output, "players-a.json"), value)
	if _, err := manager.ReadPlayers(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("wrong session error = %v", err)
	}
	value.SessionID = "CURRENT"
	value.CapturedAtUnix = 1_786_500_000
	writeSnapshot(t, filepath.Join(output, "players-a.json"), value)
	if _, err := manager.ReadPlayers(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("stale snapshot error = %v", err)
	}
	value.CapturedAtUnix = 1_786_500_099
	value.Players = nil
	writeSnapshot(t, filepath.Join(output, "players-a.json"), value)
	if _, err := manager.ReadPlayers(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("incomplete empty snapshot error = %v", err)
	}
}

func TestHealthRejectsUnknownOrInvalidPayload(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "Cluster_1", "Master", "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(output, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Cluster_1", "Master", "save", "session", "CURRENT"), 0750); err != nil {
		t.Fatal(err)
	}
	valid := Health{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "runtime", SessionID: "CURRENT", ShardID: "1", Sequence: 2, Running: true, Ready: true}
	data, _ := json.Marshal(valid)
	if err := os.WriteFile(filepath.Join(output, "health.json"), data, 0640); err != nil {
		t.Fatal(err)
	}
	value, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || !value.Ready || value.Sequence != 2 {
		t.Fatalf("health = %#v, error = %v", value, err)
	}
	if err := os.WriteFile(filepath.Join(output, "health.json"), []byte(`{"schemaVersion":1,"producerVersion":"2","producerInstanceId":"x","sequence":1,"unknown":true}`), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("unknown health field error = %v", err)
	}
	valid.SessionID = "PREVIOUS"
	data, _ = json.Marshal(valid)
	if err := os.WriteFile(filepath.Join(output, "health.json"), data, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("stale health session error = %v", err)
	}
}

func writeSnapshot(t *testing.T, path string, value Snapshot) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
}
