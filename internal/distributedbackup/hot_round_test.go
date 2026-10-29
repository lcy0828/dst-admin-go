package distributedbackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dont/internal/runtimedriver"
	"dont/internal/shards"
)

func TestHotRoundRetriesOnlyTransientSaveConflicts(t *testing.T) {
	for _, code := range []string{"SAVE_IN_PROGRESS", "TARGET_SNAPSHOT_PASSED", "SAVE_WRITE_FAILED", "RUNTIME_UPDATE_REQUIRED"} {
		t.Run(code, func(t *testing.T) {
			f := newDistributedBackupFixture(t)
			drivers := installHotBarrierDrivers(t, f, nil)
			drivers[1].armFailures, drivers[1].armCode = 1, code
			result, err := f.coordinator.CreateWithMode(context.Background(), "room", "retry", "manual", "", ModeHot)
			retryable := code == "SAVE_IN_PROGRESS" || code == "TARGET_SNAPSHOT_PASSED"
			if retryable {
				if err != nil || result.Status != StatusVerified || result.BarrierAttempts != 2 || result.BarrierReleaseState != "released" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if drivers[0].prepareIDs[0] == drivers[0].prepareIDs[1] {
					t.Fatal("attempt reused a barrier identity")
				}
			} else if err == nil || result.BarrierAttempts != 1 || result.FailureCode != code {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			for _, d := range drivers {
				if !d.released {
					t.Fatal("attempt was not released")
				}
			}
		})
	}
}

func TestHotRoundExhaustsThreeAttemptsWithoutStoppingOrInventingCorruptFiles(t *testing.T) {
	f := newDistributedBackupFixture(t)
	drivers := installHotBarrierDrivers(t, f, nil)
	drivers[1].armFailures = 3
	result, err := f.coordinator.CreateWithMode(context.Background(), "room", "exhaust", "manual", "", ModeHot)
	if err == nil || result.BarrierAttempts != 3 || result.FailureCode != "SAVE_IN_PROGRESS" || result.FailurePhase != "barrier_arming" || result.BarrierReleaseState != "released" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result, err = f.coordinator.Get(result.ID)
	if err != nil || result.ArtifactState != "not_generated" || result.ValidationError != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, part := range result.Parts {
		if part.ArtifactState != "not_generated" || part.ValidationError != "" {
			t.Fatalf("part=%+v", part)
		}
	}
	for _, control := range []*backupTestControl{f.master, f.caves} {
		for _, n := range control.stops {
			if n != 0 {
				t.Fatal("hot retry stopped a world")
			}
		}
	}
}

func TestHotRoundCannotRetryUntilCancellationIsConfirmed(t *testing.T) {
	f := newDistributedBackupFixture(t)
	drivers := installHotBarrierDrivers(t, f, nil)
	drivers[1].armFailures = 1
	drivers[0].cancelErr = errors.New("node offline during cancellation")
	result, err := f.coordinator.CreateWithMode(context.Background(), "room", "offline", "manual", "", ModeHot)
	if err == nil || result.BarrierAttempts != 1 || result.BarrierReleaseState != "unconfirmed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	operations, _ := f.coordinator.Operations("room")
	if len(operations) != 1 || operations[0].Status != OperationRecoveryRequired {
		t.Fatalf("operations=%+v", operations)
	}
	if len(drivers[0].prepareIDs) != 1 {
		t.Fatal("started another attempt without release proof")
	}
}

func TestHotArchiveMustMatchTheCallbackSessionAndWrittenSnapshot(t *testing.T) {
	for _, mismatch := range []string{"snapshot", "session"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newDistributedBackupFixture(t)
			installHotBarrierDrivers(t, f, nil)
			path := filepath.Join(f.cavesRoot, "Cluster_1", "Caves", "save", "session", "SESSION", "0000000040")
			if mismatch == "snapshot" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(filepath.Dir(path), filepath.Join(filepath.Dir(filepath.Dir(path)), "OTHER")); err != nil {
					t.Fatal(err)
				}
			}
			result, err := f.coordinator.CreateWithMode(context.Background(), "room", "mismatch", "manual", "", ModeHot)
			if !errors.Is(err, ErrIntegrity) || result.Status == StatusVerified || !strings.Contains(err.Error(), "SNAPSHOT_ARCHIVE_MISMATCH") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestPublishedMissingAndCorruptArchivesStayDifferent(t *testing.T) {
	for _, state := range []string{"missing", "corrupt"} {
		t.Run(state, func(t *testing.T) {
			f := newDistributedBackupFixture(t)
			result, err := f.coordinator.Create(context.Background(), "room", "old-backup", "manual", "")
			if err != nil {
				t.Fatal(err)
			}
			path, _ := f.coordinator.partPath(result.Parts[0])
			if state == "missing" {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte("corrupt"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err = f.coordinator.Get(result.ID)
			if err != nil || result.ArtifactState != state || result.Restorable || result.ValidationError == "" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestInterruptedHotBackupRecoveryReleasesHoldsWithoutRestartingWorlds(t *testing.T) {
	f := newDistributedBackupFixture(t)
	drivers := installHotBarrierDrivers(t, f, nil)
	result, err := f.coordinator.CreateWithMode(context.Background(), "room", "interrupted", "manual", "", ModeHot)
	if err != nil {
		t.Fatal(err)
	}
	operations, _ := f.coordinator.Operations("room")
	operation := operations[0]
	operation.Phase, operation.Status = "barrier_waiting", OperationRunning
	if _, err = f.store.SaveOperation(operation); err != nil {
		t.Fatal(err)
	}
	result.Status, result.BarrierReleaseState = StatusCreating, "pending"
	if _, err = f.store.SaveSet(result); err != nil {
		t.Fatal(err)
	}
	// Simulate the controller dying with a held save, then the world stopping
	// independently. Recovery must never bring that world back as a side effect.
	for _, d := range drivers {
		d.receipt.State = "completed"
		d.receipt.Holding = true
		d.receipt.Busy = true
		d.released = false
	}
	f.caves.states["Cluster_1\x00Caves"] = shards.RuntimeStatus{State: shards.RuntimeStopped}
	recovered, err := f.coordinator.RecoverOperation(context.Background(), operation.ID)
	if err != nil || recovered.Status != OperationFailed {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	for _, d := range drivers {
		if !d.released {
			t.Fatal("hold survived recovery")
		}
	}
	for _, control := range []*backupTestControl{f.master, f.caves} {
		for _, n := range control.starts {
			if n != 0 {
				t.Fatal("hot backup recovery restarted a world")
			}
		}
	}
	result, _ = f.store.GetSet(result.ID)
	if result.BarrierReleaseState != "released" || result.FailureCode != "CONTROLLER_RESTARTED" {
		t.Fatalf("result=%+v", result)
	}
}

// Compile-time guarantees that both transports use the round-aware contract.
var _ runtimedriver.SnapshotRoundDriver = (*runtimedriver.Native)(nil)
var _ runtimedriver.SnapshotRoundDriver = (*runtimedriver.Agent)(nil)

type invalidReceiptDriver struct {
	*hotBarrierDriver
	cause           error
	cleanupReadable bool
}

func (d *invalidReceiptDriver) SnapshotBarrier(ctx context.Context, target runtimedriver.Target, id string) (runtimedriver.SnapshotBarrierReceipt, error) {
	if d.released && d.cleanupReadable {
		return d.hotBarrierDriver.SnapshotBarrier(ctx, target, id)
	}
	return runtimedriver.SnapshotBarrierReceipt{}, d.cause
}

func TestHotReceiptFailureKeepsPrimaryCauseAndReleaseEvidence(t *testing.T) {
	for _, readable := range []bool{true, false} {
		t.Run(fmt.Sprintf("cleanup-readable-%t", readable), func(t *testing.T) {
			f := newDistributedBackupFixture(t)
			drivers := installHotBarrierDrivers(t, f, nil)
			cause := fmt.Errorf("%w: json: unknown field %q", runtimedriver.ErrSnapshotReceiptInvalid, "futureMarker")
			broken := &invalidReceiptDriver{hotBarrierDriver: drivers[0], cause: cause, cleanupReadable: readable}
			router := f.coordinator.runtimes.(backupTestRouter)
			entry := router.values["master"]
			entry.driver = broken
			router.values["master"] = entry
			result, err := f.coordinator.CreateWithMode(context.Background(), "room", "bad-receipt", "snapshot", "job", ModeHot)
			if !errors.Is(err, ErrBarrierFailed) || !errors.Is(err, cause) || !errors.Is(err, runtimedriver.ErrSnapshotReceiptInvalid) {
				t.Fatalf("lost error causes: %v", err)
			}
			if strings.Count(err.Error(), cause.Error()) != 1 {
				t.Fatalf("duplicate receipt failure: %v", err)
			}
			if result.Status != StatusFailed || result.FailureCode != "SNAPSHOT_RECEIPT_INVALID" || result.FailurePhase != "barrier_waiting" || result.BarrierAttempts != 1 {
				t.Fatalf("unexpected failure result: %+v", result)
			}
			wantRelease, wantStatus := "released", OperationFailed
			if !readable {
				wantRelease, wantStatus = "unconfirmed", OperationRecoveryRequired
			}
			stored, err := f.store.GetSet(result.ID)
			if err != nil || stored.BarrierReleaseState != wantRelease || stored.FailureCode != result.FailureCode {
				t.Fatalf("stored result=%+v err=%v", stored, err)
			}
			details := result.FailureDetails()
			if details["barrierReleaseState"] != wantRelease || details["failureCode"] != "SNAPSHOT_RECEIPT_INVALID" || details["artifactState"] != "not_generated" {
				t.Fatalf("page/MCP job details=%v", details)
			}
			operations, _ := f.coordinator.Operations("room")
			if len(operations) != 1 || operations[0].Status != wantStatus {
				t.Fatalf("operations=%+v", operations)
			}
			if !readable {
				broken.cleanupReadable = true
				if _, err := f.coordinator.RecoverOperation(context.Background(), operations[0].ID); err != nil {
					t.Fatal(err)
				}
				recovered, err := f.store.GetSet(result.ID)
				if err != nil || recovered.BarrierReleaseState != "released" || recovered.FailureCode != stored.FailureCode || recovered.Failure != stored.Failure || recovered.FailurePhase != stored.FailurePhase {
					t.Fatalf("cleanup overwrote the primary diagnosis: %+v err=%v", recovered, err)
				}
			}
			for _, control := range []*backupTestControl{f.master, f.caves} {
				for _, n := range control.starts {
					if n != 0 {
						t.Fatal("receipt recovery restarted a world")
					}
				}
				for _, n := range control.stops {
					if n != 0 {
						t.Fatal("receipt failure stopped a world")
					}
				}
			}
		})
	}
}

func TestHotBackupErrorsPreserveDistinctCauses(t *testing.T) {
	primary := errors.New("save failed")
	cleanup := &runtimedriver.SnapshotBarrierError{Code: "SAVE_WRITE_FAILED", Message: "write still pending"}
	err := joinHotBackupErrors(errors.Join(ErrBarrierFailed, primary), cleanup, primary, cleanup)
	var typed *runtimedriver.SnapshotBarrierError
	if !errors.Is(err, primary) || !errors.Is(err, ErrBarrierFailed) || !errors.As(err, &typed) || typed != cleanup || strings.Count(err.Error(), primary.Error()) != 1 || strings.Count(err.Error(), cleanup.Error()) != 1 {
		t.Fatalf("lost/duplicated causes: %v", err)
	}
	if joinHotBackupErrors(nil, nil) != nil {
		t.Fatal("nil causes created a failure")
	}
}
