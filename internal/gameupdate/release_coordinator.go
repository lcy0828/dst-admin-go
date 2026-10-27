package gameupdate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/installationmaintenance"
	"dont/internal/maintenance"
	"dont/internal/operationlease"
	"dont/internal/roomops"
	"dont/internal/runtimeaudit"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/shared"
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
	mutations     runtimedriver.RuntimeMutationObserver
	online        maintenance.OnlineCounter
	maintenance   *installationmaintenance.Gate
}

func (c *ReleaseCoordinator) ConfigureOnlineCounter(counter maintenance.OnlineCounter) {
	c.online = counter
}

func (c *ReleaseCoordinator) requireEmpty(ctx context.Context, plan ReleasePlan) error {
	if !plan.Policy.RequireEmpty {
		return nil
	}
	return maintenance.RequireEmpty(ctx, c.online, plan.AffectedRoomIDs)
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

func (c *ReleaseCoordinator) ConfigureMutationObserver(observer runtimedriver.RuntimeMutationObserver) error {
	if observer == nil {
		return errors.New("game release mutation observer is required")
	}
	c.mutations = observer
	return nil
}

func (c *ReleaseCoordinator) Preview(ctx context.Context, request ReleasePreviewRequest) (ReleasePlan, error) {
	return c.planner.Preview(ctx, request)
}

func (c *ReleaseCoordinator) Get(id string) (Release, error) { return c.store.Get(id) }

// Failed releases remain retryable and must keep their original protection.
func (c *ReleaseCoordinator) RequiresProtection(id string) (bool, error) {
	value, err := c.store.Get(id)
	if errors.Is(err, ErrReleaseNotFound) {
		return false, nil
	}
	return value.Stage != ReleaseStageSucceeded, err
}

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
		if existing, err := c.store.FindIdempotent(request.ID, request.SourceJobID); err == nil {
			if existing.Plan.PlanHash != request.Plan.PlanHash {
				return existing, ErrReleaseConflict
			}
			return existing, nil
		}
		if err := c.store.requireNoPendingRelease(request.Plan); err != nil {
			return Release{}, err
		}
		fresh, err := c.previewStoredPlan(runContext, request.Plan)
		if err != nil {
			return Release{}, err
		}
		if err := checkReleasePreflight(request.Plan, fresh, false); err != nil {
			return Release{}, err
		}
		if err := c.requireEmpty(runContext, request.Plan); err != nil {
			return Release{}, err
		}
		if err := c.notifyBeforeRelease(runContext, request.Plan, request.SourceJobID); err != nil {
			return Release{}, err
		}
		if err := c.requireEmpty(runContext, request.Plan); err != nil {
			return Release{}, err
		}
		now := c.now().UTC()
		value := Release{
			ID: request.ID, Attempt: 1, SourceJobID: request.SourceJobID, Stage: ReleaseStagePreviewed, Plan: request.Plan,
			ProtectionBackupIDs: []string{}, CreatedAt: now, UpdatedAt: now,
		}
		for _, installation := range request.Plan.Installations {
			value.Installations = append(value.Installations, ReleaseInstallationResult{
				TargetID: installation.TargetID, InstallationID: installation.InstallationID,
				Stage: ReleaseStagePreviewed, BeforeVersion: installation.CurrentVersion, ExpectedVersion: installation.DesiredVersion, WriteState: "not_started", UpdatedAt: now,
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
		return c.execute(runContext, value, fences, false, false)
	})
}

func (c *ReleaseCoordinator) Retry(ctx context.Context, id string, sourceJobIDs ...string) (Release, error) {
	return c.retry(ctx, id, ReleaseRetryRequest{}, sourceJobIDs...)
}
func (c *ReleaseCoordinator) RetryConfirmed(ctx context.Context, id string, request ReleaseRetryRequest, sourceJobID string) (Release, error) {
	return c.retry(ctx, id, request, sourceJobID)
}
func (c *ReleaseCoordinator) retry(ctx context.Context, id string, request ReleaseRetryRequest, sourceJobIDs ...string) (Release, error) {
	if request.PlanHash != request.Confirmation || request.ValidateUnknown && request.PlanHash == "" {
		return Release{}, ErrReleaseConfirmation
	}
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
		// Legacy interrupted writes with no terminal evidence cannot be
		// treated as if no write ever happened.
		for index := range value.Installations {
			result := &value.Installations[index]
			if result.WriteState == "" && result.StartedAt != nil {
				value.WritingStarted = true
				if result.Stage == ReleaseStageVerified || result.Stage == ReleaseStageSucceeded {
					result.WriteState = "verified"
				} else if result.Stage == ReleaseStageUpdating {
					result.WriteState = "unknown"
				} else {
					result.WriteState = "failed"
				}
			}
		}
		previewPlan := value.Plan
		if request.ValidateUnknown {
			previewPlan.Policy.Validate = true
		}
		if request.PlanHash != "" {
			previewPlan.DesiredVersion = ""
			previewPlan.SelectedInstallations = recoverySelections(value.Plan)
		}
		// Restoring validated writes does not need SteamCMD or spare download
		// space. It still requires fresh placement and trustworthy versions.
		if releaseWritesVerified(value) && request.PlanHash == "" {
			previewPlan.Policy.Validate = false
		}
		fresh, previewErr := c.previewStoredPlan(runContext, previewPlan)
		if previewErr != nil {
			return c.failRelease(value, "RECOVERY_PREFLIGHT_FAILED", previewErr, ReleaseStageRecoveryRequired)
		}
		if request.PlanHash != "" {
			if fresh.PlanHash != request.PlanHash {
				return value, ErrReleasePlanChanged
			}
			if !fresh.Ready {
				return c.failRelease(value, "RECOVERY_PREFLIGHT_FAILED", &ReleasePreflightError{Blockers: fresh.Blockers}, ReleaseStageRecoveryRequired)
			}
			if !sameReleaseStructure(value.Plan, fresh) {
				return value, ErrReleaseTopologyChanged
			}
			if !sameReleaseMetadata(value.Plan, fresh) {
				return value, ErrReleasePlanChanged
			}
			originalHash := value.Plan.PlanHash
			value.PlanHistory = append(value.PlanHistory, value.Plan)
			for i := range fresh.Installations {
				for j := range fresh.Installations[i].Shards {
					fresh.Installations[i].Shards[j].WasRunning = value.Plan.Installations[i].Shards[j].WasRunning
					fresh.Installations[i].Shards[j].RuntimeMode = value.Plan.Installations[i].Shards[j].RuntimeMode
				}
				item := releaseInstallationResult(&value, fresh.Installations[i].TargetID, fresh.Installations[i].InstallationID)
				if item != nil && fresh.Installations[i].CurrentVersion != fresh.Installations[i].DesiredVersion {
					if item.WriteState == "verified" || item.WriteState == "version_changed" {
						item.WriteState = "failed"
					}
					if item.Stage == ReleaseStageVerified || item.Stage == ReleaseStageSucceeded {
						item.Stage = ReleaseStageStaged
					}
				}
			}
			fresh.Policy = value.Plan.Policy
			if request.ValidateUnknown {
				fresh.Policy.Validate = true
			}
			fresh.UpdateRequired = fresh.UpdateRequired || value.Plan.UpdateRequired
			fresh.PlanHash, _ = calculateReleasePlanHash(fresh)
			value.Plan = fresh
			value, err = c.store.Rebase(value, originalHash)
			if err != nil {
				return value, err
			}
		} else if err := checkReleasePreflight(value.Plan, fresh, true); err != nil {
			return c.failRelease(value, "RECOVERY_PREFLIGHT_FAILED", err, ReleaseStageRecoveryRequired)
		}
		for _, target := range fresh.Installations {
			result := releaseInstallationResult(&value, target.TargetID, target.InstallationID)
			if result != nil && (result.Stage == ReleaseStageVerified || result.Stage == ReleaseStageSucceeded) && target.CurrentVersion != releaseTargetDesiredVersion(target, fresh.DesiredVersion) {
				return c.failRelease(value, "VERIFIED_INSTALLATION_CHANGED", ErrReleasePlanChanged, ReleaseStageRecoveryRequired)
			}
		}
		if !releaseWritesVerified(value) {
			if err := c.requireRemainingEmpty(runContext, value); err != nil {
				return value, err
			}
			if err := c.notifyBeforeRelease(runContext, value.Plan, notificationJobID); err != nil {
				return value, err
			}
		}
		value.Attempt++
		value.ErrorCode, value.ErrorMessage, value.NextAction, value.Blockers, value.FinishedAt, value.UpdatedAt = "", "", "", nil, nil, c.now().UTC()
		if saved, saveErr := c.store.Save(value); saveErr == nil {
			value = saved
		} else {
			return value, saveErr
		}
		return c.execute(runContext, value, fences, true, request.ValidateUnknown)
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

func (c *ReleaseCoordinator) execute(ctx context.Context, value Release, fences *releaseFenceSet, recovery, validateUnknown bool) (Release, error) {
	if !value.Plan.UpdateRequired {
		return c.completeNoUpdate(value)
	}
	defer func() { runtimedriver.NotifyRuntimeTargetsChanged(c.mutations, releasePlanTargetIDs(value.Plan)...) }()
	// Check existing archives before any retry can stop a previously restored world.
	if recovery {
		for index, id := range value.ProtectionBackupIDs {
			if id != "" {
				if err := c.backups.VerifyProtection(ctx, id, value.Plan.AffectedRoomIDs[index], value.ID); err != nil {
					return c.failRelease(value, "PROTECTION_BACKUP_FAILED", err, ReleaseStageRecoveryRequired)
				}
			}
		}
	}
	// Recovery of a completed write must not repeat downloads, cold backups or
	// stop worlds which have already been restored by this release.
	restoringOnly := releaseWritesVerified(value)
	var err error
	if !restoringOnly {
		if checker, ok := c.backups.(interface{ CheckProtectionSpace(context.Context) error }); ok && needsReleaseProtection(value) {
			if err := checker.CheckProtectionSpace(ctx); err != nil {
				return c.abortBeforeWrite(ctx, value, fences, "BACKUP_PREFLIGHT_FAILED", err)
			}
		}
		value, _, err = c.stopPlannedShards(ctx, value, fences)
		if err != nil {
			return c.abortBeforeWrite(ctx, value, fences, "STOP_FAILED", err)
		}
		boundary := c.now().UTC()
		if err := c.revalidate(ctx, &value, boundary, true); err != nil {
			return c.abortBeforeWrite(ctx, value, fences, "PREFLIGHT_BLOCKED", err)
		}
		value, err = c.createProtectionBackups(ctx, value, fences)
		if err != nil {
			return c.abortBeforeWrite(ctx, value, fences, "PROTECTION_BACKUP_FAILED", err)
		}
		if err := c.revalidate(ctx, &value, c.now().UTC(), true); err != nil {
			return c.abortBeforeWrite(ctx, value, fences, "PREFLIGHT_BLOCKED", err)
		}
		value.Stage, value.UpdatedAt = ReleaseStageStaged, c.now().UTC()
		value, err = c.store.Save(value)
		if err != nil {
			return c.abortBeforeWrite(ctx, value, fences, "PERSISTENCE_FAILED", err)
		}
		value, err = c.updateInstallations(ctx, value, fences, validateUnknown)
	}
	// Only complete connected scopes can restart. Independent successful
	// rooms are not held down by a failed installation in another scope.
	updateErr := err
	if updateErr != nil && !value.WritingStarted {
		return c.abortBeforeWrite(ctx, value, fences, "UPDATE_NOT_STARTED", updateErr)
	}
	readyPlan := verifiedReleaseScopes(value)
	if len(readyPlan.Installations) > 0 {
		if value.Plan.Policy.RestartRunning {
			value, err = c.restartAndConfirm(ctx, value, fences, readyPlan)
		} else {
			value = markStoppedScopeComplete(value, readyPlan)
		}
		// Subset plans are used only in memory; persistence always retains
		// the complete confirmed plan and all original running intentions.
		if saved, saveErr := c.store.Save(value); saveErr == nil {
			value = saved
		} else {
			err = errors.Join(err, saveErr)
		}
	}
	if updateErr != nil {
		return c.failRelease(value, "UPDATE_INCOMPLETE", errors.Join(updateErr, err), ReleaseStageRecoveryRequired)
	}
	if err != nil {
		return c.failRelease(value, "RESTART_INCOMPLETE", err, ReleaseStageRecoveryRequired)
	}
	now := c.now().UTC()
	value.Stage, value.ErrorCode, value.ErrorMessage, value.NextAction, value.Blockers, value.FinishedAt, value.UpdatedAt = ReleaseStageSucceeded, "", "", "", nil, &now, now
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
			if err := c.backups.VerifyProtection(ctx, value.ProtectionBackupIDs[index], roomID, value.ID); err != nil {
				return value, fmt.Errorf("更新前保护备份不可用，已阻止继续更新房间 %s: %w", roomID, err)
			}
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
		for len(value.ProtectionBackupIDs) <= index {
			value.ProtectionBackupIDs = append(value.ProtectionBackupIDs, "")
		}
		value.ProtectionBackupIDs[index] = backupID
		value.UpdatedAt = c.now().UTC()
		value, err = c.store.Save(value)
		if err != nil {
			return value, err
		}
	}
	return value, nil
}

func (c *ReleaseCoordinator) stopPlannedShards(ctx context.Context, value Release, fences *releaseFenceSet) (Release, []ReleaseShardPlan, error) {
	if err := c.requireRemainingEmpty(ctx, value); err != nil {
		return value, nil, err
	}
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
		installation := releaseInstallationResult(&value, shard.TargetID, shard.InstallationID)
		if installation != nil && installation.Stage == ReleaseStageSucceeded {
			continue
		}
		if !shard.WasRunning {
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
			if err := c.requireRemainingEmpty(ctx, value); err != nil {
				return value, stopped, err
			}
			operation, operationErr := fences.operation(shard.RoomID, releaseAttemptID(value), "stop", index)
			if operationErr != nil {
				return value, stopped, operationErr
			}
			result.StopRequestedAt = timePointer(c.now().UTC())
			value, err = c.store.Save(value)
			if err != nil {
				return value, stopped, err
			}
			result = releaseShardResult(&value, shard.RoomID, shard.WorldID)
			stopped = append(stopped, shard)
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

func (c *ReleaseCoordinator) updateInstallations(ctx context.Context, value Release, fences *releaseFenceSet, validateUnknown bool) (Release, error) {
	value.Stage, value.UpdatedAt = ReleaseStageUpdating, c.now().UTC()
	value, err := c.store.Save(value)
	if err != nil {
		return value, err
	}
	var failures error
	for index, target := range value.Plan.Installations {
		if err := ctx.Err(); err != nil {
			return value, errors.Join(failures, err)
		}
		desired := releaseTargetDesiredVersion(target, value.Plan.DesiredVersion)
		result := releaseInstallationResult(&value, target.TargetID, target.InstallationID)
		if result == nil {
			return value, ErrReleaseInvalid
		}
		// Duplicate registrations of one physical installation share one write.
		aliasHandled := false
		for _, previous := range value.Plan.Installations[:index] {
			if target.TargetID == previous.TargetID && target.ServerPath != "" && target.ServerPath == previous.ServerPath {
				source := releaseInstallationResult(&value, previous.TargetID, previous.InstallationID)
				if source != nil {
					id := result.InstallationID
					*result = *source
					result.InstallationID = id
					aliasHandled = true
				}
				break
			}
		}
		if aliasHandled {
			value, err = c.store.Save(value)
			if err != nil {
				return value, errors.Join(failures, err)
			}
			continue
		}
		alreadyValidated := result.Stage == ReleaseStageVerified || result.Stage == ReleaseStageSucceeded
		observation, observeErr := c.runtime.ObserveInstallation(ctx, target)
		if alreadyValidated {
			if observeErr != nil || !observation.Installed || observation.CurrentVersion != desired {
				err := errors.Join(errors.New("已验证的游戏安装发生变化，需要重新检查"), observeErr)
				setReleaseInstallationFailure(result, "VERIFIED_INSTALLATION_CHANGED", err, c.now().UTC())
				failures = errors.Join(failures, err)
			}
			continue
		}
		if result.WriteState == "dispatched" || result.WriteState == "unknown" {
			result.WriteState = "unknown"
			var evidence shared.RuntimeOperationEvidence
			var evidenceErr error
			if reader, ok := c.runtime.(interface {
				ObserveUpdate(context.Context, ReleaseInstallationPlan, string) (shared.RuntimeOperationEvidence, error)
			}); ok {
				evidence, evidenceErr = reader.ObserveUpdate(ctx, target, result.OperationID)
			} else {
				evidenceErr = errors.New("节点操作回执暂不可用")
			}
			matched := evidenceErr == nil && evidence.Completed && evidence.OperationID == result.OperationID && evidence.Action == string(shared.RuntimeActionGameVersionUpdate)
			if matched && evidence.Outcome == shared.RuntimeOutcomeConfirmed && observeErr == nil && observation.Installed && observation.CurrentVersion == desired {
				result.Stage, result.WriteState, result.AfterVersion, result.ErrorCode, result.ErrorMessage = ReleaseStageVerified, "verified", observation.CurrentVersion, "", ""
				result.FinishedAt, result.UpdatedAt = timePointer(c.now().UTC()), c.now().UTC()
				value, err = c.store.Save(value)
				if err != nil {
					return value, errors.Join(failures, err)
				}
				continue
			}
			// A lost receipt never authorizes an implicit resend. Explicit
			// re-preview + validation creates a new fenced repair attempt; the
			// node's directory lock and SteamCMD process guard reject overlap.
			if !validateUnknown && (!matched || evidence.Outcome != shared.RuntimeOutcomeFailed) {
				failures = errors.Join(failures, errors.Join(errors.New("游戏写入结果待确认，未重复发送更新；可重新预检并校验修复"), evidenceErr))
				continue
			}
			if validateUnknown && !matched && !releaseHasCapability(target.Capabilities, RequiredUpdateRepairCapability) {
				result.ErrorCode, result.ErrorMessage = "UPDATE_REPAIR_UNSUPPORTED", "请先升级该节点的 Agent，以支持带进程保护的未知写入校验修复"
				failures = errors.Join(failures, errors.New(result.ErrorMessage))
				continue
			}
			result.WriteState = "failed"
		}
		if !value.Plan.Policy.Validate && result.WriteState == "not_started" && observeErr == nil && observation.Installed && observation.CurrentVersion == desired {
			result.Stage, result.WriteState, result.AfterVersion = ReleaseStageVerified, "verified", observation.CurrentVersion
			result.FinishedAt, result.UpdatedAt = timePointer(c.now().UTC()), c.now().UTC()
			value, err = c.store.Save(value)
			failures = errors.Join(failures, err)
			continue
		}
		// Recheck dynamic write prerequisites immediately before each write.
		if observeErr != nil || !observation.SteamCMDAvailable || !observation.UpdateSupported || observation.AvailableBytes < target.RequiredBytes {
			err := errors.Join(errors.New("写入前检查失败：请检查 SteamCMD、节点连接和游戏磁盘空间"), observeErr)
			setReleaseInstallationFailure(result, "INSTALLATION_PREFLIGHT_FAILED", err, c.now().UTC())
			failures = errors.Join(failures, err)
			continue
		}
		latest, _, latestErr := c.planner.latest.Check(ctx, target.AppID, observation.CurrentVersion)
		if latestErr != nil || strings.TrimSpace(latest) != desired {
			err := errors.Join(ErrDesiredVersionChanged, latestErr)
			setReleaseInstallationFailure(result, "DESIRED_VERSION_CHANGED", err, c.now().UTC())
			failures = errors.Join(failures, err)
			continue
		}
		operation, operationErr := fences.operation(target.Shards[0].RoomID, releaseAttemptID(value), "update", index)
		if operationErr != nil {
			failures = errors.Join(failures, operationErr)
			continue
		}
		previousResult, writingStarted := *result, value.WritingStarted
		value.WritingStarted = true
		result.Stage, result.WriteState, result.OperationID, result.ExpectedVersion = ReleaseStageUpdating, "dispatched", operation.ID, desired
		result.StartedAt, result.UpdatedAt = timePointer(c.now().UTC()), c.now().UTC()
		value, err = c.store.Save(value)
		if err != nil {
			value.WritingStarted = writingStarted
			*releaseInstallationResult(&value, target.TargetID, target.InstallationID) = previousResult
			return value, err
		}
		result = releaseInstallationResult(&value, target.TargetID, target.InstallationID)
		updated, updateErr := c.runtime.UpdateInstallation(ctx, target, operation, desired, value.Plan.Policy.CleanCache)
		result.AfterVersion, result.Log = updated.CurrentVersion, updated.Log
		code := "UPDATE_FAILED"
		if updateErr == nil && (!updated.Installed || updated.CurrentVersion != desired) {
			updateErr = ErrDesiredVersionChanged
		}
		switch {
		case updateErr == nil:
			result.WriteState = "verified"
		case errors.Is(updateErr, runtimedriver.ErrOperationNotDispatched):
			value.WritingStarted = writingStarted
			result.WriteState = "not_started"
		case updated.Installed || !updated.ObservedAt.IsZero():
			result.WriteState = "failed"
			if updated.CurrentVersion != "" && updated.CurrentVersion != desired && updated.CurrentVersion != result.BeforeVersion {
				code, result.WriteState = "DESIRED_VERSION_CHANGED", "version_changed"
				updateErr = errors.Join(ErrDesiredVersionChanged, updateErr)
			}
		default:
			result.WriteState, code = "unknown", "UPDATE_OUTCOME_UNKNOWN"
		}
		if updateErr != nil {
			setReleaseInstallationFailure(result, code, updateErr, c.now().UTC())
			failures = errors.Join(failures, updateErr)
		} else {
			result.Stage, result.ErrorCode, result.ErrorMessage, result.FinishedAt, result.UpdatedAt = ReleaseStageVerified, "", "", timePointer(c.now().UTC()), c.now().UTC()
		}
		value, err = c.store.Save(value)
		if err != nil {
			return value, errors.Join(failures, err)
		}
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

func (c *ReleaseCoordinator) restartAndConfirm(ctx context.Context, value Release, fences *releaseFenceSet, scope ReleasePlan) (Release, error) {
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
	for index, shard := range orderedReleaseShards(scope, true) {
		result := releaseShardResult(&value, shard.RoomID, shard.WorldID)
		if result == nil || !shard.WasRunning {
			if result != nil {
				result.Stage, result.RuntimeState, result.UpdatedAt = ReleaseStageSucceeded, string(shards.RuntimeStopped), c.now().UTC()
			}
			continue
		}
		status, statusErr := c.runtime.Status(ctx, shard)
		if statusErr != nil {
			setReleaseShardFailure(result, "STATUS_FAILED", statusErr, c.now().UTC())
			failures = errors.Join(failures, statusErr)
			continue
		}
		if result.Stage == ReleaseStageSucceeded && status.State == "running" {
			continue
		}
		alreadyStarted := result.StartedAt != nil && (status.State == "running" || status.State == "starting")
		if result.StartedAt == nil && status.State != "stopped" {
			err := errors.New("无法确认待恢复世界的进程归属")
			setReleaseShardFailure(result, "START_STATE_UNCERTAIN", err, c.now().UTC())
			failures = errors.Join(failures, err)
			continue
		}
		if alreadyStarted {
			cursors[releaseShardKey(shard)] = cursor{fileID: result.LogFileID, offset: result.LogOffset}
		}
		if !alreadyStarted && value.Plan.Policy.LoadConfirmation == ReleaseLoadConfirmationLogs {
			fileID, offset, cursorErr := c.runtime.CaptureLogCursor(ctx, shard)
			if cursorErr != nil {
				setReleaseShardFailure(result, "LOG_CURSOR_FAILED", cursorErr, c.now().UTC())
				failures = errors.Join(failures, cursorErr)
				continue
			}
			cursors[releaseShardKey(shard)] = cursor{fileID: fileID, offset: offset}
			result.LogFileID, result.LogOffset = fileID, offset
		}
		result.Stage, result.ErrorCode, result.ErrorMessage = ReleaseStageConfirming, "", ""
		if !alreadyStarted {
			result.StartedAt, result.UpdatedAt = timePointer(c.now().UTC()), c.now().UTC()
			value, err = c.store.Save(value)
			if err != nil {
				return value, err
			}
			result = releaseShardResult(&value, shard.RoomID, shard.WorldID)
			operation, operationErr := fences.operation(shard.RoomID, releaseAttemptID(value), "start", index)
			if operationErr == nil {
				operation.RuntimeMode = shard.RuntimeMode
				operationErr = c.runtime.Start(ctx, shard, operation)
			}
			if operationErr != nil {
				setReleaseShardFailure(result, "START_FAILED", operationErr, c.now().UTC())
				failures = errors.Join(failures, operationErr)
				continue
			}
		}
		now := c.now().UTC()
		result.Stage, result.UpdatedAt = ReleaseStageConfirming, now
		value, err = c.store.Save(value)
		failures = errors.Join(failures, err)
	}
	value.Stage, value.UpdatedAt = ReleaseStageConfirming, c.now().UTC()
	if saved, saveErr := c.store.Save(value); saveErr == nil {
		value = saved
	} else {
		failures = errors.Join(failures, saveErr)
	}
	for _, shard := range orderedReleaseShards(scope, true) {
		if !shard.WasRunning {
			continue
		}
		result := releaseShardResult(&value, shard.RoomID, shard.WorldID)
		if result == nil || result.Stage == ReleaseStageFailed || result.Stage == ReleaseStageSucceeded {
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
		included := false
		for _, target := range scope.Installations {
			if target.TargetID == value.Installations[index].TargetID && target.InstallationID == value.Installations[index].InstallationID {
				included = true
			}
		}
		if !included {
			continue
		}
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
		status, err := c.runtime.Status(ctx, shard)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if status.State == "running" || status.State == "starting" {
			continue
		}
		if status.State != "stopped" || status.SessionExists {
			failures = errors.Join(failures, errors.New("恢复前无法确认分片已停止"))
			continue
		}
		operation, err := fences.operation(shard.RoomID, releaseAttemptID(*value), "recover", index)
		if err == nil {
			operation.RuntimeMode = shard.RuntimeMode
			err = c.runtime.Start(ctx, shard, operation)
		}
		result := releaseShardResult(value, shard.RoomID, shard.WorldID)
		if err != nil {
			setReleaseShardFailure(result, "RECOVERY_START_FAILED", err, c.now().UTC())
			failures = errors.Join(failures, err)
		} else if result != nil {
			if err := c.waitShardState(ctx, shard, true, 2*time.Minute); err != nil {
				failures = errors.Join(failures, err)
				continue
			}
			result.RuntimeState, result.UpdatedAt = string(shards.RuntimeRunning), c.now().UTC()
		}
	}
	_, saveErr := c.store.Save(*value)
	return errors.Join(failures, saveErr)
}

func (c *ReleaseCoordinator) previewStoredPlan(ctx context.Context, plan ReleasePlan) (ReleasePlan, error) {
	return c.previewStoredPlanAfter(ctx, plan, time.Time{})
}
func (c *ReleaseCoordinator) previewStoredPlanAfter(ctx context.Context, plan ReleasePlan, after time.Time) (ReleasePlan, error) {
	restart := plan.Policy.RestartRunning
	targetIDs := releasePlanTargetIDs(plan)
	var installations map[string]bool
	{
		targetIDs = nil
		installations = make(map[string]bool)
		for _, installation := range plan.Installations {
			installations[releaseInstallationKey(installation.TargetID, installation.InstallationID)] = true
		}
	}
	if len(plan.SelectedInstallations) > 0 {
		targetIDs, installations = nil, nil
	}
	return c.planner.Preview(ctx, ReleasePreviewRequest{
		observeAfter:     after,
		Installations:    plan.SelectedInstallations,
		installationKeys: installations,
		DesiredVersion:   plan.DesiredVersion,
		TargetIDs:        targetIDs,
		Policy:           ReleasePolicyInput{Validate: plan.Policy.Validate, RequireEmpty: plan.Policy.RequireEmpty, CleanCache: plan.Policy.CleanCache, RestartRunning: &restart, LoadConfirmation: plan.Policy.LoadConfirmation, TimeoutSeconds: plan.Policy.TimeoutSeconds},
	})
}

func releasePlanTargetIDs(plan ReleasePlan) []string {
	targets := make([]string, 0, len(plan.Installations))
	seen := make(map[string]bool, len(plan.Installations))
	for _, installation := range plan.Installations {
		if installation.TargetID != "" && !seen[installation.TargetID] {
			seen[installation.TargetID] = true
			targets = append(targets, installation.TargetID)
		}
	}
	sort.Strings(targets)
	return targets
}

func (c *ReleaseCoordinator) withReleaseLocks(ctx context.Context, plan ReleasePlan, operationKey string, run func(context.Context, *releaseFenceSet) (Release, error)) (Release, error) {
	// Room locks precede the installation gate consistently. Gate acquisition
	// never waits, so a lifecycle writer cannot invert this order and deadlock.
	ctx, releaseRooms, err := roomops.AcquireMany(ctx, plan.AffectedRoomIDs)
	if err != nil {
		return Release{}, err
	}
	defer releaseRooms()
	owner := strings.TrimSuffix(operationKey, ":retry")
	ctx, releaseInstallation, err := c.maintenance.Acquire(ctx, releaseResources(plan), owner)
	if err != nil {
		return Release{}, err
	}
	defer releaseInstallation()
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
	var blocked *ReleasePreflightError
	if errors.As(cause, &blocked) {
		value.Blockers = blocked.Blockers
		if len(blocked.Blockers) > 0 {
			code = blocked.Blockers[0].Code
		}
	}
	if errors.Is(cause, ErrDesiredVersionChanged) {
		code = "DESIRED_VERSION_CHANGED"
	}
	if errors.Is(cause, context.Canceled) {
		code = "CANCELED"
	}
	now := c.now().UTC()
	value.NextAction = "retry_release"
	for _, result := range value.Installations {
		if result.WriteState == "unknown" || result.WriteState == "dispatched" {
			code, value.NextAction = "UPDATE_OUTCOME_UNKNOWN", "inspect_release"
		}
	}
	if errors.Is(cause, ErrDesiredVersionChanged) {
		value.NextAction = "preview_and_retry"
	}
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
	if len(left.Installations) != len(right.Installations) {
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
