package gameupdate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/operationlease"
	"dont/internal/roomops"
	"dont/internal/runtimeaudit"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
)

const releaseLeaseTTL = 5 * time.Minute

type ReleaseCoordinator struct {
	planner       *ReleasePlanner
	runtime       ReleaseRuntime
	leases        ReleaseLeaseService
	backups       ReleaseProtectionService
	store         *ReleaseStore
	now           func() time.Time
	pollInterval  time.Duration
	renewInterval time.Duration
	notifier      LifecycleNotifier
}

func NewReleaseCoordinator(planner *ReleasePlanner, runtime ReleaseRuntime, leases ReleaseLeaseService, backups ReleaseProtectionService, store *ReleaseStore) (*ReleaseCoordinator, error) {
	if planner == nil || runtime == nil || leases == nil || backups == nil || store == nil {
		return nil, ErrReleaseInvalid
	}
	return &ReleaseCoordinator{
		planner: planner, runtime: runtime, leases: leases, backups: backups, store: store,
		now: time.Now, pollInterval: time.Second, renewInterval: time.Minute,
	}, nil
}

func (c *ReleaseCoordinator) ConfigureNotifier(notifier LifecycleNotifier) {
	c.notifier = notifier
}

func (c *ReleaseCoordinator) Preview(ctx context.Context, request ReleasePreviewRequest) (ReleasePlan, error) {
	return c.planner.Preview(ctx, request)
}

func (c *ReleaseCoordinator) Get(id string) (Release, error) { return c.store.Get(id) }

func (c *ReleaseCoordinator) List(limit, offset int) ([]Release, int, error) {
	return c.store.List(limit, offset)
}

func (c *ReleaseCoordinator) Publish(ctx context.Context, request ReleasePublishRequest) (Release, error) {
	if !releaseIdentityPattern.MatchString(request.ID) || request.SourceJobID != "" && !releaseIdentityPattern.MatchString(request.SourceJobID) || validateReleasePlan(request.Plan) != nil {
		return Release{}, ErrReleaseInvalid
	}
	if existing, err := c.store.FindIdempotent(request.ID, request.SourceJobID); err == nil {
		if existing.Plan.PlanHash != request.Plan.PlanHash {
			return existing, ErrReleaseConflict
		}
		return existing, nil
	} else if !errors.Is(err, ErrReleaseNotFound) {
		return Release{}, err
	}
	return c.withReleaseLocks(ctx, request.Plan, request.ID, func(runContext context.Context, fences *releaseFenceSet) (Release, error) {
		fresh, err := c.previewStoredPlan(runContext, request.Plan)
		if err != nil {
			return Release{}, err
		}
		if fresh.PlanHash != request.Plan.PlanHash {
			return Release{}, ErrReleasePlanChanged
		}
		if err := c.notifyBeforeRelease(runContext, request.Plan, request.SourceJobID); err != nil {
			return Release{}, err
		}
		now := c.now().UTC()
		value := Release{
			ID: request.ID, SourceJobID: request.SourceJobID, Stage: ReleaseStagePreviewed, Plan: request.Plan,
			ProtectionBackupIDs: []string{}, CreatedAt: now, UpdatedAt: now,
		}
		for _, installation := range request.Plan.Installations {
			value.Installations = append(value.Installations, ReleaseInstallationResult{
				TargetID: installation.TargetID, InstallationID: installation.InstallationID,
				Stage: ReleaseStagePreviewed, BeforeVersion: installation.CurrentVersion, UpdatedAt: now,
			})
			for _, shard := range installation.Shards {
				value.Shards = append(value.Shards, ReleaseShardResult{
					RoomID: shard.RoomID, WorldID: shard.WorldID, TargetID: shard.TargetID, InstallationID: shard.InstallationID,
					IsMaster: shard.IsMaster, WasRunning: shard.WasRunning, Stage: ReleaseStagePreviewed,
					RuntimeState: shard.RuntimeState, UpdatedAt: now,
				})
			}
		}
		value, err = c.store.Create(value)
		if err != nil {
			if existing, findErr := c.store.FindIdempotent(request.ID, request.SourceJobID); findErr == nil && existing.Plan.PlanHash == request.Plan.PlanHash {
				return existing, nil
			}
			return Release{}, err
		}
		return c.execute(runContext, value, fences, false)
	})
}

func (c *ReleaseCoordinator) Retry(ctx context.Context, id string, sourceJobIDs ...string) (Release, error) {
	value, err := c.store.Get(strings.TrimSpace(id))
	if err != nil {
		return Release{}, err
	}
	if value.Stage != ReleaseStageFailed && value.Stage != ReleaseStageRecoveryRequired {
		return value, ErrReleaseConflict
	}
	notificationJobID := value.SourceJobID
	if len(sourceJobIDs) > 0 && strings.TrimSpace(sourceJobIDs[0]) != "" {
		notificationJobID = strings.TrimSpace(sourceJobIDs[0])
	}
	return c.withReleaseLocks(ctx, value.Plan, value.ID+":retry", func(runContext context.Context, fences *releaseFenceSet) (Release, error) {
		value, err = c.store.Get(strings.TrimSpace(id))
		if err != nil {
			return Release{}, err
		}
		if value.Stage != ReleaseStageFailed && value.Stage != ReleaseStageRecoveryRequired {
			return value, ErrReleaseConflict
		}
		fresh, previewErr := c.previewStoredPlan(runContext, value.Plan)
		if previewErr != nil {
			return c.failRelease(value, "RECOVERY_PREFLIGHT_FAILED", previewErr, ReleaseStageRecoveryRequired)
		}
		if !fresh.Ready || !sameReleaseStructure(value.Plan, fresh) {
			return c.failRelease(value, "TOPOLOGY_CHANGED", ErrReleaseTopologyChanged, ReleaseStageRecoveryRequired)
		}
		if err := c.notifyBeforeRelease(runContext, value.Plan, notificationJobID); err != nil {
			return value, err
		}
		value.ErrorCode, value.ErrorMessage, value.FinishedAt, value.UpdatedAt = "", "", nil, c.now().UTC()
		if saved, saveErr := c.store.Save(value); saveErr == nil {
			value = saved
		} else {
			return value, saveErr
		}
		return c.execute(runContext, value, fences, true)
	})
}

func (c *ReleaseCoordinator) notifyBeforeRelease(ctx context.Context, plan ReleasePlan, jobID string) error {
	if c.notifier == nil || !plan.UpdateRequired {
		return nil
	}
	roomIDs := make([]string, 0, len(plan.AffectedRoomIDs))
	seen := make(map[string]bool, len(plan.AffectedRoomIDs))
	for _, installation := range plan.Installations {
		for _, shard := range installation.Shards {
			if shard.WasRunning && !seen[shard.RoomID] {
				seen[shard.RoomID] = true
				roomIDs = append(roomIDs, shard.RoomID)
			}
		}
	}
	if len(roomIDs) == 0 {
		return nil
	}
	action := string(shards.ActionStop)
	if plan.Policy.RestartRunning {
		action = string(shards.ActionRestart)
	}
	return c.notifier.BeforeOperations(ctx, roomIDs, action, string(runtimeaudit.SourceGameUpdate), jobID)
}

func (c *ReleaseCoordinator) execute(ctx context.Context, value Release, fences *releaseFenceSet, recovery bool) (Release, error) {
	if !value.Plan.UpdateRequired {
		now := c.now().UTC()
		value.Stage, value.FinishedAt, value.UpdatedAt = ReleaseStageSucceeded, &now, now
		for index := range value.Installations {
			value.Installations[index].Stage = ReleaseStageSucceeded
			value.Installations[index].AfterVersion = releaseTargetDesiredVersion(value.Plan.Installations[index], value.Plan.DesiredVersion)
			value.Installations[index].FinishedAt, value.Installations[index].UpdatedAt = &now, now
		}
		for index := range value.Shards {
			value.Shards[index].Stage, value.Shards[index].UpdatedAt = ReleaseStageSucceeded, now
		}
		return c.store.Save(value)
	}
	var err error
	value, err = c.createProtectionBackups(ctx, value, fences)
	if err != nil {
		return c.failRelease(value, "PROTECTION_BACKUP_FAILED", err, ReleaseStageRecoveryRequired)
	}
	fresh, err := c.previewStoredPlan(ctx, value.Plan)
	planChanged := fresh.PlanHash != value.Plan.PlanHash
	if recovery {
		planChanged = !fresh.Ready || !sameReleaseStructure(value.Plan, fresh)
	}
	if err != nil || planChanged {
		if err == nil {
			err = ErrReleasePlanChanged
		}
		stage := ReleaseStageFailed
		if recovery {
			stage = ReleaseStageRecoveryRequired
		}
		return c.failRelease(value, "PLAN_CHANGED", err, stage)
	}
	value, stopped, err := c.stopPlannedShards(ctx, value, fences)
	if err != nil {
		if recoveryErr := c.recoverStoppedShards(context.Background(), &value, stopped, fences); recoveryErr != nil {
			return c.failRelease(value, "STOP_RECOVERY_FAILED", errors.Join(err, recoveryErr), ReleaseStageRecoveryRequired)
		}
		return c.failRelease(value, "STOP_FAILED", err, ReleaseStageFailed)
	}
	value.Stage, value.UpdatedAt = ReleaseStageStaged, c.now().UTC()
	value, err = c.store.Save(value)
	if err != nil {
		return value, err
	}
	value, err = c.updateInstallations(ctx, value, fences)
	if err != nil {
		return c.failRelease(value, "UPDATE_INCOMPLETE", err, ReleaseStageRecoveryRequired)
	}
	if !value.Plan.Policy.RestartRunning {
		return c.completeWithoutRestart(value)
	}
	value, err = c.restartAndConfirm(ctx, value, fences)
	if err != nil {
		return c.failRelease(value, "RESTART_INCOMPLETE", err, ReleaseStageRecoveryRequired)
	}
	now := c.now().UTC()
	value.Stage, value.ErrorCode, value.ErrorMessage, value.FinishedAt, value.UpdatedAt = ReleaseStageSucceeded, "", "", &now, now
	return c.store.Save(value)
}

func (c *ReleaseCoordinator) createProtectionBackups(ctx context.Context, value Release, fences *releaseFenceSet) (Release, error) {
	value.Stage, value.UpdatedAt = ReleaseStageProtecting, c.now().UTC()
	value, err := c.store.Save(value)
	if err != nil {
		return value, err
	}
	for index, roomID := range value.Plan.AffectedRoomIDs {
		if index < len(value.ProtectionBackupIDs) && value.ProtectionBackupIDs[index] != "" {
			continue
		}
		lease, found := fences.forRoom(roomID)
		if !found {
			return value, ErrReleaseInvalid
		}
		backupID, backupErr := c.backups.CreateProtection(ctx, roomID, "DST 更新前保护备份", value.ID, &lease)
		if backupErr != nil {
			return value, backupErr
		}
		fences.replace(lease)
		value.ProtectionBackupIDs = append(value.ProtectionBackupIDs, backupID)
		value.UpdatedAt = c.now().UTC()
		value, err = c.store.Save(value)
		if err != nil {
			return value, err
		}
	}
	return value, nil
}

func (c *ReleaseCoordinator) stopPlannedShards(ctx context.Context, value Release, fences *releaseFenceSet) (Release, []ReleaseShardPlan, error) {
	value.Stage, value.UpdatedAt = ReleaseStageStopping, c.now().UTC()
	value, err := c.store.Save(value)
	if err != nil {
		return value, nil, err
	}
	stopped := make([]ReleaseShardPlan, 0)
	for index, shard := range orderedReleaseShards(value.Plan, false) {
		result := releaseShardResult(&value, shard.RoomID, shard.WorldID)
		if result == nil {
			return value, stopped, ErrReleaseInvalid
		}
		if result == nil || !shard.WasRunning {
			if result != nil {
				result.Stage, result.UpdatedAt = ReleaseStageStaged, c.now().UTC()
			}
			continue
		}
		status, statusErr := c.runtime.Status(ctx, shard)
		if statusErr != nil {
			return value, stopped, statusErr
		}
		if status.State != string(shards.RuntimeStopped) || status.SessionExists {
			operation, operationErr := fences.operation(shard.RoomID, value.ID, "stop", index)
			if operationErr != nil {
				return value, stopped, operationErr
			}
			if stopErr := c.runtime.Stop(ctx, shard, operation); stopErr != nil {
				setReleaseShardFailure(result, "STOP_FAILED", stopErr, c.now().UTC())
				_, _ = c.store.Save(value)
				return value, stopped, stopErr
			}
			if waitErr := c.waitShardState(ctx, shard, false, 2*time.Minute); waitErr != nil {
				setReleaseShardFailure(result, "STOP_TIMEOUT", waitErr, c.now().UTC())
				_, _ = c.store.Save(value)
				return value, stopped, waitErr
			}
		}
		now := c.now().UTC()
		result.Stage, result.RuntimeState, result.StoppedAt, result.UpdatedAt = ReleaseStageStaged, string(shards.RuntimeStopped), &now, now
		stopped = append(stopped, shard)
		value, err = c.store.Save(value)
		if err != nil {
			return value, stopped, err
		}
	}
	return value, stopped, nil
}

func (c *ReleaseCoordinator) updateInstallations(ctx context.Context, value Release, fences *releaseFenceSet) (Release, error) {
	value.Stage, value.UpdatedAt = ReleaseStageUpdating, c.now().UTC()
	value, err := c.store.Save(value)
	if err != nil {
		return value, err
	}
	var failures error
	for index, target := range value.Plan.Installations {
		desiredVersion := releaseTargetDesiredVersion(target, value.Plan.DesiredVersion)
		result := releaseInstallationResult(&value, target.TargetID, target.InstallationID)
		if result == nil {
			failures = errors.Join(failures, ErrReleaseInvalid)
			continue
		}
		observation, observeErr := c.runtime.ObserveInstallation(ctx, target)
		if observeErr == nil && observation.Installed && observation.CurrentVersion == desiredVersion {
			now := c.now().UTC()
			result.Stage, result.AfterVersion, result.ErrorCode, result.ErrorMessage = ReleaseStageVerified, observation.CurrentVersion, "", ""
			result.FinishedAt, result.UpdatedAt = &now, now
			value, err = c.store.Save(value)
			failures = errors.Join(failures, err)
			continue
		}
		result.Stage, result.StartedAt, result.UpdatedAt = ReleaseStageUpdating, timePointer(c.now().UTC()), c.now().UTC()
		value, err = c.store.Save(value)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		result = releaseInstallationResult(&value, target.TargetID, target.InstallationID)
		if result == nil {
			failures = errors.Join(failures, ErrReleaseInvalid)
			continue
		}
		operation, operationErr := fences.operation(target.Shards[0].RoomID, value.ID, "update", index)
		if operationErr != nil {
			setReleaseInstallationFailure(result, "LEASE_MISSING", operationErr, c.now().UTC())
			failures = errors.Join(failures, operationErr)
			continue
		}
		updated, updateErr := c.runtime.UpdateInstallation(ctx, target, operation, desiredVersion, value.Plan.Policy.CleanCache)
		result.AfterVersion, result.Log = updated.CurrentVersion, updated.Log
		if updateErr == nil && updated.CurrentVersion != desiredVersion {
			updateErr = errors.New("运行目标返回的安装版本与目标版本不一致")
		}
		if updateErr != nil {
			setReleaseInstallationFailure(result, "UPDATE_FAILED", updateErr, c.now().UTC())
			failures = errors.Join(failures, updateErr)
		} else {
			now := c.now().UTC()
			result.Stage, result.ErrorCode, result.ErrorMessage, result.FinishedAt, result.UpdatedAt = ReleaseStageVerified, "", "", &now, now
		}
		value, err = c.store.Save(value)
		failures = errors.Join(failures, err)
	}
	if failures != nil {
		return value, failures
	}
	value.Stage, value.UpdatedAt = ReleaseStageVerified, c.now().UTC()
	return c.store.Save(value)
}

func (c *ReleaseCoordinator) completeWithoutRestart(value Release) (Release, error) {
	now := c.now().UTC()
	for index := range value.Installations {
		value.Installations[index].Stage, value.Installations[index].ErrorCode, value.Installations[index].ErrorMessage = ReleaseStageSucceeded, "", ""
		value.Installations[index].FinishedAt, value.Installations[index].UpdatedAt = &now, now
	}
	for index := range value.Shards {
		value.Shards[index].Stage, value.Shards[index].RuntimeState = ReleaseStageSucceeded, string(shards.RuntimeStopped)
		value.Shards[index].ErrorCode, value.Shards[index].ErrorMessage, value.Shards[index].UpdatedAt = "", "", now
	}
	value.Stage, value.FinishedAt, value.UpdatedAt = ReleaseStageSucceeded, &now, now
	return c.store.Save(value)
}

func (c *ReleaseCoordinator) restartAndConfirm(ctx context.Context, value Release, fences *releaseFenceSet) (Release, error) {
	value.Stage, value.UpdatedAt = ReleaseStageRestarting, c.now().UTC()
	value, err := c.store.Save(value)
	if err != nil {
		return value, err
	}
	type cursor struct {
		fileID string
		offset int64
	}
	cursors := make(map[string]cursor)
	var failures error
	for index, shard := range orderedReleaseShards(value.Plan, true) {
		result := releaseShardResult(&value, shard.RoomID, shard.WorldID)
		if result == nil || !shard.WasRunning {
			if result != nil {
				result.Stage, result.RuntimeState, result.UpdatedAt = ReleaseStageSucceeded, string(shards.RuntimeStopped), c.now().UTC()
			}
			continue
		}
		if value.Plan.Policy.LoadConfirmation == ReleaseLoadConfirmationLogs {
			fileID, offset, cursorErr := c.runtime.CaptureLogCursor(ctx, shard)
			if cursorErr != nil {
				setReleaseShardFailure(result, "LOG_CURSOR_FAILED", cursorErr, c.now().UTC())
				failures = errors.Join(failures, cursorErr)
				continue
			}
			cursors[releaseShardKey(shard)] = cursor{fileID: fileID, offset: offset}
		}
		operation, operationErr := fences.operation(shard.RoomID, value.ID, "start", index)
		if operationErr == nil {
			operationErr = c.runtime.Start(ctx, shard, operation)
		}
		if operationErr != nil {
			setReleaseShardFailure(result, "START_FAILED", operationErr, c.now().UTC())
			failures = errors.Join(failures, operationErr)
			continue
		}
		now := c.now().UTC()
		result.Stage, result.StartedAt, result.UpdatedAt = ReleaseStageConfirming, &now, now
		value, err = c.store.Save(value)
		failures = errors.Join(failures, err)
	}
	value.Stage, value.UpdatedAt = ReleaseStageConfirming, c.now().UTC()
	if saved, saveErr := c.store.Save(value); saveErr == nil {
		value = saved
	} else {
		failures = errors.Join(failures, saveErr)
	}
	for _, shard := range orderedReleaseShards(value.Plan, true) {
		if !shard.WasRunning {
			continue
		}
		result := releaseShardResult(&value, shard.RoomID, shard.WorldID)
		if result == nil || result.Stage == ReleaseStageFailed {
			continue
		}
		current := cursors[releaseShardKey(shard)]
		marker, state, confirmErr := c.confirmShard(ctx, shard, current.fileID, current.offset, value.Plan.Policy)
		if confirmErr != nil {
			setReleaseShardFailure(result, "LOAD_CONFIRMATION_FAILED", confirmErr, c.now().UTC())
			result.RuntimeState = state
			failures = errors.Join(failures, confirmErr)
		} else {
			now := c.now().UTC()
			result.Stage, result.RuntimeState, result.LoadMarker, result.ErrorCode, result.ErrorMessage = ReleaseStageSucceeded, state, marker, "", ""
			result.LoadConfirmedAt, result.UpdatedAt = &now, now
		}
		value, err = c.store.Save(value)
		failures = errors.Join(failures, err)
	}
	if failures != nil {
		return value, failures
	}
	now := c.now().UTC()
	for index := range value.Installations {
		value.Installations[index].Stage, value.Installations[index].ErrorCode, value.Installations[index].ErrorMessage = ReleaseStageSucceeded, "", ""
		value.Installations[index].FinishedAt, value.Installations[index].UpdatedAt = &now, now
	}
	return c.store.Save(value)
}

func (c *ReleaseCoordinator) confirmShard(parent context.Context, shard ReleaseShardPlan, fileID string, cursor int64, policy ReleasePolicy) (string, string, error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(policy.TimeoutSeconds)*time.Second)
	defer cancel()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	state := ""
	for {
		status, err := c.runtime.Status(ctx, shard)
		if err != nil {
			return "", state, err
		}
		state = status.State
		if state == string(shards.RuntimeFailed) || state == string(shards.RuntimeStopped) && !status.SessionExists {
			return "", state, errors.New("DST 分片在加载确认前停止")
		}
		if state == string(shards.RuntimeRunning) {
			if policy.LoadConfirmation == ReleaseLoadConfirmationNone {
				return "runtime:running", state, nil
			}
			chunk, logErr := c.runtime.ReadLogs(ctx, shard, fileID, cursor)
			if logErr != nil {
				return "", state, logErr
			}
			fileID, cursor = chunk.FileID, chunk.Cursor
			for _, line := range chunk.Lines {
				if marker := releaseLoadMarker(shard, line.Text); marker != "" {
					return marker, state, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", state, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *ReleaseCoordinator) waitShardState(ctx context.Context, shard ReleaseShardPlan, running bool, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		status, err := c.runtime.Status(ctx, shard)
		if err != nil {
			return err
		}
		if running && status.State == string(shards.RuntimeRunning) || !running && status.State == string(shards.RuntimeStopped) && !status.SessionExists {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("等待分片状态变化超时")
		case <-ticker.C:
		}
	}
}

func (c *ReleaseCoordinator) recoverStoppedShards(ctx context.Context, value *Release, stopped []ReleaseShardPlan, fences *releaseFenceSet) error {
	var failures error
	for index, shard := range orderReleaseShardValues(stopped, true) {
		operation, err := fences.operation(shard.RoomID, value.ID, "recover", index)
		if err == nil {
			err = c.runtime.Start(ctx, shard, operation)
		}
		result := releaseShardResult(value, shard.RoomID, shard.WorldID)
		if err != nil {
			setReleaseShardFailure(result, "RECOVERY_START_FAILED", err, c.now().UTC())
			failures = errors.Join(failures, err)
		} else if result != nil {
			result.RuntimeState, result.UpdatedAt = string(shards.RuntimeRunning), c.now().UTC()
		}
	}
	_, saveErr := c.store.Save(*value)
	return errors.Join(failures, saveErr)
}

func (c *ReleaseCoordinator) previewStoredPlan(ctx context.Context, plan ReleasePlan) (ReleasePlan, error) {
	restart := plan.Policy.RestartRunning
	return c.planner.Preview(ctx, ReleasePreviewRequest{
		DesiredVersion: plan.DesiredVersion,
		Policy:         ReleasePolicyInput{CleanCache: plan.Policy.CleanCache, RestartRunning: &restart, LoadConfirmation: plan.Policy.LoadConfirmation, TimeoutSeconds: plan.Policy.TimeoutSeconds},
	})
}

func (c *ReleaseCoordinator) withReleaseLocks(ctx context.Context, plan ReleasePlan, operationKey string, run func(context.Context, *releaseFenceSet) (Release, error)) (Release, error) {
	ctx, releaseRooms, err := roomops.AcquireMany(ctx, plan.AffectedRoomIDs)
	if err != nil {
		return Release{}, err
	}
	defer releaseRooms()
	fences := &releaseFenceSet{}
	for _, roomID := range plan.AffectedRoomIDs {
		lease, acquireErr := c.leases.Acquire(ctx, roomID, "game-release:"+operationKey, releaseLeaseTTL)
		if acquireErr != nil {
			fences.releaseAll(c.leases)
			return Release{}, acquireErr
		}
		fences.values = append(fences.values, lease)
	}
	defer fences.releaseAll(c.leases)
	leaseContext := operationlease.WithBorrowedLeaseProvider(ctx, fences.forRoom)
	runContext, cancel := context.WithCancel(leaseContext)
	done := make(chan struct{})
	renewed := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(c.renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				renewed <- nil
				return
			case <-runContext.Done():
				renewed <- nil
				return
			case <-ticker.C:
				if renewErr := fences.renewAll(runContext, c.leases); renewErr != nil {
					renewed <- renewErr
					cancel()
					return
				}
			}
		}
	}()
	value, runErr := run(runContext, fences)
	close(done)
	renewErr := <-renewed
	cancel()
	if renewErr != nil && value.ID != "" {
		return c.failRelease(value, "LEASE_LOST", renewErr, ReleaseStageRecoveryRequired)
	}
	return value, errors.Join(runErr, renewErr)
}

func (c *ReleaseCoordinator) failRelease(value Release, code string, cause error, stage ReleaseStage) (Release, error) {
	now := c.now().UTC()
	value.Stage, value.ErrorCode, value.ErrorMessage, value.UpdatedAt = stage, code, cause.Error(), now
	if stage == ReleaseStageFailed {
		value.FinishedAt = &now
	}
	saved, saveErr := c.store.Save(value)
	if stage == ReleaseStageRecoveryRequired {
		return saved, errors.Join(ErrReleaseRecoveryNeeded, cause, saveErr)
	}
	return saved, errors.Join(cause, saveErr)
}

type releaseFenceSet struct {
	mu     sync.Mutex
	values []operationlease.Lease
}

func (s *releaseFenceSet) forRoom(roomID string) (operationlease.Lease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.values {
		if value.RoomID == roomID {
			return value, true
		}
	}
	return operationlease.Lease{}, false
}

func (s *releaseFenceSet) replace(value operationlease.Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.values {
		if s.values[index].RoomID == value.RoomID {
			s.values[index] = value
			return
		}
	}
}

func (s *releaseFenceSet) operation(roomID, releaseID, phase string, index int) (runtimedriver.Operation, error) {
	lease, found := s.forRoom(roomID)
	if !found {
		return runtimedriver.Operation{}, ErrReleaseInvalid
	}
	key := fmt.Sprintf("g.%s.%s.%d", releaseID, phase, index)
	expires := lease.ExpiresAt.UTC()
	return runtimedriver.Operation{ID: key, Key: key, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, LeaseExpiresAt: &expires}, nil
}

func (s *releaseFenceSet) renewAll(ctx context.Context, service ReleaseLeaseService) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, value := range s.values {
		renewed, err := service.Renew(ctx, value, releaseLeaseTTL)
		if err != nil {
			return err
		}
		s.values[index] = renewed
	}
	return nil
}

func (s *releaseFenceSet) releaseAll(service ReleaseLeaseService) {
	s.mu.Lock()
	values := append([]operationlease.Lease(nil), s.values...)
	s.mu.Unlock()
	for _, value := range values {
		_ = service.Release(value)
	}
}

func orderedReleaseShards(plan ReleasePlan, masterFirst bool) []ReleaseShardPlan {
	var result []ReleaseShardPlan
	for _, target := range plan.Installations {
		result = append(result, target.Shards...)
	}
	return orderReleaseShardValues(result, masterFirst)
}

func orderReleaseShardValues(values []ReleaseShardPlan, masterFirst bool) []ReleaseShardPlan {
	result := append([]ReleaseShardPlan(nil), values...)
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].IsMaster != result[j].IsMaster {
			return result[i].IsMaster == masterFirst
		}
		return releaseShardKey(result[i]) < releaseShardKey(result[j])
	})
	return result
}

func sameReleaseStructure(left, right ReleasePlan) bool {
	if left.TopologyRevision != right.TopologyRevision || len(left.Installations) != len(right.Installations) {
		return false
	}
	for index := range left.Installations {
		first, second := left.Installations[index], right.Installations[index]
		if releaseInstallationKey(first.TargetID, first.InstallationID) != releaseInstallationKey(second.TargetID, second.InstallationID) || len(first.Shards) != len(second.Shards) {
			return false
		}
		for shardIndex := range first.Shards {
			if releaseShardKey(first.Shards[shardIndex]) != releaseShardKey(second.Shards[shardIndex]) || first.Shards[shardIndex].TargetID != second.Shards[shardIndex].TargetID || first.Shards[shardIndex].InstallationID != second.Shards[shardIndex].InstallationID || first.Shards[shardIndex].TopologyRevision != second.Shards[shardIndex].TopologyRevision {
				return false
			}
		}
	}
	return true
}

func releaseTargetDesiredVersion(target ReleaseInstallationPlan, fallback string) string {
	if value := strings.TrimSpace(target.DesiredVersion); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func releaseInstallationResult(value *Release, targetID, installationID string) *ReleaseInstallationResult {
	for index := range value.Installations {
		if value.Installations[index].TargetID == targetID && value.Installations[index].InstallationID == installationID {
			return &value.Installations[index]
		}
	}
	return nil
}

func releaseShardResult(value *Release, roomID, worldID string) *ReleaseShardResult {
	for index := range value.Shards {
		if value.Shards[index].RoomID == roomID && value.Shards[index].WorldID == worldID {
			return &value.Shards[index]
		}
	}
	return nil
}

func setReleaseInstallationFailure(value *ReleaseInstallationResult, code string, err error, now time.Time) {
	if value == nil {
		return
	}
	value.Stage, value.ErrorCode, value.ErrorMessage, value.FinishedAt, value.UpdatedAt = ReleaseStageFailed, code, err.Error(), &now, now
}

func setReleaseShardFailure(value *ReleaseShardResult, code string, err error, now time.Time) {
	if value == nil {
		return
	}
	value.Stage, value.ErrorCode, value.ErrorMessage, value.UpdatedAt = ReleaseStageFailed, code, err.Error(), now
}

func timePointer(value time.Time) *time.Time { return &value }

func releaseLoadMarker(shard ReleaseShardPlan, line string) string {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "[dst-admin-runtime ready]") {
		return "dst-admin-runtime-ready"
	}
	if shard.IsMaster || strings.EqualFold(shard.WorldDirectory, "Master") {
		if strings.Contains(lower, "[shard] shard server started on port:") {
			return "master-shard-server-started"
		}
	} else if strings.Contains(lower, "[shard] secondary shard lua is now ready!") {
		return "secondary-shard-lua-ready"
	}
	return ""
}
