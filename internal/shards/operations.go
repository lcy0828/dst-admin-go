package shards

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"dont/internal/jobs"
	"dont/internal/rooms"
)

var (
	ErrRoomNotManaged = errors.New("room must be adopted before it can be controlled")
	ErrNoWorlds       = errors.New("room has no controllable worlds")
	ErrUnknownAction  = errors.New("unknown room action")
	ErrUnsafeName     = errors.New("room or world name cannot be represented safely by tmux")
	controlName       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
)

type RuntimeState string

const (
	RuntimeStopped  RuntimeState = "stopped"
	RuntimeStarting RuntimeState = "starting"
	RuntimeRunning  RuntimeState = "running"
	RuntimeFailed   RuntimeState = "failed"
	RuntimeUnknown  RuntimeState = "unknown"
)

type RuntimeStatus struct {
	State         RuntimeState
	Code          string
	Message       string
	SessionExists bool
}

type Control interface {
	IsRunning(context.Context, string, string) (bool, error)
	Start(context.Context, string, string) error
	Stop(context.Context, string, string) error
}

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}

type Operations struct {
	rooms        RoomCatalog
	control      Control
	pollInterval time.Duration
	startTimeout time.Duration
	stopTimeout  time.Duration
}

func NewOperations(roomCatalog RoomCatalog, control Control) *Operations {
	return &Operations{
		rooms: roomCatalog, control: control,
		pollInterval: 500 * time.Millisecond, startTimeout: 2 * time.Minute, stopTimeout: 60 * time.Second,
	}
}

func (o *Operations) Plan(action Action, roomID string, selectedWorldIDs []string) ([]jobs.TargetSpec, jobs.Runner, error) {
	if action != ActionStart && action != ActionStop && action != ActionRestart {
		return nil, nil, ErrUnknownAction
	}
	room, err := o.rooms.Room(roomID)
	if err != nil {
		return nil, nil, err
	}
	if !room.Managed {
		return nil, nil, ErrRoomNotManaged
	}
	if !controlName.MatchString(room.DirectoryName) {
		return nil, nil, ErrUnsafeName
	}
	worlds, err := o.rooms.Worlds(roomID)
	if err != nil {
		return nil, nil, err
	}
	worlds, err = selectWorlds(worlds, selectedWorldIDs)
	if err != nil {
		return nil, nil, err
	}
	if len(worlds) == 0 {
		return nil, nil, ErrNoWorlds
	}
	for _, world := range worlds {
		if !controlName.MatchString(world.DirectoryName) {
			return nil, nil, fmt.Errorf("%w: %s", ErrUnsafeName, world.DirectoryName)
		}
	}
	orderWorlds(worlds, action)
	targets := make([]jobs.TargetSpec, 0, len(worlds))
	for _, world := range worlds {
		targets = append(targets, jobs.TargetSpec{ID: world.ID, Name: world.Name})
	}
	runner := func(ctx context.Context, report func(jobs.TargetResult)) error {
		for _, world := range worlds {
			if err := ctx.Err(); err != nil {
				report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusCanceled, Error: &jobs.Error{Code: "JOB_CANCELED", Message: "任务已取消"}})
				continue
			}
			message, err := o.execute(ctx, action, room.DirectoryName, world.DirectoryName)
			if err != nil {
				code := strings.ToUpper(string(action)) + "_FAILED"
				if errors.Is(err, context.Canceled) {
					report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusCanceled, Error: &jobs.Error{Code: "JOB_CANCELED", Message: "任务已取消"}})
					continue
				}
				report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusFailed, Error: &jobs.Error{Code: code, Message: err.Error()}})
				continue
			}
			report(jobs.TargetResult{TargetID: world.ID, Status: jobs.StatusSucceeded, Message: message})
		}
		return nil
	}
	return targets, runner, nil
}

func (o *Operations) IsRunning(ctx context.Context, roomName, worldName string) (bool, error) {
	status, err := o.Status(ctx, roomName, worldName)
	return status.State == RuntimeRunning, err
}

func (o *Operations) Status(ctx context.Context, roomName, worldName string) (RuntimeStatus, error) {
	if control, ok := o.control.(interface {
		Status(context.Context, string, string) (RuntimeStatus, error)
	}); ok {
		return control.Status(ctx, roomName, worldName)
	}
	running, err := o.control.IsRunning(ctx, roomName, worldName)
	if err != nil {
		return RuntimeStatus{State: RuntimeUnknown}, err
	}
	if running {
		return RuntimeStatus{State: RuntimeRunning, SessionExists: true}, nil
	}
	return RuntimeStatus{State: RuntimeStopped}, nil
}

func (o *Operations) execute(ctx context.Context, action Action, roomName, worldName string) (string, error) {
	status, err := o.Status(ctx, roomName, worldName)
	if err != nil {
		return "", fmt.Errorf("检查分片状态: %w", err)
	}
	switch action {
	case ActionStart:
		if status.State == RuntimeRunning {
			return "分片已在运行", nil
		}
		if status.State != RuntimeStarting {
			if err := o.control.Start(ctx, roomName, worldName); err != nil {
				return "", err
			}
		}
		if err := o.waitFor(ctx, roomName, worldName, true, o.startTimeout); err != nil {
			return "", err
		}
		return "分片已启动", nil
	case ActionStop:
		if !status.SessionExists && status.State == RuntimeStopped {
			return "分片已停止", nil
		}
		if err := o.control.Stop(ctx, roomName, worldName); err != nil {
			return "", err
		}
		if err := o.waitFor(ctx, roomName, worldName, false, o.stopTimeout); err != nil {
			return "", err
		}
		return "分片已停止", nil
	case ActionRestart:
		if status.SessionExists || status.State != RuntimeStopped {
			if err := o.control.Stop(ctx, roomName, worldName); err != nil {
				return "", err
			}
			if err := o.waitFor(ctx, roomName, worldName, false, o.stopTimeout); err != nil {
				return "", err
			}
		}
		if err := o.control.Start(ctx, roomName, worldName); err != nil {
			return "", err
		}
		if err := o.waitFor(ctx, roomName, worldName, true, o.startTimeout); err != nil {
			return "", err
		}
		return "分片已重启", nil
	default:
		return "", ErrUnknownAction
	}
}

func (o *Operations) waitFor(ctx context.Context, roomName, worldName string, expected bool, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(o.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			state := "启动"
			if !expected {
				state = "停止"
			}
			message := fmt.Sprintf("等待分片%s超时", state)
			if expected {
				return errors.New(o.cleanupFailedStart(ctx, roomName, worldName, message))
			}
			return errors.New(message)
		case <-ticker.C:
			status, err := o.Status(ctx, roomName, worldName)
			if err != nil {
				return err
			}
			if expected && status.State == RuntimeFailed {
				if status.Message == "" {
					status.Message = "DST 启动失败，请检查分片日志"
				}
				return errors.New(o.cleanupFailedStart(ctx, roomName, worldName, status.Message))
			}
			if expected && status.State == RuntimeRunning {
				return nil
			}
			if !expected && status.State == RuntimeStopped && !status.SessionExists {
				return nil
			}
		}
	}
}

func (o *Operations) cleanupFailedStart(ctx context.Context, roomName, worldName, message string) string {
	if err := o.control.Stop(ctx, roomName, worldName); err != nil {
		return fmt.Sprintf("%s；清理失败启动会话时出错: %v", message, err)
	}
	return message
}

func selectWorlds(worlds []rooms.World, selected []string) ([]rooms.World, error) {
	if len(selected) == 0 {
		return worlds, nil
	}
	wanted := make(map[string]bool, len(selected))
	for _, id := range selected {
		if wanted[id] {
			return nil, fmt.Errorf("duplicate world id: %s", id)
		}
		wanted[id] = true
	}
	result := make([]rooms.World, 0, len(wanted))
	for _, world := range worlds {
		if wanted[world.ID] {
			result = append(result, world)
			delete(wanted, world.ID)
		}
	}
	if len(wanted) > 0 {
		return nil, rooms.ErrWorldNotFound
	}
	return result, nil
}

func orderWorlds(worlds []rooms.World, action Action) {
	sort.SliceStable(worlds, func(i, j int) bool {
		leftMaster := worlds[i].Role == rooms.WorldRoleMaster
		rightMaster := worlds[j].Role == rooms.WorldRoleMaster
		if leftMaster == rightMaster {
			return strings.ToLower(worlds[i].Name) < strings.ToLower(worlds[j].Name)
		}
		if action == ActionStop {
			return !leftMaster
		}
		return leftMaster
	})
}
