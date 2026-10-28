package runtimedriver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/runtimefiles"
	"dont/shared"
)

const snapshotBarrierRuntimeVersion = dstruntime.RuntimeVersion

var snapshotBarrierID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)

func (d *Agent) PrepareSnapshotBarrier(ctx context.Context, target Target, operation Operation, barrierID string) (SnapshotBarrierReceipt, error) {
	return prepareSnapshotBarrier(ctx, d, target, operation, barrierID)
}

func (d *Agent) CommitSnapshotBarrier(ctx context.Context, target Target, operation Operation, barrierID string) error {
	return sendSnapshotBarrier(ctx, d, target, operation, "Commit", barrierID)
}

func (d *Agent) SnapshotBarrier(ctx context.Context, target Target, barrierID string) (SnapshotBarrierReceipt, error) {
	return observeSnapshotBarrier(ctx, d, target, barrierID)
}

func (d *Agent) ReleaseSnapshotBarrier(ctx context.Context, target Target, operation Operation, barrierID string) error {
	return sendSnapshotBarrier(ctx, d, target, operation, "Release", barrierID)
}

func (d *Agent) CancelSnapshotBarrier(ctx context.Context, target Target, operation Operation, barrierID string) error {
	return sendSnapshotBarrier(ctx, d, target, operation, "Cancel", barrierID)
}

func (d *Native) PrepareSnapshotBarrier(ctx context.Context, target Target, operation Operation, barrierID string) (SnapshotBarrierReceipt, error) {
	return prepareSnapshotBarrier(ctx, d, target, operation, barrierID)
}

func (d *Native) CommitSnapshotBarrier(ctx context.Context, target Target, operation Operation, barrierID string) error {
	return sendSnapshotBarrier(ctx, d, target, operation, "Commit", barrierID)
}

func (d *Native) SnapshotBarrier(ctx context.Context, target Target, barrierID string) (SnapshotBarrierReceipt, error) {
	return observeSnapshotBarrier(ctx, d, target, barrierID)
}

func (d *Native) ReleaseSnapshotBarrier(ctx context.Context, target Target, operation Operation, barrierID string) error {
	return sendSnapshotBarrier(ctx, d, target, operation, "Release", barrierID)
}

func (d *Native) CancelSnapshotBarrier(ctx context.Context, target Target, operation Operation, barrierID string) error {
	return sendSnapshotBarrier(ctx, d, target, operation, "Cancel", barrierID)
}

func prepareSnapshotBarrier(ctx context.Context, driver Driver, target Target, operation Operation, barrierID string) (SnapshotBarrierReceipt, error) {
	if err := sendSnapshotBarrier(ctx, driver, target, operation, "Prepare", barrierID); err != nil {
		return SnapshotBarrierReceipt{}, err
	}
	return waitSnapshotBarrier(ctx, driver, target, barrierID, func(receipt SnapshotBarrierReceipt) bool { return receipt.State == "prepared" })
}

func (d *Native) ArmSnapshotBarrier(ctx context.Context, target Target, operation Operation, barrierID string, snapshot int64) (SnapshotBarrierReceipt, error) {
	return armSnapshotBarrier(ctx, d, target, operation, barrierID, snapshot)
}
func (d *Agent) ArmSnapshotBarrier(ctx context.Context, target Target, operation Operation, barrierID string, snapshot int64) (SnapshotBarrierReceipt, error) {
	return armSnapshotBarrier(ctx, d, target, operation, barrierID, snapshot)
}
func armSnapshotBarrier(ctx context.Context, driver Driver, target Target, operation Operation, barrierID string, snapshot int64) (SnapshotBarrierReceipt, error) {
	if snapshot < 0 || snapshot > 9007199254740991 {
		return SnapshotBarrierReceipt{}, ErrInvalidTarget
	}
	if err := sendSnapshotBarrier(ctx, driver, target, operation, "Arm", barrierID, snapshot); err != nil {
		return SnapshotBarrierReceipt{}, err
	}
	return waitSnapshotBarrier(ctx, driver, target, barrierID, func(r SnapshotBarrierReceipt) bool {
		return (r.State == "armed" || r.State == "completed") && r.TargetSnapshotAfter > 0 && (snapshot == 0 || r.TargetSnapshotAfter == snapshot)
	})
}
func waitSnapshotBarrier(ctx context.Context, driver Driver, target Target, barrierID string, accepted func(SnapshotBarrierReceipt) bool) (SnapshotBarrierReceipt, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var observationErr error
	for {
		receipt, err := observeSnapshotBarrier(ctx, driver, target, barrierID)
		observationErr = err
		var protocolErr *SnapshotBarrierError
		if errors.As(err, &protocolErr) {
			return SnapshotBarrierReceipt{}, err
		}
		if err == nil {
			if accepted(receipt) {
				return receipt, nil
			}
			if receipt.State == "failed" || receipt.State == "conflict" || receipt.State == "cancelled" {
				return SnapshotBarrierReceipt{}, &SnapshotBarrierError{Code: receipt.Code, Message: receipt.Message}
			}
		}
		select {
		case <-ctx.Done():
			return SnapshotBarrierReceipt{}, errors.Join(ctx.Err(), observationErr)
		case <-ticker.C:
		}
	}
}

func sendSnapshotBarrier(ctx context.Context, driver Driver, target Target, operation Operation, method, barrierID string, targetSnapshot ...int64) error {
	if !snapshotBarrierID.MatchString(barrierID) {
		return ErrInvalidTarget
	}
	argument := fmt.Sprintf("%q", barrierID)
	if len(targetSnapshot) > 0 {
		argument += fmt.Sprintf(",%d", targetSnapshot[0])
	}
	command := fmt.Sprintf(`local r=rawget(_G,"DSTAdmin"); local b=r~=nil and r.Barriers or nil; if b==nil or b.%s==nil or b.%s(%s)~=true then print("[DST-ADMIN-RUNTIME ERROR] code=SNAPSHOT_BARRIER_%s_FAILED") end`, method, method, argument, method)
	result, err := driver.SendConsole(ctx, target, operation, shared.RuntimeConsoleRequest{Mode: shared.ConsoleModeManaged, Command: command}, 30*time.Second)
	if err != nil {
		return err
	}
	if result.Outcome != shared.RuntimeOutcomeSent && result.Outcome != shared.RuntimeOutcomeConfirmed {
		return errors.New("snapshot barrier command was not accepted by the Runtime console")
	}
	return nil
}

func observeSnapshotBarrier(ctx context.Context, driver Driver, target Target, barrierID string) (SnapshotBarrierReceipt, error) {
	if !snapshotBarrierID.MatchString(barrierID) {
		return SnapshotBarrierReceipt{}, ErrInvalidTarget
	}
	bundle, err := driver.ReadArtifacts(ctx, target, shared.ArtifactRuntimeBarrier)
	if err != nil {
		return SnapshotBarrierReceipt{}, err
	}
	if err := runtimefiles.ValidateArtifactBundle(shared.ArtifactRuntimeBarrier, bundle); err != nil || len(bundle.Artifacts) != 1 {
		if err == nil {
			err = errors.New("snapshot barrier receipt is missing")
		}
		return SnapshotBarrierReceipt{}, err
	}
	var receipt SnapshotBarrierReceipt
	if err := runtimefiles.DecodeJSONArtifact(bundle.Artifacts[0].Data, &receipt); err != nil {
		return SnapshotBarrierReceipt{}, err
	}
	receipt.ReadAt = bundle.Artifacts[0].UpdatedAt
	if receipt.ProducerVersion != snapshotBarrierRuntimeVersion {
		return SnapshotBarrierReceipt{}, &SnapshotBarrierError{Code: "RUNTIME_UPDATE_REQUIRED", Message: fmt.Sprintf("snapshot barrier runtime version %q does not match %q; update the managed Runtime", receipt.ProducerVersion, snapshotBarrierRuntimeVersion)}
	}
	if receipt.SchemaVersion != 2 {
		return SnapshotBarrierReceipt{}, &SnapshotBarrierError{Code: "RUNTIME_UPDATE_REQUIRED", Message: "snapshot round protocol is unavailable; update the managed Runtime"}
	}
	if receipt.BarrierID != barrierID ||
		receipt.ProducerInstanceID == "" || receipt.SessionID == "" || receipt.ShardID == "" || receipt.PreparedAtUnix < 1 || receipt.SnapshotBefore < 0 {
		return SnapshotBarrierReceipt{}, errors.New("snapshot barrier receipt is invalid or stale")
	}
	return receipt, nil
}
