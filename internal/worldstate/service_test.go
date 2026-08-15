package worldstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/rooms"
	"dont/internal/shards"
)

type stateTestCatalog struct {
	room   rooms.Room
	worlds []rooms.World
}

func (c stateTestCatalog) Room(id string) (rooms.Room, error) {
	if id != c.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return c.room, nil
}
func (c stateTestCatalog) Worlds(roomID string) ([]rooms.World, error) {
	if roomID != c.room.ID {
		return nil, rooms.ErrRoomNotFound
	}
	return append([]rooms.World(nil), c.worlds...), nil
}
func (c stateTestCatalog) World(roomID, worldID string) (rooms.World, error) {
	if roomID != c.room.ID {
		return rooms.World{}, rooms.ErrRoomNotFound
	}
	for _, world := range c.worlds {
		if world.ID == worldID {
			return world, nil
		}
	}
	return rooms.World{}, rooms.ErrWorldNotFound
}

type stateTestRuntime struct{ running bool }

func (r *stateTestRuntime) IsRunning(context.Context, string, string) (bool, error) {
	return r.running, nil
}

type stateStatusRuntime struct{ status shards.RuntimeStatus }

func (r *stateStatusRuntime) IsRunning(context.Context, string, string) (bool, error) {
	return r.status.State == shards.RuntimeRunning, nil
}
func (r *stateStatusRuntime) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	return r.status, nil
}

type stateTestSampler struct {
	observation Observation
	err         error
}

func (s *stateTestSampler) Snapshot(context.Context, string, string) (Observation, error) {
	return s.observation, s.err
}

type stateTestCurrentSampler struct {
	stateTestSampler
	current      map[string]Observation
	currentCalls int
}

func (s *stateTestCurrentSampler) CurrentSnapshot(_ context.Context, _, worldID string) (Observation, error) {
	s.currentCalls++
	observation, exists := s.current[worldID]
	if !exists {
		return Observation{}, errors.New("current snapshot unavailable")
	}
	return observation, nil
}

func TestServiceRefreshPersistsTimeSeriesAndRejectsStoppedWorld(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	runtime := &stateTestRuntime{running: true}
	progress := .25
	cycles := 12
	sampler := &stateTestSampler{observation: Observation{Season: "autumn", Phase: "day", Cycles: &cycles, SeasonProgress: &progress}}
	service, err := NewService(catalog, runtime, newWorldStateStore(t), sampler)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 || list.Items[0].Season != "autumn" || list.LastRefreshedAt == nil || !list.LastRefreshedAt.Equal(now) {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	history, err := service.History("room", "master", 120)
	if err != nil || history.Total != 1 {
		t.Fatalf("history=%#v err=%v", history, err)
	}
	runtime.running = false
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); !errors.Is(err, ErrWorldNotRunning) {
		t.Fatalf("stopped world error = %v", err)
	}
	if _, err := service.History("room", "master", 0); !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("invalid limit error = %v", err)
	}
}

func TestServiceRefreshUsesRuntimeCaptureTimeWhenProvided(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	capturedAt := time.Date(2026, 8, 12, 10, 0, 1, 0, time.UTC)
	service, err := NewService(catalog, &stateTestRuntime{running: true}, newWorldStateStore(t), &stateTestSampler{
		observation: Observation{Season: "winter", CapturedAt: capturedAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return capturedAt.Add(4 * time.Second) }
	result, err := service.RefreshWorld(context.Background(), "room", "master")
	if err != nil || !result.ObservedAt.Equal(capturedAt) {
		t.Fatalf("refresh result = %#v, error = %v", result, err)
	}
	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 || !list.Items[0].ObservedAt.Equal(capturedAt) {
		t.Fatalf("stored capture time = %#v, error = %v", list, err)
	}
}

func TestServiceListMergesLiveRuntimeStateWithoutPersistingIt(t *testing.T) {
	catalog := stateTestCatalog{
		room: rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{
			{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
			{ID: "caves", RoomID: "room", DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves},
		},
	}
	capturedAt := time.Date(2026, 8, 13, 2, 20, 0, 0, time.UTC)
	masterCycles, cavesCycles := 161, 162
	sampler := &stateTestCurrentSampler{current: map[string]Observation{
		"master": {Season: "winter", Phase: "day", Cycles: &masterCycles, CapturedAt: capturedAt},
		"caves":  {Season: "winter", Phase: "night", Cycles: &cavesCycles, CapturedAt: capturedAt.Add(time.Second)},
	}}
	store := newWorldStateStore(t)
	service, err := NewService(catalog, &stateTestRuntime{running: true}, store, sampler)
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.List(context.Background(), "room")
	if err != nil || list.Total != 2 || sampler.currentCalls != 2 || list.LastRefreshedAt == nil || !list.LastRefreshedAt.Equal(capturedAt.Add(time.Second)) {
		t.Fatalf("live list = %#v, calls = %d, error = %v", list, sampler.currentCalls, err)
	}
	byWorld := make(map[string]Snapshot, len(list.Items))
	for _, item := range list.Items {
		byWorld[item.WorldID] = item
	}
	if byWorld["master"].Cycles == nil || *byWorld["master"].Cycles != 161 || byWorld["master"].Season != "winter" || byWorld["caves"].Cycles == nil || *byWorld["caves"].Cycles != 162 {
		t.Fatalf("live snapshots were not merged: %#v", list.Items)
	}
	stored, err := store.Current("room")
	if err != nil || len(stored) != 0 {
		t.Fatalf("read-only list persisted snapshots: %#v, error = %v", stored, err)
	}
}

func TestServiceListDecoratesFreshnessFromRuntimeAndObservationAge(t *testing.T) {
	catalog := stateTestCatalog{
		room:   rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}},
	}
	store := newWorldStateStore(t)
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	if _, err := store.Append(Snapshot{RoomID: "room", WorldID: "master", WorldName: "Master", WorldRole: "master", Season: "autumn", ObservedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	runtime := &stateStatusRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	service, err := NewService(catalog, runtime, store, &stateTestSampler{})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	list, err := service.List(context.Background(), "room")
	if err != nil || len(list.Items) != 1 || list.Items[0].Freshness != FreshnessLive || list.Items[0].Stale || list.Items[0].RuntimeState != "running" || list.Items[0].AgeSeconds != 60 {
		t.Fatalf("live snapshot = %#v, error = %v", list, err)
	}
	service.now = func() time.Time { return now.Add(3 * time.Minute) }
	list, _ = service.List(context.Background(), "room")
	if list.Items[0].Freshness != FreshnessDelayed || !list.Items[0].Stale {
		t.Fatalf("delayed snapshot = %#v", list.Items[0])
	}
	runtime.status = shards.RuntimeStatus{State: shards.RuntimeStopped}
	list, _ = service.List(context.Background(), "room")
	if list.Items[0].Freshness != FreshnessStopped || list.Items[0].RuntimeState != "stopped" || !list.Items[0].Stale {
		t.Fatalf("stopped snapshot = %#v", list.Items[0])
	}
	runtime.status = shards.RuntimeStatus{State: shards.RuntimeUnknown}
	list, _ = service.List(context.Background(), "room")
	if list.Items[0].Freshness != FreshnessUnavailable || list.Items[0].RuntimeState != "unknown" || !list.Items[0].Stale {
		t.Fatalf("unavailable snapshot = %#v", list.Items[0])
	}
}

func TestServiceListIncludesWorldsWithoutSnapshots(t *testing.T) {
	catalog := stateTestCatalog{
		room: rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{
			{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
			{ID: "caves", RoomID: "room", DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves},
		},
	}
	store := newWorldStateStore(t)
	now := time.Date(2026, 8, 16, 1, 0, 0, 0, time.UTC)
	if _, err := store.Append(Snapshot{
		RoomID: "room", WorldID: "master", WorldName: "Master", WorldRole: "master",
		Season: "autumn", ObservedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(catalog, &stateStatusRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}, store, &stateTestSampler{})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	list, err := service.List(context.Background(), "room")
	if err != nil || list.Total != 2 || len(list.Items) != 2 {
		t.Fatalf("world state list = %#v, error = %v", list, err)
	}
	if list.Items[0].WorldID != "master" || list.Items[1].WorldID != "caves" {
		t.Fatalf("world order = %#v", list.Items)
	}
	missing := list.Items[1]
	if !missing.ObservedAt.IsZero() || missing.Freshness != FreshnessUnavailable || missing.RuntimeState != "stopped" || missing.AgeSeconds != 0 || !missing.Stale {
		t.Fatalf("missing world placeholder = %#v", missing)
	}
	if list.LastRefreshedAt == nil || !list.LastRefreshedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("last refreshed = %#v", list.LastRefreshedAt)
	}
}
