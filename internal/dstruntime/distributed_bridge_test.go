package dstruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/operationlease"
	"dont/shared"
)

type distributedRuntimeFixture struct {
	mu      sync.Mutex
	status  shared.ShardRuntimeStatus
	bundles map[shared.ArtifactKind]shared.RuntimeArtifactBundle
	sent    []shared.RuntimeConsoleRequest
	onSend  func(shared.RuntimeConsoleRequest)
	sendErr error
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
	return shared.RuntimeOperationResult{Outcome: shared.RuntimeOutcomeSent}, f.sendErr
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

func TestDistributedBridgeDefersSnapshotRefreshWhileRoomOperationHoldsLease(t *testing.T) {
	bridge, fixture, roomID, worldID, _ := newDistributedBridgeFixture(t)
	fixture.sendErr = operationlease.ErrBusy

	_, err := bridge.RefreshSnapshots(context.Background(), roomID, worldID)
	if !errors.Is(err, ErrRuntimeRefreshDeferred) || !errors.Is(err, operationlease.ErrBusy) {
		t.Fatalf("refresh error = %v", err)
	}
	if errors.Is(err, ErrRuntimeRefresh) {
		t.Fatalf("deferred refresh was classified as failed: %v", err)
	}
}

func TestDistributedBridgePublishesLongCommandThroughTargetDocument(t *testing.T) {
	bridge, fixture, roomID, worldID, now := newDistributedBridgeFixture(t)
	request := CommandRequest{
		RequestID: "remote-long-command-1234", Action: "console.execute",
		Arguments: map[string]interface{}{"script": "local value=true;--" + strings.Repeat("x", 1800)},
	}
	fixture.onSend = func(console shared.RuntimeConsoleRequest) {
		if console.CommandDocument == nil || console.CommandDocument.RequestID != request.RequestID ||
			!strings.HasPrefix(console.Command, "DSTAdmin.Commands.ExecuteFile(") || len(console.Command) > maximumDirectRuntimeCommandBytes {
			t.Errorf("console=%#v", console)
		}
		receipt := CommandReceipt{
			SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-instance", SessionID: "REMOTE_SESSION", ShardID: "2",
			Sequence: 10, RequestID: request.RequestID, Action: request.Action, OK: true, Code: "COMMAND_EXECUTED", CompletedAtUnix: now.Unix(),
		}
		fixture.mu.Lock()
		fixture.bundles[shared.ArtifactRuntimeCommand] = artifactBundle(shared.ArtifactRuntimeCommand, now, "command-receipt-a.json", receipt)
		fixture.mu.Unlock()
	}
	receipt, err := bridge.ExecuteCommand(context.Background(), roomID, worldID, request)
	if err != nil || !receipt.OK || receipt.RequestID != request.RequestID {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
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

type refreshReadRuntime struct {
	*distributedRuntimeFixture
	readMu  sync.Mutex
	reads   map[shared.ArtifactKind]int
	started chan shared.ArtifactKind
	release chan struct{}
}

func (r *refreshReadRuntime) ReadArtifacts(ctx context.Context, roomID, worldID string, kind shared.ArtifactKind) (shared.RuntimeArtifactBundle, error) {
	r.readMu.Lock()
	r.reads[kind]++
	r.readMu.Unlock()
	if r.started != nil && (kind == shared.ArtifactRuntimePlayers || kind == shared.ArtifactRuntimeWorldState) {
		r.started <- kind
		select {
		case <-ctx.Done():
			return shared.RuntimeArtifactBundle{}, ctx.Err()
		case <-r.release:
		}
	}
	return r.distributedRuntimeFixture.ReadArtifacts(ctx, roomID, worldID, kind)
}

func advanceRefreshFixture(t *testing.T, fixture *distributedRuntimeFixture, now time.Time) {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, kind := range []shared.ArtifactKind{shared.ArtifactRuntimeHealth, shared.ArtifactRuntimePlayers, shared.ArtifactRuntimeWorldState} {
		artifact := fixture.bundles[kind].Artifacts[0]
		var value map[string]interface{}
		if err := json.Unmarshal(artifact.Data, &value); err != nil {
			t.Fatal(err)
		}
		value["sequence"] = value["sequence"].(float64) + 1
		if kind == shared.ArtifactRuntimeHealth {
			world := value["modules"].(map[string]interface{})["worldstate"].(map[string]interface{})
			world["sequence"] = world["sequence"].(float64) + 1
		}
		fixture.bundles[kind] = artifactBundle(kind, now, artifact.Name, value)
	}
}

func TestDistributedRefreshReusesHealthAndReadsOutputsConcurrently(t *testing.T) {
	bridge, fixture, roomID, worldID, now := newDistributedBridgeFixture(t)
	fixture.onSend = func(shared.RuntimeConsoleRequest) { advanceRefreshFixture(t, fixture, now) }
	runtime := &refreshReadRuntime{
		distributedRuntimeFixture: fixture, reads: make(map[shared.ArtifactKind]int),
		started: make(chan shared.ArtifactKind, 2), release: make(chan struct{}),
	}
	bridge.runtime = runtime
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type response struct {
		value SnapshotRefreshResult
		err   error
	}
	done := make(chan response, 1)
	go func() {
		value, err := bridge.RefreshSnapshots(ctx, roomID, worldID)
		done <- response{value: value, err: err}
	}()
	started := make(map[shared.ArtifactKind]bool)
	for range 2 {
		select {
		case kind := <-runtime.started:
			started[kind] = true
		case <-ctx.Done():
			t.Fatal("player and world-state reads did not start concurrently")
		}
	}
	close(runtime.release)
	result := <-done
	if result.err != nil || result.value.Players.Sequence != 4 || result.value.WorldState.Sequence != 5 {
		t.Fatalf("refresh=%#v err=%v", result.value, result.err)
	}
	if len(started) != 2 || len(fixture.sent) != 1 {
		t.Fatalf("started=%v sent=%d", started, len(fixture.sent))
	}
	runtime.readMu.Lock()
	defer runtime.readMu.Unlock()
	if runtime.reads[shared.ArtifactRuntimeHealth] != 2 || runtime.reads[shared.ArtifactRuntimePlayers] != 1 || runtime.reads[shared.ArtifactRuntimeWorldState] != 1 {
		t.Fatalf("unexpected redundant artifact reads: %v", runtime.reads)
	}
}

func TestDistributedRefreshStillRejectsMismatchedOrCorruptOutputs(t *testing.T) {
	for _, kind := range []shared.ArtifactKind{shared.ArtifactRuntimePlayers, shared.ArtifactRuntimeWorldState} {
		for _, corruption := range []string{"sequence", "checksum"} {
			t.Run(string(kind)+"/"+corruption, func(t *testing.T) {
				bridge, fixture, roomID, worldID, now := newDistributedBridgeFixture(t)
				bridge.timeout = 10 * time.Millisecond
				fixture.onSend = func(shared.RuntimeConsoleRequest) {
					advanceRefreshFixture(t, fixture, now)
					fixture.mu.Lock()
					defer fixture.mu.Unlock()
					bundle := fixture.bundles[kind]
					if corruption == "checksum" {
						bundle.Artifacts[0].SHA256 = "invalid"
					} else {
						var value map[string]interface{}
						if err := json.Unmarshal(bundle.Artifacts[0].Data, &value); err != nil {
							t.Fatal(err)
						}
						value["sequence"] = value["sequence"].(float64) + 1
						bundle = artifactBundle(kind, now, bundle.Artifacts[0].Name, value)
					}
					fixture.bundles[kind] = bundle
				}
				if _, err := bridge.RefreshSnapshots(context.Background(), roomID, worldID); !errors.Is(err, ErrRuntimeRefresh) {
					t.Fatalf("refresh accepted %s %s: %v", kind, corruption, err)
				}
			})
		}
	}
}

func TestDistributedRefreshCancelsBothPendingOutputReads(t *testing.T) {
	bridge, fixture, roomID, worldID, now := newDistributedBridgeFixture(t)
	fixture.onSend = func(shared.RuntimeConsoleRequest) { advanceRefreshFixture(t, fixture, now) }
	runtime := &refreshReadRuntime{
		distributedRuntimeFixture: fixture, reads: make(map[shared.ArtifactKind]int),
		started: make(chan shared.ArtifactKind, 2), release: make(chan struct{}),
	}
	bridge.runtime = runtime
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := bridge.RefreshSnapshots(ctx, roomID, worldID); done <- err }()
	select {
	case <-runtime.started:
	case <-time.After(2 * time.Second):
		t.Fatal("output read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("refresh cancellation = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not cancel both reads")
	}
}

func TestDistributedCatalogReadsDoNotQueueBehindWorldOperations(t *testing.T) {
	bridge, fixture, room, world, _ := newDistributedBridgeFixture(t)
	request := CommandRequest{RequestID: "catalog-busy-123456", Action: "catalog.entities"}
	lock := bridge.worldLock(room, world)
	lock.Lock()
	done := make(chan error, 1)
	go func() { _, err := bridge.ExecuteCommand(context.Background(), room, world, request); done <- err }()
	select {
	case err := <-done:
		lock.Unlock()
		if !errors.Is(err, ErrRuntimeCommandBusy) {
			t.Fatalf("busy catalog error: %v", err)
		}
	case <-time.After(time.Second):
		lock.Unlock()
		<-done
		t.Fatal("catalog read queued behind a world operation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := bridge.ExecuteCommand(ctx, room, world, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled catalog: %v", err)
	}
	if len(fixture.sent) != 0 {
		t.Fatal("busy or cancelled reads reached the game")
	}
}

func TestDistributedCommandWaitsForCompleteReceiptWithoutResending(t *testing.T) {
	bridge, fixture, room, world, now := newDistributedBridgeFixture(t)
	request := CommandRequest{RequestID: "catalog-write-race-1234", Action: "catalog.entities"}
	complete := make(chan struct{})
	fixture.onSend = func(shared.RuntimeConsoleRequest) {
		partial := []byte("KLEI     1 {\"requestId\":")
		hash := sha256.Sum256(partial)
		fixture.mu.Lock()
		fixture.bundles[shared.ArtifactRuntimeCommand] = shared.RuntimeArtifactBundle{Kind: shared.ArtifactRuntimeCommand, Artifacts: []shared.RuntimeArtifact{{Name: "command-receipt-a.json", Size: int64(len(partial)), Data: partial, SHA256: hex.EncodeToString(hash[:]), UpdatedAt: now}}}
		fixture.mu.Unlock()
		go func() {
			defer close(complete)
			time.Sleep(10 * time.Millisecond)
			receipt := CommandReceipt{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-instance", SessionID: "REMOTE_SESSION", ShardID: "2", Sequence: 1, RequestID: request.RequestID, Action: request.Action, OK: true, Code: "CATALOG_READY", CompletedAtUnix: now.Unix()}
			fixture.mu.Lock()
			fixture.bundles[shared.ArtifactRuntimeCommand] = artifactBundle(shared.ArtifactRuntimeCommand, now, "command-receipt-a.json", receipt)
			fixture.mu.Unlock()
		}()
	}
	receipt, err := bridge.ExecuteCommand(context.Background(), room, world, request)
	<-complete
	if err != nil || !receipt.OK || receipt.RequestID != request.RequestID {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if len(fixture.sent) != 1 {
		t.Fatalf("command was replayed %d times", len(fixture.sent))
	}
}

func TestDistributedCatalogDistinguishesBusyHealthFromUnavailableModule(t *testing.T) {
	bridge, fixture, room, world, now := newDistributedBridgeFixture(t)
	health := Health{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-instance", SessionID: "REMOTE_SESSION", ShardID: "2", Running: true, Ready: true,
		Modules: map[string]ModuleHealth{"commands": {Running: true, Ready: true, Busy: true}}}
	fixture.bundles[shared.ArtifactRuntimeHealth] = artifactBundle(shared.ArtifactRuntimeHealth, now, "health.json", health)
	request := CommandRequest{RequestID: "catalog-health-busy-01", Action: "catalog.entities"}
	if _, err := bridge.ExecuteCommand(context.Background(), room, world, request); !errors.Is(err, ErrRuntimeCommandBusy) {
		t.Fatalf("busy health must be retryable: %v", err)
	}
	health.Modules["commands"] = ModuleHealth{Running: true, Ready: false}
	fixture.bundles[shared.ArtifactRuntimeHealth] = artifactBundle(shared.ArtifactRuntimeHealth, now, "health.json", health)
	if _, err := bridge.ExecuteCommand(context.Background(), room, world, request); !errors.Is(err, ErrRuntimeUnavailable) || errors.Is(err, ErrRuntimeCommandBusy) {
		t.Fatalf("unready health must not be treated as transient busy: %v", err)
	}
	if len(fixture.sent) != 0 {
		t.Fatal("rejected read reached the world")
	}
}

func TestCatalogDeliveryLeaseContentionIsRetryableWithoutReplayingGameCommands(t *testing.T) {
	bridge, fixture, room, world, _ := newDistributedBridgeFixture(t)
	fixture.sendErr = operationlease.ErrBusy
	for _, action := range []string{"catalog.entities", "console.execute"} {
		_, err := bridge.ExecuteCommand(context.Background(), room, world, CommandRequest{RequestID: "lease-contention-1234", Action: action})
		if !errors.Is(err, operationlease.ErrBusy) || errors.Is(err, ErrRuntimeCommandBusy) != (action == "catalog.entities") {
			t.Fatalf("%s error=%v", action, err)
		}
	}
	if len(fixture.sent) != 2 {
		t.Fatal("bridge replayed a command", len(fixture.sent))
	}
}
