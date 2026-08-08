package shards

import (
	"context"
	"fmt"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"
)

type fakeRooms struct {
	room   rooms.Room
	worlds []rooms.World
}

func (f fakeRooms) Room(string) (rooms.Room, error) { return f.room, nil }
func (f fakeRooms) Worlds(string) ([]rooms.World, error) {
	return append([]rooms.World(nil), f.worlds...), nil
}

type fakeControl struct {
	running map[string]bool
	calls   []string
	fail    map[string]error
}

func (f *fakeControl) IsRunning(_ context.Context, room, world string) (bool, error) {
	return f.running[room+"/"+world], nil
}
func (f *fakeControl) Start(_ context.Context, room, world string) error {
	key := room + "/" + world
	f.calls = append(f.calls, "start:"+world)
	if err := f.fail["start:"+world]; err != nil {
		return err
	}
	f.running[key] = true
	return nil
}
func (f *fakeControl) Stop(_ context.Context, room, world string) error {
	key := room + "/" + world
	f.calls = append(f.calls, "stop:"+world)
	if err := f.fail["stop:"+world]; err != nil {
		return err
	}
	f.running[key] = false
	return nil
}

func testOperations(control *fakeControl) *Operations {
	roomID := rooms.EncodeID("summer_2026")
	operations := NewOperations(fakeRooms{
		room: rooms.Room{ID: roomID, DirectoryName: "summer_2026", Managed: true},
		worlds: []rooms.World{
			{ID: rooms.EncodeID("Caves"), RoomID: roomID, DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves},
			{ID: rooms.EncodeID("Master"), RoomID: roomID, DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
		},
	}, control)
	operations.pollInterval = time.Millisecond
	operations.startTimeout = 100 * time.Millisecond
	operations.stopTimeout = 100 * time.Millisecond
	return operations
}

func TestStartOrdersMasterFirstAndPreservesUnderscoreRoomName(t *testing.T) {
	control := &fakeControl{running: map[string]bool{}, fail: map[string]error{}}
	operations := testOperations(control)
	targets, runner, err := operations.Plan(ActionStart, rooms.EncodeID("summer_2026"), nil)
	if err != nil || len(targets) != 2 || targets[0].Name != "Master" {
		t.Fatalf("plan: %#v, %v", targets, err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(control.calls) != "[start:Master start:Caves]" {
		t.Fatalf("start order = %v", control.calls)
	}
	if len(results) != 2 || results[0].Status != jobs.StatusSucceeded || results[1].Status != jobs.StatusSucceeded {
		t.Fatalf("results = %#v", results)
	}
}

func TestStopOrdersMasterLastAndReportsPerWorldFailure(t *testing.T) {
	control := &fakeControl{
		running: map[string]bool{"summer_2026/Master": true, "summer_2026/Caves": true},
		fail:    map[string]error{"stop:Caves": fmt.Errorf("timeout")},
	}
	operations := testOperations(control)
	_, runner, err := operations.Plan(ActionStop, rooms.EncodeID("summer_2026"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	_ = runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) })
	if fmt.Sprint(control.calls) != "[stop:Caves stop:Master]" {
		t.Fatalf("stop order = %v", control.calls)
	}
	if len(results) != 2 || results[0].Status != jobs.StatusFailed || results[1].Status != jobs.StatusSucceeded {
		t.Fatalf("results = %#v", results)
	}
}

func TestRequiresAdoptionAndSafeTmuxNames(t *testing.T) {
	control := &fakeControl{running: map[string]bool{}, fail: map[string]error{}}
	operations := testOperations(control)
	base := operations.rooms.(fakeRooms)
	base.room.Managed = false
	operations.rooms = base
	if _, _, err := operations.Plan(ActionStart, base.room.ID, nil); err != ErrRoomNotManaged {
		t.Fatalf("unmanaged room error = %v", err)
	}
	base.room.Managed = true
	base.room.DirectoryName = "room name"
	operations.rooms = base
	if _, _, err := operations.Plan(ActionStart, base.room.ID, nil); err != ErrUnsafeName {
		t.Fatalf("unsafe room error = %v", err)
	}
}
