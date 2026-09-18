package distributedbackup

import (
	"context"
	"errors"
	"fmt"

	"dont/internal/roomops"

	"github.com/google/uuid"
)

// Recover resumes or rolls back operations left active by a controller crash.
// The application calls it once after startup; further recovery is explicit.
func (c *Coordinator) Recover(ctx context.Context) error {
	if err := c.recoverDeletions(ctx); err != nil {
		return err
	}
	operations, err := c.store.ActiveOperations()
	if err != nil {
		return err
	}
	var failures error
	for _, operation := range operations {
		if err := c.recoverOperation(ctx, operation); err != nil {
			failures = errors.Join(failures, fmt.Errorf("recover backup operation %s: %w", operation.ID, err))
		}
	}
	return failures
}

func (c *Coordinator) RecoverOperation(ctx context.Context, operationID string) (Operation, error) {
	operation, err := c.store.Operation(operationID)
	if err != nil {
		return Operation{}, err
	}
	if operation.Status != OperationRunning && operation.Status != OperationRecoveryRequired {
		return operation, nil
	}
	if err := c.recoverOperation(ctx, operation); err != nil {
		return Operation{}, err
	}
	return c.store.Operation(operationID)
}

func (c *Coordinator) recoverOperation(ctx context.Context, operation Operation) error {
	ctx, releaseRoom, err := roomops.Acquire(ctx, operation.RoomID)
	if err != nil {
		return err
	}
	defer releaseRoom()
	lease, err := c.leases.Acquire(ctx, operation.RoomID, "backup-set.recover:"+uuid.NewString(), leaseTTL)
	if err != nil {
		return err
	}
	defer c.leases.Release(lease)
	operation.LeaseID, operation.FencingToken = lease.LeaseID, lease.FencingToken
	_, current, revision, _, err := c.plan(ctx, operation.RoomID)
	if err != nil {
		return c.markRecoveryRequired(&operation, err)
	}
	if revision != operation.TopologyRevision {
		return c.markRecoveryRequired(&operation, ErrTopologyChanged)
	}
	if operation.Kind == "create" {
		value, loadErr := c.store.GetSet(operation.SetID)
		if loadErr != nil {
			return c.markRecoveryRequired(&operation, loadErr)
		}
		byWorld := runtimePartsByWorld(current)
		var cleanup error
		for index, part := range value.Parts {
			runtime, exists := byWorld[part.WorldID]
			if !exists {
				cleanup = errors.Join(cleanup, ErrTopologyChanged)
				continue
			}
			cleanup = errors.Join(cleanup, runtime.driver.ReleaseBackup(ctx, runtime.target, c.runtimeOperation(lease, operation.ID, "recover-release", index, 0), part.ID))
		}
		cleanup = errors.Join(cleanup, c.restartWorlds(ctx, current, operation.OriginalRunningWorlds, operation, &lease))
		value.Status, value.Failure, value.UpdatedAt = StatusFailed, "控制器重启中断了备份创建", c.now().UTC()
		verified := 0
		for _, part := range value.Parts {
			if part.Status == PartVerified {
				verified++
			}
		}
		if verified > 0 {
			value.Status = StatusPartial
		}
		_, saveErr := c.store.SaveSet(value)
		if cleanup != nil || saveErr != nil {
			return c.markRecoveryRequired(&operation, errors.Join(cleanup, saveErr))
		}
		return c.saveOperationPhase(&operation, "recovered", OperationFailed, value.Failure)
	}
	if operation.Kind != "restore" {
		return c.markRecoveryRequired(&operation, errors.New("unknown backup operation kind"))
	}
	value, err := c.store.GetSet(operation.SetID)
	if err != nil {
		return c.markRecoveryRequired(&operation, err)
	}
	plans, err := c.restorePlans(value, current, operation.ID)
	if err != nil {
		return c.markRecoveryRequired(&operation, err)
	}
	if operation.Phase == "published" || operation.Phase == "completed" {
		var cleanup error
		for index, plan := range plans {
			_, stepErr := plan.driver.CompleteRestore(ctx, plan.target, c.runtimeOperation(lease, operation.ID, "recover-complete", index, 0), plan.restoreID)
			cleanup = errors.Join(cleanup, stepErr)
		}
		cleanup = errors.Join(cleanup, c.restartWorlds(ctx, current, operation.OriginalRunningWorlds, operation, &lease))
		if cleanup != nil {
			return c.markRecoveryRequired(&operation, cleanup)
		}
		return c.saveOperationPhase(&operation, "completed", OperationSucceeded, "")
	}
	rollbackErr := c.rollbackRestore(ctx, plans, operation, &lease)
	restartErr := c.restartWorlds(ctx, current, operation.OriginalRunningWorlds, operation, &lease)
	if err := errors.Join(rollbackErr, restartErr); err != nil {
		return c.markRecoveryRequired(&operation, err)
	}
	return c.saveOperationPhase(&operation, "recovered", OperationRolledBack, "控制器重启后已回滚未完成的恢复")
}

func (c *Coordinator) markRecoveryRequired(operation *Operation, cause error) error {
	_ = c.saveOperationPhase(operation, operation.Phase, OperationRecoveryRequired, cause.Error())
	return errors.Join(ErrRecoveryIncomplete, cause)
}

func runtimePartsByWorld(values []runtimePart) map[string]runtimePart {
	result := make(map[string]runtimePart, len(values))
	for _, value := range values {
		result[value.part.WorldID] = value
	}
	return result
}
