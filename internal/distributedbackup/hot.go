package distributedbackup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"

	"github.com/google/uuid"
)

const (
	barrierPrepareTimeout    = 30 * time.Second
	barrierCompleteTimeout   = 2 * time.Minute
	barrierProtectionTimeout = 180 * time.Second
)

type stagedHotPart struct {
	index      int
	runtime    runtimePart
	part       Part
	descriptor runtimedriver.BackupDescriptor
}

func (c *Coordinator) createHotWithPlan(ctx context.Context, set Set, operation Operation, runtimeParts []runtimePart, lease *operationlease.Lease) (result Set, returnErr error) {
	result = set
	if len(runtimeParts) == 0 || len(operation.OriginalRunningWorlds) != len(runtimeParts) {
		return c.failCreate(result, operation, ErrHotUnavailable)
	}
	for _, current := range runtimeParts {
		if current.state != string(shards.RuntimeRunning) || !runtimedriver.HasTargetCapability(current.driver, current.target, runtimedriver.CapabilitySnapshotBarrier) {
			return c.failCreate(result, operation, ErrHotUnavailable)
		}
		if _, ok := current.driver.(runtimedriver.SnapshotRoundDriver); !ok {
			return c.failCreate(result, operation, ErrHotUnavailable)
		}
	}
	// The context also bounds node-side staging. No retry extends protection.
	protectionCtx, cancelProtection := context.WithTimeout(ctx, barrierProtectionTimeout)
	defer cancelProtection()
	ctx = protectionCtx
	protectionDeadline := c.now().Add(barrierProtectionTimeout)
	barrierID := ""
	barrierActive := false
	defer func() {
		if !barrierActive {
			return
		}
		cleanupErr := c.cancelHotBarriers(runtimeParts, operation, lease, barrierID, result.BarrierAttempts)
		current, loadErr := c.store.GetSet(result.ID)
		if loadErr == nil {
			current.BarrierReleaseState = "released"
			if cleanupErr != nil {
				current.BarrierReleaseState = "unconfirmed"
			}
			saved, saveErr := c.store.SaveSet(current)
			if saveErr == nil {
				result = saved
			}
			returnErr = errors.Join(returnErr, saveErr)
		}
		returnErr = errors.Join(returnErr, cleanupErr, loadErr)
		if cleanupErr != nil {
			_ = c.markRecoveryRequired(&operation, returnErr)
		}
	}()
	master := -1
	for index, current := range runtimeParts {
		if current.part.WorldRole == string(rooms.WorldRoleMaster) {
			if master >= 0 {
				return c.failCreate(result, operation, ErrBarrierFailed)
			}
			master = index
		}
	}
	if master < 0 {
		return c.failCreate(result, operation, ErrBarrierFailed)
	}
	preparationDeadline := time.Now().Add(barrierPrepareTimeout)
	var receipts []runtimedriver.SnapshotBarrierReceipt
	var snapshot int64
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		barrierID = "hot-" + uuid.NewString()
		result.BarrierID, result.BarrierAttempts, result.BarrierReleaseState = barrierID, attempt, "pending"
		saved, saveErr := c.store.SaveSet(result)
		if saveErr != nil {
			return result, saveErr
		}
		result = saved
		barrierActive = true
		receipts, snapshot, err = c.establishHotRound(ctx, runtimeParts, &operation, lease, master, barrierID, attempt, preparationDeadline)
		if err == nil {
			break
		}
		if !runtimedriver.RetryableSnapshotBarrier(err) || attempt == 3 || !time.Now().Before(preparationDeadline) {
			return c.failCreate(result, operation, errors.Join(ErrBarrierFailed, err))
		}
		if cleanupErr := c.cancelHotBarriers(runtimeParts, operation, lease, barrierID, attempt); cleanupErr != nil {
			return c.failCreate(result, operation, errors.Join(ErrBarrierFailed, err, cleanupErr))
		}
		barrierActive = false
		result.BarrierReleaseState = "released"
		saved, saveErr = c.store.SaveSet(result)
		if saveErr != nil {
			return result, saveErr
		}
		result = saved
		if err = c.saveOperationPhase(&operation, "barrier_retrying", OperationRunning, ""); err != nil {
			return result, err
		}
		delay := time.Second
		if attempt == 2 {
			delay = 3 * time.Second
		}
		retryCtx, cancel := context.WithDeadline(ctx, preparationDeadline)
		err = nil
		timer := time.NewTimer(delay)
		select {
		case <-retryCtx.Done():
			err = retryCtx.Err()
		case <-timer.C:
		}
		timer.Stop()
		cancel()
		if err != nil {
			return c.failCreate(result, operation, errors.Join(ErrBarrierFailed, err))
		}
	}
	result.Snapshot = snapshot
	for index := range runtimeParts {
		completedAt := time.Unix(receipts[index].CompletedAtUnix, 0).UTC()
		part := runtimeParts[index].part
		part.BarrierSessionID = receipts[index].SessionID
		part.BarrierShardID = receipts[index].ShardID
		part.BarrierInstance = receipts[index].ProducerInstanceID
		part.SnapshotBefore = receipts[index].SnapshotBefore
		part.SnapshotAfter = receipts[index].SnapshotAfter
		part.BarrierCompleted = &completedAt
		saved, saveErr := c.store.SavePart(part)
		if saveErr != nil {
			return c.failCreate(result, operation, saveErr)
		}
		runtimeParts[index].part = saved
	}
	if saved, saveErr := c.store.SaveSet(result); saveErr != nil {
		return c.failCreate(result, operation, saveErr)
	} else {
		result = saved
	}
	if err := c.revalidateHotPlan(ctx, result.TopologyRevision, runtimeParts, receipts, barrierID, protectionDeadline); err != nil {
		return c.failCreate(result, operation, err)
	}
	if err := c.saveOperationPhase(&operation, "staging", OperationRunning, ""); err != nil {
		return result, err
	}
	staged := make([]stagedHotPart, 0, len(runtimeParts))
	defer func() {
		for _, stagedPart := range staged {
			_ = stagedPart.runtime.driver.ReleaseBackup(context.Background(), stagedPart.runtime.target, c.runtimeOperation(*lease, operation.ID, "release", stagedPart.index, 0), stagedPart.part.ID)
		}
	}()
	for index := range runtimeParts {
		if err := c.renewLease(ctx, lease); err != nil {
			return c.failCreate(result, operation, err)
		}
		descriptor, part, stageErr := c.stageDescriptor(ctx, runtimeParts[index], operation, lease, index)
		if stageErr != nil {
			return c.failCreate(result, operation, stageErr)
		}
		staged = append(staged, stagedHotPart{index: index, runtime: runtimeParts[index], part: part, descriptor: descriptor})
	}
	// Copies are only consistent if every save barrier survived staging. Once
	// these immutable parts are sealed, transfer can continue without the hold.
	if err := c.revalidateHotPlan(ctx, result.TopologyRevision, runtimeParts, receipts, barrierID, protectionDeadline); err != nil {
		return c.failCreate(result, operation, err)
	}
	if err := c.releaseHotBarriers(ctx, runtimeParts, operation, lease, barrierID); err != nil {
		return c.failCreate(result, operation, err)
	}
	barrierActive = false
	result.BarrierReleaseState = "released"
	if saved, saveErr := c.store.SaveSet(result); saveErr != nil {
		return c.failCreate(result, operation, saveErr)
	} else {
		result = saved
	}
	if err := c.saveOperationPhase(&operation, "transferring", OperationRunning, ""); err != nil {
		return result, err
	}
	verified := 0
	sharedSHA := ""
	for _, stagedPart := range staged {
		current, collectErr := c.collectStagedPart(ctx, stagedPart.runtime, operation, lease, stagedPart.index, stagedPart.descriptor, stagedPart.part)
		if collectErr != nil {
			return c.failCreate(result, operation, collectErr)
		}
		compatibleSHA, compatibilityErr := c.partSharedCompatibilitySHA(current)
		if compatibilityErr != nil {
			return c.failCreate(result, operation, compatibilityErr)
		}
		if sharedSHA == "" {
			sharedSHA = compatibleSHA
		} else if sharedSHA != compatibleSHA {
			return c.failCreate(result, operation, ErrSharedFilesDiffer)
		}
		verified++
	}
	if verified != len(runtimeParts) {
		return c.failCreate(result, operation, ErrIncomplete)
	}
	result, err = c.finalizeSet(result.ID, sharedSHA)
	if err != nil {
		return c.failCreate(result, operation, err)
	}
	if err := c.saveOperationPhase(&operation, "completed", OperationSucceeded, ""); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Coordinator) establishHotRound(ctx context.Context, parts []runtimePart, operation *Operation, lease *operationlease.Lease, master int, id string, attempt int, deadline time.Time) ([]runtimedriver.SnapshotBarrierReceipt, int64, error) {
	if err := c.saveOperationPhase(operation, "barrier_preparing", OperationRunning, ""); err != nil {
		return nil, 0, err
	}
	prepCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	prepared := make([]runtimedriver.SnapshotBarrierReceipt, len(parts))
	for index, current := range parts {
		if err := c.renewLease(prepCtx, lease); err != nil {
			return nil, 0, err
		}
		receipt, err := current.driver.(runtimedriver.SnapshotRoundDriver).PrepareSnapshotBarrier(prepCtx, current.target, c.runtimeOperation(*lease, operation.ID, "barrier-prepare", index, int64(attempt)), id)
		if err != nil {
			return nil, 0, err
		}
		if receipt.SchemaVersion != 2 || receipt.State != "prepared" || receipt.BarrierID != id || receipt.ProducerInstanceID == "" || receipt.SessionID == "" || receipt.ShardID == "" {
			return nil, 0, ErrBarrierFailed
		}
		prepared[index] = receipt
	}
	if err := c.saveOperationPhase(operation, "barrier_arming", OperationRunning, ""); err != nil {
		return nil, 0, err
	}
	order := []int{master}
	for i := range parts {
		if i != master {
			order = append(order, i)
		}
	}
	target := int64(0)
	for _, index := range order {
		current := parts[index]
		if err := c.renewLease(prepCtx, lease); err != nil {
			return nil, 0, err
		}
		receipt, err := current.driver.(runtimedriver.SnapshotRoundDriver).ArmSnapshotBarrier(prepCtx, current.target, c.runtimeOperation(*lease, operation.ID, "barrier-arm", index, int64(attempt)), id, target)
		if err != nil {
			return nil, 0, err
		}
		if !sameBarrierIdentity(receipt, prepared[index], id) || (receipt.State != "armed" && receipt.State != "completed") || receipt.TargetSnapshotAfter < 1 || target != 0 && receipt.TargetSnapshotAfter != target {
			return nil, 0, ErrBarrierFailed
		}
		target = receipt.TargetSnapshotAfter
	}
	if err := c.saveOperationPhase(operation, "barrier_committing", OperationRunning, ""); err != nil {
		return nil, 0, err
	}
	current := parts[master]
	if err := c.renewLease(ctx, lease); err != nil {
		return nil, 0, err
	}
	if err := current.driver.(runtimedriver.SnapshotRoundDriver).CommitSnapshotBarrier(ctx, current.target, c.runtimeOperation(*lease, operation.ID, "barrier-commit", master, int64(attempt)), id); err != nil {
		return nil, 0, err
	}
	if err := c.saveOperationPhase(operation, "barrier_waiting", OperationRunning, ""); err != nil {
		return nil, 0, err
	}
	return c.waitHotBarrier(ctx, parts, prepared, id, target)
}

func sameBarrierIdentity(r, prepared runtimedriver.SnapshotBarrierReceipt, id string) bool {
	return r.BarrierID == id && r.ProducerInstanceID == prepared.ProducerInstanceID && r.SessionID == prepared.SessionID && r.ShardID == prepared.ShardID && r.PreparedAtUnix == prepared.PreparedAtUnix
}

func (c *Coordinator) waitHotBarrier(ctx context.Context, parts []runtimePart, prepared []runtimedriver.SnapshotBarrierReceipt, barrierID string, target int64) ([]runtimedriver.SnapshotBarrierReceipt, int64, error) {
	waitCtx, cancel := context.WithTimeout(ctx, barrierCompleteTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	completed := make([]runtimedriver.SnapshotBarrierReceipt, len(parts))
	done := make([]bool, len(parts))
	for {
		for index, current := range parts {
			if done[index] {
				continue
			}
			receipt, err := current.driver.(runtimedriver.SnapshotBarrierDriver).SnapshotBarrier(waitCtx, current.target, barrierID)
			if err != nil {
				return nil, 0, err
			}
			if !sameBarrierIdentity(receipt, prepared[index], barrierID) {
				return nil, 0, ErrBarrierFailed
			}
			if receipt.State == "conflict" {
				return nil, 0, &runtimedriver.SnapshotBarrierError{Code: receipt.Code, Message: receipt.Message}
			}
			if receipt.State == "failed" || receipt.State == "cancelled" || receipt.State == "released" {
				return nil, 0, errors.Join(ErrBarrierFailed, &runtimedriver.SnapshotBarrierError{Code: receipt.Code, Message: receipt.Message})
			}
			if receipt.TargetSnapshotAfter != target {
				return nil, 0, ErrBarrierFailed
			}
			if receipt.State == "completed" {
				if receipt.Proof != "save_current_callback" || receipt.CompletedAtUnix < receipt.PreparedAtUnix || receipt.SnapshotAfter != target || receipt.SnapshotBefore != target-1 || !receipt.Holding || receipt.InFlight {
					return nil, 0, ErrBarrierFailed
				}
				completed[index], done[index] = receipt, true
			}
		}
		if allTrue(done) {
			return completed, target, nil
		}
		select {
		case <-waitCtx.Done():
			return nil, 0, errors.Join(ErrBarrierFailed, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func (c *Coordinator) revalidateHotPlan(ctx context.Context, revision string, parts []runtimePart, receipts []runtimedriver.SnapshotBarrierReceipt, barrierID string, protectionDeadline time.Time) error {
	executions, err := c.placements.ResolveRoomExecutions(ctx, parts[0].part.RoomID)
	if err != nil || len(executions) != len(parts) {
		return errors.Join(ErrTopologyChanged, err)
	}
	byWorld := make(map[string]runtimePart, len(parts))
	for _, part := range parts {
		byWorld[part.part.WorldID] = part
	}
	for _, execution := range executions {
		planned, exists := byWorld[execution.World.ID]
		appliedInstallationID := execution.AppliedInstallationID
		if appliedInstallationID == "" && execution.AppliedTargetID == planned.target.TargetID {
			appliedInstallationID = planned.target.InstallationID
		}
		if !exists || execution.Revision != revision ||
			execution.AppliedTargetID != planned.target.TargetID ||
			appliedInstallationID != planned.target.InstallationID {
			return ErrTopologyChanged
		}
	}
	for index, current := range parts {
		receipt, observeErr := current.driver.(runtimedriver.SnapshotBarrierDriver).SnapshotBarrier(ctx, current.target, barrierID)
		if observeErr != nil || receipt.State != "completed" || receipt.BarrierID != barrierID ||
			receipt.PreparedAtUnix < 1 || receipt.PreparedAtUnix != receipts[index].PreparedAtUnix ||
			receipt.Proof != "save_current_callback" || receipt.ProducerInstanceID != receipts[index].ProducerInstanceID ||
			receipt.SessionID != receipts[index].SessionID || receipt.ShardID != receipts[index].ShardID ||
			receipt.SnapshotBefore != receipts[index].SnapshotBefore || receipt.SnapshotAfter != receipts[index].SnapshotAfter ||
			receipt.CompletedAtUnix != receipts[index].CompletedAtUnix || receipt.TargetSnapshotAfter != receipts[index].TargetSnapshotAfter || !receipt.Holding || receipt.InFlight {
			return errors.Join(ErrBarrierFailed, observeErr)
		}
	}
	if !c.now().Before(protectionDeadline) {
		return fmt.Errorf("%w: snapshot protection exceeded its controller deadline", ErrBarrierFailed)
	}
	return nil
}

func (c *Coordinator) releaseHotBarriers(ctx context.Context, parts []runtimePart, operation Operation, lease *operationlease.Lease, barrierID string) error {
	releaseCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.renewLease(releaseCtx, lease); err != nil {
		return err
	}
	// Release every world before waiting for delayed writes on any one world.
	var failure error
	for index, current := range parts {
		err := current.driver.(runtimedriver.SnapshotBarrierDriver).ReleaseSnapshotBarrier(releaseCtx, current.target, c.runtimeOperation(*lease, operation.ID, "barrier-release", index, 0), barrierID)
		if err != nil {
			failure = errors.Join(failure, fmt.Errorf("release snapshot barrier for %s: %w", current.part.WorldName, err))
		}
	}
	for _, current := range parts {
		failure = errors.Join(failure, waitHotBarrierReleased(releaseCtx, current, barrierID))
	}
	return failure
}

func waitHotBarrierReleased(ctx context.Context, current runtimePart, barrierID string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		receipt, err := current.driver.(runtimedriver.SnapshotBarrierDriver).SnapshotBarrier(ctx, current.target, barrierID)
		if err == nil {
			if receipt.State == "released" && receipt.Message == "" && receipt.Proof == "save_current_callback" &&
				receipt.ProducerInstanceID == current.part.BarrierInstance && receipt.SessionID == current.part.BarrierSessionID &&
				receipt.ShardID == current.part.BarrierShardID && receipt.SnapshotAfter == current.part.SnapshotAfter && !receipt.Busy && !receipt.Holding && !receipt.InFlight {
				return nil
			}
			if receipt.State != "completed" && receipt.State != "released" {
				return ErrBarrierFailed
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrBarrierFailed, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Cancellation is acknowledged by every node, including any delayed normal
// or shutdown write. A delivered console command alone is not a release proof.
func (c *Coordinator) cancelHotBarriers(parts []runtimePart, operation Operation, lease *operationlease.Lease, barrierID string, attempt int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.renewLease(ctx, lease); err != nil {
		return err
	}
	// A disconnected target must not delay cancellation on healthy targets.
	failures := make(chan error, len(parts))
	for index, current := range parts {
		step := c.runtimeOperation(*lease, operation.ID, "barrier-cancel", index, int64(attempt))
		go func(current runtimePart, step runtimedriver.Operation) {
			barrier, ok := current.driver.(runtimedriver.SnapshotBarrierDriver)
			if !ok {
				failures <- ErrHotUnavailable
				return
			}
			failures <- barrier.CancelSnapshotBarrier(ctx, current.target, step, barrierID)
		}(current, step)
	}
	var failure error
	for range parts {
		failure = errors.Join(failure, <-failures)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		all := true
		for _, current := range parts {
			barrier, ok := current.driver.(runtimedriver.SnapshotBarrierDriver)
			if !ok {
				return errors.Join(failure, ErrHotUnavailable)
			}
			r, err := barrier.SnapshotBarrier(ctx, current.target, barrierID)
			if err != nil {
				return errors.Join(failure, err)
			}
			if r.BarrierID != barrierID || r.Busy || r.Holding || r.InFlight || (r.State != "released" && r.State != "cancelled" && r.State != "failed" && r.State != "conflict") {
				all = false
			}
		}
		if all {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(failure, ErrBarrierFailed, ctx.Err())
		case <-ticker.C:
		}
	}
}

func allTrue(values []bool) bool {
	for _, value := range values {
		if !value {
			return false
		}
	}
	return true
}
