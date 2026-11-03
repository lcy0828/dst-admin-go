package modupdates

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"dont/internal/jobs"
	"dont/internal/operationprogress"
	"dont/internal/rooms"
	"dont/internal/shards"
)

type worldCatalog interface {
	Worlds(string) ([]rooms.World, error)
}
type worldOperations interface {
	StatusFor(context.Context, string, string) (shards.RuntimeStatus, error)
	PlanWithOptions(shards.Action, string, []string, shards.PlanOptions) ([]jobs.TargetSpec, jobs.Runner, error)
}

var ErrRestartNotStarted = errors.New("世界重启尚未开始")
var ErrRestartIncomplete = errors.New("世界重启未完成")
var ErrRestartUncertain = errors.New("世界重启结果尚未确认")

type RestartWorld struct {
	WorldID string `json:"worldId"`
	Status  string `json:"status"`
}
type restartTrackingKey struct{}
type restartTracking struct {
	previous []RestartWorld
	record   func([]RestartWorld) error
}

type WorldRestarter struct {
	rooms      worldCatalog
	operations worldOperations
}

func NewWorldRestarter(catalog worldCatalog, operations worldOperations) *WorldRestarter {
	return &WorldRestarter{rooms: catalog, operations: operations}
}

// Reuse ordinary room restart semantics, including shard ordering, operation
// errors and readiness. Stopped worlds are never added to the restart plan.
func (r *WorldRestarter) RestartRunningWorlds(ctx context.Context, roomID, jobID string) error {
	worlds, err := r.rooms.Worlds(roomID)
	if err != nil {
		return errors.Join(ErrRestartNotStarted, err)
	}
	tracking, _ := ctx.Value(restartTrackingKey{}).(restartTracking)
	states := append([]RestartWorld{}, tracking.previous...)
	ids := make([]string, 0, len(worlds))
	if len(states) > 0 {
		for _, item := range states {
			if item.Status == "ready" {
				continue
			}
			if item.Status != "failed" && item.Status != "pending" {
				return ErrRestartUncertain
			}
			exists := false
			for _, world := range worlds {
				if world.ID == item.WorldID {
					exists = true
				}
			}
			if !exists {
				return errors.Join(ErrRestartNotStarted, errors.New("待恢复的世界已变化"))
			}
			ids = append(ids, item.WorldID)
		}
	} else {
		for _, world := range worlds {
			status, err := r.operations.StatusFor(ctx, roomID, world.ID)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrRestartNotStarted, fmt.Errorf("读取 %s 运行状态: %w", world.Name, err))
			}
			if status.State == shards.RuntimeUnknown {
				return fmt.Errorf("%w: %v", ErrRestartNotStarted, fmt.Errorf("%s 运行状态未知，未执行重启", world.Name))
			}
			if status.SessionExists || status.State == shards.RuntimeRunning || status.State == shards.RuntimeStarting {
				ids = append(ids, world.ID)
			}
		}

		for _, id := range ids {
			states = append(states, RestartWorld{WorldID: id, Status: "pending"})
		}
	}
	if len(ids) == 0 {
		return nil
	}
	targets, run, err := r.operations.PlanWithOptions(shards.ActionRestart, roomID, ids, shards.PlanOptions{PreserveRuntimeMode: true})
	if err != nil {
		return errors.Join(ErrRestartNotStarted, err)
	}
	for i := range states {
		if states[i].Status != "ready" {
			states[i].Status = "running"
		}
	}
	if tracking.record != nil {
		if err := tracking.record(states); err != nil {
			return errors.Join(ErrRestartNotStarted, err)
		}
	}
	ctx = shards.WithOperationAudit(ctx, shards.OperationAuditMetadata{JobID: jobID, Source: "mod.update"})
	var resultMu sync.Mutex
	var checkpointErr error
	results := make(map[string]jobs.TargetResult)
	unconfirmed := make(map[string]bool)
	runCtx := operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
		if len(update.Worlds) == 0 {
			return
		}
		resultMu.Lock()
		for _, world := range update.Worlds {
			unconfirmed[world.WorldID] = world.Stage == "unconfirmed"
		}
		resultMu.Unlock()
		update.Percent = 80 + update.Percent*19/99
		operationprogress.Report(ctx, update)
	})
	err = shards.RunWithWorldProgress(runCtx, shards.ActionRestart, targets, run, func(result jobs.TargetResult) {
		resultMu.Lock()
		results[result.TargetID] = result
		for i := range states {
			if states[i].WorldID != result.TargetID {
				continue
			}
			switch {
			case unconfirmed[result.TargetID]:
				states[i].Status = "uncertain"
			case result.Status == jobs.StatusSucceeded:
				states[i].Status = "ready"
			case result.Status == jobs.StatusFailed:
				states[i].Status = "failed"
			default:
				states[i].Status = "uncertain"
			}
		}
		if tracking.record != nil {
			checkpointErr = errors.Join(checkpointErr, tracking.record(append([]RestartWorld{}, states...)))
		}
		resultMu.Unlock()
	})
	if err != nil || checkpointErr != nil {
		return errors.Join(ErrRestartUncertain, err, checkpointErr)
	}
	for _, pending := range unconfirmed {
		if pending {
			return ErrRestartUncertain
		}
	}
	var failures []error
	for _, target := range targets {
		result, ok := results[target.ID]
		if !ok || result.Status != jobs.StatusSucceeded && result.Status != jobs.StatusFailed {
			return ErrRestartUncertain
		}
		if ok && result.Status == jobs.StatusSucceeded {
			continue
		}
		message := "重启未完成"
		if result.Error != nil {
			message = result.Error.Message
		} else if result.Message != "" {
			message = result.Message
		}
		failures = append(failures, fmt.Errorf("%s: %s", target.Name, message))
	}
	if len(failures) > 0 {
		return errors.Join(append([]error{ErrRestartIncomplete}, failures...)...)
	}
	return nil
}
