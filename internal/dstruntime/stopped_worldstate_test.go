package dstruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/shared"
)

func TestReadStoppedWorldStateKeepsTimestampAndIdentityChecks(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	now := time.Now().UTC().Truncate(time.Second)
	manager.now = func() time.Time { return now }
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
	value := WorldStateSnapshot{
		SchemaVersion: ProtocolVersion, ProducerVersion: "2.4.5", ProducerInstanceID: "instance", SessionID: "CURRENT", ShardID: "1",
		Sequence: 10, CapturedAtUnix: now.Add(-time.Hour).Unix(), Complete: true, Season: "summer", Phase: "day",
	}
	path := filepath.Join(output, "worldstate-a.json")
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("KLEI     1 "), data...), 0640); err != nil {
		t.Fatal(err)
	}
	got, err := manager.ReadStoppedWorldState(context.Background(), catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || got.Season != "summer" || got.CapturedAt.Unix() != value.CapturedAtUnix {
		t.Fatalf("stopped state=%#v err=%v", got, err)
	}
	for _, change := range []func(*WorldStateSnapshot){
		func(v *WorldStateSnapshot) { v.SessionID = "OTHER" },
		func(v *WorldStateSnapshot) { v.ShardID = "2" },
		func(v *WorldStateSnapshot) { v.CapturedAtUnix = now.Add(time.Hour).Unix() },
		func(v *WorldStateSnapshot) { v.Complete = false },
	} {
		invalid := value
		change(&invalid)
		writeSnapshot(t, path, invalid)
		if _, err := manager.ReadStoppedWorldState(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err == nil {
			t.Fatalf("accepted invalid stopped output: %#v", invalid)
		}
	}
}

func TestDistributedStoppedWorldStateReadsLatestSlotWithoutHealthSequenceBarrier(t *testing.T) {
	bridge, fixture, roomID, worldID, now := newDistributedBridgeFixture(t)
	fixture.status.State = "stopped"
	value := WorldStateSnapshot{
		SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "last-instance", SessionID: "REMOTE_SESSION", ShardID: "2",
		Sequence: 12, CapturedAtUnix: now.Add(-time.Hour).Unix(), Complete: true, Season: "summer", Phase: "day",
	}
	first := artifactBundle(shared.ArtifactRuntimeWorldState, now, "worldstate-a.json", value)
	value.Sequence++
	value.CapturedAtUnix += 30
	value.Phase = "dusk"
	second := artifactBundle(shared.ArtifactRuntimeWorldState, now, "worldstate-b.json", value)
	first.Artifacts = append(first.Artifacts, second.Artifacts...)
	fixture.bundles[shared.ArtifactRuntimeWorldState] = first
	got, err := bridge.ReadStoppedWorldState(context.Background(), roomID, worldID)
	if err != nil || got.Sequence != 13 || got.Phase != "dusk" || got.CapturedAt.Unix() != value.CapturedAtUnix || len(fixture.sent) != 0 {
		t.Fatalf("stopped state=%#v err=%v sent=%#v", got, err, fixture.sent)
	}
	if _, err := bridge.ReadWorldState(context.Background(), roomID, worldID); err == nil {
		t.Fatal("live read accepted stale output")
	}
	value.SessionID = "WRONG_SESSION"
	fixture.bundles[shared.ArtifactRuntimeWorldState] = artifactBundle(shared.ArtifactRuntimeWorldState, now, "worldstate-a.json", value)
	if _, err := bridge.ReadStoppedWorldState(context.Background(), roomID, worldID); err == nil {
		t.Fatal("stopped read accepted another session")
	}
}
