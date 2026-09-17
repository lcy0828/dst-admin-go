package runtimeobservation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/shared"
)

type observationSource struct {
	mu                 sync.Mutex
	items              []agents.RuntimeTargetInventory
	collect            func(context.Context, agents.RuntimeTargetInventory) (agents.RuntimeTargetInventory, error)
	collects           int
	checkpoints        int
	checkpointFailures int
}

func (s *observationSource) CachedRuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agents.RuntimeTargetInventory(nil), s.items...), nil
}

func (s *observationSource) CollectRuntimeTargetInventory(ctx context.Context, targetID, installationID string) (agents.RuntimeTargetInventory, error) {
	s.mu.Lock()
	s.collects++
	var selected agents.RuntimeTargetInventory
	for _, item := range s.items {
		if item.Target.ID == targetID && item.Target.Config.InstallationID == installationID {
			selected = item
			break
		}
	}
	collect := s.collect
	s.mu.Unlock()
	if collect != nil {
		return collect(ctx, selected)
	}
	return selected, nil
}

func (s *observationSource) CheckpointRuntimeTargetInventory(string, shared.RuntimeInventoryReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoints++
	if s.checkpointFailures > 0 {
		s.checkpointFailures--
		return errors.New("checkpoint unavailable")
	}
	return nil
}

func observationInventory(now time.Time) agents.RuntimeTargetInventory {
	return agents.RuntimeTargetInventory{
		Target: agents.RuntimeTarget{
			ID: "agent:node", AgentID: "node", Name: "node", Online: true, Configured: true,
			Config: agents.RuntimeConfig{InstallationID: "default"},
		},
		Available: true,
		Inventory: shared.RuntimeInventoryReport{
			ProtocolVersion: 1, ObservedAt: now,
			Installation: shared.RuntimeInstallationReport{ID: "default"},
			Rooms:        []shared.RoomInventoryReport{}, Processes: []shared.ShardProcessReport{}, Warnings: []string{},
		},
		ObservedAt: &now, ReceivedAt: &now,
	}
}

func TestRefreshStoresHotSnapshotAndAvoidsRepeatedCheckpoints(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	item := observationInventory(now)
	source := &observationSource{items: []agents.RuntimeTargetInventory{item}}
	coordinator, err := New(source, Options{Now: func() time.Time { return now }, CheckpointEvery: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	for index := 0; index < 2; index++ {
		results, refreshErr := coordinator.Refresh(context.Background(), Scope{TargetID: item.Target.ID})
		if refreshErr != nil || len(results) != 1 || results[0].State != StateFresh {
			t.Fatalf("refresh %d = %#v, %v", index, results, refreshErr)
		}
	}
	items, err := coordinator.RuntimeTargetInventories(context.Background())
	if err != nil || len(items) != 1 || items[0].ObservationState != string(StateFresh) || items[0].Stale {
		t.Fatalf("hot inventory = %#v, %v", items, err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.collects != 2 || source.checkpoints != 1 {
		t.Fatalf("collects=%d checkpoints=%d", source.collects, source.checkpoints)
	}
}

func TestConcurrentRefreshCoalescesOneEndpoint(t *testing.T) {
	now := time.Now().UTC()
	item := observationInventory(now)
	started, release := make(chan struct{}), make(chan struct{})
	source := &observationSource{items: []agents.RuntimeTargetInventory{item}}
	source.collect = func(ctx context.Context, selected agents.RuntimeTargetInventory) (agents.RuntimeTargetInventory, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-ctx.Done():
			return agents.RuntimeTargetInventory{}, ctx.Err()
		case <-release:
			return selected, nil
		}
	}
	coordinator, _ := New(source)
	errorsChannel := make(chan error, 2)
	go func() {
		_, err := coordinator.Refresh(context.Background(), Scope{TargetID: item.Target.ID})
		errorsChannel <- err
	}()
	<-started
	go func() {
		_, err := coordinator.Refresh(context.Background(), Scope{TargetID: item.Target.ID})
		errorsChannel <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		coordinator.mu.Lock()
		waiting := coordinator.flights[endpointKey(item.Target.ID, "default")].waiters
		coordinator.mu.Unlock()
		if waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second refresh did not join the endpoint flight")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for index := 0; index < 2; index++ {
		if err := <-errorsChannel; err != nil {
			t.Fatal(err)
		}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.collects != 1 {
		t.Fatalf("collects=%d", source.collects)
	}
}

func TestCheckpointFailureRetriesOnNextRefresh(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	item := observationInventory(now)
	source := &observationSource{
		items:              []agents.RuntimeTargetInventory{item},
		checkpointFailures: 1,
	}
	coordinator, err := New(source, Options{
		Now: func() time.Time { return now }, CheckpointEvery: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	for index := 0; index < 3; index++ {
		if _, refreshErr := coordinator.Refresh(context.Background(), Scope{TargetID: item.Target.ID}); refreshErr != nil {
			t.Fatalf("refresh %d: %v", index, refreshErr)
		}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.checkpoints != 2 {
		t.Fatalf("checkpoints=%d, want initial failure followed by one retry", source.checkpoints)
	}
}

func TestHotSnapshotBecomesStaleWithoutDiscardingInventory(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	item := observationInventory(now)
	source := &observationSource{items: []agents.RuntimeTargetInventory{item}}
	coordinator, _ := New(source, Options{Now: func() time.Time { return now }, FreshnessWindow: 90 * time.Second})
	if _, err := coordinator.Refresh(context.Background(), Scope{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(91 * time.Second)
	items, err := coordinator.RuntimeTargetInventories(context.Background())
	if err != nil || len(items) != 1 || !items[0].Available || !items[0].Stale || items[0].ObservationState != string(StateStale) {
		t.Fatalf("stale hot inventory = %#v, %v", items, err)
	}
}

func TestEnsureRuntimeTargetsFreshReturnsRecognizableUnavailableError(t *testing.T) {
	now := time.Now().UTC()
	item := observationInventory(now)
	item.Target.Online = false
	source := &observationSource{items: []agents.RuntimeTargetInventory{item}}
	coordinator, err := New(source)
	if err != nil {
		t.Fatal(err)
	}

	err = coordinator.EnsureRuntimeTargetsFresh(context.Background(), []string{item.Target.ID})
	if !errors.Is(err, ErrObservationUnavailable) {
		t.Fatalf("error=%v, want ErrObservationUnavailable", err)
	}
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || unavailable.TargetID != item.Target.ID || unavailable.State != StateOffline {
		t.Fatalf("unavailable error=%#v", unavailable)
	}
}
