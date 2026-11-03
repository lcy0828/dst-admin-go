package gameupdate

import (
	"context"
	"errors"
	"time"
)

// A backup failure is actionable before any game files have been written.
// Keep this separate from persistence, fencing and cancellation failures.
type releaseBackupError struct{ cause error }

func (e *releaseBackupError) Error() string { return e.cause.Error() }
func (e *releaseBackupError) Unwrap() error { return e.cause }

func (c *ReleaseCoordinator) backupFailed(ctx context.Context, value Release, fences *releaseFenceSet, cause error) (Release, error) {
	if !value.Plan.Policy.PauseOnBackupFailure || ctx.Err() != nil {
		return c.abortBeforeWrite(ctx, value, fences, "PROTECTION_BACKUP_FAILED", cause)
	}
	value.Stage, value.ErrorCode, value.ErrorMessage = ReleaseStageRecoveryRequired, "PROTECTION_BACKUP_FAILED", cause.Error()
	value.NextAction, value.FinishedAt, value.UpdatedAt = "backup_decision", nil, c.now().UTC()
	saved, err := c.store.Save(value)
	if err != nil {
		return c.abortBeforeWrite(ctx, value, fences, "PERSISTENCE_FAILED", errors.Join(cause, err))
	}
	return saved, errors.Join(ErrReleaseRecoveryNeeded, cause)
}

func (c *ReleaseCoordinator) waitBeforeRelease(ctx context.Context, value Release) (Release, error) {
	if !value.Plan.UpdateRequired {
		return value, nil
	}
	if value.Plan.Policy.StopDelaySeconds == nil {
		jobID := value.CurrentJobID
		if jobID == "" {
			jobID = value.SourceJobID
		}
		return value, c.notifyBeforeRelease(ctx, value.Plan, jobID)
	}
	seconds := *value.Plan.Policy.StopDelaySeconds
	roomIDs := make([]string, 0, len(value.Plan.AffectedRoomIDs))
	seen := make(map[string]bool)
	for _, shard := range orderedReleaseShards(value.Plan, true) {
		if shard.WasRunning && !seen[shard.RoomID] {
			status, err := c.runtime.Status(ctx, shard)
			if err != nil {
				return value, err
			}
			if status.State != "running" && status.State != "starting" {
				continue
			}
			roomIDs = append(roomIDs, shard.RoomID)
			seen[shard.RoomID] = true
		}
	}
	if len(roomIDs) == 0 {
		return value, nil
	}
	value.Stage, value.CountdownEndsAt, value.UpdatedAt = ReleaseStageNotifying, nil, c.now().UTC()
	saved, err := c.store.Save(value)
	if err != nil {
		return value, err
	}
	value = saved
	if notifier, ok := c.notifier.(interface {
		AnnounceGameUpdate(context.Context, []string, int, string) error
	}); ok {
		jobID := value.CurrentJobID
		if jobID == "" {
			jobID = value.SourceJobID
		}
		if err := notifier.AnnounceGameUpdate(ctx, roomIDs, seconds, jobID); err != nil {
			return value, err
		}
	}
	// Announcement delivery can be slow. Players still get the full chosen
	// warning window after delivery, rather than losing it to transport latency.
	end := c.now().UTC().Add(time.Duration(seconds) * time.Second)
	value.CountdownEndsAt, value.UpdatedAt = &end, c.now().UTC()
	saved, err = c.store.Save(value)
	if err != nil {
		return value, err
	}
	value = saved
	if seconds > 0 {
		wait := c.wait
		if wait == nil {
			wait = waitReleaseContext
		}
		remaining := end.Sub(c.now().UTC())
		if remaining < 0 {
			remaining = 0
		}
		if err := wait(ctx, remaining); err != nil {
			return value, err
		}
	}
	return value, ctx.Err()
}

func waitReleaseContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
