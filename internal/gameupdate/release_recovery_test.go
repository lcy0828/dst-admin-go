package gameupdate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dont/internal/installationmaintenance"
	"dont/internal/runtimedriver"
	"dont/shared"
)

type releaseFaultRuntime struct {
	*coordinatorRuntime
	loseUpdateReply bool
	completed       bool
	updateID        string
	failedStop      bool
}

func (r *releaseFaultRuntime) UpdateInstallation(ctx context.Context, target ReleaseInstallationPlan, op runtimedriver.Operation, desired string, clean bool) (shared.RuntimeGameVersionResult, error) {
	result, err := r.coordinatorRuntime.UpdateInstallation(ctx, target, op, desired, clean)
	r.updateID = op.ID
	if r.loseUpdateReply {
		return shared.RuntimeGameVersionResult{}, errors.New("connection lost after dispatch")
	}
	return result, err
}
func (r *releaseFaultRuntime) ObserveUpdate(context.Context, ReleaseInstallationPlan, string) (shared.RuntimeOperationEvidence, error) {
	return shared.RuntimeOperationEvidence{OperationID: r.updateID, Action: string(shared.RuntimeActionGameVersionUpdate), Completed: r.completed, Outcome: shared.RuntimeOutcomeConfirmed}, nil
}
func (r *releaseFaultRuntime) Stop(ctx context.Context, shard ReleaseShardPlan, op runtimedriver.Operation) error {
	err := r.coordinatorRuntime.Stop(ctx, shard, op)
	if r.failedStop {
		r.failedStop = false
		return errors.New("stop reply lost")
	}
	return err
}

func selectedFixturePlan(t *testing.T, f releaseCoordinatorFixture) ReleasePlan {
	t.Helper()
	plan, err := f.coordinator.Preview(context.Background(), ReleasePreviewRequest{Installations: []ReleaseInstallationSelection{{TargetID: "agent:node-a", InstallationID: "primary"}}, Policy: ReleasePolicyInput{LoadConfirmation: ReleaseLoadConfirmationNone}})
	if err != nil || !plan.Ready {
		t.Fatalf("plan=%+v error=%v", plan, err)
	}
	return plan
}

func TestReleaseUnknownWriteQueriesOriginalReceiptAndNeverResends(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	runtime := &releaseFaultRuntime{coordinatorRuntime: f.runtime, loseUpdateReply: true}
	f.coordinator.runtime = runtime
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "lost-reply", Plan: selectedFixturePlan(t, f)})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) || value.ErrorCode != "UPDATE_OUTCOME_UNKNOWN" {
		t.Fatalf("stage=%s code=%s err=%v", value.Stage, value.ErrorCode, err)
	}
	operationID := value.Installations[0].OperationID
	// A Controller restart retains both update intent and the dispatch identity.
	if err := f.coordinator.store.Migrate(); err != nil {
		t.Fatal(err)
	}
	value, err = f.coordinator.Retry(context.Background(), value.ID)
	if !errors.Is(err, ErrReleaseRecoveryNeeded) || value.Installations[0].OperationID != operationID || f.runtime.updateCounts["agent:node-a\x00primary"] != 1 {
		t.Fatalf("uncertain update resent: %+v %v", value.Installations, err)
	}
	runtime.completed = true
	f.coordinator.backups = fullBackupVolume{f.coordinator.backups}
	value, err = f.coordinator.Retry(context.Background(), value.ID)
	if err != nil || value.Stage != ReleaseStageSucceeded || f.runtime.updateCounts["agent:node-a\x00primary"] != 1 {
		t.Fatalf("receipt recovery failed: stage=%s err=%v", value.Stage, err)
	}
}

func TestReleaseLostStopReplyRestoresOriginalWorldsWithoutWriting(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	f.coordinator.runtime = &releaseFaultRuntime{coordinatorRuntime: f.runtime, failedStop: true}
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "lost-stop", Plan: f.plan})
	if err == nil || value.Stage != ReleaseStageFailed {
		t.Fatalf("stage=%s err=%v", value.Stage, err)
	}
	for key, status := range f.runtime.states {
		if key != "room-b\x00Archive" && status.State != "running" {
			t.Fatalf("lost stop reply forgot recovery intention: %s", key)
		}
	}
	if len(f.runtime.updateCounts) != 0 {
		t.Fatal("game files written after stop failure")
	}
}

func TestReleaseColdProtectionStopsOnceAndCancellationRestores(t *testing.T) {
	for _, cancelDuringBackup := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "cancel"}[cancelDuringBackup], func(t *testing.T) {
			f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.coordinator.backups = regressionReleaseBackups{base: f.coordinator.backups.(*coordinatorBackups), after: func() {
				for key, status := range f.runtime.states {
					if status.State != "stopped" {
						t.Errorf("backup saw running world %s", key)
					}
				}
				if cancelDuringBackup {
					cancel()
				}
			}}
			value, err := f.coordinator.Publish(ctx, ReleasePublishRequest{ID: "cold-protection", Plan: f.plan})
			if cancelDuringBackup {
				if !errors.Is(err, context.Canceled) || value.Stage != ReleaseStageFailed {
					t.Fatalf("cancel stage=%s err=%v", value.Stage, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			counts := make(map[string]int)
			for _, event := range f.runtime.mutationEvents() {
				if strings.HasPrefix(event, "stop:") {
					counts[event]++
				}
			}
			for event, count := range counts {
				if count != 1 {
					t.Fatalf("stopped more than once: %s %d", event, count)
				}
			}
			if len(counts) != 3 {
				t.Fatalf("wrong original running set: %v", counts)
			}
			if cancelDuringBackup && len(f.runtime.updateCounts) != 0 {
				t.Fatal("write after cancel")
			}
			for key, status := range f.runtime.states {
				if key != "room-b\x00Archive" && status.State != "running" {
					t.Fatalf("world not restored: %s", key)
				}
			}
		})
	}
}

func TestReleaseRestoreRetryDoesNotStopOrDownloadAgain(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationLogs)
	f.runtime.logErrors["room-a\x00Master"] = errors.New("temporary log failure")
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "restore-only", Plan: f.plan})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) {
		t.Fatal(err)
	}
	before := len(f.runtime.mutationEvents())
	delete(f.runtime.logErrors, "room-a\x00Master")
	value, err = f.coordinator.Retry(context.Background(), value.ID)
	if err != nil || value.Stage != ReleaseStageSucceeded {
		t.Fatalf("restore=%s error=%v", value.Stage, err)
	}
	if len(f.runtime.mutationEvents()) != before {
		t.Fatalf("restoring reread logs must not mutate processes: %v", f.runtime.mutationEvents()[before:])
	}
}

func TestReleasePrewriteVersionDriftRequiresConfirmedRepreview(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	base := f.coordinator.backups.(*coordinatorBackups)
	f.coordinator.backups = regressionReleaseBackups{base: base, after: func() { f.coordinator.planner.latest = fixedLatest{version: "702"} }}
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "steam-drift", Plan: f.plan})
	if !errors.Is(err, ErrDesiredVersionChanged) || value.NextAction != "preview_and_retry" || len(f.runtime.updateCounts) != 0 {
		t.Fatalf("drift stage=%s next=%s err=%v", value.Stage, value.NextAction, err)
	}
	originalModes := value.Plan.Installations[0].Shards
	previewPlan := value.Plan
	previewPlan.DesiredVersion = ""
	previewPlan.SelectedInstallations = recoverySelections(value.Plan)
	fresh, err := f.coordinator.previewStoredPlan(context.Background(), previewPlan)
	if err != nil {
		t.Fatal(err)
	}
	value, err = f.coordinator.RetryConfirmed(context.Background(), value.ID, ReleaseRetryRequest{PlanHash: fresh.PlanHash, Confirmation: fresh.PlanHash}, "new-job")
	if err != nil || value.Stage != ReleaseStageSucceeded || value.Plan.DesiredVersion != "702" || len(value.PlanHistory) != 1 {
		t.Fatalf("repreview=%+v error=%v", value, err)
	}
	for i, shard := range value.Plan.Installations[0].Shards {
		if shard.WasRunning != originalModes[i].WasRunning {
			t.Fatal("repreview overwrote original restore intentions")
		}
	}
}

func TestReleaseMaintenanceSurvivesRestartAndMatchesAliases(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	f.runtime.updateErrors["agent:node-b\x00primary"] = errors.New("failed write")
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "retained-maintenance", Plan: f.plan})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) {
		t.Fatal(err)
	}
	gate := installationmaintenance.New(f.coordinator.store.CheckMaintenance)
	_, _, err = gate.Acquire(context.Background(), releaseResources(value.Plan), "")
	if !errors.Is(err, installationmaintenance.ErrBusy) {
		t.Fatalf("interrupted release lost maintenance: %v", err)
	}
	owner, release, err := gate.Acquire(context.Background(), releaseResources(value.Plan), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if installationmaintenance.Owner(owner) != value.ID {
		t.Fatal("lost operation ownership")
	}
}

func TestReleasePlanHashNormalizesDefaultModeAndKeepsLegacyHash(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	plan := f.plan
	original, _ := calculateReleasePlanHash(plan)
	plan.Installations[0].Shards[0].RuntimeMode = shared.RuntimePerformanceModeGame
	same, _ := calculateReleasePlanHash(plan)
	if original != same {
		t.Fatal("equivalent default mode changed v2 hash")
	}
	plan.Installations[0].Shards[0].RuntimeMode = shared.RuntimePerformanceModeLuaJIT
	changed, _ := calculateReleasePlanHash(plan)
	if changed == same {
		t.Fatal("real runtime mode change ignored")
	}
	plan.Version = 1
	plan.PlanHash, _ = calculateReleasePlanHash(plan)
	if err := validateReleasePlan(plan); err != nil {
		t.Fatalf("legacy hash no longer readable: %v", err)
	}
}

func TestReleaseUnknownWriteRequiresExplicitValidationRepair(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	snapshot := f.coordinator.planner.snapshots.(fakeReleaseSnapshots).value
	for i := range snapshot.Shards {
		snapshot.Shards[i].Target.Capabilities = append(snapshot.Shards[i].Target.Capabilities, RequiredUpdateRepairCapability)
	}
	f.coordinator.planner.snapshots = fakeReleaseSnapshots{snapshot}
	runtime := &releaseFaultRuntime{coordinatorRuntime: f.runtime, loseUpdateReply: true}
	f.coordinator.runtime = runtime
	plan := selectedFixturePlan(t, f)
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "repair-unknown", Plan: plan})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) {
		t.Fatal(err)
	}
	before := len(f.runtime.mutationEvents())
	value, err = f.coordinator.Retry(context.Background(), value.ID)
	if !errors.Is(err, ErrReleaseRecoveryNeeded) || len(f.runtime.mutationEvents()) != before {
		t.Fatalf("implicit resend occurred: %v", err)
	}
	request := ReleaseRetryRequest{ValidateUnknown: true}
	if _, err = f.coordinator.RetryConfirmed(context.Background(), value.ID, request, "job"); !errors.Is(err, ErrReleaseConfirmation) {
		t.Fatalf("unconfirmed repair: %v", err)
	}
	preview := value.Plan
	preview.SelectedInstallations = recoverySelections(value.Plan)
	preview.DesiredVersion, preview.Policy.Validate = "", true
	fresh, err := f.coordinator.previewStoredPlan(context.Background(), preview)
	if err != nil {
		t.Fatal(err)
	}
	runtime.loseUpdateReply = false
	oldID := runtime.updateID
	request.PlanHash, request.Confirmation = fresh.PlanHash, fresh.PlanHash
	value, err = f.coordinator.RetryConfirmed(context.Background(), value.ID, request, "repair-job")
	if err != nil || value.Stage != ReleaseStageSucceeded {
		t.Fatalf("explicit repair failed: %s %v", value.Stage, err)
	}
	if runtime.updateID == oldID || len(value.PlanHistory) != 1 || !value.Plan.Policy.Validate {
		t.Fatal("repair lost attempt identity or confirmation history")
	}
}

func TestReleaseConnectedSplitScopeDoesNotRestartPartialVersions(t *testing.T) {
	a := ReleaseInstallationPlan{TargetID: "local", InstallationID: "primary", ServerPath: "/game", Shards: []ReleaseShardPlan{{RoomID: "split"}}}
	b := ReleaseInstallationPlan{TargetID: "agent:other", InstallationID: "primary", ServerPath: "/game", Shards: []ReleaseShardPlan{{RoomID: "split"}}}
	c := ReleaseInstallationPlan{TargetID: "local", InstallationID: "independent", ServerPath: "/other", Shards: []ReleaseShardPlan{{RoomID: "independent"}}}
	value := Release{Plan: ReleasePlan{Installations: []ReleaseInstallationPlan{a, b, c}, AffectedRoomIDs: []string{"split", "independent"}}, Installations: []ReleaseInstallationResult{
		{TargetID: a.TargetID, InstallationID: a.InstallationID, Stage: ReleaseStageVerified},
		{TargetID: b.TargetID, InstallationID: b.InstallationID, Stage: ReleaseStageFailed},
		{TargetID: c.TargetID, InstallationID: c.InstallationID, Stage: ReleaseStageVerified},
	}}
	ready := verifiedReleaseScopes(value)
	if len(ready.Installations) != 1 || ready.Installations[0].InstallationID != "independent" {
		t.Fatalf("partial split scope was allowed to restart: %+v", ready)
	}
}

func TestReleaseAliasesShareOnePhysicalWrite(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	snapshot := f.coordinator.planner.snapshots.(fakeReleaseSnapshots).value
	for i := range snapshot.Shards {
		snapshot.Shards[i].Target.ID = "agent:one"
		snapshot.Shards[i].Target.Config.ServerPath = "/opt/dst/server"
		snapshot.Shards[i].Target.Config.InstallationID = "primary"
		if snapshot.Shards[i].Room.ID == "room-b" {
			snapshot.Shards[i].Target.Config.InstallationID = "alias"
		}
		snapshot.Shards[i].ServerRoot = "/opt/dst/server"
		snapshot.Shards[i].Target.Config.ServerPath = "/configured/" + snapshot.Shards[i].Target.Config.InstallationID
	}
	f.coordinator.planner.snapshots = fakeReleaseSnapshots{snapshot}
	for _, id := range []string{"primary", "alias"} {
		f.runtime.versions[releaseInstallationKey("agent:one", id)] = "700"
	}
	plan, err := f.coordinator.Preview(context.Background(), ReleasePreviewRequest{Installations: []ReleaseInstallationSelection{{TargetID: "agent:one", InstallationID: "primary"}}, Policy: ReleasePolicyInput{LoadConfirmation: ReleaseLoadConfirmationNone}})
	if err != nil || !plan.Ready || len(plan.Installations) != 2 {
		t.Fatalf("alias scope=%+v error=%v", plan, err)
	}
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "same-directory", Plan: plan})
	if err != nil || value.Stage != ReleaseStageSucceeded {
		t.Fatalf("stage=%s error=%v", value.Stage, err)
	}
	writes := 0
	for _, count := range f.runtime.updateCounts {
		writes += count
	}
	if writes != 1 {
		t.Fatalf("physical installation was written %d times", writes)
	}
}

func TestReleaseUnknownRepairRequiresAgentGuardCapability(t *testing.T) {
	f := newReleaseCoordinatorFixture(t, ReleaseLoadConfirmationNone)
	runtime := &releaseFaultRuntime{coordinatorRuntime: f.runtime, loseUpdateReply: true}
	f.coordinator.runtime = runtime
	value, err := f.coordinator.Publish(context.Background(), ReleasePublishRequest{ID: "old-agent-repair", Plan: selectedFixturePlan(t, f)})
	if !errors.Is(err, ErrReleaseRecoveryNeeded) {
		t.Fatal(err)
	}
	preview := value.Plan
	preview.SelectedInstallations = recoverySelections(value.Plan)
	preview.DesiredVersion, preview.Policy.Validate = "", true
	fresh, err := f.coordinator.previewStoredPlan(context.Background(), preview)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.runtime.mutationEvents())
	value, err = f.coordinator.RetryConfirmed(context.Background(), value.ID, ReleaseRetryRequest{ValidateUnknown: true, PlanHash: fresh.PlanHash, Confirmation: fresh.PlanHash}, "repair-job")
	if !errors.Is(err, ErrReleaseRecoveryNeeded) || len(f.runtime.mutationEvents()) != before {
		t.Fatalf("unsafe legacy repair stage=%s error=%v", value.Stage, err)
	}
	if value.Installations[0].ErrorCode != "UPDATE_REPAIR_UNSUPPORTED" {
		t.Fatalf("missing upgrade guidance: %+v", value.Installations)
	}
}

// Existing verified archives do not require space for a new backup when an
// update receipt is recovered; recovery must not be blocked by the full volume.
type fullBackupVolume struct{ ReleaseProtectionService }

func (fullBackupVolume) CheckProtectionSpace(context.Context) error {
	return errors.New("backup volume full")
}
