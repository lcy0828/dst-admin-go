package modupdates

import (
	"context"
	"errors"
	"fmt"

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
	Plan(shards.Action, string, []string) ([]jobs.TargetSpec, jobs.Runner, error)
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
		return err
	}
	ids := make([]string, 0, len(worlds))
	for _, world := range worlds {
		status, err := r.operations.StatusFor(ctx, roomID, world.ID)
		if err != nil {
			return fmt.Errorf("读取 %s 运行状态: %w", world.Name, err)
		}
		if status.State == shards.RuntimeUnknown {
			return fmt.Errorf("%s 运行状态未知，未执行重启", world.Name)
		}
		if status.SessionExists || status.State == shards.RuntimeRunning || status.State == shards.RuntimeStarting {
			ids = append(ids, world.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	targets, run, err := r.operations.Plan(shards.ActionRestart, roomID, ids)
	if err != nil {
		return err
	}
	ctx = shards.WithOperationAudit(ctx, shards.OperationAuditMetadata{JobID: jobID, Source: "mod.update"})
	results := make(map[string]jobs.TargetResult)
	runCtx := operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
		if len(update.Worlds) == 0 {
			return
		}
		update.Percent = 80 + update.Percent*19/99
		operationprogress.Report(ctx, update)
	})
	err = shards.RunWithWorldProgress(runCtx, shards.ActionRestart, targets, run, func(result jobs.TargetResult) {
		results[result.TargetID] = result
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, target := range targets {
		result, ok := results[target.ID]
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
	return errors.Join(failures...)
}
