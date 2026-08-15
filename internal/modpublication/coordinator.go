package modpublication

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type Coordinator struct {
	planner  *Planner
	runtime  Runtime
	leases   Lease
	backups  Backup
	store    *Store
	leaseTTL time.Duration
	now      func() time.Time
}

func NewCoordinator(planner *Planner, runtime Runtime, leases Lease, backups Backup, store *Store, leaseTTL time.Duration) (*Coordinator, error) {
	if planner == nil || runtime == nil || leases == nil || backups == nil || store == nil {
		return nil, ErrInvalidInput
	}
	if leaseTTL <= 0 {
		leaseTTL = 5 * time.Minute
	}
	return &Coordinator{planner: planner, runtime: runtime, leases: leases, backups: backups, store: store, leaseTTL: leaseTTL, now: time.Now}, nil
}

func (c *Coordinator) Preview(ctx context.Context, roomID string) (Plan, error) {
	return c.planner.Preview(ctx, roomID)
}

func (c *Coordinator) Get(id string) (Publication, error) { return c.store.Get(id) }

func (c *Coordinator) Publish(ctx context.Context, request PublishRequest) (Publication, error) {
	if !validID(request.ID) || request.SourceJobID != "" && !validID(request.SourceJobID) || validatePlan(request.Plan) != nil {
		return Publication{}, ErrInvalidInput
	}
	if existing, err := c.store.FindIdempotent(request.ID, request.SourceJobID); err == nil {
		if existing.Plan.PlanHash != request.Plan.PlanHash || existing.ID != request.ID && request.SourceJobID == "" {
			return existing, ErrIdempotencyConflict
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Publication{}, err
	}
	fences, err := c.acquireFences(ctx, request.Plan.AffectedRoomIDs, request.ID)
	if err != nil {
		return Publication{}, err
	}
	defer c.releaseFences(fences)
	if existing, err := c.store.FindIdempotent(request.ID, request.SourceJobID); err == nil {
		if existing.Plan.PlanHash != request.Plan.PlanHash {
			return existing, ErrIdempotencyConflict
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Publication{}, err
	}
	now := c.now().UTC()
	publication := Publication{
		ID: request.ID, SourceJobID: request.SourceJobID, RoomID: request.Plan.RoomID,
		Status: StatusPreviewed, Outcome: OutcomeNone, Plan: request.Plan, Fences: fences,
		RestartRequired: request.Plan.RestartRequired, CreatedAt: now, UpdatedAt: now,
	}
	for _, target := range request.Plan.Targets {
		publication.Targets = append(publication.Targets, TargetResult{
			TargetID: target.TargetID, InstallationID: target.InstallationID,
			Status: StatusPreviewed, UpdatedAt: now,
		})
	}
	publication, err = c.store.Create(publication)
	if err != nil {
		if existing, findErr := c.store.FindIdempotent(request.ID, request.SourceJobID); findErr == nil {
			if existing.Plan.PlanHash == request.Plan.PlanHash {
				return existing, nil
			}
			return existing, ErrIdempotencyConflict
		}
		return Publication{}, err
	}
	fresh, previewErr := c.planner.Preview(ctx, request.Plan.RoomID)
	if previewErr != nil {
		return c.fail(publication, "PREVIEW_RECHECK_FAILED", previewErr)
	}
	if fresh.TopologyRevision != request.Plan.TopologyRevision {
		return c.fail(publication, "TOPOLOGY_CHANGED", ErrTopologyChanged)
	}
	if fresh.PlanHash != request.Plan.PlanHash {
		return c.fail(publication, "PLAN_CHANGED", ErrPlanChanged)
	}
	if !fresh.Ready {
		return c.fail(publication, "PREVIEW_BLOCKED", ErrPreviewBlocked)
	}
	publication.Status = StatusPreparing
	publication.UpdatedAt = c.now().UTC()
	publication, err = c.store.Save(publication)
	if err != nil {
		return publication, err
	}
	fences, err = c.renewFences(ctx, fences)
	if err != nil {
		return c.fail(publication, "LEASE_RENEW_FAILED", err)
	}
	publication.Fences = fences
	backup, err := c.backups.CreateProtection(ctx, ProtectionRequest{
		PublicationID: publication.ID, RoomIDs: append([]string(nil), publication.Plan.AffectedRoomIDs...),
		TopologyRevision: publication.Plan.TopologyRevision, PlanHash: publication.Plan.PlanHash, Fences: append([]Fence(nil), fences...),
	})
	if err != nil || len(backup.IDs) == 0 {
		if err == nil {
			err = errors.New("protection backup returned no backup IDs")
		}
		return c.fail(publication, "PROTECTION_BACKUP_FAILED", err)
	}
	publication.ProtectionBackupIDs = normalizedStrings(backup.IDs)
	publication.UpdatedAt = c.now().UTC()
	publication, err = c.store.Save(publication)
	if err != nil {
		return publication, err
	}
	for index, target := range publication.Plan.Targets {
		fences, err = c.renewFences(ctx, fences)
		if err != nil {
			return c.rollbackAfterFailure(ctx, publication, fences, "LEASE_RENEW_FAILED", err)
		}
		publication.Fences = fences
		operation := c.operation(publication, target, fences, "ensure-cache")
		if err := c.runtime.EnsureCache(ctx, target, operation); err != nil {
			publication.Targets[index].Status = StatusFailed
			publication.Targets[index].ErrorCode, publication.Targets[index].ErrorMessage = "CACHE_ENSURE_FAILED", err.Error()
			_, _ = c.store.SaveTarget(publication.ID, publication.Targets[index])
			return c.rollbackAfterFailure(ctx, publication, fences, "CACHE_ENSURE_FAILED", err)
		}
		publication.Targets[index].CacheEnsured = true
		publication.Targets[index].Status = StatusPreparing
		publication.Targets[index].UpdatedAt = c.now().UTC()
		if savedTarget, saveErr := c.store.SaveTarget(publication.ID, publication.Targets[index]); saveErr != nil {
			err = saveErr
			return c.rollbackAfterFailure(ctx, publication, fences, "TARGET_STATE_FAILED", err)
		} else {
			publication.Targets[index] = savedTarget
		}
		operation = c.operation(publication, target, fences, "prepare")
		if err := c.runtime.Prepare(ctx, target, operation); err != nil {
			publication.Targets[index].Status = StatusFailed
			publication.Targets[index].ErrorCode, publication.Targets[index].ErrorMessage = "TARGET_PREPARE_FAILED", err.Error()
			_, _ = c.store.SaveTarget(publication.ID, publication.Targets[index])
			return c.rollbackAfterFailure(ctx, publication, fences, "TARGET_PREPARE_FAILED", err)
		}
		preparedAt := c.now().UTC()
		publication.Targets[index].Prepared, publication.Targets[index].PreparedAt = true, &preparedAt
		publication.Targets[index].Status = StatusPrepared
		publication.Targets[index].UpdatedAt = preparedAt
		if savedTarget, saveErr := c.store.SaveTarget(publication.ID, publication.Targets[index]); saveErr != nil {
			err = saveErr
			return c.rollbackAfterFailure(ctx, publication, fences, "TARGET_STATE_FAILED", err)
		} else {
			publication.Targets[index] = savedTarget
		}
	}
	publication.Status = StatusPrepared
	publication.Fences = fences
	publication.UpdatedAt = c.now().UTC()
	publication, err = c.store.Save(publication)
	if err != nil {
		return c.rollbackAfterFailure(ctx, publication, fences, "PUBLICATION_STATE_FAILED", err)
	}
	publication.Status = StatusPublishing
	publication.UpdatedAt = c.now().UTC()
	publication, err = c.store.Save(publication)
	if err != nil {
		return c.rollbackAfterFailure(ctx, publication, fences, "PUBLICATION_STATE_FAILED", err)
	}
	for index, target := range publication.Plan.Targets {
		fences, err = c.renewFences(ctx, fences)
		if err != nil {
			return c.rollbackAfterFailure(ctx, publication, fences, "LEASE_RENEW_FAILED", err)
		}
		operation := c.operation(publication, target, fences, "publish")
		if err := c.runtime.Publish(ctx, target, operation); err != nil {
			publication.Targets[index].Status = StatusFailed
			publication.Targets[index].ErrorCode, publication.Targets[index].ErrorMessage = "TARGET_PUBLISH_FAILED", err.Error()
			_, _ = c.store.SaveTarget(publication.ID, publication.Targets[index])
			return c.rollbackAfterFailure(ctx, publication, fences, "TARGET_PUBLISH_FAILED", err)
		}
		publishedAt := c.now().UTC()
		publication.Targets[index].Published, publication.Targets[index].PublishedAt = true, &publishedAt
		publication.Targets[index].Status, publication.Targets[index].UpdatedAt = StatusPublishing, publishedAt
		if savedTarget, saveErr := c.store.SaveTarget(publication.ID, publication.Targets[index]); saveErr != nil {
			err = saveErr
			return c.rollbackAfterFailure(ctx, publication, fences, "TARGET_STATE_FAILED", err)
		} else {
			publication.Targets[index] = savedTarget
		}
	}
	publication.Fences = fences
	publication, err = c.store.DecideCommit(publication.ID, c.now().UTC())
	if err != nil {
		if current, readErr := c.store.Get(publication.ID); readErr == nil && current.CommitDecision {
			return c.completeCommitted(ctx, current, fences)
		}
		return c.rollbackAfterFailure(ctx, publication, fences, "COMMIT_DECISION_FAILED", err)
	}
	return c.completeCommitted(ctx, publication, fences)
}

func (c *Coordinator) completeCommitted(ctx context.Context, publication Publication, fences []Fence) (Publication, error) {
	publication.Status = StatusCompleting
	publication.Outcome = OutcomeFull
	publication.Fences = fences
	publication.UpdatedAt = c.now().UTC()
	saved, err := c.store.Save(publication)
	if err != nil {
		publication.Status = StatusRecoveryRequired
		publication.ErrorCode, publication.ErrorMessage = "COMPLETE_STATE_FAILED", err.Error()
		publication.UpdatedAt = c.now().UTC()
		if recoverySaved, recoverySaveErr := c.store.Save(publication); recoverySaveErr == nil {
			return recoverySaved, errors.Join(ErrRecoveryRequired, err)
		} else {
			return publication, errors.Join(ErrRecoveryRequired, err, recoverySaveErr)
		}
	}
	publication = saved
	var completionErr error
	for index, target := range publication.Plan.Targets {
		fences, err = c.renewFences(ctx, fences)
		if err == nil {
			err = c.runtime.Complete(ctx, target, c.operation(publication, target, fences, "complete"))
		}
		if err != nil {
			publication.Targets[index].Status = StatusRecoveryRequired
			publication.Targets[index].ErrorCode, publication.Targets[index].ErrorMessage = "TARGET_COMPLETE_FAILED", err.Error()
			completionErr = errors.Join(completionErr, err)
		} else {
			completedAt := c.now().UTC()
			publication.Targets[index].Completed, publication.Targets[index].CompletedAt = true, &completedAt
			publication.Targets[index].Status = StatusSucceeded
			publication.Targets[index].ErrorCode, publication.Targets[index].ErrorMessage = "", ""
		}
		publication.Targets[index].UpdatedAt = c.now().UTC()
		if _, saveErr := c.store.SaveTarget(publication.ID, publication.Targets[index]); saveErr != nil {
			completionErr = errors.Join(completionErr, saveErr)
		}
	}
	publication.Fences = fences
	publication.UpdatedAt = c.now().UTC()
	if completionErr != nil {
		publication.Status, publication.Outcome = StatusRecoveryRequired, OutcomeFull
		publication.ErrorCode, publication.ErrorMessage = "COMPLETE_RECOVERY_REQUIRED", completionErr.Error()
		saved, saveErr := c.store.Save(publication)
		return saved, errors.Join(ErrRecoveryRequired, completionErr, saveErr)
	}
	finished := c.now().UTC()
	publication.Status, publication.Outcome = StatusSucceeded, OutcomeFull
	publication.ErrorCode, publication.ErrorMessage = "", ""
	publication.FinishedAt, publication.UpdatedAt = &finished, finished
	publication.RestartRequired = true
	saved, err = c.store.Save(publication)
	return saved, err
}

func (c *Coordinator) rollbackAfterFailure(ctx context.Context, publication Publication, fences []Fence, code string, cause error) (Publication, error) {
	var rollbackErr error
	rolledAny := false
	for index := len(publication.Plan.Targets) - 1; index >= 0; index-- {
		result := publication.Targets[index]
		if !result.Prepared && !result.CacheEnsured {
			continue
		}
		rolledAny = true
		target := publication.Plan.Targets[index]
		var err error
		fences, err = c.renewFences(context.Background(), fences)
		if err == nil {
			err = c.runtime.Rollback(context.Background(), target, c.operation(publication, target, fences, "rollback"))
		}
		if err != nil {
			result.Status = StatusRecoveryRequired
			result.ErrorCode, result.ErrorMessage = "TARGET_ROLLBACK_FAILED", err.Error()
			rollbackErr = errors.Join(rollbackErr, err)
		} else {
			rolledAt := c.now().UTC()
			result.RolledBack, result.RolledBackAt = true, &rolledAt
			result.Status = StatusRolledBack
		}
		result.UpdatedAt = c.now().UTC()
		publication.Targets[index] = result
		if _, err := c.store.SaveTarget(publication.ID, result); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	finished := c.now().UTC()
	publication.Fences, publication.ErrorCode, publication.ErrorMessage = fences, code, cause.Error()
	publication.UpdatedAt, publication.FinishedAt = finished, &finished
	if rollbackErr != nil {
		publication.Status = StatusRecoveryRequired
		publication.Outcome = OutcomePartial
		publication.ErrorMessage = errors.Join(cause, rollbackErr).Error()
	} else if rolledAny {
		publication.Status, publication.Outcome = StatusRolledBack, OutcomeNone
	} else {
		publication.Status, publication.Outcome = StatusFailed, OutcomeNone
	}
	saved, saveErr := c.store.Save(publication)
	return saved, errors.Join(cause, rollbackErr, saveErr)
}

func (c *Coordinator) fail(publication Publication, code string, cause error) (Publication, error) {
	finished := c.now().UTC()
	publication.Status, publication.Outcome = StatusFailed, OutcomeNone
	publication.ErrorCode, publication.ErrorMessage = code, cause.Error()
	publication.FinishedAt, publication.UpdatedAt = &finished, finished
	saved, err := c.store.Save(publication)
	return saved, errors.Join(cause, err)
}

func (c *Coordinator) operation(publication Publication, target TargetPlan, fences []Fence, action string) RuntimeOperation {
	return RuntimeOperation{
		PublicationID: publication.ID, PlanHash: publication.Plan.PlanHash, Fences: append([]Fence(nil), fences...), Action: action,
		IdempotencyKey: fmt.Sprintf("%s:%s:%s:%s", publication.ID, target.TargetID, target.InstallationID, action),
	}
}

func (c *Coordinator) acquireFences(ctx context.Context, roomIDs []string, publicationID string) ([]Fence, error) {
	roomIDs = normalizedStrings(roomIDs)
	if len(roomIDs) == 0 {
		return nil, ErrInvalidInput
	}
	fences := make([]Fence, 0, len(roomIDs))
	for _, roomID := range roomIDs {
		fence, err := c.leases.Acquire(ctx, roomID, "mod.publish:"+publicationID, c.leaseTTL)
		if err != nil {
			c.releaseFences(fences)
			return nil, err
		}
		if fence.RoomID != roomID || !validID(fence.LeaseID) || fence.FencingToken == 0 {
			c.releaseFences(append(fences, fence))
			return nil, ErrInvalidInput
		}
		fences = append(fences, fence)
	}
	return fences, nil
}

func (c *Coordinator) renewFences(ctx context.Context, fences []Fence) ([]Fence, error) {
	updated := append([]Fence(nil), fences...)
	for index := range updated {
		fence, err := c.leases.Renew(ctx, updated[index], c.leaseTTL)
		if err != nil {
			return updated, err
		}
		if fence.RoomID != updated[index].RoomID || fence.LeaseID != updated[index].LeaseID || fence.FencingToken != updated[index].FencingToken {
			return updated, ErrConflict
		}
		updated[index] = fence
	}
	return updated, nil
}

func (c *Coordinator) releaseFences(fences []Fence) {
	for index := len(fences) - 1; index >= 0; index-- {
		_ = c.leases.Release(fences[index])
	}
}

func publicationTargetIndex(publication Publication, target TargetPlan) int {
	for index := range publication.Targets {
		if publication.Targets[index].TargetID == target.TargetID && publication.Targets[index].InstallationID == target.InstallationID {
			return index
		}
	}
	return -1
}

func normalizedRoomIDs(values []string) []string {
	result := normalizedStrings(values)
	sort.Strings(result)
	return result
}

var _ = strings.Builder{}
