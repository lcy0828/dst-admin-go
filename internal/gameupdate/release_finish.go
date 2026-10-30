package gameupdate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dont/internal/shards"
	"dont/shared"
)

func runtimeModeUnavailable(err error) bool {
	return errors.Is(err, shards.ErrRuntimeModeUnavailable) || err != nil && strings.HasPrefix(err.Error(), "RUNTIME_MODE_UNAVAILABLE:")
}

// Compare already-restored processes with the persisted effective selection,
// while retaining the original runtime preference in the confirmed plan.
func releaseEffectiveRuntimePlan(value Release) ReleasePlan {
	plan := value.Plan
	plan.Installations = append([]ReleaseInstallationPlan(nil), plan.Installations...)
	for i := range plan.Installations {
		plan.Installations[i].Shards = append([]ReleaseShardPlan(nil), plan.Installations[i].Shards...)
		for j := range plan.Installations[i].Shards {
			shard := &plan.Installations[i].Shards[j]
			if result := releaseShardResult(&value, shard.RoomID, shard.WorldID); result != nil && result.WarningCode == "RUNTIME_FALLBACK_GAME" && result.RuntimeMode == "game" {
				shard.RuntimeMode = result.RuntimeMode
			}
		}
	}
	return plan
}

func (c *ReleaseCoordinator) verifyCompletedInstallations(ctx context.Context, value Release) error {
	if !releaseWritesVerified(value) {
		return ErrReleaseRecoveryNeeded
	}
	for _, target := range value.Plan.Installations {
		observed, err := c.runtime.ObserveInstallation(ctx, target)
		if err != nil {
			return err
		}
		if !observed.Installed || observed.IntegrityError != "" || observed.CurrentVersion != releaseTargetDesiredVersion(target, value.Plan.DesiredVersion) {
			return fmt.Errorf("%w: %s %s", ErrReleaseRecoveryNeeded, target.InstallationID, observed.IntegrityError)
		}
	}
	return nil
}

// Ending maintenance abandons only the outstanding restart intention. It never
// unlocks an incomplete/unknown write, modifies runtime preferences, starts or
// stops a world, or forgets the original release and its protection backups.
func (c *ReleaseCoordinator) finishWithoutRestart(ctx context.Context, value Release) (Release, error) {
	if err := c.verifyCompletedInstallations(ctx, value); err != nil {
		return value, err
	}
	for _, shard := range orderedReleaseShards(value.Plan, true) {
		status, err := c.runtime.Status(ctx, shard)
		if err != nil {
			return value, err
		}
		if !(status.State == "stopped" && !status.SessionExists || status.State == "running" && status.SessionExists) {
			return value, fmt.Errorf("%w: 世界状态尚未确定，不能结束维护", ErrReleaseRecoveryNeeded)
		}
		result := releaseShardResult(&value, shard.RoomID, shard.WorldID)
		if result == nil {
			return value, ErrReleaseInvalid
		}
		result.Stage, result.RuntimeState, result.UpdatedAt = ReleaseStageAbandoned, status.State, c.now().UTC()
		result.ErrorCode, result.ErrorMessage = "", ""
		if mode, valid := shared.NormalizeRuntimePerformanceMode(status.RuntimeMode); status.RuntimeMode != "" && valid {
			result.RuntimeMode = mode
		}
	}
	now := c.now().UTC()
	value.Stage, value.NextAction, value.Blockers = ReleaseStageAbandoned, "", nil
	value.ErrorCode, value.ErrorMessage = "RESTORE_SKIPPED", "已验证游戏安装并结束维护；世界保持当前状态，未继续自动启动。可修复加速组件后手动启动。"
	value.FinishedAt, value.UpdatedAt = &now, now
	return c.store.Save(value)
}
