package shards

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/jobs"
	"dont/internal/operationlease"
	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"
)

type fakePlacementResolver struct {
	applied         topology.ExecutionPlacement
	resolved        topology.ExecutionPlacement
	preview         topology.StartCapacityPreview
	batchPreview    topology.BatchStartCapacityPreview
	err             error
	previewErr      error
	appliedByWorld  map[string]topology.ExecutionPlacement
	resolvedByWorld map[string]topology.ExecutionPlacement
	errByWorld      map[string]error
}

func (resolver *fakePlacementResolver) AppliedPlacement(_ string, worldID string) (topology.ExecutionPlacement, error) {
	if err := resolver.errByWorld[worldID]; err != nil {
		return topology.ExecutionPlacement{}, err
	}
	if value, exists := resolver.appliedByWorld[worldID]; exists {
		return value, nil
	}
	return resolver.applied, resolver.err
}

func (resolver *fakePlacementResolver) ResolveExecution(_ context.Context, _ string, worldID string) (topology.ExecutionPlacement, error) {
	if err := resolver.errByWorld[worldID]; err != nil {
		return topology.ExecutionPlacement{}, err
	}
	if value, exists := resolver.resolvedByWorld[worldID]; exists {
		return value, nil
	}
	return resolver.resolved, resolver.err
}

func (resolver *fakePlacementResolver) PreviewStartCapacity(context.Context, string, []string) (topology.StartCapacityPreview, error) {
	return resolver.preview, resolver.previewErr
}

func (resolver *fakePlacementResolver) PreviewBatchStartCapacity(context.Context, []topology.StartCapacitySelection) (topology.BatchStartCapacityPreview, error) {
	return resolver.batchPreview, resolver.previewErr
}

type fakeRemoteExecutor struct {
	targetID string
	request  shared.ShardOperationRequest
	calls    int
	err      error
}

type fakePlacedRuntime struct {
	status  shared.ShardRuntimeStatus
	request shared.ShardOperationRequest
	roomID  string
	worldID string
	calls   int
	err     error
}

func (runtime *fakePlacedRuntime) Status(context.Context, string, string) (shared.ShardRuntimeStatus, error) {
	return runtime.status, runtime.err
}

func (runtime *fakePlacedRuntime) ExecutePlacedShard(_ context.Context, roomID, worldID string, request shared.ShardOperationRequest, _ time.Duration) (shared.ShardOperationResult, error) {
	runtime.roomID, runtime.worldID, runtime.request = roomID, worldID, request
	runtime.calls++
	if runtime.err != nil {
		return shared.ShardOperationResult{}, runtime.err
	}
	return shared.ShardOperationResult{ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: request.OperationID, Action: request.Action, Message: "Runtime 完成"}, nil
}

func (executor *fakeRemoteExecutor) ExecuteShard(_ context.Context, targetID string, request shared.ShardOperationRequest, _ int) (agents.ShardExecutionResult, error) {
	executor.targetID, executor.request = targetID, request
	executor.calls++
	if executor.err != nil {
		return agents.ShardExecutionResult{}, executor.err
	}
	state := "running"
	if request.Action == shared.ShardActionStop {
		state = "stopped"
	}
	return agents.ShardExecutionResult{Result: shared.ShardOperationResult{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: request.OperationID,
		InstallationID: "default", Action: request.Action, Cluster: request.Cluster, Shard: request.Shard,
		Status: shared.ShardRuntimeStatus{State: state, SessionExists: state == "running"}, Message: "远程完成",
	}}, nil
}

type fakeLeaseService struct {
	lease      operationlease.Lease
	acquireErr error
	released   bool
}

type fakeOperationObserver struct{ items []OperationAudit }

func (observer *fakeOperationObserver) ObserveOperation(_ context.Context, audit OperationAudit) error {
	observer.items = append(observer.items, audit)
	return nil
}

type fakeOperationNotifier struct {
	calls []string
	err   error
}

type roomLockOperationNotifier struct{}

func (roomLockOperationNotifier) BeforeOperation(ctx context.Context, roomID, _, _, _ string) error {
	_, release, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return err
	}
	release()
	return nil
}

func (notifier *fakeOperationNotifier) BeforeOperation(_ context.Context, roomID, action, source, jobID string) error {
	notifier.calls = append(notifier.calls, roomID+":"+action+":"+source+":"+jobID)
	return notifier.err
}

func (service *fakeLeaseService) Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error) {
	return service.lease, service.acquireErr
}

func (service *fakeLeaseService) Renew(context.Context, operationlease.Lease, time.Duration) (operationlease.Lease, error) {
	return service.lease, nil
}

func (service *fakeLeaseService) Release(operationlease.Lease) error {
	service.released = true
	return nil
}

type fakeRooms struct {
	room   rooms.Room
	worlds []rooms.World
}

type batchRooms struct {
	rooms  map[string]rooms.Room
	worlds map[string][]rooms.World
}

func (f batchRooms) Room(id string) (rooms.Room, error) {
	room, exists := f.rooms[id]
	if !exists {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return room, nil
}

func (f batchRooms) Worlds(id string) ([]rooms.World, error) {
	return append([]rooms.World(nil), f.worlds[id]...), nil
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
	mu      sync.Mutex
	running map[string]bool
	status  map[string]RuntimeStatus
	calls   []string
	fail    map[string]error
}

type interruptControl struct {
	mu      sync.Mutex
	started chan struct{}
	state   RuntimeStatus
	calls   []string
}

type stagedStartControl struct {
	mu               sync.Mutex
	masterStarted    chan struct{}
	dependentStarted chan struct{}
	masterReady      chan struct{}
	masterStartOnce  sync.Once
	dependentOnce    sync.Once
	states           map[string]RuntimeStatus
	calls            []string
}

func (c *stagedStartControl) IsRunning(ctx context.Context, room, world string) (bool, error) {
	status, err := c.Status(ctx, room, world)
	return status.State == RuntimeRunning, err
}

func (c *stagedStartControl) Status(_ context.Context, _, world string) (RuntimeStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[world], nil
}

func (c *stagedStartControl) Start(_ context.Context, _, world string) error {
	c.mu.Lock()
	c.calls = append(c.calls, "start:"+world)
	if world == "Master" {
		c.states[world] = RuntimeStatus{State: RuntimeStarting, SessionExists: true}
		c.masterStartOnce.Do(func() { close(c.masterStarted) })
		c.mu.Unlock()
		go func() {
			<-c.masterReady
			c.mu.Lock()
			c.states[world] = RuntimeStatus{State: RuntimeRunning, SessionExists: true}
			c.mu.Unlock()
		}()
		return nil
	}
	c.states[world] = RuntimeStatus{State: RuntimeRunning, SessionExists: true}
	c.dependentOnce.Do(func() { close(c.dependentStarted) })
	c.mu.Unlock()
	return nil
}

func (c *stagedStartControl) Stop(_ context.Context, _, world string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "stop:"+world)
	c.states[world] = RuntimeStatus{State: RuntimeStopped}
	return nil
}

func (c *interruptControl) IsRunning(ctx context.Context, room, world string) (bool, error) {
	status, err := c.Status(ctx, room, world)
	return status.State == RuntimeRunning, err
}

func (c *interruptControl) Status(_ context.Context, _, _ string) (RuntimeStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, nil
}

func (c *interruptControl) Start(_ context.Context, _, world string) error {
	c.mu.Lock()
	c.calls = append(c.calls, "start:"+world)
	c.state = RuntimeStatus{State: RuntimeStarting, SessionExists: true}
	close(c.started)
	c.mu.Unlock()
	return nil
}

func (c *interruptControl) Stop(_ context.Context, _, world string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "stop:"+world)
	c.state = RuntimeStatus{State: RuntimeStopped}
	return nil
}

func (f *fakeControl) IsRunning(_ context.Context, room, world string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[room+"/"+world], nil
}
func (f *fakeControl) Status(_ context.Context, room, world string) (RuntimeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
	key := room + "/" + world
	f.calls = append(f.calls, "start:"+world)
	if err := f.fail["start:"+world]; err != nil {
		return err
	}
	f.running[key] = true
	return nil
}
func (f *fakeControl) Stop(_ context.Context, room, world string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
	key := room + "/" + world
	f.calls = append(f.calls, "cleanup:"+world)
	if err := f.fail["cleanup:"+world]; err != nil {
		return err
	}
	f.running[key] = false
	delete(f.status, key)
	return nil
}

func testOperations(control Control) *Operations {
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

func TestStartPlansMasterFirstAndLaunchesBothWorlds(t *testing.T) {
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
	calls := fmt.Sprint(control.calls)
	if calls != "[start:Master start:Caves]" && calls != "[start:Caves start:Master]" {
		t.Fatalf("start order = %v", control.calls)
	}
	if len(results) != 2 || results[0].Status != jobs.StatusSucceeded || results[1].Status != jobs.StatusSucceeded {
		t.Fatalf("results = %#v", results)
	}
}

func TestStartSupportsCaveMasterWithMultipleWorldTypes(t *testing.T) {
	roomID := rooms.EncodeID("mixed_worlds")
	control := &fakeControl{running: map[string]bool{}, fail: map[string]error{}}
	operations := NewOperations(fakeRooms{
		room: rooms.Room{ID: roomID, DirectoryName: "mixed_worlds", Managed: true},
		worlds: []rooms.World{
			{ID: "forest-3", RoomID: roomID, DirectoryName: "ForestThree", Name: "Forest Three", Role: rooms.WorldRoleCustom, Type: rooms.WorldTypeForest, ShardID: 3},
			{ID: "cave-7", RoomID: roomID, DirectoryName: "CavePrime", Name: "Cave Prime", Role: rooms.WorldRoleMaster, Type: rooms.WorldTypeCave, IsMaster: true, ShardID: 7},
			{ID: "cave-9", RoomID: roomID, DirectoryName: "DeepTwo", Name: "Deep Two", Role: rooms.WorldRoleCaves, Type: rooms.WorldTypeCave, ShardID: 9},
		},
	}, control)
	operations.pollInterval = time.Millisecond
	operations.startTimeout = 100 * time.Millisecond
	_, runner, err := operations.Plan(ActionStart, roomID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner(context.Background(), func(jobs.TargetResult) {}); err != nil {
		t.Fatal(err)
	}
	started := make(map[string]bool, len(control.calls))
	for _, call := range control.calls {
		started[call] = true
	}
	if len(control.calls) != 3 || !started["start:CavePrime"] || !started["start:ForestThree"] || !started["start:DeepTwo"] {
		t.Fatalf("start calls=%v", control.calls)
	}
}

func TestStartRejectsInvalidMasterCardinality(t *testing.T) {
	tests := []struct {
		name     string
		worlds   []rooms.World
		wantCode string
	}{
		{
			name: "missing", wantCode: "ROOM_MASTER_MISSING",
			worlds: []rooms.World{{ID: "forest", DirectoryName: "Forest", Name: "Forest", Role: rooms.WorldRoleCustom, Type: rooms.WorldTypeForest}},
		},
		{
			name: "multiple", wantCode: "ROOM_MASTER_MULTIPLE",
			worlds: []rooms.World{
				{ID: "forest-master", DirectoryName: "ForestMaster", Name: "Forest Master", Role: rooms.WorldRoleMaster, Type: rooms.WorldTypeForest, IsMaster: true},
				{ID: "cave-master", DirectoryName: "CaveMaster", Name: "Cave Master", Role: rooms.WorldRoleMaster, Type: rooms.WorldTypeCave, IsMaster: true},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			roomID := rooms.EncodeID("invalid_" + test.name)
			for index := range test.worlds {
				test.worlds[index].RoomID = roomID
			}
			control := &fakeControl{running: map[string]bool{}, fail: map[string]error{}}
			operations := NewOperations(fakeRooms{
				room: rooms.Room{ID: roomID, DirectoryName: "invalid_" + test.name, Managed: true}, worlds: test.worlds,
			}, control)
			_, runner, err := operations.Plan(ActionStart, roomID, nil)
			if err != nil {
				t.Fatal(err)
			}
			var results []jobs.TargetResult
			if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
				t.Fatal(err)
			}
			if len(results) != len(test.worlds) || len(control.calls) != 0 {
				t.Fatalf("results=%#v calls=%v", results, control.calls)
			}
			for _, result := range results {
				if result.Error == nil || result.Error.Code != test.wantCode {
					t.Fatalf("result=%#v wantCode=%s", result, test.wantCode)
				}
			}
		})
	}
}

func TestStartLaunchesSameEndpointDependentsBeforeMasterIsReady(t *testing.T) {
	roomID := rooms.EncodeID("summer_2026")
	control := &stagedStartControl{
		masterStarted: make(chan struct{}), dependentStarted: make(chan struct{}), masterReady: make(chan struct{}),
		states: map[string]RuntimeStatus{"Master": {State: RuntimeStopped}, "Caves": {State: RuntimeStopped}},
	}
	operations := NewOperations(fakeRooms{
		room: rooms.Room{ID: roomID, DirectoryName: "summer_2026", Managed: true},
		worlds: []rooms.World{
			{ID: rooms.EncodeID("Master"), RoomID: roomID, DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
			{ID: rooms.EncodeID("Caves"), RoomID: roomID, DirectoryName: "Caves", Name: "Caves", Role: rooms.WorldRoleCaves},
		},
	}, control)
	operations.pollInterval = time.Millisecond
	operations.startTimeout = time.Second
	_, runner, err := operations.Plan(ActionStart, roomID, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []jobs.TargetResult, 1)
	go func() {
		var results []jobs.TargetResult
		_ = runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) })
		done <- results
	}()
	select {
	case <-control.masterStarted:
	case <-time.After(time.Second):
		t.Fatal("Master was not launched")
	}
	select {
	case <-control.dependentStarted:
	case <-time.After(time.Second):
		t.Fatal("dependent shard waited for Master readiness")
	}
	close(control.masterReady)
	results := <-done
	if len(results) != 2 || results[0].Status != jobs.StatusSucceeded || results[1].Status != jobs.StatusSucceeded {
		t.Fatalf("results = %#v", results)
	}
	control.mu.Lock()
	calls := fmt.Sprint(control.calls)
	control.mu.Unlock()
	if calls != "[start:Master start:Caves]" && calls != "[start:Caves start:Master]" {
		t.Fatalf("calls = %s", calls)
	}
}

func TestStopRunsNotificationHookBeforeLifecycleControl(t *testing.T) {
	control := &fakeControl{
		running: map[string]bool{"summer_2026/Master": true},
		fail:    map[string]error{},
	}
	operations := testOperations(control)
	notifier := &fakeOperationNotifier{}
	operations.ConfigureNotifier(notifier)
	_, runner, err := operations.Plan(ActionStop, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithOperationAudit(context.Background(), OperationAuditMetadata{JobID: "job-stop"})
	if err := runner(ctx, func(jobs.TargetResult) {}); err != nil {
		t.Fatal(err)
	}
	want := []string{rooms.EncodeID("summer_2026") + ":stop::job-stop"}
	if !reflect.DeepEqual(notifier.calls, want) || fmt.Sprint(control.calls) != "[stop:Master]" {
		t.Fatalf("notifier=%v control=%v", notifier.calls, control.calls)
	}
}

func TestStopNotificationRunsBeforeRoomLockAndThenStopsInDependencyOrder(t *testing.T) {
	control := &fakeControl{
		running: map[string]bool{"summer_2026/Master": true, "summer_2026/Caves": true},
		fail:    map[string]error{},
	}
	operations := testOperations(control)
	operations.ConfigureNotifier(roomLockOperationNotifier{})
	_, runner, err := operations.Plan(ActionStop, rooms.EncodeID("summer_2026"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var results []jobs.TargetResult
	if err := runner(ctx, func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(control.calls) != "[stop:Caves stop:Master]" {
		t.Fatalf("stop order = %v", control.calls)
	}
	if len(results) != 2 || results[0].Status != jobs.StatusSucceeded || results[1].Status != jobs.StatusSucceeded {
		t.Fatalf("results = %#v", results)
	}
}

func TestImmediateStopSkipsOnlyThisOperationsNotice(t *testing.T) {
	control := &fakeControl{running: map[string]bool{"summer_2026/Master": true}, fail: map[string]error{}}
	operations := testOperations(control)
	notifier := &fakeOperationNotifier{}
	operations.ConfigureNotifier(notifier)
	_, runner, err := operations.PlanWithOptions(ActionStop, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")}, PlanOptions{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner(context.Background(), func(jobs.TargetResult) {}); err != nil {
		t.Fatal(err)
	}
	if len(notifier.calls) != 0 || fmt.Sprint(control.calls) != "[stop:Master]" {
		t.Fatalf("immediate stop: notices=%v control=%v", notifier.calls, control.calls)
	}
	_, runner, err = operations.Plan(ActionStop, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner(context.Background(), func(jobs.TargetResult) {}); err != nil {
		t.Fatal(err)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("immediate mode leaked into next operation: %v", notifier.calls)
	}
}

func TestNotificationCountdownCancellationPreventsLifecycleControl(t *testing.T) {
	control := &fakeControl{
		running: map[string]bool{"summer_2026/Master": true}, fail: map[string]error{},
	}
	operations := testOperations(control)
	operations.ConfigureNotifier(&fakeOperationNotifier{err: context.Canceled})
	_, runner, err := operations.Plan(ActionStop, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if len(control.calls) != 0 || len(results) != 1 || results[0].Status != jobs.StatusCanceled {
		t.Fatalf("control=%v results=%#v", control.calls, results)
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

func TestRestartStopsDependencyBeforeMasterThenStartsWorldsConcurrently(t *testing.T) {
	control := &fakeControl{
		running: map[string]bool{"summer_2026/Master": true, "summer_2026/Caves": true},
		fail:    map[string]error{},
	}
	operations := testOperations(control)
	_, runner, err := operations.Plan(ActionRestart, rooms.EncodeID("summer_2026"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	calls := control.calls
	if len(calls) != 4 || calls[0] != "stop:Caves" || calls[1] != "stop:Master" ||
		!((calls[2] == "start:Master" && calls[3] == "start:Caves") || (calls[2] == "start:Caves" && calls[3] == "start:Master")) {
		t.Fatalf("restart order = %v", control.calls)
	}
	if len(results) != 2 || results[0].Status != jobs.StatusSucceeded || results[1].Status != jobs.StatusSucceeded {
		t.Fatalf("results = %#v", results)
	}
}

func TestStartDependentRequiresMasterInSamePlanWhenMasterIsStopped(t *testing.T) {
	control := &fakeControl{running: map[string]bool{}, fail: map[string]error{}}
	operations := testOperations(control)
	_, runner, err := operations.Plan(ActionStart, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Caves")})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if len(control.calls) != 0 || len(results) != 1 || results[0].Status != jobs.StatusFailed ||
		results[0].Error == nil || results[0].Error.Code != "DEPENDENCY_SELECTION_INCOMPLETE" {
		t.Fatalf("control=%v results=%#v", control.calls, results)
	}
}

func TestStartDependentAloneIsAllowedWhenMasterIsAlreadyRunning(t *testing.T) {
	control := &fakeControl{
		running: map[string]bool{"summer_2026/Master": true},
		fail:    map[string]error{},
	}
	operations := testOperations(control)
	_, runner, err := operations.Plan(ActionStart, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Caves")})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(control.calls) != "[start:Caves]" || len(results) != 1 || results[0].Status != jobs.StatusSucceeded {
		t.Fatalf("control=%v results=%#v", control.calls, results)
	}
}

func TestStopOrRestartMasterRequiresActiveDependentsInSamePlan(t *testing.T) {
	for _, action := range []Action{ActionStop, ActionRestart} {
		t.Run(string(action), func(t *testing.T) {
			control := &fakeControl{
				running: map[string]bool{"summer_2026/Master": true, "summer_2026/Caves": true},
				fail:    map[string]error{},
			}
			operations := testOperations(control)
			notifier := &fakeOperationNotifier{}
			operations.ConfigureNotifier(notifier)
			_, runner, err := operations.Plan(action, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")})
			if err != nil {
				t.Fatal(err)
			}
			var results []jobs.TargetResult
			if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
				t.Fatal(err)
			}
			if len(control.calls) != 0 || len(notifier.calls) != 0 || len(results) != 1 || results[0].Status != jobs.StatusFailed ||
				results[0].Error == nil || results[0].Error.Code != "DEPENDENCY_SELECTION_INCOMPLETE" {
				t.Fatalf("control=%v notifier=%v results=%#v", control.calls, notifier.calls, results)
			}
		})
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

func TestStopInterruptsActiveStartBeforeTakingRoomLease(t *testing.T) {
	roomID := rooms.EncodeID("summer_2026")
	worldID := rooms.EncodeID("Master")
	control := &interruptControl{started: make(chan struct{}), state: RuntimeStatus{State: RuntimeStopped}}
	operations := NewOperations(fakeRooms{
		room: rooms.Room{ID: roomID, DirectoryName: "summer_2026", Managed: true},
		worlds: []rooms.World{
			{ID: worldID, RoomID: roomID, DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
		},
	}, control)
	operations.pollInterval = time.Millisecond
	operations.startTimeout = time.Second
	operations.stopTimeout = time.Second

	_, startRunner, err := operations.Plan(ActionStart, roomID, []string{worldID})
	if err != nil {
		t.Fatal(err)
	}
	startDone := make(chan []jobs.TargetResult, 1)
	go func() {
		var results []jobs.TargetResult
		_ = startRunner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) })
		startDone <- results
	}()
	select {
	case <-control.started:
	case <-time.After(time.Second):
		t.Fatal("start did not reach the runtime wait")
	}

	_, stopRunner, err := operations.Plan(ActionStop, roomID, []string{worldID})
	if err != nil {
		t.Fatal(err)
	}
	var stopResults []jobs.TargetResult
	if err := stopRunner(context.Background(), func(result jobs.TargetResult) { stopResults = append(stopResults, result) }); err != nil {
		t.Fatal(err)
	}
	startResults := <-startDone
	if len(startResults) != 1 || startResults[0].Status != jobs.StatusCanceled {
		t.Fatalf("start results = %#v", startResults)
	}
	if len(stopResults) != 1 || stopResults[0].Status != jobs.StatusSucceeded {
		t.Fatalf("stop results = %#v", stopResults)
	}
	control.mu.Lock()
	calls := fmt.Sprint(control.calls)
	control.mu.Unlock()
	if calls != "[start:Master stop:Master]" {
		t.Fatalf("calls = %s", calls)
	}
}

func TestStopInvalidatesStartPlanBeforeItsRunnerRegisters(t *testing.T) {
	roomID := rooms.EncodeID("summer_2026")
	worldID := rooms.EncodeID("Master")
	control := &fakeControl{running: map[string]bool{}, fail: map[string]error{}}
	operations := NewOperations(fakeRooms{
		room: rooms.Room{ID: roomID, DirectoryName: "summer_2026", Managed: true},
		worlds: []rooms.World{
			{ID: worldID, RoomID: roomID, DirectoryName: "Master", Name: "Master", Role: rooms.WorldRoleMaster},
		},
	}, control)
	_, startRunner, err := operations.Plan(ActionStart, roomID, []string{worldID})
	if err != nil {
		t.Fatal(err)
	}
	_, stopRunner, err := operations.Plan(ActionStop, roomID, []string{worldID})
	if err != nil {
		t.Fatal(err)
	}
	var stopResults []jobs.TargetResult
	if err := stopRunner(context.Background(), func(result jobs.TargetResult) { stopResults = append(stopResults, result) }); err != nil {
		t.Fatal(err)
	}
	var startResults []jobs.TargetResult
	if err := startRunner(context.Background(), func(result jobs.TargetResult) { startResults = append(startResults, result) }); err != nil {
		t.Fatal(err)
	}
	if len(control.calls) != 0 {
		t.Fatalf("invalidated start reached runtime control: %v", control.calls)
	}
	if len(startResults) != 1 || startResults[0].Status != jobs.StatusCanceled {
		t.Fatalf("start results = %#v", startResults)
	}
	if len(stopResults) != 1 || stopResults[0].Status != jobs.StatusSucceeded {
		t.Fatalf("stop results = %#v", stopResults)
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

func TestCapacityRiskRequiresExplicitConfirmation(t *testing.T) {
	operations := testOperations(&fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}})
	resolver := &fakePlacementResolver{
		applied: topology.ExecutionPlacement{AppliedTargetID: "local"},
		preview: topology.StartCapacityPreview{
			RoomID: rooms.EncodeID("summer_2026"), RequiresRiskConfirmation: true,
			Targets: []topology.StartCapacityTarget{{TargetID: "local", RequiresRiskConfirmation: true}},
		},
	}
	if err := operations.ConfigureDistributed(resolver, &fakeRemoteExecutor{}, &fakeLeaseService{}); err != nil {
		t.Fatal(err)
	}
	worldIDs := []string{rooms.EncodeID("Master"), rooms.EncodeID("Caves")}
	err := operations.RequireCapacityConfirmation(context.Background(), ActionStart, rooms.EncodeID("summer_2026"), worldIDs, false)
	var risk *CapacityRiskError
	if !errors.As(err, &risk) || !risk.Preview.RequiresRiskConfirmation || len(risk.Preview.Targets) != 1 {
		t.Fatalf("risk error = %#v", err)
	}
	if err := operations.RequireCapacityConfirmation(context.Background(), ActionStart, rooms.EncodeID("summer_2026"), worldIDs, true); err != nil {
		t.Fatalf("confirmed risk was rejected: %v", err)
	}
	if err := operations.RequireCapacityConfirmation(context.Background(), ActionStop, rooms.EncodeID("summer_2026"), worldIDs, false); err != nil {
		t.Fatalf("stop should not require capacity confirmation: %v", err)
	}
}

func TestRuntimeModesIntersectEverySelectedTarget(t *testing.T) {
	operations := testOperations(&fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}})
	masterID, cavesID := rooms.EncodeID("Master"), rooms.EncodeID("Caves")
	allModes := []shared.RuntimePerformanceMode{
		shared.RuntimePerformanceModeGame,
		shared.RuntimePerformanceModeLuaJIT,
		shared.RuntimePerformanceModeArenaGC,
	}
	commonModes := allModes[:2]
	resolver := &fakePlacementResolver{resolvedByWorld: map[string]topology.ExecutionPlacement{
		masterID: {
			AppliedTargetID: "local",
			Target: agents.RuntimeTarget{Name: "本机", OS: "linux", Arch: "amd64", Performance: &shared.RuntimePerformanceReport{
				Provider: "dontstarve-luajit2", Status: shared.RuntimePerformanceReady, CanEnable: true,
				PackageVersion: "3.0.0", GameVersion: "747465", SupportedModes: allModes,
			}},
		},
		cavesID: {
			AppliedTargetID: "agent:node-a",
			Target: agents.RuntimeTarget{Name: "节点 A", Capabilities: []string{"shard.runtime-mode.v1"}, Performance: &shared.RuntimePerformanceReport{
				Provider: "dontstarve-luajit2", Status: shared.RuntimePerformanceReady, CanEnable: true,
				PackageVersion: "3.0.0", SupportedModes: commonModes,
			}},
		},
	}}
	if err := operations.ConfigureDistributed(resolver, &fakeRemoteExecutor{}, &fakeLeaseService{}); err != nil {
		t.Fatal(err)
	}

	availability, err := operations.RuntimeModes(context.Background(), rooms.EncodeID("summer_2026"), []string{masterID, cavesID})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(availability.Modes, commonModes) || len(availability.Targets) != 2 || len(availability.Packages) != 3 {
		t.Fatalf("availability=%#v", availability)
	}
	if availability.Targets[0].CompatibilityStatus != shared.RuntimePerformanceReady || availability.Targets[1].CompatibilityStatus != shared.RuntimePerformanceReady || availability.Targets[1].OS != "linux" || availability.Targets[1].Arch != "amd64" || availability.Targets[1].GameVersion != "747465" {
		t.Fatalf("target compatibility evidence=%#v", availability.Targets)
	}
	if _, err := operations.RequireRuntimeSelection(context.Background(), rooms.EncodeID("summer_2026"), []string{masterID, cavesID}, shared.RuntimePerformanceModeLuaJIT, "3.0.0"); err != nil {
		t.Fatalf("matching package version was rejected: %v", err)
	}
	if _, err := operations.RequireRuntimeSelection(context.Background(), rooms.EncodeID("summer_2026"), []string{masterID, cavesID}, shared.RuntimePerformanceModeLuaJIT, "2.9.2"); !errors.Is(err, ErrRuntimeVersionUnavailable) {
		t.Fatalf("mismatched package version error=%v", err)
	}
	if _, err := operations.RequireRuntimeSelection(context.Background(), rooms.EncodeID("summer_2026"), []string{masterID, cavesID}, shared.RuntimePerformanceModeLuaJIT, "future"); !errors.Is(err, ErrInvalidRuntimeVersion) {
		t.Fatalf("invalid package version error=%v", err)
	}
	if _, err := operations.RequireRuntimeMode(context.Background(), rooms.EncodeID("summer_2026"), []string{masterID, cavesID}, shared.RuntimePerformanceModeArenaGC); !errors.Is(err, ErrRuntimeModeUnavailable) {
		t.Fatalf("arena-gc error=%v", err)
	}
}

func TestRuntimeModesExposeIncompatibilityEvidence(t *testing.T) {
	operations := testOperations(&fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}})
	masterID := rooms.EncodeID("Master")
	resolver := &fakePlacementResolver{resolvedByWorld: map[string]topology.ExecutionPlacement{
		masterID: {
			AppliedTargetID: "local",
			Target: agents.RuntimeTarget{Name: "本机", Performance: &shared.RuntimePerformanceReport{
				Provider: "dontstarve-luajit2", Status: shared.RuntimePerformanceIncompatible,
				SupportedModes: []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame},
				Issues:         []string{"platform_not_verified", "installation_incomplete"},
			}},
		},
	}}
	if err := operations.ConfigureDistributed(resolver, &fakeRemoteExecutor{}, &fakeLeaseService{}); err != nil {
		t.Fatal(err)
	}

	availability, err := operations.RuntimeModes(context.Background(), rooms.EncodeID("summer_2026"), []string{masterID})
	if err != nil {
		t.Fatal(err)
	}
	if len(availability.Targets) != 1 {
		t.Fatalf("targets=%#v", availability.Targets)
	}
	target := availability.Targets[0]
	if target.CompatibilityStatus != shared.RuntimePerformanceIncompatible || target.ReasonCode != "runtime_incompatible" ||
		!reflect.DeepEqual(target.Issues, []string{"platform_not_verified", "installation_incomplete"}) {
		t.Fatalf("target=%#v", target)
	}
}

func TestGameRuntimeModeDoesNotMaskPlacementPreflight(t *testing.T) {
	operations := testOperations(&fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}})
	resolver := &fakePlacementResolver{err: errors.New("Agent 已离线")}
	if err := operations.ConfigureDistributed(resolver, &fakeRemoteExecutor{}, &fakeLeaseService{}); err != nil {
		t.Fatal(err)
	}
	availability, err := operations.RequireRuntimeMode(context.Background(), rooms.EncodeID("summer_2026"), nil, shared.RuntimePerformanceModeGame)
	if err != nil || !reflect.DeepEqual(availability.Modes, []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame}) {
		t.Fatalf("availability=%#v err=%v", availability, err)
	}
}

func TestStartAttemptsEachWorldAndReportsUnavailableRemoteTarget(t *testing.T) {
	roomID := rooms.EncodeID("summer_2026")
	masterID := rooms.EncodeID("Master")
	cavesID := rooms.EncodeID("Caves")
	control := &fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}}
	operations := testOperations(control)
	resolver := &fakePlacementResolver{
		appliedByWorld: map[string]topology.ExecutionPlacement{
			masterID: {AppliedTargetID: "local"},
			cavesID:  {AppliedTargetID: "agent:node-a"},
		},
		resolvedByWorld: map[string]topology.ExecutionPlacement{
			cavesID: {AppliedTargetID: "agent:node-a"},
		},
		errByWorld: map[string]error{cavesID: errors.New("Agent 已离线")},
	}
	remote := &fakeRemoteExecutor{}
	if err := operations.ConfigureDistributed(resolver, remote, &fakeLeaseService{lease: operationlease.Lease{
		RoomID: roomID, LeaseID: "lease", OperationKey: "operation", FencingToken: 1,
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}}); err != nil {
		t.Fatal(err)
	}
	_, runner, err := operations.Plan(ActionStart, roomID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(control.calls) != "[start:Master]" || remote.calls != 0 {
		t.Fatalf("direct start calls: local=%v remote=%d", control.calls, remote.calls)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v", results)
	}
	statuses := map[string]jobs.Status{}
	for _, result := range results {
		statuses[result.TargetID] = result.Status
	}
	if statuses[masterID] != jobs.StatusSucceeded || statuses[cavesID] != jobs.StatusFailed {
		t.Fatalf("statuses = %#v results=%#v", statuses, results)
	}
}

func TestBatchPlanUsesRoomScopedTargetsAndMergedCapacityConfirmation(t *testing.T) {
	roomAID := rooms.EncodeID("cluster_a")
	roomBID := rooms.EncodeID("cluster_b")
	worldID := rooms.EncodeID("Master")
	catalog := batchRooms{
		rooms: map[string]rooms.Room{
			roomAID: {ID: roomAID, DirectoryName: "cluster_a", Name: "房间 A", Managed: true},
			roomBID: {ID: roomBID, DirectoryName: "cluster_b", Name: "房间 B", Managed: true},
		},
		worlds: map[string][]rooms.World{
			roomAID: {{ID: worldID, RoomID: roomAID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}},
			roomBID: {{ID: worldID, RoomID: roomBID, DirectoryName: "Master", Name: "地表", Role: rooms.WorldRoleMaster}},
		},
	}
	control := &fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}}
	operations := NewOperations(catalog, control)
	operations.pollInterval = time.Millisecond
	operations.startTimeout = 100 * time.Millisecond
	resolver := &fakePlacementResolver{
		applied: topology.ExecutionPlacement{AppliedTargetID: "local"},
		batchPreview: topology.BatchStartCapacityPreview{
			RequiresRiskConfirmation: true,
			Targets:                  []topology.StartCapacityTarget{{TargetID: "local", RequiresRiskConfirmation: true}},
		},
	}
	leaseService := &fakeLeaseService{lease: operationlease.Lease{
		LeaseID: "lease", OperationKey: "operation", FencingToken: 1, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}}
	if err := operations.ConfigureDistributed(resolver, &fakeRemoteExecutor{}, leaseService); err != nil {
		t.Fatal(err)
	}
	selections := []BatchRoomSelection{{RoomID: roomAID}, {RoomID: roomBID}}
	if _, _, err := operations.PlanBatch(ActionStart, []BatchRoomSelection{{RoomID: roomAID}, {RoomID: roomAID}}); !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("duplicate room error = %v", err)
	}
	err := operations.RequireBatchCapacityConfirmation(context.Background(), ActionStart, selections, false)
	var risk *BatchCapacityRiskError
	if !errors.As(err, &risk) || !risk.Preview.RequiresRiskConfirmation {
		t.Fatalf("risk error = %#v", err)
	}
	targets, runner, err := operations.PlanBatch(ActionStart, selections)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].ID == targets[1].ID || targets[0].ID != batchTargetID(roomAID, worldID) {
		t.Fatalf("targets = %#v", targets)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(control.calls) != "[start:Master start:Master]" || len(results) != 2 {
		t.Fatalf("calls=%v results=%#v", control.calls, results)
	}
	if results[0].TargetID == results[1].TargetID || results[0].Status != jobs.StatusSucceeded || results[1].Status != jobs.StatusSucceeded {
		t.Fatalf("results = %#v", results)
	}
}

func TestDistributedOperationUsesAppliedRemoteTargetWithLeaseAndNoLocalFallback(t *testing.T) {
	control := &fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}}
	operations := testOperations(control)
	resolver := &fakePlacementResolver{
		applied:  topology.ExecutionPlacement{AppliedTargetID: "agent:node-a"},
		resolved: topology.ExecutionPlacement{Revision: "revision-1", AppliedTargetID: "agent:node-a", Target: agents.RuntimeTarget{AgentID: "node-a"}},
	}
	remote := &fakeRemoteExecutor{}
	leaseService := &fakeLeaseService{lease: operationlease.Lease{
		RoomID: rooms.EncodeID("summer_2026"), LeaseID: "lease-1", OperationKey: "plan-1",
		FencingToken: 7, ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}}
	if err := operations.ConfigureDistributed(resolver, remote, leaseService); err != nil {
		t.Fatal(err)
	}
	observer := &fakeOperationObserver{}
	operations.ConfigureObserver(observer)
	masterID := rooms.EncodeID("Master")
	_, runner, err := operations.Plan(ActionStart, rooms.EncodeID("summer_2026"), []string{masterID})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	runnerContext := WithOperationAudit(context.Background(), OperationAuditMetadata{JobID: "job-1", RequestID: "request-1", Source: "api"})
	if err := runner(runnerContext, func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != jobs.StatusSucceeded || remote.calls != 1 || remote.targetID != "agent:node-a" {
		t.Fatalf("results=%#v remote=%#v", results, remote)
	}
	if remote.request.Action != shared.ShardActionStart || remote.request.FencingToken != 7 || remote.request.LeaseID != "lease-1" || remote.request.OperationKey == "" {
		t.Fatalf("request=%#v", remote.request)
	}
	if len(control.calls) != 0 || !leaseService.released {
		t.Fatalf("local calls=%v released=%v", control.calls, leaseService.released)
	}
	if len(observer.items) != 1 || observer.items[0].TargetID != "agent:node-a" || observer.items[0].FencingToken != 7 || observer.items[0].JobID != "job-1" {
		t.Fatalf("audit=%#v", observer.items)
	}

	remote.err = errors.New("connection lost")
	_, runner, err = operations.Plan(ActionRestart, rooms.EncodeID("summer_2026"), []string{masterID})
	if err != nil {
		t.Fatal(err)
	}
	results = nil
	_ = runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) })
	if len(results) != 1 || results[0].Status != jobs.StatusFailed || len(control.calls) != 0 {
		t.Fatalf("remote failure fell back locally: results=%#v calls=%v", results, control.calls)
	}
}

func TestDistributedStatusIsReadOnlyAndLeaseBusyBlocksExecution(t *testing.T) {
	control := &fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}}
	operations := testOperations(control)
	resolver := &fakePlacementResolver{
		applied:  topology.ExecutionPlacement{AppliedTargetID: "agent:node-a"},
		resolved: topology.ExecutionPlacement{Revision: "revision-1", AppliedTargetID: "agent:node-a"},
	}
	remote := &fakeRemoteExecutor{}
	leaseService := &fakeLeaseService{acquireErr: operationlease.ErrBusy}
	if err := operations.ConfigureDistributed(resolver, remote, leaseService); err != nil {
		t.Fatal(err)
	}
	status, err := operations.StatusFor(context.Background(), rooms.EncodeID("summer_2026"), rooms.EncodeID("Master"))
	if err != nil || status.State != RuntimeRunning || remote.request.Action != shared.ShardActionStatus || remote.request.FencingToken != 0 {
		t.Fatalf("status=%#v request=%#v err=%v", status, remote.request, err)
	}
	remote.calls = 0
	_, runner, err := operations.Plan(ActionSave, rooms.EncodeID("summer_2026"), []string{rooms.EncodeID("Master")})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	_ = runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) })
	if len(results) != 1 || results[0].Error == nil || results[0].Error.Code != "ROOM_LEASE_BUSY" || remote.calls != 0 {
		t.Fatalf("results=%#v remote calls=%d", results, remote.calls)
	}
}

func TestRuntimeConfigurationRoutesLocalLifecycleThroughPlacedDriver(t *testing.T) {
	control := &fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}}
	operations := testOperations(control)
	roomID, worldID := rooms.EncodeID("summer_2026"), rooms.EncodeID("Master")
	resolver := &fakePlacementResolver{applied: topology.ExecutionPlacement{Revision: "revision-local", AppliedTargetID: "local"}}
	runtime := &fakePlacedRuntime{status: shared.ShardRuntimeStatus{State: string(RuntimeRunning), SessionExists: true}}
	leaseService := &fakeLeaseService{lease: operationlease.Lease{
		RoomID: roomID, LeaseID: "lease-local", FencingToken: 9, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}}
	if err := operations.ConfigureRuntime(resolver, runtime, leaseService); err != nil {
		t.Fatal(err)
	}
	_, runner, err := operations.Plan(ActionSave, roomID, []string{worldID})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != jobs.StatusSucceeded || runtime.calls != 1 {
		t.Fatalf("results=%#v runtime=%#v", results, runtime)
	}
	if runtime.roomID != roomID || runtime.worldID != worldID || runtime.request.TopologyRevision != "revision-local" || runtime.request.FencingToken != 9 {
		t.Fatalf("placed request=%#v", runtime.request)
	}
	if len(control.calls) != 0 {
		t.Fatalf("local control bypassed Runtime Driver: %v", control.calls)
	}
}

func TestStartUsesCurrentDiskStateWithoutManagedModLaunchOptions(t *testing.T) {
	control := &fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}}
	operations := testOperations(control)
	roomID, worldID := rooms.EncodeID("summer_2026"), rooms.EncodeID("Master")
	resolver := &fakePlacementResolver{applied: topology.ExecutionPlacement{Revision: "revision-local", AppliedTargetID: "local"}}
	runtime := &fakePlacedRuntime{status: shared.ShardRuntimeStatus{State: string(RuntimeStopped)}}
	leaseService := &fakeLeaseService{lease: operationlease.Lease{
		RoomID: roomID, LeaseID: "lease-mod-start", OperationKey: "room-start", FencingToken: 11,
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}}
	if err := operations.ConfigureRuntime(resolver, runtime, leaseService); err != nil {
		t.Fatal(err)
	}
	_, runner, err := operations.Plan(ActionStart, roomID, []string{worldID})
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := runner(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != jobs.StatusSucceeded {
		t.Fatalf("results=%#v", results)
	}
	if runtime.request.LaunchOptions.SkipUpdateServerMods {
		t.Fatalf("ordinary start overrode DST's on-disk Mod behavior: %#v", runtime.request)
	}
}

type modeRecordingControl struct {
	*fakeControl
	modes map[string]shared.RuntimePerformanceMode
}

func (c *modeRecordingControl) StartWithRuntimeMode(ctx context.Context, room, world string, mode shared.RuntimePerformanceMode) error {
	if err := c.fakeControl.Start(ctx, room, world); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modes[world] = mode
	return nil
}
func TestMaintenanceRestartPreservesDifferentWorldRuntimeModes(t *testing.T) {
	control := &modeRecordingControl{fakeControl: &fakeControl{
		running: map[string]bool{"summer_2026/Master": true, "summer_2026/Caves": true},
		status: map[string]RuntimeStatus{
			"summer_2026/Master": {State: RuntimeRunning, SessionExists: true, RuntimeMode: shared.RuntimePerformanceModeLuaJIT},
			"summer_2026/Caves":  {State: RuntimeRunning, SessionExists: true, RuntimeMode: shared.RuntimePerformanceModeArenaGC},
		}, fail: map[string]error{},
	}, modes: map[string]shared.RuntimePerformanceMode{}}
	operations := testOperations(control)
	_, run, err := operations.PlanWithOptions(ActionRestart, rooms.EncodeID("summer_2026"), nil, PlanOptions{PreserveRuntimeMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = run(context.Background(), func(result jobs.TargetResult) {
		if result.Status != jobs.StatusSucceeded {
			t.Errorf("result=%#v", result)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if control.modes["Master"] != shared.RuntimePerformanceModeLuaJIT || control.modes["Caves"] != shared.RuntimePerformanceModeArenaGC {
		t.Fatalf("modes=%v", control.modes)
	}
}
