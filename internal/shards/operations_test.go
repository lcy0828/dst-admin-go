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

type mutableRooms struct {
	room   rooms.Room
	worlds []rooms.World
}

func (f *mutableRooms) Room(string) (rooms.Room, error) { return f.room, nil }
func (f *mutableRooms) Worlds(string) ([]rooms.World, error) {
	return append([]rooms.World(nil), f.worlds...), nil
}

func (f fakeRooms) Room(string) (rooms.Room, error) { return f.room, nil }
func (f fakeRooms) Worlds(string) ([]rooms.World, error) {
	return append([]rooms.World(nil), f.worlds...), nil
}

type fakeControl struct {
	running map[string]bool
	status  map[string]RuntimeStatus
	calls   []string
	fail    map[string]error
}

type fakePreparer struct {
	calls []string
	err   error
}

func (f *fakePreparer) Prepare(_ context.Context, room, world string) error {
	f.calls = append(f.calls, room+"/"+world)
	return f.err
}

func (f *fakeControl) IsRunning(_ context.Context, room, world string) (bool, error) {
	return f.running[room+"/"+world], nil
}
func (f *fakeControl) Status(_ context.Context, room, world string) (RuntimeStatus, error) {
	key := room + "/" + world
	if status, ok := f.status[key]; ok {
		return status, nil
	}
	if f.running[key] {
		return RuntimeStatus{State: RuntimeRunning, SessionExists: true}, nil
	}
	return RuntimeStatus{State: RuntimeStopped}, nil
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
	delete(f.status, key)
	return nil
}
func (f *fakeControl) Cleanup(_ context.Context, room, world string) error {
	key := room + "/" + world
	f.calls = append(f.calls, "cleanup:"+world)
	if err := f.fail["cleanup:"+world]; err != nil {
		return err
	}
	f.running[key] = false
	delete(f.status, key)
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

func TestStartDoesNotCallControlWhenRuntimePreparationFails(t *testing.T) {
	control := &fakeControl{running: map[string]bool{}, fail: map[string]error{}}
	operations := testOperations(control)
	preparer := &fakePreparer{err: fmt.Errorf("customcommands.lua is invalid")}
	operations.preparers = []RuntimePreparer{preparer}
	_, runner, err := operations.Plan(ActionStart, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(preparer.calls) != "[summer_2026/Master]" {
		t.Fatalf("preparer calls = %v", preparer.calls)
	}
	if len(control.calls) != 0 {
		t.Fatalf("control was called after preparation failed: %v", control.calls)
	}
	if len(results) != 1 || results[0].Status != jobs.StatusFailed || results[0].Error == nil || results[0].Error.Code != "START_FAILED" {
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

func TestStopStartingSessionUsesGracefulShutdown(t *testing.T) {
	control := &fakeControl{
		running: map[string]bool{},
		status: map[string]RuntimeStatus{
			"summer_2026/Master": {State: RuntimeStarting, SessionExists: true},
		},
		fail: map[string]error{},
	}
	operations := testOperations(control)
	_, runner, err := operations.Plan(ActionStop, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(control.calls) != "[stop:Master]" {
		t.Fatalf("starting session did not use graceful stop: %v", control.calls)
	}
	if len(results) != 1 || results[0].Status != jobs.StatusSucceeded {
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

func TestStartReportsRuntimeFailureInsteadOfTmuxSuccess(t *testing.T) {
	control := &fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}}
	operations := testOperations(control)
	operations.pollInterval = time.Millisecond
	control.status["summer_2026/Master"] = RuntimeStatus{State: RuntimeFailed, SessionExists: true, Message: "Klei 集群令牌已过期或无效"}
	_, runner, err := operations.Plan(ActionStart, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != jobs.StatusFailed || results[0].Error == nil || results[0].Error.Message != "Klei 集群令牌已过期或无效" {
		t.Fatalf("results = %#v", results)
	}
	if fmt.Sprint(control.calls) != "[start:Master cleanup:Master]" {
		t.Fatalf("failed start was not cleaned up: %v", control.calls)
	}
}

func TestStartTimeoutCleansUpSession(t *testing.T) {
	control := &fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}}
	operations := testOperations(control)
	operations.startTimeout = 5 * time.Millisecond
	control.status["summer_2026/Master"] = RuntimeStatus{State: RuntimeStarting, SessionExists: true}
	_, runner, err := operations.Plan(ActionStart, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != jobs.StatusFailed || results[0].Error == nil || results[0].Error.Message != "等待分片启动超时" {
		t.Fatalf("results = %#v", results)
	}
	if fmt.Sprint(control.calls) != "[cleanup:Master]" {
		t.Fatalf("timed out start was not cleaned up: %v", control.calls)
	}
}

func TestRunnerRejectsWorldPlanChangedWhileWaiting(t *testing.T) {
	roomID := rooms.EncodeID("summer_2026")
	master := rooms.World{ID: rooms.EncodeID("Master"), RoomID: roomID, DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster}
	caves := rooms.World{ID: rooms.EncodeID("Caves"), RoomID: roomID, DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves}
	catalog := &mutableRooms{
		room:   rooms.Room{ID: roomID, DirectoryName: "summer_2026", Managed: true},
		worlds: []rooms.World{master, caves},
	}
	control := &fakeControl{running: map[string]bool{}, fail: map[string]error{}}
	operations := NewOperations(catalog, control)
	_, runner, err := operations.Plan(ActionStart, roomID, nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog.worlds = []rooms.World{master}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if len(control.calls) != 0 {
		t.Fatalf("stale plan executed runtime calls: %v", control.calls)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v", results)
	}
	for _, result := range results {
		if result.Status != jobs.StatusFailed || result.Error == nil || result.Error.Code != "ROOM_CHANGED" {
			t.Fatalf("result = %#v", result)
		}
	}
}
