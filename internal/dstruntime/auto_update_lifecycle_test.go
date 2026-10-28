package dstruntime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"
)

type countedMaintenanceCatalog struct {
	maintenanceTestCatalog
	lists atomic.Int32
}

func (c *countedMaintenanceCatalog) List() ([]rooms.Room, error) {
	c.lists.Add(1)
	return c.maintenanceTestCatalog.List()
}

func TestRuntimeUpdateRetriesOnlyStartedWorldWithoutFleetRescan(t *testing.T) {
	_, fixture, _ := newRuntimeTestManager(t)
	catalog := &countedMaintenanceCatalog{maintenanceTestCatalog: maintenanceTestCatalog{fixture}}
	runtime := &maintenanceTestRuntime{calls: make(chan string, 16)}
	u := NewAutoUpdater(catalog, runtime)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); u.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	next := func() string {
		t.Helper()
		select {
		case id := <-runtime.calls:
			return id
		case <-time.After(3 * time.Second):
			t.Fatal("no runtime maintenance")
			return ""
		}
	}
	next()
	next() // Startup checks both installed worlds once.
	u.OnJob(jobs.Job{Kind: "room.start", RoomID: fixture.room.ID, Status: jobs.StatusFailed, Targets: []jobs.Target{
		{TargetID: rooms.EncodeID("Master"), Status: jobs.StatusFailed},
		{TargetID: rooms.EncodeID("Caves"), Status: jobs.StatusSucceeded},
	}})
	if got := next(); got != rooms.EncodeID("Caves") {
		t.Fatalf("maintenance ran for failed world: %s", got)
	}
	if catalog.lists.Load() != 1 {
		t.Fatalf("world start rescanned the fleet: %d", catalog.lists.Load())
	}
}

func TestRuntimeMaintenanceJobSignalsAreBoundedAndRespectCompletedTargets(t *testing.T) {
	u := NewAutoUpdater(nil, nil)
	for i := 0; i < 100; i++ {
		u.OnJob(jobs.Job{Kind: "rooms.restart", Status: jobs.StatusSucceeded,
			Targets: []jobs.Target{
				{TargetID: "room-a:caves", Status: jobs.StatusSucceeded},
				{TargetID: "room-b:master", Status: jobs.StatusFailed},
				{TargetID: "malformed", Status: jobs.StatusSucceeded},
			},
		})
	}
	u.OnJob(jobs.Job{Kind: "room.stop", RoomID: "room-a", Status: jobs.StatusSucceeded, Targets: []jobs.Target{{TargetID: "master", Status: jobs.StatusSucceeded}}})
	u.OnJob(jobs.Job{Kind: "room.start", RoomID: "room-a", Status: jobs.StatusRunning, Targets: []jobs.Target{{TargetID: "master", Status: jobs.StatusSucceeded}}})
	all, targets := u.takeRequests()
	if all || len(targets) != 1 || targets["room-a\x00caves"].worldID != "caves" {
		t.Fatalf("duplicate, failed or unfinished targets were queued: all=%v targets=%+v", all, targets)
	}
}
