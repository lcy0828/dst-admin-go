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

type preflightPlacementResolver struct {
	*fakePlacementResolver
	preflight    topology.ResourcePreflight
	preflightErr error
	calls        int
}

func (resolver *preflightPlacementResolver) PreflightExecution(context.Context, string, []string) (topology.ResourcePreflight, error) {
	resolver.calls++
	return resolver.preflight, resolver.preflightErr
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
	running map[string]bool
	status  map[string]RuntimeStatus
	calls   []string
	fail    map[string]error
}

type fakePreparer struct {
	calls []string
	err   error
}

type interruptControl struct {
	mu      sync.Mutex
	started chan struct{}
	state   RuntimeStatus
	calls   []string
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

func TestStopRunsNotificationHookBeforeLifecycleControl(t *testing.T) {
	control := &fakeControl{
		running: map[string]bool{"summer_2026/Master": true, "summer_2026/Caves": true},
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

func TestRoomPreflightPreventsPartialStartWhenRemoteWorldIsUnavailable(t *testing.T) {
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
	if len(control.calls) != 0 || remote.calls != 0 {
		t.Fatalf("preflight allowed partial execution: local=%v remote=%d", control.calls, remote.calls)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v", results)
	}
	codes := map[string]string{}
	for _, result := range results {
		if result.Error == nil {
			t.Fatalf("result = %#v", result)
		}
		codes[result.TargetID] = result.Error.Code
	}
	if codes[masterID] != "ROOM_PREFLIGHT_ABORTED" || codes[cavesID] == "ROOM_PREFLIGHT_ABORTED" {
		t.Fatalf("codes = %#v", codes)
	}
}

func TestResourcePreflightPreventsEveryShardStart(t *testing.T) {
	roomID := rooms.EncodeID("summer_2026")
	control := &fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}}
	operations := testOperations(control)
	preflight := topology.ResourcePreflight{
		Ready: false,
		Conflicts: []topology.ResourceConflict{{
			Code: "UDP_PORT_CONFLICT", Port: 10999, Message: "UDP 10999 已被其他 Shard 占用",
		}},
	}
	resolver := &preflightPlacementResolver{
		fakePlacementResolver: &fakePlacementResolver{applied: topology.ExecutionPlacement{AppliedTargetID: "local"}},
		preflight:             preflight,
		preflightErr:          &topology.ResourceConflictError{Preflight: preflight},
	}
	if err := operations.ConfigureDistributed(resolver, &fakeRemoteExecutor{}, &fakeLeaseService{lease: operationlease.Lease{
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
	if resolver.calls != 1 || len(control.calls) != 0 || len(results) != 2 {
		t.Fatalf("preflight calls=%d control=%#v results=%#v", resolver.calls, control.calls, results)
	}
	for _, result := range results {
		if result.Error == nil || result.Error.Code != "RESOURCE_PREFLIGHT_FAILED" || result.Error.Message != preflight.Conflicts[0].Message {
			t.Fatalf("result=%#v", result)
		}
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
	if _, _, err := operations.PlanBatch(ActionStart, []BatchRoomSelection{{RoomID: roomAID}, {RoomID: roomAID}}, BatchPlanOptions{}); !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("duplicate room error = %v", err)
	}
	err := operations.RequireBatchCapacityConfirmation(context.Background(), ActionStart, selections, false)
	var risk *BatchCapacityRiskError
	if !errors.As(err, &risk) || !risk.Preview.RequiresRiskConfirmation {
		t.Fatalf("risk error = %#v", err)
	}
	targets, runner, err := operations.PlanBatch(ActionStart, selections, BatchPlanOptions{})
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
	if len(control.calls) != 0 || len(results) != 2 || results[0].Error == nil || results[0].Error.Code != "CAPACITY_RISK_CONFIRMATION_REQUIRED" {
		t.Fatalf("unconfirmed batch executed: calls=%v results=%#v", control.calls, results)
	}

	resolver.batchPreview.RequiresRiskConfirmation = false
	targets, runner, err = operations.PlanBatch(ActionStart, selections, BatchPlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	results = nil
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
	if len(results) != 1 || results[0].Status != jobs.StatusSucceeded || remote.calls != 2 || remote.targetID != "agent:node-a" {
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
