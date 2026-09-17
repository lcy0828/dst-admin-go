package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"dont/internal/operationprogress"
	"dont/shared"
)

func TestLegacyCommandsReturnWithoutPolling(t *testing.T) {
	transport, peer, _, commands := legacyGatewayPeer(t)
	for _, kind := range []string{"runtime", "shard"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			started := time.Now()
			completed := startLegacyRead(ctx, transport, kind, "read-now")
			command := receiveLegacyCommand(t, ctx, commands)
			replyLegacyRead(t, peer, command)
			if err := <-completed; err != nil {
				t.Fatalf("already received reply was not returned promptly: %v", err)
			}
			t.Logf("%s round trip: %s", kind, time.Since(started))
		})
	}
}

func TestLegacyShardProgressArrivesBeforeCommandCompletes(t *testing.T) {
	transport, peer, _, commands := legacyGatewayPeer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	updates := make(chan operationprogress.Update, 2)
	progressCtx := operationprogress.WithReporter(ctx, func(update operationprogress.Update) { updates <- update })
	completed := startLegacyRead(progressCtx, transport, "shard", "startup-progress")
	command := receiveLegacyCommand(t, ctx, commands)
	sendLegacyCommandMessage(t, peer, shared.TypeCommandProgress, shared.CommandProgressPayload{
		CommandID: command.CommandID, Sequence: 1, Stage: "world.start.loading_world", Percent: 80, Message: "loading world",
	})
	select {
	case update := <-updates:
		if update.Stage != "world.start.loading_world" || update.Percent != 80 {
			t.Fatalf("lost startup progress: %+v", update)
		}
	case <-ctx.Done():
		t.Fatal("startup log progress did not reach Controller before completion")
	}
	replyLegacyRead(t, peer, command)
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}

func TestLegacyCommandsPreserveFailuresAndValidation(t *testing.T) {
	transport, peer, _, commands := legacyGatewayPeer(t)
	for _, kind := range []string{"runtime", "shard"} {
		for _, test := range []struct {
			name    string
			output  string
			success bool
			errMsg  string
			wantErr string
		}{
			{name: "failed", errMsg: "I/O Operation Failed", wantErr: "I/O Operation Failed"},
			{name: "invalid JSON", success: true, output: "not-json", wantErr: "解析 Agent"},
			{name: "wrong protocol", success: true, output: `{"protocol_version":99,"operation_id":"read"}`, wantErr: "结果无效"},
			{name: "wrong operation", success: true, output: fmt.Sprintf(`{"protocol_version":%d,"operation_id":"another-read"}`, legacyProtocol(kind)), wantErr: "结果无效"},
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				completed := startLegacyRead(ctx, transport, kind, "read")
				command := receiveLegacyCommand(t, ctx, commands)
				sendLegacyCommandMessage(t, peer, shared.TypeCommandResp, shared.CommandResponsePayload{
					CommandID: command.CommandID, Success: test.success, Output: test.output, ErrorMsg: test.errMsg,
				})
				if err := <-completed; err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("expected %q, got %v", test.wantErr, err)
				}
			})
		}
	}
}

func TestLegacyRuntimeCommandProgressAndConcurrentResults(t *testing.T) {
	transport, peer, _, commands := legacyGatewayPeer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	updates := make(chan operationprogress.Update, 8)
	progressCtx := operationprogress.WithReporter(ctx, func(update operationprogress.Update) { updates <- update })
	completed := startLegacyRead(progressCtx, transport, "runtime", "progress-read")
	command := receiveLegacyCommand(t, ctx, commands)
	sendLegacyCommandMessage(t, peer, shared.TypeCommandProgress, shared.CommandProgressPayload{
		CommandID: command.CommandID, Sequence: 2, Percent: 40, Message: "downloading", CurrentBytes: 40, TotalBytes: 100, BytesPerSecond: 20,
	})
	select {
	case update := <-updates:
		if update.Percent != 40 || update.CurrentBytes != 40 || update.BytesPerSecond != 20 {
			t.Fatalf("progress metadata changed: %#v", update)
		}
	case <-ctx.Done():
		t.Fatal("progress was not delivered before completion")
	}
	sendLegacyCommandMessage(t, peer, shared.TypeCommandProgress, shared.CommandProgressPayload{
		CommandID: command.CommandID, Sequence: 1, Percent: 10, Message: "stale",
	})
	sendLegacyCommandMessage(t, peer, shared.TypeCommandProgress, shared.CommandProgressPayload{
		CommandID: command.CommandID, Sequence: 3, Percent: 100, Message: "done",
	})
	replyLegacyRead(t, peer, command)
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		if update.Percent != 100 {
			t.Fatalf("unexpected final progress: %#v", update)
		}
	default:
		t.Fatal("final progress was lost when completion followed immediately")
	}
	if len(updates) != 0 {
		t.Fatal("stale or duplicate progress was delivered")
	}

	var pending []shared.CommandPayload
	var results []<-chan error
	for index := 0; index < 6; index++ {
		kind := "runtime"
		if index%2 == 1 {
			kind = "shard"
		}
		results = append(results, startLegacyRead(ctx, transport, kind, fmt.Sprintf("parallel-%d", index)))
		pending = append(pending, receiveLegacyCommand(t, ctx, commands))
	}
	for index := len(pending) - 1; index >= 0; index-- {
		replyLegacyRead(t, peer, pending[index])
	}
	for _, result := range results {
		if err := <-result; err != nil {
			t.Fatalf("concurrent command received an incorrect result: %v", err)
		}
	}
}

func TestLegacyCommandsCancellationAndDisconnect(t *testing.T) {
	for _, kind := range []string{"runtime", "shard"} {
		t.Run(kind, func(t *testing.T) {
			transport, peer, _, commands := legacyGatewayPeer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			completed := startLegacyRead(ctx, transport, kind, "cancel-read")
			command := receiveLegacyCommand(t, ctx, commands)
			cancel()
			if err := <-completed; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation returned %v", err)
			}
			replyLegacyRead(t, peer, command)

			ctx, cancel = context.WithCancel(context.Background())
			cancel()
			if err := <-startLegacyRead(ctx, transport, kind, "already-cancelled"); !errors.Is(err, context.Canceled) {
				t.Fatalf("pre-cancelled call returned %v", err)
			}
			select {
			case command := <-commands:
				t.Fatalf("pre-cancelled call sent a command: %s", command.CommandID)
			case <-time.After(25 * time.Millisecond):
			}

			ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			completed = startLegacyRead(ctx, transport, kind, "disconnected-read")
			receiveLegacyCommand(t, ctx, commands)
			_ = peer.Close()
			if err := <-completed; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("disconnected call did not honor its deadline: %v", err)
			}
		})
	}
}

func startLegacyRead(ctx context.Context, transport *LegacyTransport, kind, operationID string) <-chan error {
	completed := make(chan error, 1)
	go func() {
		var err error
		if kind == "shard" {
			_, err = transport.ExecuteShard(ctx, "worker", shared.ShardOperationRequest{
				ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: operationID,
				InstallationID: "native", Cluster: "all", Shard: "Master", Action: shared.ShardActionStatus,
			}, 5)
		} else {
			_, err = transport.ExecuteRuntime(ctx, "worker", shared.RuntimeOperationRequest{
				ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: operationID,
				InstallationID: "native", Cluster: "all", Shard: "Master", Action: shared.RuntimeActionReadArtifacts,
			}, 5)
		}
		completed <- err
	}()
	return completed
}

func receiveLegacyCommand(t *testing.T, ctx context.Context, commands <-chan shared.CommandPayload) shared.CommandPayload {
	t.Helper()
	select {
	case command := <-commands:
		return command
	case <-ctx.Done():
		t.Fatal("command was not sent")
		return shared.CommandPayload{}
	}
}

func replyLegacyRead(t *testing.T, peer *shared.SecureConnection, command shared.CommandPayload) {
	t.Helper()
	var result any
	if request := command.ShardOperation; request != nil {
		result = shared.ShardOperationResult{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: request.OperationID}
	} else if request := command.RuntimeOperation; request != nil {
		result = shared.RuntimeOperationResult{ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: request.OperationID}
	} else {
		t.Fatal("unexpected command type")
	}
	output, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	sendLegacyCommandMessage(t, peer, shared.TypeCommandResp, shared.CommandResponsePayload{
		CommandID: command.CommandID, Success: true, Output: string(output),
	})
}

func sendLegacyCommandMessage(t *testing.T, peer *shared.SecureConnection, kind shared.MessageType, payload any) {
	t.Helper()
	message, err := shared.CreateMessage(kind, "worker", payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.SendEncrypted(message); err != nil {
		t.Fatal(err)
	}
}

func legacyProtocol(kind string) int {
	if kind == "shard" {
		return shared.ShardOperationProtocolVersion
	}
	return shared.RuntimeOperationProtocolVersion
}
