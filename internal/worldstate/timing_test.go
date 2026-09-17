package worldstate

import (
	"context"
	"testing"
	"time"

	"dont/internal/requesttiming"
	"dont/internal/rooms"
	"dont/internal/shards"
)

func TestListTimingSeparatesWorldsWithoutExtraReads(t *testing.T) {
	catalog := stateTestCatalog{
		room: rooms.Room{ID: "room", DirectoryName: "Cluster_1", Managed: true},
		worlds: []rooms.World{
			{ID: "master", RoomID: "room", DirectoryName: "Master", Role: rooms.WorldRoleMaster},
			{ID: "caves", RoomID: "room", DirectoryName: "Caves", Role: rooms.WorldRoleCaves},
		},
	}
	runtime := &stateStatusRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	sampler := &stateTestCurrentSampler{current: map[string]Observation{
		"master": {Season: "winter", Phase: "day", CapturedAt: time.Now()},
		"caves":  {Season: "winter", Phase: "night", CapturedAt: time.Now()},
	}}
	service, err := NewService(catalog, runtime, newWorldStateStore(t), sampler)
	if err != nil {
		t.Fatal(err)
	}
	ctx, timing := requesttiming.New(context.Background())
	list, err := service.List(ctx, "room")
	if err != nil || list.Total != 2 || runtime.calls.Load() != 2 || sampler.currentCalls.Load() != 2 {
		t.Fatalf("timing changed collection: %#v, %v", list, err)
	}
	_, metrics := timing.Snapshot()
	counts := map[string]int{}
	for _, metric := range metrics {
		counts[metric.Name] = metric.Calls
	}
	for _, name := range []string{"state.collect", "state.result", "world_master.status", "world_master.sample", "world_caves.status", "world_caves.sample"} {
		if counts[name] != 1 {
			t.Fatalf("missing or repeated measurement %s: %#v", name, counts)
		}
	}
	if counts["state.stored_read"] != 0 || counts["state.merge"] != 0 {
		t.Fatalf("current read still loaded or merged stored state: %#v", counts)
	}
}
