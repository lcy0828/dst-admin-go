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

func TestReadWorldStateChoosesNewestValidSlotAndPreservesEveryMetric(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	manager.now = func() time.Time { return time.Unix(1_786_500_010, 0).UTC() }
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	worldRoot := filepath.Join(root, "Cluster_1", "Master")
	if err := os.WriteFile(filepath.Join(worldRoot, "server.ini"), []byte("[SHARD]\nid = 1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worldRoot, "save", "session", "SESSION_ONE"), 0750); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(worldRoot, "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(output, 0750); err != nil {
		t.Fatal(err)
	}
	integer := func(value int) *int { return &value }
	number := func(value float64) *float64 { return &value }
	a := WorldStateSnapshot{
		SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "SESSION_ONE", ShardID: "1",
		Sequence: 4, CapturedAtUnix: 1_786_500_004, Complete: true, Season: "autumn", Phase: "day", Cycles: integer(48),
		ElapsedDaysInSeason: integer(6), RemainingDaysInSeason: integer(14), SeasonProgress: number(.3), DayProgress: number(.34),
		PhaseProgress: number(.57), Precipitation: "acid_rain", MoonPhase: "new", Temperature: number(18.5), Wetness: number(.18),
		Moisture: number(18), MoistureCeil: number(100), PrecipitationRate: number(.25), NightmarePhase: "warn", NightmareProgress: number(.46),
	}
	b := a
	b.Sequence = 5
	b.CapturedAtUnix = 1_786_500_005
	b.Phase = "dusk"
	writeSnapshot(t, filepath.Join(output, "worldstate-a.json"), a)
	writeSnapshot(t, filepath.Join(output, "worldstate-b.json"), b)

	value, err := manager.ReadWorldState(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.Sequence != 5 || value.Phase != "dusk" || value.Cycles == nil || *value.Cycles != 48 || value.ElapsedDaysInSeason == nil || *value.ElapsedDaysInSeason != 6 || value.RemainingDaysInSeason == nil || *value.RemainingDaysInSeason != 14 || value.SeasonProgress == nil || *value.SeasonProgress != .3 || value.DayProgress == nil || *value.DayProgress != .34 || value.PhaseProgress == nil || *value.PhaseProgress != .57 || value.Precipitation != "acid_rain" || value.MoonPhase != "new" || value.Temperature == nil || *value.Temperature != 18.5 || value.Wetness == nil || *value.Wetness != .18 || value.Moisture == nil || *value.Moisture != 18 || value.MoistureCeil == nil || *value.MoistureCeil != 100 || value.PrecipitationRate == nil || *value.PrecipitationRate != .25 || value.NightmarePhase != "warn" || value.NightmareProgress == nil || *value.NightmareProgress != .46 {
		t.Fatalf("world state fields were not preserved: %#v", value)
	}
	if err := os.WriteFile(filepath.Join(output, "worldstate-b.json"), []byte("{"), 0640); err != nil {
		t.Fatal(err)
	}
	value, err = manager.ReadWorldState(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || value.Sequence != 4 || value.Phase != "day" {
		t.Fatalf("corrupt slot fallback = %#v, error = %v", value, err)
	}
}

func TestReadWorldStateRejectsWrongIdentityStaleVersionUnknownAndTrailingData(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	manager.now = func() time.Time { return time.Unix(1_786_500_100, 0).UTC() }
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	worldRoot := filepath.Join(root, "Cluster_1", "Master")
	if err := os.WriteFile(filepath.Join(worldRoot, "server.ini"), []byte("[SHARD]\nid = 1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worldRoot, "save", "session", "CURRENT"), 0750); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(worldRoot, "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(output, 0750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(output, "worldstate-a.json")
	value := WorldStateSnapshot{
		SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "instance", SessionID: "OLD", ShardID: "1",
		Sequence: 1, CapturedAtUnix: 1_786_500_099, Complete: true, Precipitation: "none",
	}
	writeSnapshot(t, path, value)
	if _, err := manager.ReadWorldState(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("wrong session error = %v", err)
	}
	value.SessionID, value.ShardID = "CURRENT", "2"
	writeSnapshot(t, path, value)
	if _, err := manager.ReadWorldState(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("wrong shard error = %v", err)
	}
	value.ShardID, value.CapturedAtUnix = "1", 1_786_500_000
	writeSnapshot(t, path, value)
	if _, err := manager.ReadWorldState(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("stale snapshot error = %v", err)
	}
	value.CapturedAtUnix, value.ProducerVersion = 1_786_500_099, "2.2.0"
	writeSnapshot(t, path, value)
	if _, err := manager.ReadWorldState(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("old producer error = %v", err)
	}
	value.ProducerVersion = RuntimeVersion
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] = ','
	data = append(data, []byte(`"unknown":true}`)...)
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReadWorldState(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("unknown field error = %v", err)
	}
	writeSnapshot(t, path, value)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n{}")...), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReadWorldState(context.Background(), catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("trailing JSON error = %v", err)
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
	valid.Modules = map[string]ModuleHealth{"barriers": {
		Running: true, Ready: true, Busy: true, Writing: true, Holding: true, BarrierID: "backup-1",
	}}
	data, _ = json.Marshal(valid)
	if err := os.WriteFile(filepath.Join(output, "health.json"), data, 0640); err != nil {
		t.Fatal(err)
	}
	value, err = manager.Health(catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || !value.Modules["barriers"].Holding || value.Modules["barriers"].BarrierID != "backup-1" {
		t.Fatalf("barrier health = %#v, error = %v", value.Modules["barriers"], err)
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
	invalidFailures := -1
	valid.SessionID = "CURRENT"
	valid.Modules = map[string]ModuleHealth{"worldstate": {Running: true, Ready: true, ConsecutiveFailures: invalidFailures}}
	data, _ = json.Marshal(valid)
	if err := os.WriteFile(filepath.Join(output, "health.json"), data, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID); !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("invalid module health error = %v", err)
	}
}

func TestDecodeStrictJSONSupportsKLEIPersistentHeader(t *testing.T) {
	type payload struct {
		Value string `json:"value"`
	}
	for name, source := range map[string][]byte{
		"plain": []byte(`{"value":"plain"}`),
		"klei":  []byte("KLEI     1 {\"value\":\"klei\"}"),
	} {
		t.Run(name, func(t *testing.T) {
			var value payload
			if err := decodeStrictJSON(source, &value); err != nil {
				t.Fatal(err)
			}
			if value.Value != name {
				t.Fatalf("decoded value = %q", value.Value)
			}
		})
	}
	for name, source := range map[string][]byte{
		"missing separator":  []byte(`KLEI1 {"value":"x"}`),
		"missing version":    []byte(`KLEI     {"value":"x"}`),
		"non-object payload": []byte(`KLEI     1 ["x"]`),
		"unknown field":      []byte(`KLEI     1 {"value":"x","extra":true}`),
		"trailing content":   []byte(`KLEI     1 {"value":"x"}{}`),
	} {
		t.Run(name, func(t *testing.T) {
			var value payload
			if err := decodeStrictJSON(source, &value); err == nil {
				t.Fatal("invalid persistent JSON was accepted")
			}
		})
	}
}

func TestConfiguredShardIDUsesRuntimeIdentityForUnshardedWorld(t *testing.T) {
	root := t.TempDir()
	worldRoot := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(worldRoot, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worldRoot, "server.ini"), []byte("[SHARD]\nid = 1\n"), 0640); err != nil {
		t.Fatal(err)
	}

	for name, testCase := range map[string]struct {
		cluster  string
		expected string
	}{
		"sharding disabled": {cluster: "[SHARD]\nshard_enabled = false\n", expected: "0"},
		"sharding enabled":  {cluster: "[SHARD]\nshard_enabled = true\n", expected: "1"},
		"setting absent":    {cluster: "[NETWORK]\ncluster_name = Legacy\n", expected: "1"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(root, "Cluster_1", "cluster.ini"), []byte(testCase.cluster), 0640); err != nil {
				t.Fatal(err)
			}
			actual, err := configuredShardID(worldRoot)
			if err != nil {
				t.Fatal(err)
			}
			if actual != testCase.expected {
				t.Fatalf("configured shard ID = %q, want %q", actual, testCase.expected)
			}
		})
	}
}

func writeSnapshot(t *testing.T, path string, value interface{}) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
}
