// Package roomreset resets every world of one Room through a persistent job.
//
// A protection backup is created first unless the caller explicitly opts out.
// A failed backup cancels the reset: the save is never regenerated without the
// backup the caller asked for.
package roomreset

import (
	"context"
	"errors"
	"strings"
	"time"

	backupapi "dont/internal/backups"
	consoleapi "dont/internal/console"
	"dont/internal/jobs"
	"dont/internal/rooms"
)

const (
	JobKind        = "room.regenerate"
	backupTarget   = "backup"
	resetTarget    = "regenerate"
	regenerateID   = "regenerate"
	backupNameBase = "重置世界前保护备份 "
)

var (
	ErrConfirmation = errors.New("room reset confirmation does not match the room name")
	ErrNoMaster     = errors.New("room has no master world")
)

type Rooms interface {
	Room(roomID string) (rooms.Room, error)
	Worlds(roomID string) ([]rooms.World, error)
}

type Backups interface {
	Create(ctx context.Context, roomID, name string, kind backupapi.Kind, sourceJobID string) (backupapi.Backup, error)
}

type Console interface {
	ExecuteWorkbench(ctx context.Context, room, world, id string, args map[string]interface{}, confirmed bool) (consoleapi.WorkbenchResult, error)
}

type Jobs interface {
	SubmitFactory(kind, roomID, worldID string, targets []jobs.TargetSpec, factory func(jobs.Job) jobs.Runner) (jobs.Job, error)
}

// Request is the public input. Backup defaults to true when omitted.
type Request struct {
	Backup       *bool  `json:"backup,omitempty"`
	Confirmation string `json:"confirmation"`
}

func (r Request) WantsBackup() bool { return r.Backup == nil || *r.Backup }

type Service struct {
	rooms   Rooms
	backups Backups
	console Console
	jobs    Jobs
	now     func() time.Time
}

func New(roomService Rooms, backupService Backups, consoleService Console, jobService Jobs) *Service {
	return &Service{rooms: roomService, backups: backupService, console: consoleService, jobs: jobService, now: time.Now}
}

// Submit validates synchronously and returns the accepted job. Validation
// failures never create a job, so nothing was executed.
func (s *Service) Submit(roomID string, request Request) (jobs.Job, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return jobs.Job{}, err
	}
	if strings.TrimSpace(request.Confirmation) != room.Name {
		return jobs.Job{}, ErrConfirmation
	}
	worlds, err := s.rooms.Worlds(roomID)
	if err != nil {
		return jobs.Job{}, err
	}
	master := ""
	for _, world := range worlds {
		if world.IsMaster {
			master = world.ID
			break
		}
	}
	if master == "" {
		return jobs.Job{}, ErrNoMaster
	}
	backup := request.WantsBackup()
	targets := []jobs.TargetSpec{}
	if backup {
		targets = append(targets, jobs.TargetSpec{ID: backupTarget, Name: "重置前备份"})
	}
	targets = append(targets, jobs.TargetSpec{ID: resetTarget, Name: "重置全部世界"})
	return s.jobs.SubmitFactory(JobKind, roomID, master, targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			s.run(ctx, job.ID, roomID, master, backup, report)
			return nil
		}
	})
}

func (s *Service) run(ctx context.Context, jobID, roomID, master string, backup bool, report func(jobs.TargetResult)) {
	note := "未备份（按要求跳过）"
	if backup {
		value, err := s.backups.Create(ctx, roomID, backupNameBase+s.now().Format("2006-01-02 15:04:05"), backupapi.KindProtection, jobID)
		if err != nil {
			report(jobs.TargetResult{TargetID: backupTarget, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "BACKUP_CREATE_FAILED", Message: err.Error()}})
			report(jobs.TargetResult{TargetID: resetTarget, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "RESET_SKIPPED_BACKUP_FAILED", Message: "备份失败，已取消重置，世界未改动"}})
			return
		}
		note = "重置前备份：" + value.Name
		report(jobs.TargetResult{TargetID: backupTarget, Status: jobs.StatusSucceeded, Message: "备份已创建：" + value.Name})
	}
	result, err := s.console.ExecuteWorkbench(ctx, roomID, master, regenerateID, map[string]interface{}{}, true)
	switch {
	case err != nil:
		report(jobs.TargetResult{TargetID: resetTarget, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "RESET_FAILED", Message: err.Error() + "；" + note}})
	case result.Status == consoleapi.RunSucceeded && result.Complete:
		report(jobs.TargetResult{TargetID: resetTarget, Status: jobs.StatusSucceeded, Message: "已重置全部世界；" + note})
	case result.Status == consoleapi.RunFailed:
		report(jobs.TargetResult{TargetID: resetTarget, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "RESET_FAILED", Message: firstNonEmpty(result.Message, "重置指令执行失败") + "；" + note}})
	default:
		// The command may have reached the game. Never report success, and
		// never ask the caller to retry blindly.
		report(jobs.TargetResult{TargetID: resetTarget, Status: jobs.StatusFailed, Error: &jobs.Error{Code: "RESET_UNCONFIRMED", Message: "重置指令已发送但未收到确认回执，请查看世界状态，不要重复提交；" + note, Details: map[string]interface{}{"runId": result.RunID, "mayHaveExecuted": true}}})
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
