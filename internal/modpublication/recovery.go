package modpublication

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

func (c *Coordinator) Recover(ctx context.Context) ([]Publication, error) {
	active, err := c.store.Active()
	if err != nil {
		return nil, err
	}
	results := make([]Publication, 0, len(active))
	var combined error
	for _, publication := range active {
		if err := ctx.Err(); err != nil {
			return results, errors.Join(combined, err)
		}
		recovered, recoverErr := c.RecoverOne(ctx, publication.ID)
		if recoverErr != nil {
			combined = errors.Join(combined, recoverErr)
		}
		results = append(results, recovered)
	}
	return results, combined
}

func (c *Coordinator) RecoverOne(ctx context.Context, publicationID string) (Publication, error) {
	publication, err := c.store.Get(publicationID)
	if err != nil {
		return Publication{}, err
	}
	activationRecovery := publication.Status == StatusSucceeded && publication.RestartRequired &&
		(publication.Activation.Status == ActivationStatusPending || publication.Activation.Status == ActivationStatusRestarting || publication.Activation.Status == ActivationStatusConfirming)
	if publication.Status == StatusSucceeded && !activationRecovery || publication.Status == StatusFailed || publication.Status == StatusRolledBack {
		return publication, ErrConflict
	}
	recoveryAttemptID := publication.ID + ":recover:" + uuid.NewString()
	fences, acquireErr := c.acquireFences(ctx, publication.Plan, recoveryAttemptID)
	if acquireErr != nil {
		return publication, acquireErr
	}
	defer c.releaseFences(fences)
	publication.Fences = fences
	publication.UpdatedAt = c.now().UTC()
	if saved, saveErr := c.store.Save(publication); saveErr == nil {
		publication = saved
	} else {
		return publication, saveErr
	}
	var recovered Publication
	var recoverErr error
	if activationRecovery {
		recovered, recoverErr = c.activateCommitted(ctx, publication, fences, publication.Activation.Policy)
	} else if publication.CommitDecision {
		recovered, recoverErr = c.completeAndActivate(ctx, publication, fences)
	} else {
		recovered, recoverErr = c.recoverRollback(publication, fences)
	}
	return recovered, recoverErr
}

func (c *Coordinator) recoverRollback(publication Publication, fences []Fence) (Publication, error) {
	var rollbackErr error
	for planIndex := len(publication.Plan.Targets) - 1; planIndex >= 0; planIndex-- {
		target := publication.Plan.Targets[planIndex]
		resultIndex := publicationTargetIndex(publication, target)
		if resultIndex < 0 {
			rollbackErr = errors.Join(rollbackErr, ErrInvalidInput)
			continue
		}
		updatedFences, err := c.renewFences(context.Background(), fences)
		if err == nil {
			fences = updatedFences
			err = c.runtime.Rollback(context.Background(), target, c.operation(publication, target, fences, "rollback"))
		}
		result := publication.Targets[resultIndex]
		if err != nil {
			result.Status = StatusRecoveryRequired
			result.ErrorCode, result.ErrorMessage = "TARGET_ROLLBACK_FAILED", err.Error()
			rollbackErr = errors.Join(rollbackErr, err)
		} else {
			rolledAt := c.now().UTC()
			result.RolledBack, result.RolledBackAt = true, &rolledAt
			result.Status = StatusRolledBack
			result.ErrorCode, result.ErrorMessage = "", ""
		}
		result.UpdatedAt = c.now().UTC()
		publication.Targets[resultIndex] = result
		if _, err := c.store.SaveTarget(publication.ID, result); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	finished := c.now().UTC()
	publication.Fences = fences
	publication.UpdatedAt, publication.FinishedAt = finished, &finished
	if rollbackErr != nil {
		publication.Status, publication.Outcome = StatusRecoveryRequired, OutcomePartial
		publication.ErrorCode, publication.ErrorMessage = "ROLLBACK_RECOVERY_REQUIRED", rollbackErr.Error()
	} else {
		publication.Status, publication.Outcome = StatusRolledBack, OutcomeNone
		publication.ErrorCode, publication.ErrorMessage = "RECOVERED_WITHOUT_COMMIT", "未发现全局 commit decision，已回滚全部目标"
	}
	saved, saveErr := c.store.Save(publication)
	return saved, errors.Join(rollbackErr, saveErr)
}
