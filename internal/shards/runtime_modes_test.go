package shards

import (
	"context"
	"errors"
	"sync"
	"testing"

	"dont/internal/agents"
	"dont/internal/jobs"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"

	"github.com/google/uuid"
)

type memoryModePreferences struct {
	mu       sync.Mutex
	values   map[string]rooms.RuntimeModePreference
	err      error
	writeErr error
}

func (s *memoryModePreferences) RuntimeModePreferences(string) (map[string]rooms.RuntimeModePreference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := map[string]rooms.RuntimeModePreference{}
	for key, value := range s.values {
		result[key] = value
	}
	return result, s.err
}
func (s *memoryModePreferences) RememberRuntimeMode(roomID, worldID string, mode shared.RuntimePerformanceMode, revision string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.writeErr != nil {
		return s.writeErr
	}
	if s.values[worldID].Revision != revision {
		return rooms.ErrRuntimePreferenceConflict
	}
	s.values[worldID] = rooms.RuntimeModePreference{RoomID: roomID, WorldID: worldID, Mode: mode, Revision: uuid.NewString()}
	return nil
}

type memoryModePlans struct {
	values []jobs.RuntimeModeSelection
	err    error
}

func (s *memoryModePlans) RuntimeModePlan(string, string) ([]jobs.RuntimeModeSelection, error) {
	return append([]jobs.RuntimeModeSelection(nil), s.values...), s.err
}
func (s *memoryModePlans) SaveRuntimeModePlan(_, _ string, values []jobs.RuntimeModeSelection) error {
	if s.err != nil {
		return s.err
	}
	s.values = append([]jobs.RuntimeModeSelection(nil), values...)
	return nil
}

func modeOperations(t *testing.T) (*Operations, *modeRecordingControl, *memoryModePreferences, *memoryModePlans) {
	t.Helper()
	control := &modeRecordingControl{fakeControl: &fakeControl{running: map[string]bool{}, status: map[string]RuntimeStatus{}, fail: map[string]error{}}, modes: map[string]shared.RuntimePerformanceMode{}}
	prefs, plans := &memoryModePreferences{values: map[string]rooms.RuntimeModePreference{}}, &memoryModePlans{}
	operations := testOperations(control)
	if err := operations.ConfigureRuntimeModePersistence(prefs, plans); err != nil {
		t.Fatal(err)
	}
	return operations, control, prefs, plans
}
func runModeAction(t *testing.T, o *Operations, action Action, options PlanOptions) []jobs.TargetResult {
	t.Helper()
	_, run, err := o.PlanWithOptions(action, rooms.EncodeID("summer_2026"), nil, options)
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	if err := run(context.Background(), func(result jobs.TargetResult) { results = append(results, result) }); err != nil {
		t.Fatal(err)
	}
	return results
}
func requireModeSuccess(t *testing.T, results []jobs.TargetResult) {
	t.Helper()
	if len(results) != 2 {
		t.Fatalf("results=%+v", results)
	}
	for _, result := range results {
		if result.Status != jobs.StatusSucceeded || result.Warning != nil {
			t.Fatalf("result=%+v error=%+v warning=%+v", result, result.Error, result.Warning)
		}
	}
}
func (c *modeRecordingControl) Status(ctx context.Context, room, world string) (RuntimeStatus, error) {
	status, err := c.fakeControl.Status(ctx, room, world)
	c.mu.Lock()
	defer c.mu.Unlock()
	if status.State == RuntimeRunning && c.modes[world] != "" {
		status.RuntimeMode = c.modes[world]
	}
	return status, err
}

func TestRuntimeSelectionSurvivesStopAndNewOperationsInstance(t *testing.T) {
	o, control, prefs, plans := modeOperations(t)
	requireModeSuccess(t, runModeAction(t, o, ActionStart, PlanOptions{RuntimeMode: shared.RuntimePerformanceModeLuaJIT}))
	requireModeSuccess(t, runModeAction(t, o, ActionStop, PlanOptions{}))
	o = testOperations(control)
	_ = o.ConfigureRuntimeModePersistence(prefs, plans)
	requireModeSuccess(t, runModeAction(t, o, ActionStart, PlanOptions{}))
	for _, world := range []string{"Master", "Caves"} {
		if control.modes[world] != shared.RuntimePerformanceModeLuaJIT {
			t.Fatalf("lost mode: %v", control.modes)
		}
	}
	requireModeSuccess(t, runModeAction(t, o, ActionRestart, PlanOptions{RuntimeMode: shared.RuntimePerformanceModeGame}))
	for _, pref := range prefs.values {
		if pref.Mode != shared.RuntimePerformanceModeGame {
			t.Fatalf("explicit Game not remembered: %+v", prefs.values)
		}
	}
}
func TestOrdinaryRestartPreservesMixedWorldModes(t *testing.T) {
	o, control, _, _ := modeOperations(t)
	control.running["summer_2026/Master"], control.running["summer_2026/Caves"] = true, true
	control.modes["Master"], control.modes["Caves"] = shared.RuntimePerformanceModeLuaJIT, shared.RuntimePerformanceModeArenaGC
	requireModeSuccess(t, runModeAction(t, o, ActionRestart, PlanOptions{}))
	if control.modes["Master"] != shared.RuntimePerformanceModeLuaJIT || control.modes["Caves"] != shared.RuntimePerformanceModeArenaGC {
		t.Fatalf("modes=%v", control.modes)
	}
	availability, err := o.RuntimeModes(context.Background(), rooms.EncodeID("summer_2026"), nil)
	if err != nil || availability.SelectionState != "mixed" || availability.DefaultMode != "" || len(availability.Worlds) != 2 {
		t.Fatalf("availability=%+v err=%v", availability, err)
	}
}
func TestStopCapturesExistingRuntimeAndIsAvailableDuringStoreFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "capture", true: "store failure"}[fail], func(t *testing.T) {
			o, control, prefs, _ := modeOperations(t)
			control.running["summer_2026/Master"], control.running["summer_2026/Caves"] = true, true
			control.modes["Master"], control.modes["Caves"] = shared.RuntimePerformanceModeLuaJIT, shared.RuntimePerformanceModeLuaJIT
			if fail {
				prefs.err = errors.New("database unavailable")
			}
			results := runModeAction(t, o, ActionStop, PlanOptions{})
			for _, result := range results {
				if result.Status != jobs.StatusSucceeded || fail && result.Warning == nil {
					t.Fatalf("stop blocked or warning missing: %+v", result)
				}
			}
			if !fail {
				requireModeSuccess(t, runModeAction(t, o, ActionStart, PlanOptions{}))
				if control.modes["Master"] != shared.RuntimePerformanceModeLuaJIT {
					t.Fatal("stop lost unrecorded mode")
				}
			}
		})
	}
}
func TestFailedWorldDoesNotChangeSavedMode(t *testing.T) {
	o, control, prefs, _ := modeOperations(t)
	control.fail["start:Caves"] = errors.New("bad mod")
	results := runModeAction(t, o, ActionStart, PlanOptions{RuntimeMode: shared.RuntimePerformanceModeLuaJIT})
	if len(results) != 2 || len(prefs.values) != 1 || prefs.values[rooms.EncodeID("Master")].Mode != shared.RuntimePerformanceModeLuaJIT {
		t.Fatalf("results=%+v prefs=%+v", results, prefs.values)
	}
}
func TestUnknownRunningModeNeverBecomesGame(t *testing.T) {
	o, control, _, _ := modeOperations(t)
	control.status["summer_2026/Master"] = RuntimeStatus{State: RuntimeRunning, SessionExists: true}
	results := runModeAction(t, o, ActionRestart, PlanOptions{})
	if len(control.calls) != 0 || len(results) != 2 || results[0].Status != jobs.StatusFailed {
		t.Fatalf("unknown mode stopped or downgraded: %v %+v", control.calls, results)
	}
	availability, err := o.RuntimeModes(context.Background(), rooms.EncodeID("summer_2026"), nil)
	if err != nil || availability.SelectionState != "unknown" || availability.DefaultMode != "" {
		t.Fatalf("availability=%+v err=%v", availability, err)
	}
	results = runModeAction(t, o, ActionStop, PlanOptions{})
	var warned bool
	for _, result := range results {
		if result.Status != jobs.StatusSucceeded {
			t.Fatalf("unknown mode blocked stop: %+v", result)
		}
		if result.TargetID == rooms.EncodeID("Master") && result.Warning != nil && result.Warning.Code == "RUNTIME_MODE_UNCONFIRMED" {
			warned = true
		}
	}
	if !warned {
		t.Fatal("unknown mode stop did not explain that the mode could not be remembered")
	}
}
func TestRestartPersistsPlanBeforeStopAndReusesModesOnRetry(t *testing.T) {
	o, control, prefs, plans := modeOperations(t)
	control.running["summer_2026/Master"], control.running["summer_2026/Caves"] = true, true
	control.modes["Master"], control.modes["Caves"] = shared.RuntimePerformanceModeLuaJIT, shared.RuntimePerformanceModeLuaJIT
	control.fail["start:Caves"] = errors.New("temporary failure")
	_, run, err := o.Plan(ActionRestart, rooms.EncodeID("summer_2026"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithOperationAudit(context.Background(), OperationAuditMetadata{JobID: "job-mode"})
	if err = run(ctx, func(jobs.TargetResult) {}); err != nil {
		t.Fatal(err)
	}
	if len(plans.values) != 2 || len(prefs.values) != 1 {
		t.Fatalf("plan=%+v prefs=%+v", plans.values, prefs.values)
	}
	delete(control.fail, "start:Caves")
	var results []jobs.TargetResult
	if err = run(ctx, func(r jobs.TargetResult) { results = append(results, r) }); err != nil {
		t.Fatal(err)
	}
	requireModeSuccess(t, results)
	if control.modes["Caves"] != shared.RuntimePerformanceModeLuaJIT {
		t.Fatal("retry downgraded mode")
	}
}
func TestPlanPersistenceFailurePreventsStoppingWorlds(t *testing.T) {
	o, control, _, plans := modeOperations(t)
	plans.err = errors.New("cannot save job")
	_, run, err := o.Plan(ActionRestart, rooms.EncodeID("summer_2026"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var results []jobs.TargetResult
	_ = run(WithOperationAudit(context.Background(), OperationAuditMetadata{JobID: "job"}), func(r jobs.TargetResult) { results = append(results, r) })
	if len(control.calls) != 0 || len(results) != 2 || results[0].Status != jobs.StatusFailed {
		t.Fatalf("calls=%v results=%+v", control.calls, results)
	}
}

type unavailableModeControl struct{ *modeRecordingControl }

func (*unavailableModeControl) requireRuntimeMode(mode shared.RuntimePerformanceMode) error {
	if mode != shared.RuntimePerformanceModeGame {
		return ErrRuntimeModeUnavailable
	}
	return nil
}

func TestUnavailableInheritedModeFailsBeforeStoppingAnyWorld(t *testing.T) {
	o, control, _, _ := modeOperations(t)
	control.running["summer_2026/Master"], control.running["summer_2026/Caves"] = true, true
	control.modes["Master"], control.modes["Caves"] = shared.RuntimePerformanceModeLuaJIT, shared.RuntimePerformanceModeGame
	o.control = &unavailableModeControl{control}
	results := runModeAction(t, o, ActionRestart, PlanOptions{})
	if len(control.calls) != 0 || len(results) != 2 || results[0].Error == nil || results[0].Error.Code != "RUNTIME_MODE_UNAVAILABLE" {
		t.Fatalf("calls=%v results=%+v", control.calls, results)
	}
}

func TestInheritedModesAcrossNodesDoNotRequireACommonLuaJITMode(t *testing.T) {
	o, control, _, _ := modeOperations(t)
	masterID, cavesID := rooms.EncodeID("Master"), rooms.EncodeID("Caves")
	control.running["summer_2026/Master"] = true
	control.modes["Master"] = shared.RuntimePerformanceModeLuaJIT
	local := topology.ExecutionPlacement{AppliedTargetID: "local", AppliedInstallationID: "default", Revision: "local-1", Target: agents.RuntimeTarget{ID: "local", Performance: &shared.RuntimePerformanceReport{Status: shared.RuntimePerformanceReady, CanEnable: true, PackageVersion: "3.4.0", SupportedModes: []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame, shared.RuntimePerformanceModeLuaJIT}}}}
	remotePlacement := topology.ExecutionPlacement{AppliedTargetID: "agent:windows", AppliedInstallationID: "default", Revision: "remote-1", Target: agents.RuntimeTarget{ID: "agent:windows", OS: "windows"}}
	resolver := &fakePlacementResolver{appliedByWorld: map[string]topology.ExecutionPlacement{masterID: local, cavesID: remotePlacement}, resolvedByWorld: map[string]topology.ExecutionPlacement{masterID: local, cavesID: remotePlacement}}
	if err := o.ConfigureDistributed(resolver, &fakeRemoteExecutor{}, &fakeLeaseService{}); err != nil {
		t.Fatal(err)
	}
	requireModeSuccess(t, runModeAction(t, o, ActionRestart, PlanOptions{}))
	if control.modes["Master"] != shared.RuntimePerformanceModeLuaJIT {
		t.Fatal("mixed nodes downgraded local world")
	}
}

func TestOldRetryCannotReplaceANewerModeChoice(t *testing.T) {
	o, control, prefs, _ := modeOperations(t)
	_, run, err := o.PlanWithOptions(ActionRestart, rooms.EncodeID("summer_2026"), nil, PlanOptions{RuntimeMode: shared.RuntimePerformanceModeLuaJIT})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithOperationAudit(context.Background(), OperationAuditMetadata{JobID: "old-job"})
	if err = run(ctx, func(jobs.TargetResult) {}); err != nil {
		t.Fatal(err)
	}
	worldID := rooms.EncodeID("Master")
	if err = prefs.RememberRuntimeMode(rooms.EncodeID("summer_2026"), worldID, shared.RuntimePerformanceModeGame, prefs.values[worldID].Revision); err != nil {
		t.Fatal(err)
	}
	previousCalls := len(control.calls)
	var results []jobs.TargetResult
	_ = run(ctx, func(result jobs.TargetResult) { results = append(results, result) })
	if len(control.calls) != previousCalls || len(results) != 2 || results[0].Error == nil || results[0].Error.Code != "RUNTIME_MODE_PREFERENCE_CHANGED" {
		t.Fatalf("stale retry affected world: %+v", results)
	}
}

func TestPreferenceWriteFailureReportsRunningWorldWithoutRememberingMode(t *testing.T) {
	o, control, prefs, _ := modeOperations(t)
	prefs.writeErr = errors.New("disk full")
	results := runModeAction(t, o, ActionStart, PlanOptions{RuntimeMode: shared.RuntimePerformanceModeLuaJIT})
	for _, result := range results {
		if result.Status != jobs.StatusSucceeded || result.Warning == nil || result.Warning.Code != "RUNTIME_MODE_PREFERENCE_SAVE_FAILED" {
			t.Fatalf("running world lost its persistence warning: %+v", result)
		}
	}
	if len(prefs.values) != 0 || !control.running["summer_2026/Master"] {
		t.Fatalf("prefs=%+v running=%v", prefs.values, control.running)
	}
}

func TestStartRetryResolvesPreviouslyUnavailablePlacementWithoutLosingMode(t *testing.T) {
	o, control, prefs, plans := modeOperations(t)
	roomID, masterID, cavesID := rooms.EncodeID("summer_2026"), rooms.EncodeID("Master"), rooms.EncodeID("Caves")
	_ = prefs.RememberRuntimeMode(roomID, cavesID, shared.RuntimePerformanceModeLuaJIT, "")
	placement := topology.ExecutionPlacement{AppliedTargetID: "local", AppliedInstallationID: "default", Revision: "v1", Target: agents.RuntimeTarget{ID: "local", Performance: &shared.RuntimePerformanceReport{Status: shared.RuntimePerformanceReady, CanEnable: true, SupportedModes: []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame, shared.RuntimePerformanceModeLuaJIT}}}}
	resolver := &fakePlacementResolver{applied: placement, resolved: placement, errByWorld: map[string]error{cavesID: agents.ErrAgentOffline}}
	_ = o.ConfigureDistributed(resolver, &fakeRemoteExecutor{}, &fakeLeaseService{})
	_, run, err := o.Plan(ActionStart, roomID, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithOperationAudit(context.Background(), OperationAuditMetadata{JobID: "partial-start"})
	var results []jobs.TargetResult
	_ = run(ctx, func(r jobs.TargetResult) { results = append(results, r) })
	if len(results) != 2 || !control.running["summer_2026/Master"] || control.running["summer_2026/Caves"] || len(plans.values) != 2 {
		t.Fatalf("initial partial start results=%+v plans=%+v", results, plans.values)
	}
	delete(resolver.errByWorld, cavesID)
	results = nil
	_ = run(ctx, func(r jobs.TargetResult) { results = append(results, r) })
	requireModeSuccess(t, results)
	if control.modes["Caves"] != shared.RuntimePerformanceModeLuaJIT || prefs.values[masterID].Mode != shared.RuntimePerformanceModeGame {
		t.Fatalf("retry lost mode: %+v", control.modes)
	}
}

func TestStartRetryDoesNotReportDifferentRunningModeAsSuccessful(t *testing.T) {
	o, control, _, _ := modeOperations(t)
	_, run, err := o.PlanWithOptions(ActionStart, rooms.EncodeID("summer_2026"), nil, PlanOptions{RuntimeMode: shared.RuntimePerformanceModeLuaJIT})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithOperationAudit(context.Background(), OperationAuditMetadata{JobID: "start-replay"})
	_ = run(ctx, func(jobs.TargetResult) {})
	control.modes["Caves"] = shared.RuntimePerformanceModeGame
	var results []jobs.TargetResult
	_ = run(ctx, func(r jobs.TargetResult) { results = append(results, r) })
	found := false
	for _, result := range results {
		if result.TargetID == rooms.EncodeID("Caves") && result.Error != nil && result.Error.Code == "RUNTIME_MODE_CHANGE_REQUIRES_RESTART" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mismatched running mode was accepted: %+v", results)
	}
}
