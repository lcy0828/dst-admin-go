package systemstatus

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalProviderReportsConfiguredRuntimeDisk(t *testing.T) {
	runtimeRoot := t.TempDir()
	provider := NewLocalProvider(runtimeRoot)
	status := provider.Status()
	if !status.Disk.Available || status.Disk.Path != runtimeRoot {
		t.Fatalf("configured Runtime disk was not reported: %#v", status.Disk)
	}
}

type countingProvider struct {
	calls   atomic.Int32
	now     func() time.Time
	started chan struct{}
	release chan struct{}
}

func (p *countingProvider) Status() Status {
	p.calls.Add(1)
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
	if p.release != nil {
		<-p.release
	}
	return Status{ObservedAt: p.now()}
}

func TestServiceCachesStatusAndDatabaseIndependently(t *testing.T) {
	now := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	provider := &countingProvider{now: func() time.Time { return now }}
	service := NewService(provider)
	service.now = func() time.Time { return now }
	service.statusTTL = time.Second
	service.databaseTTL = 30 * time.Second
	var databaseCalls atomic.Int32
	service.SetDatabaseProvider(func() (DatabaseStatus, error) {
		databaseCalls.Add(1)
		return DatabaseStatus{Driver: "sqlite3"}, nil
	})

	first := service.Status()
	second := service.Status()
	if provider.calls.Load() != 1 || databaseCalls.Load() != 1 {
		t.Fatalf("cached request repeated collection: provider=%d database=%d", provider.calls.Load(), databaseCalls.Load())
	}
	if !first.Database.Available || !second.Database.Available {
		t.Fatal("cached database status was not marked available")
	}

	now = now.Add(2 * time.Second)
	service.Status()
	if provider.calls.Load() != 2 || databaseCalls.Load() != 1 {
		t.Fatalf("short status refresh should reuse database health: provider=%d database=%d", provider.calls.Load(), databaseCalls.Load())
	}

	now = now.Add(31 * time.Second)
	service.Status()
	if provider.calls.Load() != 3 || databaseCalls.Load() != 2 {
		t.Fatalf("expired database health was not refreshed: provider=%d database=%d", provider.calls.Load(), databaseCalls.Load())
	}
}

func TestServiceSharesConcurrentCollection(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	provider := &countingProvider{now: time.Now, started: started, release: release}
	service := NewService(provider)
	service.SetDatabaseProvider(func() (DatabaseStatus, error) {
		return DatabaseStatus{Driver: "sqlite3"}, nil
	})

	const consumers = 12
	results := make(chan Status, consumers)
	for range consumers {
		go func() { results <- service.Status() }()
	}
	<-started
	close(release)
	for range consumers {
		<-results
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("concurrent requests collected status %d times, want 1", calls)
	}
}
