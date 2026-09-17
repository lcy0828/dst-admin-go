package runtimeobservation

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/shared"
)

func TestRefreshEmitsViewChangesOnlyForChangedContentOrAvailability(t *testing.T) {
	item := observationInventory(time.Now().UTC())
	source := &observationSource{items: []agents.RuntimeTargetInventory{item}}
	coordinator, err := New(source)
	if err != nil {
		t.Fatal(err)
	}
	updates, unsubscribe := coordinator.Subscribe()
	defer unsubscribe()
	refresh := func(want string) {
		t.Helper()
		if _, err := coordinator.Refresh(context.Background(), Scope{}); err != nil {
			t.Fatal(err)
		}
		if event := <-updates; event.Type != "observation.refreshing" {
			t.Fatalf("start event=%#v", event)
		}
		if event := <-updates; event.Type != want {
			t.Fatalf("completion event=%#v, want %s", event, want)
		}
		select {
		case event := <-updates:
			t.Fatalf("unexpected extra event=%#v", event)
		default:
		}
	}
	refresh("inventory.updated")
	refresh("observation.refreshed")
	source.items[0].Inventory.ObservedAt = time.Now().UTC()
	source.items[0].Inventory.Memory.AvailableBytes++
	refresh("observation.refreshed")
	source.items[0].Inventory.Processes = []shared.ShardProcessReport{{PID: 123, Cluster: "all", Shard: "Master"}}
	refresh("inventory.updated")
	refresh("observation.refreshed")
	source.collect = func(context.Context, agents.RuntimeTargetInventory) (agents.RuntimeTargetInventory, error) {
		return agents.RuntimeTargetInventory{}, errors.New("node unavailable")
	}
	refresh("observation.error")
	refresh("observation.refreshed")
	source.collect = nil
	refresh("inventory.updated")
	refresh("observation.refreshed")
}

func TestCompletedMutationNotifiesViewsEvenWhenProcessIdentityIsUnchanged(t *testing.T) {
	item := observationInventory(time.Now().UTC())
	coordinator, err := New(&observationSource{items: []agents.RuntimeTargetInventory{item}})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	if _, err := coordinator.Refresh(context.Background(), Scope{}); err != nil {
		t.Fatal(err)
	}
	updates, unsubscribe := coordinator.Subscribe()
	defer unsubscribe()
	coordinator.RuntimeTargetChanged(item.Target.ID)
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		select {
		case event := <-updates:
			if event.Type != "topology.changed" {
				continue
			}
			if event.Observation.TargetID != item.Target.ID || event.Observation.State != StateFresh ||
				event.Observation.ReceivedAt == nil || !event.Observation.ReceivedAt.Equal(*item.ReceivedAt) {
				t.Fatalf("mutation notification=%#v", event)
			}
			return
		case <-timeout.C:
			t.Fatal("unchanged inventory hid the completed mutation")
		}
	}
}
