package worldstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/rooms"
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

type stateTestSampler struct {
	observation Observation
	err         error
}

func (s *stateTestSampler) Snapshot(context.Context, string, string) (Observation, error) {
	return s.observation, s.err
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
	list, err := service.List("room")
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
	list, err := service.List("room")
	if err != nil || len(list.Items) != 1 || !list.Items[0].ObservedAt.Equal(capturedAt) {
		t.Fatalf("stored capture time = %#v, error = %v", list, err)
	}
}
