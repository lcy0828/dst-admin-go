package automation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dont/internal/backups"
	"dont/internal/console"
	"dont/internal/jobs"
	"dont/internal/players"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/internal/structuredlogs"
	"dont/internal/worldstate"
)

type ActionExecutor interface {
	Validate(Task) error
	Execute(context.Context, Task, string) (ExecutionResult, error)
}

type RoomActionPlanner interface {
	Plan(shards.Action, string, []string) ([]jobs.TargetSpec, jobs.Runner, error)
}

type BackupExecutor interface {
	Create(context.Context, string, string, backups.Kind, string) (backups.Backup, error)
	PruneSnapshots(string, int) (int64, int, error)
}

type CommandExecutor interface {
	Definitions() []console.Definition
	Execute(context.Context, string, string, console.ExecuteRequest) (console.Run, error)
}

type PlayerRefresher interface {
	WorldTargets(string) ([]players.WorldTarget, error)
	RefreshWorld(context.Context, string, string) (players.RefreshResult, error)
}

type playerBatchRefresher interface {
	RefreshWorlds(context.Context, string, []string) ([]players.RefreshOutcome, error)
}

type StructuredLogRefresher interface {
	WorldTargets(string) ([]rooms.World, error)
	RefreshWorld(context.Context, string, string) (structuredlogs.RefreshResult, error)
}

type WorldStateRefresher interface {
	WorldTargets(string) ([]rooms.World, error)
	RefreshWorld(context.Context, string, string) (worldstate.RefreshResult, error)
}

type DomainExecutor struct {
	roomActions    RoomActionPlanner
	backups        BackupExecutor
	commands       CommandExecutor
	players        PlayerRefresher
	structuredLogs StructuredLogRefresher
	worldStates    WorldStateRefresher
}

func NewDomainExecutor(roomActions RoomActionPlanner, backupService BackupExecutor, commands CommandExecutor, playerService PlayerRefresher, logs StructuredLogRefresher, states WorldStateRefresher) (*DomainExecutor, error) {
	if roomActions == nil || backupService == nil || commands == nil || playerService == nil || logs == nil || states == nil {
		return nil, errors.New("automation domain executors are required")
	}
	return &DomainExecutor{roomActions: roomActions, backups: backupService, commands: commands, players: playerService, structuredLogs: logs, worldStates: states}, nil
}

func (e *DomainExecutor) Validate(task Task) error {
	switch task.Action {
	case ActionRoomStart, ActionRoomStop, ActionRoomRestart, ActionPlayerRefresh, ActionStructuredLogRefresh, ActionWorldStateRefresh:
		if len(task.Parameters) > 0 {
			return &FieldError{Fields: map[string]string{"parameters": "此动作不接受额外参数"}}
		}
	case ActionBackupCreate:
		if name, exists := task.Parameters["name"]; exists {
			value, ok := name.(string)
			if !ok || len([]rune(strings.TrimSpace(value))) > 80 {
				return &FieldError{Fields: map[string]string{"parameters.name": "备份名称不能超过 80 个字符"}}
			}
		}
	case ActionBackupPrune:
		if _, err := integerParameter(task.Parameters, "keep", 1, 100); err != nil {
			return &FieldError{Fields: map[string]string{"parameters.keep": "保留数量必须在 1-100 之间"}}
		}
	case ActionCommandExecute:
		if len(task.WorldIDs) != 1 {
			return &FieldError{Fields: map[string]string{"worldIds": "参数化命令必须且只能选择一个世界"}}
		}
		commandID, ok := task.Parameters["commandId"].(string)
		if !ok || strings.TrimSpace(commandID) == "" {
			return &FieldError{Fields: map[string]string{"parameters.commandId": "请选择内建命令"}}
		}
		definition, exists := e.commandDefinition(commandID)
		if !exists {
			return &FieldError{Fields: map[string]string{"parameters.commandId": "内建命令不存在"}}
		}
		if definition.Risk == console.RiskHigh || definition.Risk == console.RiskCritical {
			return ErrUnsafeAction
		}
		if arguments, exists := task.Parameters["arguments"]; exists {
			if _, ok := arguments.(map[string]interface{}); !ok {
				return &FieldError{Fields: map[string]string{"parameters.arguments": "命令参数必须是对象"}}
			}
		}
	default:
		return ErrUnsafeAction
	}
	return nil
}

func (e *DomainExecutor) Execute(ctx context.Context, task Task, jobID string) (ExecutionResult, error) {
	if err := e.Validate(task); err != nil {
		return ExecutionResult{}, err
	}
	switch task.Action {
	case ActionRoomStart, ActionRoomStop, ActionRoomRestart:
		return e.executeRoomAction(ctx, task)
	case ActionBackupCreate:
		name, _ := task.Parameters["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			name = "自动化快照 " + time.Now().Format("2006-01-02 15:04")
		}
		backup, err := e.backups.Create(ctx, task.RoomID, name, backups.KindSnapshot, jobID)
		if err != nil {
			return ExecutionResult{}, err
		}
		return ExecutionResult{Message: "已创建快照 " + backup.Name}, nil
	case ActionBackupPrune:
		keep, _ := integerParameter(task.Parameters, "keep", 1, 100)
		_, removed, err := e.backups.PruneSnapshots(task.RoomID, keep)
		if err != nil {
			return ExecutionResult{}, err
		}
		return ExecutionResult{Message: fmt.Sprintf("已清理 %d 个旧快照", removed)}, nil
	case ActionCommandExecute:
		commandID := strings.TrimSpace(task.Parameters["commandId"].(string))
		arguments, _ := task.Parameters["arguments"].(map[string]interface{})
		if arguments == nil {
			arguments = map[string]interface{}{}
		}
		run, err := e.commands.Execute(ctx, task.RoomID, task.WorldIDs[0], console.ExecuteRequest{CommandID: commandID, Arguments: arguments})
		if err != nil {
			return ExecutionResult{}, err
		}
		return ExecutionResult{Message: "已执行 " + run.Name}, nil
	case ActionPlayerRefresh:
		targets, err := e.players.WorldTargets(task.RoomID)
		if err != nil {
			return ExecutionResult{}, err
		}
		available := make([]string, 0, len(targets))
		for _, target := range targets {
			available = append(available, target.ID)
		}
		selected, err := selectWorldIDs(task.WorldIDs, available)
		if err != nil {
			return ExecutionResult{}, err
		}
		if batch, supported := e.players.(playerBatchRefresher); supported {
			outcomes, refreshErr := batch.RefreshWorlds(ctx, task.RoomID, selected)
			if refreshErr != nil {
				return ExecutionResult{}, refreshErr
			}
			messages := make([]string, 0, len(outcomes))
			var resultErr error
			for _, outcome := range outcomes {
				if outcome.Err != nil {
					resultErr = errors.Join(resultErr, outcome.Err)
					continue
				}
				messages = append(messages, outcome.Result.Message)
			}
			return ExecutionResult{Message: strings.Join(messages, "；")}, resultErr
		}
		return executeRefresh(ctx, selected, available, func(worldID string) (string, error) {
			result, refreshErr := e.players.RefreshWorld(ctx, task.RoomID, worldID)
			return result.Message, refreshErr
		})
	case ActionStructuredLogRefresh:
		targets, err := e.structuredLogs.WorldTargets(task.RoomID)
		if err != nil {
			return ExecutionResult{}, err
		}
		available := make([]string, 0, len(targets))
		for _, target := range targets {
			available = append(available, target.ID)
		}
		return executeRefresh(ctx, task.WorldIDs, available, func(worldID string) (string, error) {
			result, refreshErr := e.structuredLogs.RefreshWorld(ctx, task.RoomID, worldID)
			return result.Message, refreshErr
		})
	case ActionWorldStateRefresh:
		targets, err := e.worldStates.WorldTargets(task.RoomID)
		if err != nil {
			return ExecutionResult{}, err
		}
		available := make([]string, 0, len(targets))
		for _, target := range targets {
			available = append(available, target.ID)
		}
		return executeRefresh(ctx, task.WorldIDs, available, func(worldID string) (string, error) {
			result, refreshErr := e.worldStates.RefreshWorld(ctx, task.RoomID, worldID)
			return result.Message, refreshErr
		})
	default:
		return ExecutionResult{}, ErrUnsafeAction
	}
}

func (e *DomainExecutor) executeRoomAction(ctx context.Context, task Task) (ExecutionResult, error) {
	action := shards.Action(strings.TrimPrefix(string(task.Action), "room."))
	targets, runner, err := e.roomActions.Plan(action, task.RoomID, task.WorldIDs)
	if err != nil {
		return ExecutionResult{}, err
	}
	results := make([]jobs.TargetResult, 0, len(targets))
	runnerErr := runner(ctx, func(result jobs.TargetResult) { results = append(results, result) })
	var executionErr error
	for _, result := range results {
		if result.Status == jobs.StatusFailed || result.Status == jobs.StatusCanceled {
			if result.Error != nil {
				executionErr = errors.Join(executionErr, errors.New(result.Error.Message))
			} else {
				executionErr = errors.Join(executionErr, errors.New("分片动作失败"))
			}
		}
	}
	executionErr = errors.Join(executionErr, runnerErr)
	if executionErr != nil {
		return ExecutionResult{}, executionErr
	}
	return ExecutionResult{Message: fmt.Sprintf("已完成 %d 个分片动作", len(results))}, nil
}

func (e *DomainExecutor) commandDefinition(commandID string) (console.Definition, bool) {
	for _, definition := range e.commands.Definitions() {
		if definition.ID == commandID {
			return definition, true
		}
	}
	return console.Definition{}, false
}

func executeRefresh(ctx context.Context, selected, available []string, refresh func(string) (string, error)) (ExecutionResult, error) {
	ids, err := selectWorldIDs(selected, available)
	if err != nil {
		return ExecutionResult{}, err
	}
	messages := make([]string, 0, len(ids))
	var resultErr error
	for _, worldID := range ids {
		if err := ctx.Err(); err != nil {
			return ExecutionResult{}, errors.Join(resultErr, err)
		}
		message, refreshErr := refresh(worldID)
		if refreshErr != nil {
			resultErr = errors.Join(resultErr, refreshErr)
			continue
		}
		if message != "" {
			messages = append(messages, message)
		}
	}
	if resultErr != nil {
		return ExecutionResult{}, resultErr
	}
	return ExecutionResult{Message: strings.Join(messages, "; ")}, nil
}

func selectWorldIDs(selected, available []string) ([]string, error) {
	if len(available) == 0 {
		return nil, rooms.ErrWorldNotFound
	}
	if len(selected) == 0 {
		return append([]string(nil), available...), nil
	}
	allowed := make(map[string]bool, len(available))
	for _, id := range available {
		allowed[id] = true
	}
	seen := make(map[string]bool, len(selected))
	for _, id := range selected {
		if !allowed[id] || seen[id] {
			return nil, rooms.ErrWorldNotFound
		}
		seen[id] = true
	}
	result := append([]string(nil), selected...)
	sort.Strings(result)
	return result, nil
}

func integerParameter(parameters map[string]interface{}, name string, minimum, maximum int) (int, error) {
	value, exists := parameters[name]
	if !exists {
		return 0, ErrInvalidInput
	}
	var integer int
	switch typed := value.(type) {
	case float64:
		integer = int(typed)
		if typed != float64(integer) {
			return 0, ErrInvalidInput
		}
	case int:
		integer = typed
	default:
		return 0, ErrInvalidInput
	}
	if integer < minimum || integer > maximum {
		return 0, ErrInvalidInput
	}
	return integer, nil
}

func ActionDefinitions() []ActionDefinition {
	return []ActionDefinition{
		{ID: ActionRoomStart, Name: "启动分片", Description: "启动选中的分片；未选择时启动全部分片", Parameters: []string{}},
		{ID: ActionRoomStop, Name: "停止分片", Description: "停止选中的分片；未选择时停止全部分片", Parameters: []string{}},
		{ID: ActionRoomRestart, Name: "重启分片", Description: "重启选中的分片；未选择时重启全部分片", Parameters: []string{}},
		{ID: ActionBackupCreate, Name: "创建快照", Description: "创建一致性房间快照", Parameters: []string{"name"}},
		{ID: ActionBackupPrune, Name: "清理快照", Description: "按保留数量清理旧快照", Parameters: []string{"keep"}},
		{ID: ActionCommandExecute, Name: "执行内建命令", Description: "执行低或中风险参数化命令", NeedsWorld: true, Parameters: []string{"commandId", "arguments"}},
		{ID: ActionPlayerRefresh, Name: "刷新玩家", Description: "采样分片玩家状态", Parameters: []string{}},
		{ID: ActionStructuredLogRefresh, Name: "刷新结构化日志", Description: "刷新分片结构化日志快照", Parameters: []string{}},
		{ID: ActionWorldStateRefresh, Name: "刷新世界状态", Description: "采样分片世界状态", Parameters: []string{}},
	}
}
