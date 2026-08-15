package dstruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"dont/shared"
)

type distributedRuntimeFixture struct {
	mu      sync.Mutex
	status  shared.ShardRuntimeStatus
	bundles map[shared.ArtifactKind]shared.RuntimeArtifactBundle
	sent    []shared.RuntimeConsoleRequest
	onSend  func(shared.RuntimeConsoleRequest)
}

func (f *distributedRuntimeFixture) Status(context.Context, string, string) (shared.ShardRuntimeStatus, error) {
	return f.status, nil
}

func (f *distributedRuntimeFixture) SendID(_ context.Context, _, _ string, request shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error) {
	f.mu.Lock()
	f.sent = append(f.sent, request)
	onSend := f.onSend
	f.mu.Unlock()
	if onSend != nil {
		onSend(request)
	}
	return shared.RuntimeOperationResult{Outcome: shared.RuntimeOutcomeSent}, nil
}

func (f *distributedRuntimeFixture) ReadArtifacts(_ context.Context, _, _ string, kind shared.ArtifactKind) (shared.RuntimeArtifactBundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bundle, exists := f.bundles[kind]
	if !exists {
		return shared.RuntimeArtifactBundle{}, errors.New("artifact unavailable")
	}
	return bundle, nil
}

func TestDistributedBridgeReadsPlacementArtifactsWithCoherentIdentity(t *testing.T) {
	bridge, fixture, roomID, worldID, now := newDistributedBridgeFixture(t)
	players, err := bridge.ReadPlayers(context.Background(), roomID, worldID)
	if err != nil || players.Sequence != 3 || len(players.Players) != 1 || players.Players[0].ID != "KU_REMOTE" {
		t.Fatalf("players=%#v err=%v", players, err)
	}
	world, err := bridge.ReadWorldState(context.Background(), roomID, worldID)
	if err != nil || world.Sequence != 4 || world.Season != "autumn" || !world.CapturedAt.Equal(now) {
		t.Fatalf("world=%#v err=%v", world, err)
	}

	corrupt := fixture.bundles[shared.ArtifactRuntimePlayers]
	corrupt.Artifacts[0].SHA256 = "00"
	fixture.bundles[shared.ArtifactRuntimePlayers] = corrupt
	if _, err := bridge.ReadPlayers(context.Background(), roomID, worldID); !errors.Is(err, ErrRuntimeResultInvalid) {
		t.Fatalf("corrupt artifact error=%v", err)
	}
}

func TestDistributedBridgeExecutesManagedCommandAndWaitsForRemoteReceipt(t *testing.T) {
	bridge, fixture, roomID, worldID, now := newDistributedBridgeFixture(t)
	request := CommandRequest{RequestID: "remote-command-1234", Action: "player.kick", Arguments: map[string]interface{}{"userId": "KU_REMOTE"}}
	fixture.onSend = func(console shared.RuntimeConsoleRequest) {
		receipt := CommandReceipt{
			SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-instance", SessionID: "REMOTE_SESSION", ShardID: "2",
			Sequence: 9, RequestID: request.RequestID, Action: request.Action, OK: true, Code: "ACTION_COMPLETE", CompletedAtUnix: now.Unix(),
		}
		fixture.mu.Lock()
		fixture.bundles[shared.ArtifactRuntimeCommand] = artifactBundle(shared.ArtifactRuntimeCommand, now, "command-receipt-a.json", receipt)
		fixture.mu.Unlock()
	}
	receipt, err := bridge.ExecuteCommand(context.Background(), roomID, worldID, request)
	if err != nil || !receipt.OK || receipt.RequestID != request.RequestID {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if len(fixture.sent) != 1 || fixture.sent[0].Mode != shared.ConsoleModeManaged {
		t.Fatalf("sent=%#v", fixture.sent)
	}
}

func newDistributedBridgeFixture(t *testing.T) (*DistributedBridge, *distributedRuntimeFixture, string, string, time.Time) {
	t.Helper()
	manager, catalog, _ := newRuntimeTestManager(t)
	local, err := NewBridge(manager, bridgeProcess{running: true}, &bridgeSender{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_786_500_100, 0).UTC()
	health := Health{
		SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-instance", SessionID: "REMOTE_SESSION", ShardID: "2",
		Running: true, Ready: true, Sequence: 3, Modules: map[string]ModuleHealth{
			"worldstate": {Running: true, Ready: true, Sequence: 4}, "commands": {Running: true, Ready: true},
			"events": {Running: true, Ready: true}, "diagnostics": {Running: true, Ready: true},
		},
	}
	players := Snapshot{
		SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-instance", SessionID: "REMOTE_SESSION", ShardID: "2",
		Sequence: 3, CapturedAtUnix: now.Unix(), Complete: true, Players: []SnapshotPlayer{{ID: "KU_REMOTE", Name: "Remote player"}},
	}
	world := WorldStateSnapshot{
		SchemaVersion: ProtocolVersion, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-world-instance", SessionID: "REMOTE_SESSION", ShardID: "2",
		Sequence: 4, CapturedAtUnix: now.Unix(), Complete: true, Season: "autumn", Phase: "day", Precipitation: "none",
	}
	fixture := &distributedRuntimeFixture{
		status: shared.ShardRuntimeStatus{State: "running", SessionExists: true},
		bundles: map[shared.ArtifactKind]shared.RuntimeArtifactBundle{
			shared.ArtifactRuntimeHealth:     artifactBundle(shared.ArtifactRuntimeHealth, now, "health.json", health),
			shared.ArtifactRuntimePlayers:    artifactBundle(shared.ArtifactRuntimePlayers, now, "players-a.json", players),
			shared.ArtifactRuntimeWorldState: artifactBundle(shared.ArtifactRuntimeWorldState, now, "worldstate-a.json", world),
		},
	}
	bridge, err := NewDistributedBridge(local, fixture)
	if err != nil {
		t.Fatal(err)
	}
	bridge.now = func() time.Time { return now }
	bridge.pollInterval = time.Millisecond
	bridge.timeout = 100 * time.Millisecond
	return bridge, fixture, catalog.room.ID, catalog.worlds[0].ID, now
}

func artifactBundle(kind shared.ArtifactKind, updatedAt time.Time, name string, value interface{}) shared.RuntimeArtifactBundle {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return shared.RuntimeArtifactBundle{Kind: kind, Artifacts: []shared.RuntimeArtifact{{
		Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), UpdatedAt: updatedAt, Data: data,
	}}}
}
