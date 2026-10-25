package dstruntime

import (
	"context"
	"dont/shared"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestReceiptBeyondOldFiveSecondBoundaryIsConfirmedOnce(t *testing.T) {
	bridge, fixture, room, world, now := newDistributedBridgeFixture(t)
	bridge.timeout = defaultCommandTimeout
	bridge.pollInterval = 10 * time.Millisecond
	req := CommandRequest{RequestID: "delayed-receipt-1234", Action: "system.ping"}
	done := make(chan struct{})
	fixture.onSend = func(shared.RuntimeConsoleRequest) {
		go func() {
			defer close(done)
			time.Sleep(5100 * time.Millisecond)
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			fixture.bundles[shared.ArtifactRuntimeCommand] = artifactBundle(shared.ArtifactRuntimeCommand, now, "command-receipt-a.json", CommandReceipt{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-instance", SessionID: "REMOTE_SESSION", ShardID: "2", Sequence: 1, RequestID: req.RequestID, Action: req.Action, OK: true, Code: "ACTION_COMPLETE", CompletedAtUnix: now.Unix()})
		}()
	}
	receipt, err := bridge.ExecuteCommand(context.Background(), room, world, req)
	<-done
	if err != nil || !receipt.OK || len(fixture.sent) != 1 {
		t.Fatalf("receipt=%+v err=%v sent=%d", receipt, err, len(fixture.sent))
	}
}

func TestCommandTimeoutAndCancellationAfterDispatchAreUncertain(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "canceled"}[cancelled], func(t *testing.T) {
			bridge, fixture, room, world, _ := newDistributedBridgeFixture(t)
			bridge.timeout = 20 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelled {
				fixture.onSend = func(shared.RuntimeConsoleRequest) { cancel() }
			}
			_, err := bridge.ExecuteCommand(ctx, room, world, CommandRequest{RequestID: "unknown-outcome-1234", Action: "system.ping"})
			if !errors.Is(err, ErrRuntimeCommandUnconfirmed) || !errors.Is(err, ErrRuntimeResultAbsent) || len(fixture.sent) != 1 {
				t.Fatalf("err=%v sent=%d", err, len(fixture.sent))
			}
		})
	}
}

type lateReceiptRuntime struct {
	*distributedRuntimeFixture
	receipt shared.RuntimeArtifactBundle
}

func (r lateReceiptRuntime) ReadArtifacts(ctx context.Context, room, world string, kind shared.ArtifactKind) (shared.RuntimeArtifactBundle, error) {
	if kind == shared.ArtifactRuntimeCommand {
		time.Sleep(40 * time.Millisecond)
		return r.receipt, nil
	}
	return r.distributedRuntimeFixture.ReadArtifacts(ctx, room, world, kind)
}
func TestRemoteReadCannotConfirmAfterDeadlineOrFromAnotherInstance(t *testing.T) {
	for _, instance := range []string{"remote-instance", "old-instance"} {
		t.Run(instance, func(t *testing.T) {
			bridge, fixture, room, world, now := newDistributedBridgeFixture(t)
			healthBundle := fixture.bundles[shared.ArtifactRuntimeHealth]
			var health Health
			json.Unmarshal(healthBundle.Artifacts[0].Data, &health)
			module := health.Modules["commands"]
			module.ProducerInstanceID = "command-module-instance"
			health.Modules["commands"] = module
			fixture.bundles[shared.ArtifactRuntimeHealth] = artifactBundle(shared.ArtifactRuntimeHealth, now, "health.json", health)
			req := CommandRequest{RequestID: "late-receipt-123456", Action: "system.ping"}
			receipt := artifactBundle(shared.ArtifactRuntimeCommand, now, "command-receipt-a.json", CommandReceipt{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: map[string]string{"remote-instance": "command-module-instance", "old-instance": "old-instance"}[instance], SessionID: "REMOTE_SESSION", ShardID: "2", Sequence: 1, RequestID: req.RequestID, Action: req.Action, OK: true, Code: "ACTION_COMPLETE", CompletedAtUnix: now.Unix()})
			if instance == "remote-instance" {
				bridge.runtime = lateReceiptRuntime{fixture, receipt}
			} else {
				fixture.bundles[shared.ArtifactRuntimeCommand] = receipt
			}
			bridge.timeout = 10 * time.Millisecond
			_, err := bridge.ExecuteCommand(context.Background(), room, world, req)
			if !errors.Is(err, ErrRuntimeCommandUnconfirmed) || len(fixture.sent) != 1 {
				t.Fatalf("err=%v sent=%d", err, len(fixture.sent))
			}
		})
	}
}

func TestCommandProducerIdentityIsIndependentFromTelemetry(t *testing.T) {
	health := Health{ProducerInstanceID: "telemetry-instance", Modules: map[string]ModuleHealth{"commands": {ProducerInstanceID: "commands-instance"}}}
	if !commandInstanceMatches(health, CommandReceipt{ProducerInstanceID: "commands-instance"}) || commandInstanceMatches(health, CommandReceipt{ProducerInstanceID: "telemetry-instance"}) || commandInstanceMatches(health, CommandReceipt{ProducerInstanceID: "old-commands-instance"}) {
		t.Fatal("confused independent producer identities")
	}
}
func TestQueuedCommandCancellationDoesNotDispatch(t *testing.T) {
	bridge, fixture, room, world, _ := newDistributedBridgeFixture(t)
	lock := bridge.worldLock(room, world)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := bridge.ExecuteCommand(ctx, room, world, CommandRequest{RequestID: "queued-cancel-12345", Action: "system.ping"})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRuntimeCommandUnconfirmed) || len(fixture.sent) != 0 {
		t.Fatalf("err=%v sent=%d", err, len(fixture.sent))
	}
}
