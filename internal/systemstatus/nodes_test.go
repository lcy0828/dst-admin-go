package systemstatus

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/agents"
)

type nodeAgentCatalog struct {
	items     []agents.Agent
	listCalls int
	getCalls  int
	refresh   func(context.Context, string) (agents.Agent, error)
}

func (c *nodeAgentCatalog) RefreshSystemInfo(ctx context.Context, id string) (agents.Agent, error) {
	if c.refresh != nil {
		return c.refresh(ctx, id)
	}
	return agents.Agent{}, errors.New("unexpected live refresh")
}

func (c *nodeAgentCatalog) Agents() ([]agents.Agent, bool, error) {
	c.listCalls++
	return append([]agents.Agent(nil), c.items...), true, nil
}

func (c *nodeAgentCatalog) Agent(id string) (agents.Agent, error) {
	c.getCalls++
	for _, item := range c.items {
		if item.ID == id {
			return item, nil
		}
	}
	return agents.Agent{}, agents.ErrAgentNotFound
}

func TestNodeResourcesUseOneContractForLocalAndAgent(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	localProvider := NewMemoryProvider()
	localProvider.now = func() time.Time { return now }
	local := NewService(localProvider)
	catalog := &nodeAgentCatalog{items: []agents.Agent{{
		ID: "debian12", DisplayName: "Debian 12", Hostname: "debian12", OS: "linux", Arch: "amd64",
		Version: "2.9.0", Status: agents.StatusOnline, LastReportAt: &now, MetricsStale: true, StaleReason: "report_expired",
		Metrics: agents.Metrics{
			LogicalProcessors: 16, PhysicalCores: 8, CPUModel: "Remote CPU", CPUUsage: 12.5,
			CPUCoreUsage: []float64{10, 15}, CPUUsageAvailable: true, Load1: 1, Load5: 0.5, Load15: 0.25, LoadSupported: true,
			MemoryTotal: 8 << 30, MemoryUsed: 2 << 30, MemoryAvailable: 6 << 30,
			DiskPath: "/srv", DiskTotal: 100 << 30, DiskUsed: 25 << 30, DiskAvailable: 75 << 30,
			DiskUsage: 25, DiskUsageAvailable: true, UptimeSeconds: 3600, ObservedAt: &now,
		},
	}}}
	service, err := NewNodeResourceService(local, catalog)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }

	snapshot, err := service.Snapshot("")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Total != 2 || len(snapshot.Items) != 2 || snapshot.Items[0].TargetID != "local" || snapshot.Items[1].TargetID != "agent:debian12" {
		t.Fatalf("unexpected node snapshot: %#v", snapshot)
	}
	remote := snapshot.Items[1]
	if !remote.Online || !remote.Stale || remote.StaleReason != "report_expired" || remote.CPU.Usage != 12.5 || !remote.CPU.UsageAvailable {
		t.Fatalf("unexpected remote resource: %#v", remote)
	}
	if remote.Memory.Usage != 25 || remote.Disk.Usage != 25 || remote.AgentVersion != "2.9.0" {
		t.Fatalf("remote metrics were not normalized: %#v", remote)
	}
}

func TestNodeResourcesResolveLocalWithoutAgentRoundTrip(t *testing.T) {
	catalog := &nodeAgentCatalog{}
	service, err := NewNodeResourceService(NewService(NewMemoryProvider()), catalog)
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := service.Snapshot("local")
	if err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].TargetID != "local" {
		t.Fatalf("local snapshot=%#v err=%v", snapshot, err)
	}
	if catalog.listCalls != 0 || catalog.getCalls != 0 {
		t.Fatalf("local status queried remote catalog: list=%d get=%d", catalog.listCalls, catalog.getCalls)
	}
	if _, err := service.Snapshot("agent:missing"); !errors.Is(err, ErrNodeResourceNotFound) {
		t.Fatalf("missing target error=%v", err)
	}
}

func TestNodeResourceRefreshCollectsOnlyTheSelectedAgent(t *testing.T) {
	before := time.Now().UTC().Add(-time.Minute)
	after := before.Add(5 * time.Second)
	provider := &countingProvider{now: time.Now}
	catalog := &nodeAgentCatalog{items: []agents.Agent{{
		ID: "debian12", Status: agents.StatusOnline, Metrics: agents.Metrics{ObservedAt: &before, CPUUsage: 1},
	}}}
	var calls int
	catalog.refresh = func(_ context.Context, id string) (agents.Agent, error) {
		calls++
		if id != "debian12" {
			t.Fatalf("refreshed unrelated agent %s", id)
		}
		item := catalog.items[0]
		item.Metrics.ObservedAt = &after
		item.Metrics.CPUUsage = 12
		return item, nil
	}
	service, _ := NewNodeResourceService(NewService(provider), catalog)
	passive, _ := service.Snapshot("agent:debian12")
	if calls != 0 || !passive.Items[0].ObservedAt.Equal(before) {
		t.Fatal("passive reads should retain the latest existing report")
	}
	fresh, err := service.Refresh(context.Background(), "agent:debian12")
	if err != nil || calls != 1 || fresh.Items[0].CPU.Usage != 12 || !fresh.Items[0].ObservedAt.Equal(after) {
		t.Fatalf("fresh=%#v calls=%d err=%v", fresh, calls, err)
	}
	if provider.calls.Load() != 0 || catalog.listCalls != 0 {
		t.Fatal("selected remote refresh collected unrelated machines")
	}
}

func TestNodeResourceRefreshBypassesLocalStatusCache(t *testing.T) {
	now := time.Now().UTC()
	provider := &countingProvider{now: func() time.Time { return now }}
	catalog := &nodeAgentCatalog{}
	service, _ := NewNodeResourceService(NewService(provider), catalog)
	_, _ = service.Snapshot("local")
	now = now.Add(time.Millisecond)
	fresh, err := service.Refresh(context.Background(), "local")
	if err != nil || provider.calls.Load() != 2 || !fresh.Items[0].ObservedAt.Equal(now) {
		t.Fatalf("fresh=%#v calls=%d err=%v", fresh, provider.calls.Load(), err)
	}
	if catalog.listCalls != 0 || catalog.getCalls != 0 {
		t.Fatal("local refresh must not query remote Agents")
	}
}

func TestFleetResourceRefreshIsBoundedAndPreservesFailedSamples(t *testing.T) {
	before := time.Now().UTC().Add(-time.Minute)
	after := before.Add(5 * time.Second)
	catalog := &nodeAgentCatalog{}
	for index := range 10 {
		catalog.items = append(catalog.items, agents.Agent{
			ID: fmt.Sprint(index), Status: agents.StatusOnline, Metrics: agents.Metrics{ObservedAt: &before},
		})
	}
	catalog.items[9].Status = agents.StatusOffline
	var active, peak, calls atomic.Int32
	catalog.refresh = func(ctx context.Context, id string) (agents.Agent, error) {
		calls.Add(1)
		count := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); count > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, count) {
				break
			}
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return agents.Agent{}, ctx.Err()
		}
		if id == "0" {
			return agents.Agent{}, context.DeadlineExceeded
		}
		return agents.Agent{ID: id, Status: agents.StatusOnline, Metrics: agents.Metrics{ObservedAt: &after}}, nil
	}
	service, _ := NewNodeResourceService(NewService(NewMemoryProvider()), catalog)
	fresh, err := service.Refresh(context.Background(), "")
	if err != nil || fresh.Total != 11 || calls.Load() != 9 || peak.Load() > 4 || peak.Load() < 2 {
		t.Fatalf("total=%d calls=%d peak=%d err=%v", fresh.Total, calls.Load(), peak.Load(), err)
	}
	failed := fresh.Items[1]
	if !failed.Stale || failed.StaleReason != "refresh_failed" || len(failed.Warnings) == 0 || !failed.ObservedAt.Equal(before) {
		t.Fatalf("failed collection must preserve its time and expose the failure: %#v", failed)
	}
	if !fresh.Items[2].ObservedAt.Equal(after) || fresh.Items[10].Online {
		t.Fatal("one failed machine must not discard successful refreshes or hide offline machines")
	}
}

func TestNodeResourceRefreshHonorsRequestCancellation(t *testing.T) {
	before := time.Now().UTC().Add(-time.Minute)
	catalog := &nodeAgentCatalog{items: []agents.Agent{{ID: "remote", Status: agents.StatusOnline, Metrics: agents.Metrics{ObservedAt: &before}}}}
	catalog.refresh = func(ctx context.Context, _ string) (agents.Agent, error) {
		<-ctx.Done()
		return agents.Agent{}, ctx.Err()
	}
	service, _ := NewNodeResourceService(NewService(NewMemoryProvider()), catalog)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	fresh, err := service.Refresh(ctx, "agent:remote")
	if err != nil || !fresh.Items[0].Stale || !fresh.Items[0].ObservedAt.Equal(before) {
		t.Fatalf("canceled refresh=%#v err=%v", fresh, err)
	}
}
