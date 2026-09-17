package runtimedriver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"dont/internal/runtimefiles"
	"dont/shared"
)

const snapshotBarrierRuntimeVersion = "2.4.6"

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
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		receipt, err := observeSnapshotBarrier(ctx, driver, target, barrierID)
		if err == nil && (receipt.State == "prepared" || receipt.State == "completed") {
			return receipt, nil
		}
		if err == nil && (receipt.State == "failed" || receipt.State == "cancelled") {
			return SnapshotBarrierReceipt{}, fmt.Errorf("snapshot barrier prepare failed: %s", receipt.Message)
		}
		select {
		case <-ctx.Done():
			return SnapshotBarrierReceipt{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func sendSnapshotBarrier(ctx context.Context, driver Driver, target Target, operation Operation, method, barrierID string) error {
	if !snapshotBarrierID.MatchString(barrierID) {
		return ErrInvalidTarget
	}
	command := fmt.Sprintf(`local r=rawget(_G,"DSTAdmin"); local b=r~=nil and r.Barriers or nil; if b==nil or b.%s==nil or b.%s("%s")~=true then print("[DST-ADMIN-RUNTIME ERROR] code=SNAPSHOT_BARRIER_%s_FAILED") end`, method, method, barrierID, method)
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
	if receipt.SchemaVersion != 1 || receipt.ProducerVersion != snapshotBarrierRuntimeVersion || receipt.BarrierID != barrierID ||
		receipt.ProducerInstanceID == "" || receipt.SessionID == "" || receipt.ShardID == "" || receipt.PreparedAtUnix < 1 || receipt.SnapshotBefore < 0 {
		return SnapshotBarrierReceipt{}, errors.New("snapshot barrier receipt is invalid or stale")
	}
	return receipt, nil
}
