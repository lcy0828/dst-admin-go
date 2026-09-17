package distributedbackup

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"

	"github.com/google/uuid"
)

const (
	barrierPrepareTimeout  = 30 * time.Second
	barrierCompleteTimeout = 2 * time.Minute
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
		if _, ok := current.driver.(runtimedriver.SnapshotBarrierDriver); !ok {
			return c.failCreate(result, operation, ErrHotUnavailable)
		}
	}
	barrierID := "hot-" + uuid.NewString()
	result.BarrierID = barrierID
	if saved, err := c.store.SaveSet(result); err != nil {
		return result, err
	} else {
		result = saved
	}

	prepared := make([]runtimedriver.SnapshotBarrierReceipt, len(runtimeParts))
	barrierActive := true
	defer func() {
		if barrierActive {
			c.cancelHotBarriers(runtimeParts, operation, lease, barrierID)
		}
	}()
	if err := c.saveOperationPhase(&operation, "barrier_preparing", OperationRunning, ""); err != nil {
		return result, err
	}
	master := -1
	for index, current := range runtimeParts {
		if current.part.WorldRole == string(rooms.WorldRoleMaster) {
			if master >= 0 {
				return c.failCreate(result, operation, ErrBarrierFailed)
			}
			master = index
		}
		if err := c.renewLease(ctx, lease); err != nil {
			return c.failCreate(result, operation, err)
		}
		barrier := current.driver.(runtimedriver.SnapshotBarrierDriver)
		prepareCtx, cancel := context.WithTimeout(ctx, barrierPrepareTimeout)
		receipt, err := barrier.PrepareSnapshotBarrier(prepareCtx, current.target, c.runtimeOperation(*lease, operation.ID, "barrier-prepare", index, 0), barrierID)
		cancel()
		if err != nil || receipt.State != "prepared" {
			return c.failCreate(result, operation, errors.Join(ErrBarrierFailed, err))
		}
		prepared[index] = receipt
	}
	if master < 0 {
		return c.failCreate(result, operation, ErrBarrierFailed)
	}
	if err := c.saveOperationPhase(&operation, "barrier_committing", OperationRunning, ""); err != nil {
		return result, err
	}
	masterBarrier := runtimeParts[master].driver.(runtimedriver.SnapshotBarrierDriver)
	if err := masterBarrier.CommitSnapshotBarrier(ctx, runtimeParts[master].target, c.runtimeOperation(*lease, operation.ID, "barrier-commit", master, 0), barrierID); err != nil {
		return c.failCreate(result, operation, errors.Join(ErrBarrierFailed, err))
	}
	if err := c.saveOperationPhase(&operation, "barrier_waiting", OperationRunning, ""); err != nil {
		return result, err
	}
	receipts, snapshot, err := c.waitHotBarrier(ctx, runtimeParts, prepared, barrierID)
	if err != nil {
		return c.failCreate(result, operation, err)
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
	if err := c.revalidateHotPlan(ctx, result.TopologyRevision, runtimeParts, receipts, barrierID); err != nil {
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
	if err := c.releaseHotBarriers(ctx, runtimeParts, operation, lease, barrierID); err != nil {
		return c.failCreate(result, operation, err)
	}
	barrierActive = false

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

func (c *Coordinator) waitHotBarrier(ctx context.Context, parts []runtimePart, prepared []runtimedriver.SnapshotBarrierReceipt, barrierID string) ([]runtimedriver.SnapshotBarrierReceipt, int64, error) {
	waitCtx, cancel := context.WithTimeout(ctx, barrierCompleteTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	completed := make([]runtimedriver.SnapshotBarrierReceipt, len(parts))
	done := make([]bool, len(parts))
	for {
		all := true
		for index, current := range parts {
			if done[index] {
				continue
			}
			all = false
			receipt, err := current.driver.(runtimedriver.SnapshotBarrierDriver).SnapshotBarrier(waitCtx, current.target, barrierID)
			if err != nil {
				continue
			}
			if receipt.State == "failed" || receipt.State == "cancelled" || receipt.ProducerInstanceID != prepared[index].ProducerInstanceID ||
				receipt.SessionID != prepared[index].SessionID || receipt.ShardID != prepared[index].ShardID || receipt.SnapshotBefore < prepared[index].SnapshotBefore {
				return nil, 0, ErrBarrierFailed
			}
			if receipt.State == "completed" && receipt.Proof == "save_current_callback" && receipt.CompletedAtUnix >= receipt.PreparedAtUnix && receipt.SnapshotAfter > receipt.SnapshotBefore {
				completed[index], done[index] = receipt, true
			}
		}
		if all || allTrue(done) {
			break
		}
		select {
		case <-waitCtx.Done():
			return nil, 0, errors.Join(ErrBarrierFailed, waitCtx.Err())
		case <-ticker.C:
		}
	}
	snapshots := make([]int64, 0, len(completed))
	for _, receipt := range completed {
		snapshots = append(snapshots, receipt.SnapshotAfter)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i] < snapshots[j] })
	if len(snapshots) == 0 || snapshots[0] != snapshots[len(snapshots)-1] {
		return nil, 0, ErrBarrierFailed
	}
	return completed, snapshots[0], nil
}

func (c *Coordinator) revalidateHotPlan(ctx context.Context, revision string, parts []runtimePart, receipts []runtimedriver.SnapshotBarrierReceipt, barrierID string) error {
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
		if observeErr != nil || receipt.State != "completed" || receipt.ProducerInstanceID != receipts[index].ProducerInstanceID || receipt.SnapshotAfter != receipts[index].SnapshotAfter {
			return errors.Join(ErrBarrierFailed, observeErr)
		}
	}
	return nil
}

func (c *Coordinator) releaseHotBarriers(ctx context.Context, parts []runtimePart, operation Operation, lease *operationlease.Lease, barrierID string) error {
	for index, current := range parts {
		if err := c.renewLease(ctx, lease); err != nil {
			return err
		}
		if err := current.driver.(runtimedriver.SnapshotBarrierDriver).ReleaseSnapshotBarrier(ctx, current.target, c.runtimeOperation(*lease, operation.ID, "barrier-release", index, 0), barrierID); err != nil {
			return fmt.Errorf("release snapshot barrier for %s: %w", current.part.WorldName, err)
		}
	}
	return nil
}

func (c *Coordinator) cancelHotBarriers(parts []runtimePart, operation Operation, lease *operationlease.Lease, barrierID string) {
	for index, current := range parts {
		barrier, ok := current.driver.(runtimedriver.SnapshotBarrierDriver)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = barrier.CancelSnapshotBarrier(ctx, current.target, c.runtimeOperation(*lease, operation.ID, "barrier-cancel", index, 0), barrierID)
		cancel()
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
