package agents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	legacyserver "dont/server"
	"dont/shared"

	"github.com/gorilla/websocket"
)

func TestLegacyInventoryGatewayInterleavedReports(t *testing.T) {
	transport, peer, requests := inventoryGatewayPeer(t)
	config := RuntimeConfig{InstallationID: "native", SavePath: "/opt/dst/saves", ServerPath: "/opt/dst/server"}
	send := func(t *testing.T, kind string, active bool, data map[string]interface{}) {
		t.Helper()
		messageType := shared.TypePassiveReport
		if active {
			messageType = shared.TypeActiveReport
		}
		message, err := shared.CreateMessage(messageType, "worker", shared.ReportDataPayload{
			ReportID: shared.GenerateUUID(), ReportType: kind, Data: data,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := peer.SendEncrypted(message); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name       string
		wantError  string
		active     bool
		delayReply bool
	}{
		{name: "success followed by passive system report"},
		{name: "inventory error followed by system report", wantError: "inventory read failed"},
		{name: "recovery followed by active system report", active: true},
		{name: "next request ignores previous inventory", delayReply: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			type result struct {
				report shared.RuntimeInventoryReport
				err    error
			}
			completed := make(chan result, 1)
			started := time.Now()
			go func() {
				report, err := transport.Inventory(ctx, "worker", config, 20)
				completed <- result{report: report, err: err}
			}()
			select {
			case kind := <-requests:
				if kind != "dst_runtime_inventory" {
					t.Fatalf("unexpected request: %s", kind)
				}
			case <-ctx.Done():
				t.Fatal("inventory request was not sent")
			}
			if test.delayReply {
				send(t, "system_info", true, map[string]interface{}{"hostname": "worker"})
				select {
				case got := <-completed:
					t.Fatalf("unrelated report completed a new request with old data: %#v", got)
				case <-time.After(250 * time.Millisecond):
				}
			}
			observedAt := time.Now().UTC()
			data := map[string]interface{}{"inventory": shared.RuntimeInventoryReport{
				ProtocolVersion: shared.RuntimeInventoryProtocolVersion, ObservedAt: observedAt,
				Installation: shared.RuntimeInstallationReport{ID: config.InstallationID},
			}}
			if test.wantError != "" {
				data = map[string]interface{}{"error": test.wantError}
			}
			send(t, "dst_runtime_inventory", false, data)
			send(t, "system_info", test.active, map[string]interface{}{"hostname": "worker", "error": "unrelated resource error"})
			select {
			case got := <-completed:
				if test.wantError != "" {
					if got.err == nil || got.err.Error() != test.wantError {
						t.Fatalf("inventory error was lost: %v", got.err)
					}
				} else if got.err != nil || !got.report.ObservedAt.Equal(observedAt) || got.report.Installation.ID != config.InstallationID {
					t.Fatalf("inventory response mismatch: report=%#v err=%v", got.report, got.err)
				}
				t.Logf("inventory completed in %s", time.Since(started).Round(time.Millisecond))
			case <-ctx.Done():
				t.Fatal("received inventory was hidden by an unrelated report")
			}
		})
	}
	t.Run("inventory and resource refresh complete independently", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		completed := make(chan error, 2)
		go func() {
			_, err := transport.Inventory(ctx, "worker", config, 20)
			completed <- err
		}()
		go func() {
			_, err := transport.Execute(ctx, "worker", ActionSystemRefresh, 20)
			completed <- err
		}()
		seen := map[string]bool{}
		for len(seen) < 2 {
			select {
			case kind := <-requests:
				seen[kind] = true
			case <-ctx.Done():
				t.Fatal("concurrent report requests were not sent")
			}
		}
		if !seen["dst_runtime_inventory"] || !seen["system_info"] {
			t.Fatalf("unexpected requests: %#v", seen)
		}
		send(t, "dst_runtime_inventory", false, map[string]interface{}{"inventory": shared.RuntimeInventoryReport{
			ProtocolVersion: shared.RuntimeInventoryProtocolVersion, ObservedAt: time.Now().UTC(),
			Installation: shared.RuntimeInstallationReport{ID: config.InstallationID},
		}})
		send(t, "system_info", false, map[string]interface{}{"hostname": "worker"})
		for count := 0; count < 2; count++ {
			select {
			case err := <-completed:
				if err != nil {
					t.Fatalf("concurrent refresh failed: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("concurrent refresh did not complete")
			}
		}
	})
	t.Run("cancelled request does not trigger collection", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := transport.Inventory(ctx, "worker", config, 20); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled inventory returned %v", err)
		}
		select {
		case kind := <-requests:
			t.Fatalf("cancelled request sent %s", kind)
		case <-time.After(50 * time.Millisecond):
		}
	})
	t.Run("missing response still times out", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if _, err := transport.Inventory(ctx, "worker", config, 20); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("missing response returned %v", err)
		}
	})
}

func inventoryGatewayPeer(t *testing.T) (*LegacyTransport, *shared.SecureConnection, <-chan string) {
	t.Helper()
	transport, peer, reports, _ := legacyGatewayPeer(t)
	return transport, peer, reports
}

func legacyGatewayPeer(t *testing.T) (*LegacyTransport, *shared.SecureConnection, <-chan string, <-chan shared.CommandPayload) {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	gateway, err := legacyserver.NewServer(&legacyserver.Config{KeyFile: filepath.Join(t.TempDir(), "gateway.conf"), SecurityKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gateway.Stop)
	httpServer := httptest.NewServer(gateway.Handler())
	t.Cleanup(httpServer.Close)
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http"), http.Header{"Authorization": {"Bearer " + key}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	keys, err := shared.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	register, err := shared.CreateMessage(shared.TypeRegister, "worker", shared.RegisterPayload{
		PublicKey: shared.EncodePublicKey(keys.PublicKey), AgentUUID: "worker", Hostname: "worker", OS: "linux", Arch: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteJSON(register); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ack shared.Message
	if err := connection.ReadJSON(&ack); err != nil {
		t.Fatal(err)
	}
	var payload shared.RegisterAckPayload
	if err := json.Unmarshal(ack.Payload, &payload); err != nil || !payload.Success {
		t.Fatalf("registration failed: %#v, %v", payload, err)
	}
	serverKey, err := shared.DecodePublicKey(payload.ServerPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	peer := shared.NewSecureConnection(connection, keys, false)
	peer.SetRemotePublicKey(serverKey)
	requests := make(chan string, 16)
	commands := make(chan shared.CommandPayload, 16)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			message, err := peer.ReadEncrypted()
			if err != nil {
				return
			}
			if message.Type == shared.TypeCommand {
				var command shared.CommandPayload
				if err := json.Unmarshal(message.Payload, &command); err == nil {
					commands <- command
				}
				continue
			}
			if message.Type != shared.TypePassiveReport && message.Type != shared.TypeReportRequest {
				continue
			}
			var request struct {
				ReportType string `json:"report_type"`
			}
			if err := json.Unmarshal(message.Payload, &request); err == nil {
				requests <- request.ReportType
			}
		}
	}()
	t.Cleanup(func() {
		_ = peer.Close()
		<-readDone
	})
	// The initial system request also confirms registration is complete.
	select {
	case kind := <-requests:
		if kind != "system_info" {
			t.Fatalf("unexpected initial report request: %s", kind)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not finish registration")
	}
	return NewLegacyTransport(func() *legacyserver.Server { return gateway }), peer, requests, commands
}
