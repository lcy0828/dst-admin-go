package dstruntime

import (
	"context"
	"dont/shared"
	"errors"
	"testing"
	"time"
)

type currentPlayerRuntimeFixture struct {
	*distributedRuntimeFixture
	current       shared.RuntimeWorldStateRead
	identityReads int
}

func (r *currentPlayerRuntimeFixture) ReadWorldState(context.Context, string, string) (shared.RuntimeWorldStateRead, error) {
	r.identityReads++
	return r.current, nil
}

func TestPlayerSnapshotMustBelongToCurrentBoot(t *testing.T) {
	bridge, fixture, room, world, now := newDistributedBridgeFixture(t)
	captured := now.Add(-5 * time.Second)
	capturedUnix := captured.Unix()
	health := Health{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "previous-process", SessionID: "REMOTE_SESSION", ShardID: "2", Running: true, Ready: true, Sequence: 50, LastCapturedAtUnix: &capturedUnix, LastWrittenAtUnix: &capturedUnix}
	old := Snapshot{SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "previous-process", SessionID: "REMOTE_SESSION", ShardID: "2", Sequence: 50, CapturedAtUnix: capturedUnix, Complete: true, Players: []SnapshotPlayer{{ID: "KU_OLD", Name: "Previous player"}}}
	fixture.bundles[shared.ArtifactRuntimeHealth] = artifactBundle(shared.ArtifactRuntimeHealth, captured, "health.json", health)
	fixture.bundles[shared.ArtifactRuntimePlayers] = artifactBundle(shared.ArtifactRuntimePlayers, captured, "players-a.json", old)
	reader := &currentPlayerRuntimeFixture{distributedRuntimeFixture: fixture, current: shared.RuntimeWorldStateRead{Runtime: fixture.status, StartedAt: now.Add(-time.Second), SessionID: "REMOTE_SESSION", ShardID: "2"}}
	bridge.runtime = reader
	got, err := bridge.ReadPlayers(context.Background(), room, world)
	if !errors.Is(err, ErrWorldStatePending) {
		t.Fatalf("previous process player accepted as live: snapshot=%+v err=%v", got, err)
	}
	old.ProducerInstanceID, health.ProducerInstanceID = "new-process", "new-process"
	old.CapturedAtUnix, health.Sequence, old.Sequence = now.Unix(), 1, 1
	fixture.bundles[shared.ArtifactRuntimeHealth] = artifactBundle(shared.ArtifactRuntimeHealth, now, "health.json", health)
	fixture.bundles[shared.ArtifactRuntimePlayers] = artifactBundle(shared.ArtifactRuntimePlayers, now, "players-b.json", old)
	if got, err := bridge.ReadPlayers(context.Background(), room, world); err != nil || got.ProducerInstanceID != "new-process" {
		t.Fatalf("current player rejected: %+v %v", got, err)
	}
}

func TestRotatedWorldStateUsesLatestPersistedProducer(t *testing.T) {
	bridge, _, room, world, now := newDistributedBridgeFixture(t)
	old := WorldStateSnapshot{SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "old-producer", SessionID: "SESSION", ShardID: "1", Sequence: 100, CapturedAtUnix: now.Unix(), Complete: true, Season: "winter"}
	latest := old
	latest.ProducerInstanceID = "new-producer"
	latest.Sequence = 1
	latest.Season = "autumn"
	bundle := artifactBundle(shared.ArtifactRuntimeWorldState, now, "worldstate-a.json", old)
	next := artifactBundle(shared.ArtifactRuntimeWorldState, now.Add(time.Millisecond), "worldstate-b.json", latest)
	bundle.Artifacts = append(bundle.Artifacts, next.Artifacts...)
	fixture := &currentWorldFilesFixture{value: shared.RuntimeWorldStateRead{Runtime: shared.ShardRuntimeStatus{State: "running"}, StartedAt: now.Add(-time.Minute), SessionID: "SESSION", ShardID: "1", Artifacts: bundle}}
	bridge.runtime = fixture
	got, err := bridge.ReadCurrentWorldState(context.Background(), room, world)
	if err != nil {
		t.Fatal(err)
	}
	if got.Snapshot.ProducerInstanceID != "new-producer" {
		t.Errorf("selected old producer at equal capture second, ignored newer file timestamp: %+v", got.Snapshot)
	}
}

func TestPausedPlayerSamplesRequireCurrentValidatedEvidence(t *testing.T) {
	for _, change := range []string{"paused", "resumed", "wrong-session", "wrong-shard", "old-boot", "wrong-producer", "no-locality", "corrupt", "fresh"} {
		t.Run(change, func(t *testing.T) {
			bridge, fixture, room, world, now := newDistributedBridgeFixture(t)
			paused := true
			fixture.current.Runtime.Paused = &paused
			captured := now.Add(-time.Minute)
			if change == "fresh" {
				captured = now
			}
			stamp := captured.Unix()
			health := Health{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "current", SessionID: "REMOTE_SESSION", ShardID: "2", Running: true, Ready: true, Sequence: 3, LastCapturedAtUnix: &stamp, LastWrittenAtUnix: &stamp}
			value := Snapshot{SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "current", SessionID: "REMOTE_SESSION", ShardID: "2", Sequence: 3, CapturedAtUnix: stamp, Complete: true, LocalityKnown: true, Players: []SnapshotPlayer{}}
			switch change {
			case "resumed":
				paused = false
			case "wrong-session":
				value.SessionID = "OLD_SESSION"
			case "wrong-shard":
				value.ShardID = "1"
			case "old-boot":
				fixture.current.StartedAt = now.Add(-time.Second)
			case "wrong-producer":
				value.ProducerInstanceID = "previous"
			case "no-locality":
				value.LocalityKnown = false
			}
			fixture.bundles[shared.ArtifactRuntimeHealth] = artifactBundle(shared.ArtifactRuntimeHealth, now, "health.json", health)
			fixture.bundles[shared.ArtifactRuntimePlayers] = artifactBundle(shared.ArtifactRuntimePlayers, captured, "players-a.json", value)
			if change == "corrupt" {
				fixture.bundles[shared.ArtifactRuntimePlayers].Artifacts[0].SHA256 = "00"
			}
			got, err := bridge.ReadPlayers(context.Background(), room, world)
			if change == "fresh" {
				if err != nil || !got.CapturedAt.Equal(now) {
					t.Fatalf("fresh paused sample lost: %+v %v", got, err)
				}
			} else if change == "paused" {
				if !errors.Is(err, ErrPlayerSamplingPaused) || !got.CapturedAt.IsZero() {
					t.Fatalf("paused sample reported live or failed: %+v %v", got, err)
				}
			} else if err == nil || errors.Is(err, ErrPlayerSamplingPaused) {
				t.Fatalf("%s hidden by pause flag: %+v %v", change, got, err)
			}
			if len(fixture.sent) != 0 {
				t.Fatal("passive read sent a command")
			}
		})
	}
}
