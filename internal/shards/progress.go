package shards

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"dont/internal/jobs"
	"dont/internal/operationprogress"
	"dont/shared"
)

// RunWithWorldProgress accumulates the existing per-world observations for a
// job. It adds no runtime reads and keeps concurrent worlds independent.
func RunWithWorldProgress(ctx context.Context, action Action, targets []jobs.TargetSpec, run jobs.Runner, report func(jobs.TargetResult)) error {
	if !operationprogress.Enabled(ctx) || (action != ActionStart && action != ActionRestart) || len(targets) == 0 {
		return run(ctx, report)
	}
	var mu sync.Mutex
	progress := make(map[string]shared.WorldOperationProgress, len(targets))
	for _, target := range targets {
		progress[target.ID] = shared.WorldOperationProgress{WorldID: target.ID, Name: target.Name, Stage: "queued", Message: "等待执行"}
	}
	emit := func() {
		ready, percent := 0, 0
		worlds := make([]shared.WorldOperationProgress, 0, len(targets))
		for _, target := range targets {
			world := progress[target.ID]
			if world.Stage == "ready" {
				ready++
			}
			percent += world.Percent
			worlds = append(worlds, world)
		}
		operationprogress.Report(ctx, operationprogress.Update{
			Stage: "world." + string(action), Percent: min(99, percent/len(targets)), Worlds: worlds,
			Message: fmt.Sprintf("已就绪 %d/%d 个世界", ready, len(targets)),
		})
	}
	emit()
	runCtx := operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
		mu.Lock()
		defer mu.Unlock()
		if len(update.Worlds) == 0 {
			operationprogress.Report(ctx, update)
			return
		}
		changed := false
		for _, world := range update.Worlds {
			if previous, ok := progress[world.WorldID]; ok {
				// Batch targets already carry a readable room/world name.
				world.Name = previous.Name
				if world != previous {
					progress[world.WorldID], changed = world, true
				}
			}
		}
		if changed {
			emit()
		}
	})
	err := run(runCtx, func(result jobs.TargetResult) {
		mu.Lock()
		if world, ok := progress[result.TargetID]; ok {
			previous := world
			if result.Status == jobs.StatusSucceeded && world.Stage != "unconfirmed" {
				world.Stage, world.Percent, world.Message = "ready", 100, "世界已就绪"
			} else if result.Status != jobs.StatusSucceeded {
				world.Stage, world.Message = "failed", result.Message
				if result.Status == jobs.StatusCanceled {
					world.Stage = "canceled"
				}
				if result.Error != nil {
					world.Message = result.Error.Message
				}
			}
			if world != previous {
				progress[result.TargetID] = world
				emit()
			}
		}
		report(result)
		mu.Unlock()
	})
	if err != nil {
		mu.Lock()
		for id, world := range progress {
			if world.Stage != "ready" && world.Stage != "failed" && world.Stage != "canceled" {
				world.Stage, world.Message = "unconfirmed", err.Error()
				if errors.Is(err, context.Canceled) {
					world.Stage = "canceled"
				}
				progress[id] = world
			}
		}
		emit()
		mu.Unlock()
	}
	return err
}

// StartupProgressReporter consumes the existing readiness observation. It adds
// no reads/timers and emits only forward stage changes during this startup.
func StartupProgressReporter(ctx context.Context) func(RuntimeStatus) {
	last := 0
	return func(status RuntimeStatus) {
		if !operationprogress.Enabled(ctx) || status.State != RuntimeStarting {
			return
		}
		percent, message := startupMilestone(status.StartupStage)
		if percent <= last {
			return
		}
		last = percent
		operationprogress.Report(ctx, operationprogress.Update{Stage: "world.start." + status.StartupStage, Percent: percent, Message: message})
	}
}

func startupMilestone(stage string) (int, string) {
	switch stage {
	case "initializing":
		return 40, "正在初始化游戏"
	case "loading_mods":
		return 55, "正在加载模组"
	case "generating_world":
		return 65, "正在生成世界"
	case "loading_world":
		return 80, "正在加载世界存档"
	case "connecting":
		return 95, "正在连接分片与注册服务器"
	default:
		return 0, ""
	}
}

func reportStartupResult(ctx context.Context, action Action, status shared.ShardRuntimeStatus) {
	if action != ActionStart || !operationprogress.Enabled(ctx) {
		return
	}
	stage := "unconfirmed"
	if status.State == "running" {
		stage = "ready"
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: "world.start." + stage})
}

func (o *Operations) executeWorldWithProgress(ctx context.Context, action Action, execute func(context.Context) worldExecutionResult, world shared.WorldOperationProgress) worldExecutionResult {
	if !operationprogress.Enabled(ctx) || action != ActionStart && action != ActionStop {
		return execute(ctx)
	}
	parent := ctx
	ready := o.placements == nil
	emit := func() {
		operationprogress.Report(parent, operationprogress.Update{Stage: "world." + string(action), Message: world.Name + " · " + world.Message, Worlds: []shared.WorldOperationProgress{world}})
	}
	world.Stage, world.Percent, world.Message = "stopping", 5, "正在停止旧进程"
	if action == ActionStart {
		world.Stage, world.Percent, world.Message = "starting", 25, "正在启动，等待游戏日志"
	}
	emit()
	ctx = operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
		if !strings.HasPrefix(update.Stage, "world.start.") {
			return
		}
		stage := strings.TrimPrefix(update.Stage, "world.start.")
		if stage == "ready" || stage == "unconfirmed" {
			ready = stage == "ready"
			return
		}
		percent, message := startupMilestone(stage)
		if action == ActionStart && percent > world.Percent {
			world.Stage, world.Percent, world.Message = stage, percent, message
			emit()
		}
	})
	result := execute(ctx)
	if result.err != nil {
		world.Stage, world.Message = "failed", result.err.Error()
	} else if action == ActionStop {
		world.Stage, world.Percent, world.Message = "stopped", 20, "旧进程已停止"
	} else if ready {
		world.Stage, world.Percent, world.Message = "ready", 100, "世界已就绪"
	} else {
		world.Stage, world.Message = "unconfirmed", "进程已启动，尚未确认世界就绪；可查看世界状态和日志"
	}
	emit()
	return result
}
