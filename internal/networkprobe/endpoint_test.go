package networkprobe

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"dont/shared"
)

func TestListenEndpointAndProbeEndpoints(t *testing.T) {
	port := availableUDPPort(t)
	token := "0123456789abcdef0123456789abcdef"
	listenResult := make(chan struct {
		tokens []string
		err    error
	}, 1)
	go func() {
		tokens, err := ListenEndpoint(context.Background(), "127.0.0.1", port, []string{token}, 3*time.Second)
		listenResult <- struct {
			tokens []string
			err    error
		}{tokens: tokens, err: err}
	}()

	results := ProbeEndpoints(context.Background(), []shared.RuntimeNetworkEndpointRequest{{
		Address: "127.0.0.1", Port: port, Token: token,
	}}, 2*time.Second)
	if len(results) != 1 || !results[0].Reachable || results[0].LatencyMillis < 1 {
		t.Fatalf("probe results=%#v", results)
	}
	listened := <-listenResult
	if listened.err != nil || len(listened.tokens) != 1 || listened.tokens[0] != token {
		t.Fatalf("listen tokens=%v err=%v", listened.tokens, listened.err)
	}
}

func TestProbeEndpointsReportsUnreachableWithoutFailingBatch(t *testing.T) {
	port := availableUDPPort(t)
	results := ProbeEndpoints(context.Background(), []shared.RuntimeNetworkEndpointRequest{{
		Address: "127.0.0.1", Port: port, Token: "abcdef0123456789abcdef0123456789",
	}}, 400*time.Millisecond)
	if len(results) != 1 || results[0].Reachable || results[0].Error == "" {
		t.Fatalf("probe results=%#v", results)
	}
}

func TestProbeEndpointsRetriesUntilListenerStarts(t *testing.T) {
	port := availableUDPPort(t)
	token := "0123456789abcdef0123456789abcdef"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	listened := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		tokens, err := ListenEndpoint(ctx, "127.0.0.1", port, []string{token}, time.Second)
		if err == nil && (len(tokens) != 1 || tokens[0] != token) {
			err = fmt.Errorf("unexpected probe tokens: %v", tokens)
		}
		listened <- err
	}()
	results := ProbeEndpoints(ctx, []shared.RuntimeNetworkEndpointRequest{{Address: "127.0.0.1", Port: port, Token: token}}, time.Second)
	if len(results) != 1 || !results[0].Reachable {
		t.Fatalf("listener startup was treated as unreachable: %#v", results)
	}
	if err := <-listened; err != nil {
		t.Fatal(err)
	}
}

func TestProbeEndpointsCancellationInterruptsRetryBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	results := ProbeEndpoints(ctx, []shared.RuntimeNetworkEndpointRequest{{Address: "127.0.0.1", Port: availableUDPPort(t), Token: "0123456789abcdef0123456789abcdef"}}, time.Second)
	if len(results) != 1 || results[0].Reachable || results[0].Error == "" || time.Since(started) > 300*time.Millisecond {
		t.Fatalf("cancelled probe was not bounded: %#v, elapsed=%s", results, time.Since(started))
	}
}

func availableUDPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, value, err := net.SplitHostPort(listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return port
}
