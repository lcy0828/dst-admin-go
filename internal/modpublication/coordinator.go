package modpublication

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dont/internal/operationprogress"
)

type Coordinator struct {
	planner                *Planner
	runtime                Runtime
	activation             ActivationRuntime
	leases                 Lease
	backups                Backup
	store                  *Store
	replicas               *ReplicaStore
	leaseTTL               time.Duration
	now                    func() time.Time
	activationPollInterval time.Duration
	notifier               LifecycleNotifier
}

func (c *Coordinator) ConfigureNotifier(notifier LifecycleNotifier) {
	c.notifier = notifier
}

func (c *Coordinator) ConfigureReplicaStore(store *ReplicaStore) error {
	if store == nil {
		return ErrInvalidInput
	}
	c.replicas = store
	return nil
}

func (c *Coordinator) RoomReplicas(roomID string) (RoomReplicaState, error) {
	if c.replicas == nil {
		return RoomReplicaState{}, ErrInvalidInput
	}
	return c.replicas.Room(roomID)
}

func (c *Coordinator) ConfirmWorldLoaded(ctx context.Context, plan Plan, roomID, worldID string) error {
	if c.activation == nil || validatePlan(plan) != nil || !validID(roomID) || !validID(worldID) {
		return ErrInvalidInput
	}
	var world WorldPlan
	found := false
	for _, target := range plan.Targets {
		for _, candidate := range target.Worlds {
			if candidate.RoomID == roomID && candidate.WorldID == worldID {
				world, found = candidate, true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		return ErrInvalidInput
	}
	_, _, confirmErr := c.confirmShardLoaded(ctx, world, LogCursor{}, LoadConfirmationLogs)
	if c.replicas == nil {
		return confirmErr
	}
	replicaErr := c.replicas.MarkWorldLoaded(plan, roomID, worldID, confirmErr == nil, confirmErr)
	return errors.Join(confirmErr, replicaErr)
}

func NewCoordinator(planner *Planner, runtime Runtime, leases Lease, backups Backup, store *Store, leaseTTL time.Duration, activations ...ActivationRuntime) (*Coordinator, error) {
	if planner == nil || runtime == nil || leases == nil || backups == nil || store == nil {
		return nil, ErrInvalidInput
	}
	if len(activations) > 1 {
		return nil, ErrInvalidInput
	}
	if leaseTTL <= 0 {
		leaseTTL = 5 * time.Minute
	}
	coordinator := &Coordinator{
		planner: planner, runtime: runtime, leases: leases, backups: backups, store: store,
		leaseTTL: leaseTTL, now: time.Now, activationPollInterval: time.Second,
	}
	if len(activations) == 1 {
		coordinator.activation = activations[0]
	}
	return coordinator, nil
}

func (c *Coordinator) Preview(ctx context.Context, roomID string) (Plan, error) {
	return c.planner.Preview(ctx, roomID)
}

// EnsureCache prepares exact Mod artifacts on every installation in the plan
// without publishing setup files, world overrides, or restarting shards.
func (c *Coordinator) EnsureCache(ctx context.Context, plan Plan) (CachePreparation, error) {
	result := CachePreparation{
		RoomID: plan.RoomID, TopologyRevision: plan.TopologyRevision, PlanHash: plan.PlanHash,
		Targets: make([]CacheTargetResult, 0, len(plan.Targets)), PreparedAt: c.now().UTC(),
	}
	if validatePlan(plan) != nil || !plan.Ready {
		return result, ErrInvalidInput
	}
	fresh, err := c.planner.Preview(ctx, plan.RoomID)
	if err != nil {
		return result, err
	}
	if fresh.TopologyRevision != plan.TopologyRevision {
		return result, ErrTopologyChanged
	}
	if fresh.PlanHash != plan.PlanHash {
		return result, ErrPlanChanged
	}
	if !fresh.Ready {
		return result, ErrPreviewBlocked
	}
	operationID := "modcache-" + plan.PlanHash[:32]
	fences, err := c.acquireFencesForResources(ctx, installationLeaseResources(plan), "mod.cache:"+plan.PlanHash[:32])
	if err != nil {
		return result, err
	}
	defer c.releaseFences(fences)
	publication := Publication{ID: operationID, RoomID: plan.RoomID, Plan: plan}
	for _, target := range plan.Targets {
		fences, err = c.renewFences(ctx, fences)
		item := CacheTargetResult{TargetID: target.TargetID, InstallationID: target.InstallationID, UpdatedAt: c.now().UTC()}
		if err == nil {
			err = c.runtime.EnsureCache(ctx, target, c.operation(publication, target, fences, "prepare-cache"))
		}
		if err != nil {
			item.ErrorCode, item.ErrorMessage = "CACHE_PREPARATION_FAILED", err.Error()
			result.Targets = append(result.Targets, item)
			result.PreparedAt = c.now().UTC()
			return result, err
		}
		item.Prepared = true
		item.UpdatedAt = c.now().UTC()
		result.Targets = append(result.Targets, item)
	}
	result.Ready = true
	result.PreparedAt = c.now().UTC()
	return result, nil
}

func (c *Coordinator) Get(id string) (Publication, error) { return c.store.Get(id) }

func (c *Coordinator) List(roomID string, limit, offset int) ([]Publication, int, error) {
	return c.store.List(roomID, limit, offset)
}

func (c *Coordinator) Publish(ctx context.Context, request PublishRequest) (Publication, error) {
	publication, transaction, err := c.beginTransaction(ctx, request)
	if err != nil || transaction == nil {
		return publication, err
	}
	defer transaction.Close()
	publication, err = transaction.Publish(ctx)
	if err != nil {
		return publication, err
	}
	return transaction.Commit(ctx)
}

func (c *Coordinator) BeginTransaction(ctx context.Context, request PublishRequest) (PreparedTransaction, error) {
	publication, transaction, err := c.beginTransaction(ctx, request)
	if err != nil {
		return nil, err
	}
	if transaction == nil {
		return nil, fmt.Errorf("%w: publication %s is already terminal", ErrConflict, publication.ID)
	}
	return transaction, nil
}

func (c *Coordinator) beginTransaction(ctx context.Context, request PublishRequest) (Publication, *publicationTransaction, error) {
	activation, activationErr := NormalizeActivationPolicy(request.Activation)
	if !validID(request.ID) || request.SourceJobID != "" && !validID(request.SourceJobID) || validatePlan(request.Plan) != nil || activationErr != nil {
		return Publication{}, nil, ErrInvalidInput
	}
	if existing, err := c.store.FindIdempotent(request.ID, request.SourceJobID); err == nil {
		if existing.Plan.PlanHash != request.Plan.PlanHash || existing.Activation.Policy != activation || existing.ID != request.ID && request.SourceJobID == "" {
			return existing, nil, ErrIdempotencyConflict
		}
		return existing, nil, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Publication{}, nil, err
	}
	fences, ownedFences, err := c.acquireFencesWithBorrowed(ctx, request.Plan, request.ID, request.BorrowedFences)
	if err != nil {
		return Publication{}, nil, err
	}
	releaseFences := true
	defer func() {
		if releaseFences {
			c.releaseFences(ownedFences)
		}
	}()
	if existing, err := c.store.FindIdempotent(request.ID, request.SourceJobID); err == nil {
		if existing.Plan.PlanHash != request.Plan.PlanHash || existing.Activation.Policy != activation {
			return existing, nil, ErrIdempotencyConflict
		}
		return existing, nil, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Publication{}, nil, err
	}
	now := c.now().UTC()
	publication := Publication{
		ID: request.ID, SourceJobID: request.SourceJobID, RoomID: request.Plan.RoomID,
		Status: StatusPreviewed, Outcome: OutcomeNone, Plan: request.Plan, Fences: fences,
		RestartRequired: request.Plan.RestartRequired, Activation: initialActivation(activation), CreatedAt: now, UpdatedAt: now,
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
			if existing.Plan.PlanHash == request.Plan.PlanHash && existing.Activation.Policy == activation {
				return existing, nil, nil
			}
			return existing, nil, ErrIdempotencyConflict
		}
		return Publication{}, nil, err
	}
	if c.replicas != nil {
		if err := c.replicas.SetDesired(publication.Plan); err != nil {
			failed, failErr := c.fail(publication, "REPLICA_DESIRED_STATE_FAILED", err)
			return failed, nil, failErr
		}
	}
	fresh, previewErr := c.planner.Preview(ctx, request.Plan.RoomID)
	if previewErr != nil {
		failed, failErr := c.fail(publication, "PREVIEW_RECHECK_FAILED", previewErr)
		return failed, nil, failErr
	}
	if fresh.TopologyRevision != request.Plan.TopologyRevision {
		failed, failErr := c.fail(publication, "TOPOLOGY_CHANGED", ErrTopologyChanged)
		return failed, nil, failErr
	}
	if fresh.PlanHash != request.Plan.PlanHash {
		failed, failErr := c.fail(publication, "PLAN_CHANGED", ErrPlanChanged)
		return failed, nil, failErr
	}
	if !fresh.Ready {
		failed, failErr := c.fail(publication, "PREVIEW_BLOCKED", ErrPreviewBlocked)
		return failed, nil, failErr
	}
	publication.Status = StatusPreparing
	publication.UpdatedAt = c.now().UTC()
	publication, err = c.store.Save(publication)
	if err != nil {
		return publication, nil, err
	}
	fences, err = c.renewFences(ctx, fences)
	if err != nil {
		failed, failErr := c.fail(publication, "LEASE_RENEW_FAILED", err)
		return failed, nil, failErr
	}
	publication.Fences = fences
	if !request.SkipProtectionBackup {
		backup, backupErr := c.backups.CreateProtection(ctx, ProtectionRequest{
			PublicationID: publication.ID, RoomIDs: append([]string(nil), publication.Plan.AffectedRoomIDs...),
			TopologyRevision: publication.Plan.TopologyRevision, PlanHash: publication.Plan.PlanHash, Fences: append([]Fence(nil), fences...),
		})
		if backupErr != nil || len(backup.IDs) == 0 {
			if backupErr == nil {
				backupErr = errors.New("protection backup returned no backup IDs")
			}
			failed, failErr := c.fail(publication, "PROTECTION_BACKUP_FAILED", backupErr)
			return failed, nil, failErr
		}
		publication.ProtectionBackupIDs = normalizedStrings(backup.IDs)
		publication.UpdatedAt = c.now().UTC()
		publication, err = c.store.Save(publication)
		if err != nil {
			return publication, nil, err
		}
	}
	for index, target := range publication.Plan.Targets {
		fences, err = c.renewFences(ctx, fences)
		if err != nil {
			failed, failErr := c.rollbackAfterFailure(ctx, publication, fences, "LEASE_RENEW_FAILED", err)
			return failed, nil, failErr
		}
		publication.Fences = fences
		operation := c.operation(publication, target, fences, "ensure-cache")
		operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModCache, Message: "正在同步模组缓存到运行机器"})
		if err := c.runtime.EnsureCache(ctx, target, operation); err != nil {
			if c.replicas != nil {
				_ = c.replicas.MarkTargetError(target, publication.Plan.PlanHash, ReplicaStageCache, err)
			}
			publication.Targets[index].Status = StatusFailed
			publication.Targets[index].ErrorCode, publication.Targets[index].ErrorMessage = "CACHE_ENSURE_FAILED", err.Error()
			_, _ = c.store.SaveTarget(publication.ID, publication.Targets[index])
			failed, failErr := c.rollbackAfterFailure(ctx, publication, fences, "CACHE_ENSURE_FAILED", err)
			return failed, nil, failErr
		}
		if c.replicas != nil {
			if err := c.replicas.MarkCache(target, publication.Plan.PlanHash); err != nil {
				failed, failErr := c.rollbackAfterFailure(ctx, publication, fences, "REPLICA_CACHE_STATE_FAILED", err)
				return failed, nil, failErr
			}
		}
		publication.Targets[index].CacheEnsured = true
		publication.Targets[index].Status = StatusPreparing
		publication.Targets[index].UpdatedAt = c.now().UTC()
		if savedTarget, saveErr := c.store.SaveTarget(publication.ID, publication.Targets[index]); saveErr != nil {
			err = saveErr
			failed, failErr := c.rollbackAfterFailure(ctx, publication, fences, "TARGET_STATE_FAILED", err)
			return failed, nil, failErr
		} else {
			publication.Targets[index] = savedTarget
		}
		operation = c.operation(publication, target, fences, "prepare")
		operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModPrepare, Percent: 100, Message: "正在准备模组与世界配置"})
		if err := c.runtime.Prepare(ctx, target, operation); err != nil {
			if c.replicas != nil {
				_ = c.replicas.MarkTargetError(target, publication.Plan.PlanHash, ReplicaStagePublish, err)
			}
			publication.Targets[index].Status = StatusFailed
			publication.Targets[index].ErrorCode, publication.Targets[index].ErrorMessage = "TARGET_PREPARE_FAILED", err.Error()
			_, _ = c.store.SaveTarget(publication.ID, publication.Targets[index])
			failed, failErr := c.rollbackAfterFailure(ctx, publication, fences, "TARGET_PREPARE_FAILED", err)
			return failed, nil, failErr
		}
		preparedAt := c.now().UTC()
		publication.Targets[index].Prepared, publication.Targets[index].PreparedAt = true, &preparedAt
		publication.Targets[index].Status = StatusPrepared
		publication.Targets[index].UpdatedAt = preparedAt
		if savedTarget, saveErr := c.store.SaveTarget(publication.ID, publication.Targets[index]); saveErr != nil {
			err = saveErr
			failed, failErr := c.rollbackAfterFailure(ctx, publication, fences, "TARGET_STATE_FAILED", err)
			return failed, nil, failErr
		} else {
			publication.Targets[index] = savedTarget
		}
	}
	publication.Status = StatusPrepared
	publication.Fences = fences
	publication.UpdatedAt = c.now().UTC()
	publication, err = c.store.Save(publication)
	if err != nil {
		failed, failErr := c.rollbackAfterFailure(ctx, publication, fences, "PUBLICATION_STATE_FAILED", err)
		return failed, nil, failErr
	}
	releaseFences = false
	transaction := &publicationTransaction{
		coordinator: c, publication: publication, fences: append([]Fence(nil), fences...),
		ownedFences: append([]Fence(nil), ownedFences...),
	}
	return publication, transaction, nil
}

func (c *Coordinator) completeAndActivate(ctx context.Context, publication Publication, fences []Fence) (Publication, error) {
	completed, err := c.completeCommitted(ctx, publication, fences)
	if err != nil || completed.Activation.Policy.Mode != ActivationModeRestart {
		return completed, err
	}
	return c.activateCommitted(ctx, completed, fences, completed.Activation.Policy, completed.SourceJobID)
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
			if c.replicas != nil {
				_ = c.replicas.MarkTargetError(target, publication.Plan.PlanHash, ReplicaStageComplete, err)
			}
			publication.Targets[index].Status = StatusRecoveryRequired
			publication.Targets[index].ErrorCode, publication.Targets[index].ErrorMessage = "TARGET_COMPLETE_FAILED", err.Error()
			completionErr = errors.Join(completionErr, err)
		} else {
			if c.replicas != nil {
				if replicaErr := c.replicas.MarkCompleted(target, publication.Plan.PlanHash); replicaErr != nil {
					completionErr = errors.Join(completionErr, replicaErr)
					publication.Targets[index].Status = StatusRecoveryRequired
					publication.Targets[index].ErrorCode, publication.Targets[index].ErrorMessage = "REPLICA_COMPLETE_STATE_FAILED", replicaErr.Error()
					publication.Targets[index].UpdatedAt = c.now().UTC()
					if _, saveErr := c.store.SaveTarget(publication.ID, publication.Targets[index]); saveErr != nil {
						completionErr = errors.Join(completionErr, saveErr)
					}
					continue
				}
			}
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
			if c.replicas != nil {
				_ = c.replicas.MarkTargetError(target, publication.Plan.PlanHash, ReplicaStageRollback, err)
			}
			result.Status = StatusRecoveryRequired
			result.ErrorCode, result.ErrorMessage = "TARGET_ROLLBACK_FAILED", err.Error()
			rollbackErr = errors.Join(rollbackErr, err)
		} else {
			if c.replicas != nil {
				if replicaErr := c.replicas.MarkRolledBack(target, publication.Plan.PlanHash, cause); replicaErr != nil {
					rollbackErr = errors.Join(rollbackErr, replicaErr)
				}
			}
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
	attempt := ""
	if len(fences) > 0 {
		attempt = fences[0].OperationKey
	}
	return RuntimeOperation{
		PublicationID: publication.ID, TopologyRevision: publication.Plan.TopologyRevision,
		PlanHash: publication.Plan.PlanHash, Fences: append([]Fence(nil), fences...), Action: action,
		IdempotencyKey: fmt.Sprintf("%s:%s:%s:%s:%s", publication.ID, target.TargetID, target.InstallationID, action, attempt),
		RenewFences:    c.renewFences,
	}
}

func (c *Coordinator) acquireFences(ctx context.Context, plan Plan, publicationID string) ([]Fence, error) {
	return c.acquireFencesFor(ctx, plan, publicationOperationKey(publicationID))
}

func (c *Coordinator) acquireFencesWithBorrowed(ctx context.Context, plan Plan, publicationID string, borrowed []Fence) ([]Fence, []Fence, error) {
	if len(borrowed) == 0 {
		fences, err := c.acquireFences(ctx, plan, publicationID)
		return fences, fences, err
	}
	resources := publicationLeaseResources(plan)
	borrowedByResource := make(map[string]Fence, len(borrowed))
	for _, fence := range borrowed {
		if fence.RoomID == "" || !validID(fence.LeaseID) || fence.OperationKey == "" || fence.FencingToken == 0 {
			return nil, nil, ErrInvalidInput
		}
		if _, duplicate := borrowedByResource[fence.RoomID]; duplicate {
			return nil, nil, ErrInvalidInput
		}
		borrowedByResource[fence.RoomID] = fence
	}
	operationKey := publicationOperationKey(publicationID)
	all := make([]Fence, 0, len(resources))
	owned := make([]Fence, 0, len(resources))
	usedBorrowed := 0
	for _, resourceID := range resources {
		if fence, exists := borrowedByResource[resourceID]; exists {
			renewed, err := c.leases.Renew(ctx, fence, c.leaseTTL)
			if err != nil || renewed.RoomID != fence.RoomID || renewed.LeaseID != fence.LeaseID || renewed.FencingToken != fence.FencingToken {
				c.releaseFences(owned)
				if err == nil {
					err = ErrConflict
				}
				return nil, nil, err
			}
			all = append(all, renewed)
			usedBorrowed++
			continue
		}
		fence, err := c.leases.Acquire(ctx, resourceID, operationKey, c.leaseTTL)
		if err != nil {
			c.releaseFences(owned)
			return nil, nil, err
		}
		if fence.RoomID != resourceID || fence.OperationKey != operationKey || !validID(fence.LeaseID) || fence.FencingToken == 0 {
			c.releaseFences(append(owned, fence))
			return nil, nil, ErrInvalidInput
		}
		all = append(all, fence)
		owned = append(owned, fence)
	}
	if usedBorrowed != len(borrowed) {
		c.releaseFences(owned)
		return nil, nil, ErrInvalidInput
	}
	return all, owned, nil
}

func (c *Coordinator) acquireFencesFor(ctx context.Context, plan Plan, operationKey string) ([]Fence, error) {
	return c.acquireFencesForResources(ctx, publicationLeaseResources(plan), operationKey)
}

func (c *Coordinator) acquireFencesForResources(ctx context.Context, resources []string, operationKey string) ([]Fence, error) {
	if len(resources) == 0 {
		return nil, ErrInvalidInput
	}
	fences := make([]Fence, 0, len(resources))
	for _, resourceID := range resources {
		fence, err := c.leases.Acquire(ctx, resourceID, operationKey, c.leaseTTL)
		if err != nil {
			c.releaseFences(fences)
			return nil, err
		}
		if fence.RoomID != resourceID || fence.OperationKey != operationKey || !validID(fence.LeaseID) || fence.FencingToken == 0 {
			c.releaseFences(append(fences, fence))
			return nil, ErrInvalidInput
		}
		fences = append(fences, fence)
	}
	return fences, nil
}

func (c *Coordinator) activationOperation(publication Publication, world WorldPlan, fences []Fence, action string) RuntimeOperation {
	attempt := ""
	for _, fence := range fences {
		if fence.RoomID == world.RoomID {
			attempt = fence.OperationKey
			break
		}
	}
	return RuntimeOperation{
		PublicationID: publication.ID, TopologyRevision: publication.Plan.TopologyRevision,
		PlanHash: publication.Plan.PlanHash, Fences: append([]Fence(nil), fences...), Action: "activate-" + action,
		IdempotencyKey: "mod-activation:" + hashBytes([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", publication.ID, world.RoomID, world.WorldID, action, attempt))),
		RenewFences:    c.renewFences,
	}
}

func publicationOperationKey(publicationID string) string {
	value := "mod.publish:" + publicationID
	if len(value) <= 128 {
		return value
	}
	return "mod.publish:" + hashBytes([]byte(publicationID))
}

func publicationLeaseResources(plan Plan) []string {
	resources := normalizedStrings(plan.AffectedRoomIDs)
	resources = append(resources, installationLeaseResources(plan)...)
	return normalizedStrings(resources)
}

func installationLeaseResources(plan Plan) []string {
	resources := make([]string, 0, len(plan.Targets))
	for _, target := range plan.Targets {
		digest := hashBytes([]byte(target.TargetID + "\x00" + target.InstallationID))
		resources = append(resources, "@mod-installation/"+digest)
	}
	return normalizedStrings(resources)
}

func (c *Coordinator) renewFences(ctx context.Context, fences []Fence) ([]Fence, error) {
	updated := append([]Fence(nil), fences...)
	for index := range updated {
		fence, err := c.leases.Renew(ctx, updated[index], c.leaseTTL)
		if err != nil {
			return updated, err
		}
		if fence.RoomID != updated[index].RoomID || fence.LeaseID != updated[index].LeaseID ||
			fence.OperationKey != updated[index].OperationKey || fence.FencingToken != updated[index].FencingToken {
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
