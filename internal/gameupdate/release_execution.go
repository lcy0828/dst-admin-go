package gameupdate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"dont/internal/maintenance"
	"dont/shared"
)

// ReleasePreflightError retains the actual dynamic blockers for HTTP, Jobs and
// MCP instead of replacing unavailable evidence with a PLAN_CHANGED error.
type ReleasePreflightError struct{ Blockers []ReleaseBlocker }

func (e *ReleasePreflightError) Error() string {
	messages := make([]string, 0, len(e.Blockers))
	for _, b := range e.Blockers {
		messages = append(messages, b.Code+": "+b.Message)
	}
	return strings.Join(messages, "; ")
}
func (*ReleasePreflightError) Unwrap() error { return ErrReleasePreviewBlocked }

func checkReleasePreflight(original, fresh ReleasePlan, changedByTask bool) error {
	if !fresh.Ready || len(fresh.Blockers) > 0 {
		return &ReleasePreflightError{Blockers: fresh.Blockers}
	}
	if !sameReleaseStructure(original, fresh) {
		return ErrReleaseTopologyChanged
	}
	for i, target := range original.Installations {
		actual := fresh.Installations[i]
		if target.AppID != actual.AppID || target.UpdateMethod != actual.UpdateMethod || (original.Version >= 2 && (target.ServerPath != actual.ServerPath || target.Branch != actual.Branch)) {
			return ErrReleasePlanChanged
		}
		if releaseTargetDesiredVersion(target, original.DesiredVersion) != releaseTargetDesiredVersion(actual, fresh.DesiredVersion) {
			return ErrDesiredVersionChanged
		}
		if !changedByTask && target.CurrentVersion != actual.CurrentVersion {
			return ErrReleasePlanChanged
		}
		for j, shard := range target.Shards {
			current := actual.Shards[j]
			if !changedByTask && shard.WasRunning != current.WasRunning {
				return ErrReleasePlanChanged
			}
			if current.WasRunning {
				expectedMode, _ := shared.NormalizeRuntimePerformanceMode(shard.RuntimeMode)
				actualMode, valid := shared.NormalizeRuntimePerformanceMode(current.RuntimeMode)
				if !valid || expectedMode != actualMode {
					return ErrReleasePlanChanged
				}
			}
		}
	}
	return nil
}

func (c *ReleaseCoordinator) revalidate(ctx context.Context, value *Release, after time.Time, changedByTask bool) error {
	fresh, err := c.previewStoredPlanAfter(ctx, value.Plan, after)
	if err != nil {
		return err
	}
	if err := checkReleasePreflight(value.Plan, fresh, changedByTask); err != nil {
		return err
	}
	// A world which was stopped by this task must still be stopped before a
	// write. Originally stopped worlds may not appear unexpectedly either.
	for _, target := range fresh.Installations {
		for _, shard := range target.Shards {
			result := releaseInstallationResult(value, shard.TargetID, shard.InstallationID)
			if result != nil && result.Stage == ReleaseStageSucceeded {
				continue
			}
			if shard.WasRunning {
				return &ReleasePreflightError{Blockers: []ReleaseBlocker{releaseShardBlocker("SHARD_RUNNING", "世界在维护停服后仍在运行，已阻止写入游戏文件", shard.TargetID, shard.InstallationID, shard.RoomID, shard.WorldID)}}
			}
		}
	}
	return nil
}

func releaseAttemptID(value Release) string { return fmt.Sprintf("%s.a%d", value.ID, value.Attempt) }

func releaseWritesVerified(value Release) bool {
	for _, result := range value.Installations {
		if result.Stage != ReleaseStageVerified && result.Stage != ReleaseStageSucceeded {
			return false
		}
	}
	return len(value.Installations) > 0
}

func (c *ReleaseCoordinator) completeNoUpdate(value Release) (Release, error) {
	now := c.now().UTC()
	value.Stage, value.FinishedAt, value.UpdatedAt = ReleaseStageSucceeded, &now, now
	for i := range value.Installations {
		result := &value.Installations[i]
		result.Stage, result.AfterVersion, result.UpdatedAt, result.FinishedAt = ReleaseStageSucceeded, result.BeforeVersion, now, &now
	}
	for i := range value.Shards {
		value.Shards[i].Stage, value.Shards[i].UpdatedAt = ReleaseStageSucceeded, now
	}
	return c.store.Save(value)
}

// Connected installations share either a room or a real server directory.
// Successful independent components can resume even when another one fails.
func verifiedReleaseScopes(value Release) ReleasePlan {
	plan := value.Plan
	plan.Installations, plan.AffectedRoomIDs = nil, nil
	excluded := make(map[string]bool)
	for _, target := range value.Plan.Installations {
		result := releaseInstallationResult(&value, target.TargetID, target.InstallationID)
		if result == nil || result.Stage != ReleaseStageVerified && result.Stage != ReleaseStageSucceeded {
			excluded[releaseInstallationKey(target.TargetID, target.InstallationID)] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, a := range value.Plan.Installations {
			if !excluded[releaseInstallationKey(a.TargetID, a.InstallationID)] {
				continue
			}
			for _, b := range value.Plan.Installations {
				key := releaseInstallationKey(b.TargetID, b.InstallationID)
				if !excluded[key] && releaseInstallationsConnected(a, b) {
					excluded[key], changed = true, true
				}
			}
		}
	}
	rooms := make(map[string]bool)
	for _, target := range value.Plan.Installations {
		if excluded[releaseInstallationKey(target.TargetID, target.InstallationID)] {
			continue
		}
		plan.Installations = append(plan.Installations, target)
		for _, shard := range target.Shards {
			rooms[shard.RoomID] = true
		}
	}
	for _, roomID := range value.Plan.AffectedRoomIDs {
		if rooms[roomID] {
			plan.AffectedRoomIDs = append(plan.AffectedRoomIDs, roomID)
		}
	}
	return plan
}

func releaseInstallationsConnected(a, b ReleaseInstallationPlan) bool {
	if a.TargetID == b.TargetID && a.ServerPath != "" && a.ServerPath == b.ServerPath {
		return true
	}
	for _, first := range a.Shards {
		for _, second := range b.Shards {
			if first.RoomID == second.RoomID {
				return true
			}
		}
	}
	return false
}

func markStoppedScopeComplete(value Release, plan ReleasePlan) Release {
	for _, target := range plan.Installations {
		if item := releaseInstallationResult(&value, target.TargetID, target.InstallationID); item != nil {
			item.Stage = ReleaseStageSucceeded
		}
		for _, shard := range target.Shards {
			if item := releaseShardResult(&value, shard.RoomID, shard.WorldID); item != nil {
				item.Stage, item.RuntimeState = ReleaseStageSucceeded, "stopped"
			}
		}
	}
	return value
}

// Only still-running rooms need presence evidence. Stopped worlds cannot
// provide live telemetry and must never be interpreted as an unknown player.
func (c *ReleaseCoordinator) requireRemainingEmpty(ctx context.Context, value Release) error {
	if !value.Plan.Policy.RequireEmpty {
		return nil
	}
	rooms := make(map[string]bool)
	for _, shard := range orderedReleaseShards(value.Plan, true) {
		if result := releaseInstallationResult(&value, shard.TargetID, shard.InstallationID); result != nil && result.Stage == ReleaseStageSucceeded {
			continue
		}
		status, err := c.runtime.Status(ctx, shard)
		if err != nil {
			return errors.Join(maintenance.ErrPresenceUnavailable, err)
		}
		switch status.State {
		case "running", "starting":
			rooms[shard.RoomID] = true
		case "stopped":
			if status.SessionExists {
				return maintenance.ErrPresenceUnavailable
			}
		default:
			return maintenance.ErrPresenceUnavailable
		}
	}
	ids := make([]string, 0, len(rooms))
	for _, id := range value.Plan.AffectedRoomIDs {
		if rooms[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return maintenance.RequireEmpty(ctx, c.online, ids)
}

func (c *ReleaseCoordinator) abortBeforeWrite(ctx context.Context, value Release, fences *releaseFenceSet, code string, cause error) (Release, error) {
	// Never compensate after a write may have been dispatched in an earlier
	// attempt; file versions alone are not proof of a complete installation.
	if value.WritingStarted {
		return c.failRelease(value, code, cause, ReleaseStageRecoveryRequired)
	}
	for _, result := range value.Installations {
		if result.WriteState != "" && result.WriteState != "not_started" {
			return c.failRelease(value, code, cause, ReleaseStageRecoveryRequired)
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	// Lease ownership must still be provable before recovery starts.
	if err := fences.renewAll(cleanup, c.leases); err != nil {
		return c.failRelease(value, "LEASE_LOST", errors.Join(cause, err), ReleaseStageRecoveryRequired)
	}
	var stopped []ReleaseShardPlan
	for _, shard := range orderedReleaseShards(value.Plan, true) {
		result := releaseShardResult(&value, shard.RoomID, shard.WorldID)
		if shard.WasRunning && result != nil && (result.StopRequestedAt != nil || result.StoppedAt != nil) {
			stopped = append(stopped, shard)
		}
	}
	if err := c.recoverStoppedShards(cleanup, &value, stopped, fences); err != nil {
		return c.failRelease(value, "STOP_RECOVERY_FAILED", errors.Join(cause, err), ReleaseStageRecoveryRequired)
	}
	return c.failRelease(value, code, cause, ReleaseStageFailed)
}

func sameReleaseMetadata(original, fresh ReleasePlan) bool {
	for index, target := range original.Installations {
		actual := fresh.Installations[index]
		if target.AppID != actual.AppID || target.UpdateMethod != actual.UpdateMethod || original.Version >= 2 && (target.ServerPath != actual.ServerPath || target.Branch != actual.Branch) {
			return false
		}
	}
	return true
}

func recoverySelections(plan ReleasePlan) []ReleaseInstallationSelection {
	if len(plan.SelectedInstallations) > 0 {
		return plan.SelectedInstallations
	}
	values := make([]ReleaseInstallationSelection, 0, len(plan.Installations))
	for _, target := range plan.Installations {
		values = append(values, ReleaseInstallationSelection{TargetID: target.TargetID, InstallationID: target.InstallationID})
	}
	return values
}

func releaseHasCapability(values []string, required string) bool {
	for _, value := range values {
		if value == required {
			return true
		}
	}
	return false
}

func needsReleaseProtection(value Release) bool {
	for i := range value.Plan.AffectedRoomIDs {
		if i >= len(value.ProtectionBackupIDs) || value.ProtectionBackupIDs[i] == "" {
			return true
		}
	}
	return false
}
