package modupdates

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"dont/internal/mods"
)

func (s *Service) recordCheckResult(state State, checked mods.ActionResult) State {
	previousTime := state.LastCheckedAt
	state = s.recordCheck(state, checked.ModIDs)
	state.CheckSummary = checked.CheckSummary
	if checked.CheckSummary != nil && !checked.CheckSummary.Complete {
		state.LastCheckedAt = previousTime
		if len(checked.ModIDs) == 0 && state.PreparedAt == nil {
			state.Status = StatusBlocked
			state.ErrorCode, state.ErrorMessage = "MOD_UPDATE_VERSION_UNCONFIRMED", checked.CheckSummary.Message()
		}
	}
	if state.PendingUpdate != nil {
		state.AvailableModIDs = normalizedIDs(append(state.AvailableModIDs, state.PendingUpdate.ModIDs...))
		if state.Status == StatusIdle {
			state.Status = StatusAvailable
		}
	}
	return state
}

func pendingDownloadIDs(state State, ids []string) []string {
	if state.PendingUpdate != nil && state.PreparedAt == nil {
		return normalizedIDs(append(append([]string{}, ids...), state.PendingUpdate.ModIDs...))
	}
	return ids
}

func (s *Service) trackDownloads(ctx context.Context, state *State, ids []string) (outCtx context.Context, resultErr error) {
	previousState := *state
	if state.PendingUpdate != nil {
		copy := *state.PendingUpdate
		copy.ModIDs = append([]string{}, copy.ModIDs...)
		copy.Targets = append([]mods.ModUpdateTarget{}, copy.Targets...)
		state.PendingUpdate = &copy
	}
	defer func() {
		if resultErr != nil {
			*state = previousState
		}
	}()
	affected, err := s.affectedRooms(ctx, state.RoomID)
	if err != nil {
		return ctx, err
	}
	affected = normalizedIDs(affected)
	if state.PendingUpdate == nil {
		state.PendingUpdate = &PendingUpdate{ModIDs: normalizedIDs(ids), AffectedRoomIDs: affected, Targets: []mods.ModUpdateTarget{}, RestartedRoomIDs: []string{}}
	} else {
		if !slices.Equal(state.PendingUpdate.AffectedRoomIDs, affected) {
			return ctx, errors.New("待完成更新的影响范围已变化，请核对原任务")
		}
		if state.PendingUpdate.RestartingRoomID != "" || len(state.PendingUpdate.RestartedRoomIDs) > 0 || len(state.PendingUpdate.RestartWorlds) > 0 {
			return ctx, errors.New("上次重启尚未全部完成，请先核对原任务")
		}
		state.PendingUpdate.ModIDs = normalizedIDs(append(state.PendingUpdate.ModIDs, ids...))
	}
	if state.CheckSummary != nil {
		for _, id := range ids {
			index := slices.IndexFunc(state.CheckSummary.Items, func(item mods.ModUpdateCheckItem) bool { return item.ModID == id })
			if index < 0 || state.CheckSummary.Items[index].Status == "unknown" {
				return ctx, errors.New("待更新模组的配置或版本尚未确认，请重新检查")
			}
		}
	}
	if state.CheckSummary != nil {
		revision := state.CheckSummary.Scope.Revision
		if state.PendingUpdate.ScopeRevision != "" && state.PendingUpdate.ScopeRevision != revision {
			return ctx, errors.New("模组配置或运行位置已变化，请核对原任务")
		}
		state.PendingUpdate.ScopeRevision = revision
	}
	if err := s.validatePendingScope(ctx, *state); err != nil {
		return ctx, err
	}
	// A newer batch must finish before an older prepared batch can restart.
	state.PreparedAt = nil
	ctx = mods.WithUpdateScope(ctx, state.PendingUpdate.ScopeRevision)
	if *state, err = s.saveState(*state); err != nil {
		return ctx, err
	}
	previous := append([]mods.ModUpdateTarget{}, state.PendingUpdate.Targets...)
	var mu sync.Mutex
	return mods.WithUpdateTracking(ctx, previous, func(target mods.ModUpdateTarget) error {
		mu.Lock()
		defer mu.Unlock()
		pending := state.PendingUpdate
		index := slices.IndexFunc(pending.Targets, func(item mods.ModUpdateTarget) bool {
			return item.TargetID == target.TargetID && item.InstallationID == target.InstallationID
		})
		if index < 0 {
			pending.Targets = append(pending.Targets, target)
		} else {
			pending.Targets[index] = target
		}
		var err error
		*state, err = s.saveState(*state)
		return err
	}), nil
}

func (s *Service) validatePendingScope(ctx context.Context, state State) error {
	if state.PendingUpdate == nil {
		return nil
	}
	if id := state.PendingUpdate.RestartingRoomID; id != "" {
		worlds := state.PendingUpdate.RestartWorlds[id]
		terminal, ready := len(worlds) > 0, true
		for _, world := range worlds {
			terminal = terminal && (world.Status == "ready" || world.Status == "failed")
			ready = ready && world.Status == "ready"
		}
		if !terminal {
			return errors.New("上次重启结果尚未确认，请核对原任务和世界状态；未重复重启")
		}
		state.PendingUpdate.RestartingRoomID = ""
		if ready {
			state.PendingUpdate.RestartedRoomIDs = normalizedIDs(append(state.PendingUpdate.RestartedRoomIDs, id))
		}
	}
	reader, ok := s.catalog.(interface {
		ModUpdateScope(context.Context, string) (mods.ModUpdateScope, error)
	})
	if !ok || state.PendingUpdate.ScopeRevision == "" {
		return nil
	}
	current, err := reader.ModUpdateScope(ctx, state.RoomID)
	if err != nil {
		return err
	}
	if current.Revision != state.PendingUpdate.ScopeRevision {
		return errors.New("模组配置或运行位置已变化，请核对原任务")
	}
	return nil
}

func updateResultMessage(state State) string {
	if state.Status == StatusLoaded {
		message := "模组已更新，原本运行的世界已重启"
		if state.CheckSummary != nil && state.CheckSummary.Unknown > 0 {
			message += fmt.Sprintf("；另有 %d 个模组版本尚未确认", state.CheckSummary.Unknown)
		}
		return message
	}
	return state.CheckSummary.Message()
}
