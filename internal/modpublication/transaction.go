package modpublication

import (
	"context"
	"errors"
	"sync"

	"dont/internal/operationprogress"
)

type PreparedTransaction interface {
	Publication() Publication
	Renew(context.Context) error
	Publish(context.Context) (Publication, error)
	Commit(context.Context) (Publication, error)
	Rollback(context.Context, string, error) (Publication, error)
	Close()
}

type publicationTransaction struct {
	mu          sync.Mutex
	coordinator *Coordinator
	publication Publication
	fences      []Fence
	ownedFences []Fence
	closed      bool
}

func (t *publicationTransaction) Publication() Publication {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.publication
}

func (t *publicationTransaction) Renew(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.publication.CommitDecision {
		return ErrConflict
	}
	fences, err := t.coordinator.renewFences(ctx, t.fences)
	if err != nil {
		return err
	}
	t.fences = fences
	t.publication.Fences = append([]Fence(nil), fences...)
	return nil
}

func (t *publicationTransaction) Publish(ctx context.Context) (Publication, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.publication.Status != StatusPrepared || t.publication.CommitDecision {
		return t.publication, ErrConflict
	}
	publication := t.publication
	fences, err := t.coordinator.renewFences(ctx, t.fences)
	if err != nil {
		return t.rollbackLocked(ctx, "LEASE_RENEW_FAILED", err)
	}
	t.fences = fences
	publication.Fences = append([]Fence(nil), fences...)
	publication.Status = StatusPublishing
	publication.UpdatedAt = t.coordinator.now().UTC()
	publication, err = t.coordinator.store.Save(publication)
	if err != nil {
		t.publication = publication
		return t.rollbackLocked(ctx, "PUBLICATION_STATE_FAILED", err)
	}
	for index, target := range publication.Plan.Targets {
		fences, err = t.coordinator.renewFences(ctx, fences)
		if err != nil {
			t.fences, t.publication = fences, publication
			return t.rollbackLocked(ctx, "LEASE_RENEW_FAILED", err)
		}
		t.fences = fences
		operation := t.coordinator.operation(publication, target, fences, "publish")
		operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModPublish, Percent: 100, Message: "正在提交模组与世界配置"})
		if err := t.coordinator.runtime.Publish(ctx, target, operation); err != nil {
			if t.coordinator.replicas != nil {
				_ = t.coordinator.replicas.MarkTargetError(target, publication.Plan.PlanHash, ReplicaStagePublish, err)
			}
			publication.Targets[index].Status = StatusFailed
			publication.Targets[index].ErrorCode, publication.Targets[index].ErrorMessage = "TARGET_PUBLISH_FAILED", err.Error()
			_, _ = t.coordinator.store.SaveTarget(publication.ID, publication.Targets[index])
			t.publication = publication
			return t.rollbackLocked(ctx, "TARGET_PUBLISH_FAILED", err)
		}
		if t.coordinator.replicas != nil {
			if err := t.coordinator.replicas.MarkPublished(target, publication.Plan.PlanHash); err != nil {
				t.publication = publication
				return t.rollbackLocked(ctx, "REPLICA_PUBLISH_STATE_FAILED", err)
			}
		}
		publishedAt := t.coordinator.now().UTC()
		publication.Targets[index].Published, publication.Targets[index].PublishedAt = true, &publishedAt
		publication.Targets[index].Status, publication.Targets[index].UpdatedAt = StatusPublishing, publishedAt
		if savedTarget, saveErr := t.coordinator.store.SaveTarget(publication.ID, publication.Targets[index]); saveErr != nil {
			t.publication = publication
			return t.rollbackLocked(ctx, "TARGET_STATE_FAILED", saveErr)
		} else {
			publication.Targets[index] = savedTarget
		}
	}
	publication.Fences = append([]Fence(nil), fences...)
	publication.UpdatedAt = t.coordinator.now().UTC()
	publication, err = t.coordinator.store.Save(publication)
	t.publication = publication
	if err != nil {
		return t.rollbackLocked(ctx, "PUBLICATION_STATE_FAILED", err)
	}
	return publication, nil
}

func (t *publicationTransaction) Commit(ctx context.Context) (Publication, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.publication.Status != StatusPublishing || t.publication.CommitDecision {
		return t.publication, ErrConflict
	}
	fences, err := t.coordinator.renewFences(ctx, t.fences)
	if err != nil {
		return t.rollbackLocked(ctx, "LEASE_RENEW_FAILED", err)
	}
	t.fences = fences
	t.publication.Fences = append([]Fence(nil), fences...)
	publication, err := t.coordinator.store.DecideCommit(t.publication.ID, t.coordinator.now().UTC())
	if err != nil {
		if current, readErr := t.coordinator.store.Get(t.publication.ID); readErr == nil && current.CommitDecision {
			t.publication = current
			publication, err = t.coordinator.completeAndActivate(ctx, current, fences)
			t.publication = publication
			t.closeLocked()
			return publication, err
		}
		return t.rollbackLocked(ctx, "COMMIT_DECISION_FAILED", err)
	}
	t.publication = publication
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModComplete, Percent: 100, Message: "正在确认模组发布结果"})
	completed, completeErr := t.coordinator.completeAndActivate(ctx, publication, fences)
	t.publication = completed
	t.closeLocked()
	if completeErr == nil {
		operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModDone, Percent: 100, Message: "模组发布完成"})
	}
	return completed, completeErr
}

func (t *publicationTransaction) Rollback(ctx context.Context, code string, cause error) (Publication, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cause == nil {
		cause = errors.New("publication transaction was cancelled")
	}
	return t.rollbackLocked(ctx, code, cause)
}

func (t *publicationTransaction) rollbackLocked(ctx context.Context, code string, cause error) (Publication, error) {
	if t.closed {
		return t.publication, nil
	}
	if t.publication.CommitDecision {
		return t.publication, ErrConflict
	}
	publication, err := t.coordinator.rollbackAfterFailure(ctx, t.publication, t.fences, code, cause)
	t.publication = publication
	t.closeLocked()
	return publication, err
}

func (t *publicationTransaction) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeLocked()
}

func (t *publicationTransaction) closeLocked() {
	if t.closed {
		return
	}
	t.coordinator.releaseFences(t.ownedFences)
	t.ownedFences = nil
	t.closed = true
}

var _ PreparedTransaction = (*publicationTransaction)(nil)
