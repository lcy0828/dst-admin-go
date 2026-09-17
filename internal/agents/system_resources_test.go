package agents

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type systemResourceTransport struct {
	*MemoryTransport
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	err     error
}

func (t *systemResourceTransport) Execute(ctx context.Context, id string, action Action, timeout int) (ExecutionResult, error) {
	t.calls.Add(1)
	if t.started != nil {
		t.started <- struct{}{}
	}
	if t.release != nil {
		select {
		case <-ctx.Done():
			return ExecutionResult{}, ctx.Err()
		case <-t.release:
		}
	}
	if t.err != nil {
		return ExecutionResult{}, t.err
	}
	return t.MemoryTransport.Execute(ctx, id, action, timeout)
}

func TestSystemResourceRefreshReadsNewMetricsWithoutCreatingCommand(t *testing.T) {
	service, store, _, transport := newAgentTestService(t)
	before, _ := service.Agent("agent-primary")
	now := before.Metrics.ObservedAt.Add(5 * time.Second)
	transport.now = func() time.Time { return now }
	after, err := service.RefreshSystemInfo(context.Background(), "agent-primary")
	if err != nil || !after.Metrics.ObservedAt.Equal(now) || after.Metrics.UptimeSeconds != before.Metrics.UptimeSeconds+1 {
		t.Fatalf("after=%#v err=%v", after, err)
	}
	commands, err := store.Commands(CommandFilter{Limit: 10})
	if err != nil || len(commands.Items) != 0 {
		t.Fatalf("resource reads created commands: %#v err=%v", commands, err)
	}
	if _, err := service.RefreshSystemInfo(context.Background(), "agent-offline"); !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("offline refresh error=%v", err)
	}
}

func TestSystemResourceRefreshSharesPendingCollectionAndLetsWaiterCancel(t *testing.T) {
	service, _, _, memory := newAgentTestService(t)
	transport := &systemResourceTransport{MemoryTransport: memory, started: make(chan struct{}, 2), release: make(chan struct{})}
	service.transport = transport
	done := make(chan error, 1)
	go func() { _, err := service.RefreshSystemInfo(context.Background(), "agent-primary"); done <- err }()
	<-transport.started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := service.RefreshSystemInfo(ctx, "agent-primary"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter error=%v", err)
	}
	if transport.calls.Load() != 1 {
		t.Fatalf("overlapping reads sent %d reports", transport.calls.Load())
	}
	close(transport.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	transport.err = errors.New("report connection failed")
	if _, err := service.RefreshSystemInfo(context.Background(), "agent-primary"); !errors.Is(err, transport.err) {
		t.Fatalf("failed report error=%v", err)
	}
	transport.err = nil
	transport.started = nil
	if _, err := service.RefreshSystemInfo(context.Background(), "agent-primary"); err != nil {
		t.Fatalf("failed refresh blocked later attempts: %v", err)
	}
}
