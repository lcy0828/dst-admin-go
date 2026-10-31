package automation

import (
	"fmt"
	"time"

	"dont/internal/backups"
	"dont/internal/distributedbackup"
	"dont/internal/rooms"
	"github.com/google/uuid"
)

type BackupPolicyManager interface {
	Policy(string) (backups.Policy, error)
	SavePolicy(string, backups.PolicyRequest) (backups.Policy, error)
}

type BackupScheduleDefaults struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Timezone string `json:"timezone"`
	Keep     int    `json:"keep"`
	Mode     string `json:"mode"`
}

func DefaultBackupSchedule() BackupScheduleDefaults {
	return BackupScheduleDefaults{Name: "房间定时备份", Schedule: "0 4 * * *", Timezone: "Asia/Shanghai", Keep: 7, Mode: distributedbackup.ModeAutomatic}
}

// Omitted fields preserve the selected plan. Defaults only apply to creation.
type BackupScheduleInput struct {
	TaskID           string  `json:"taskId,omitempty"`
	CreateNew        bool    `json:"createNew,omitempty"`
	ExpectedRevision string  `json:"expectedRevision,omitempty"`
	Enabled          *bool   `json:"enabled,omitempty"`
	Name             *string `json:"name,omitempty"`
	Schedule         *string `json:"schedule,omitempty"`
	Timezone         *string `json:"timezone,omitempty"`
	Keep             *int    `json:"keep,omitempty"`
	Mode             *string `json:"mode,omitempty"`
	ClearRetention   bool    `json:"clearRetention,omitempty"`
}

type BackupScheduleResult struct {
	Source           string          `json:"source"`
	EffectiveEnabled bool            `json:"effectiveEnabled"`
	Created          bool            `json:"created"`
	Task             *Task           `json:"task,omitempty"`
	Policy           *backups.Policy `json:"policy,omitempty"`
	NextRunAt        *time.Time      `json:"nextRunAt,omitempty"`
	NextRunLocal     string          `json:"nextRunLocal,omitempty"`
}

type backupScheduleRoom struct{ room rooms.Room }

func (c backupScheduleRoom) Room(id string) (rooms.Room, error) {
	if id != c.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return c.room, nil
}

func (s *Service) ConfigureBackupSchedule(roomID string, input BackupScheduleInput, legacy BackupPolicyManager) (BackupScheduleResult, error) {
	s.defaultsMu.Lock()
	defer s.defaultsMu.Unlock()
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	if input.CreateNew && input.TaskID != "" || input.ClearRetention && input.Keep != nil || input.Keep != nil && (*input.Keep < 1 || *input.Keep > 100) {
		return BackupScheduleResult{}, ErrInvalidInput
	}
	if input.Mode != nil && !validBackupMode(*input.Mode) {
		return BackupScheduleResult{}, &FieldError{Fields: map[string]string{"mode": "请选择自动、不停服或停服后备份"}}
	}
	tasks, err := s.Tasks(roomID)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	var candidates []Task
	for _, task := range tasks {
		if task.Action == ActionBackupCreate {
			candidates = append(candidates, task)
		}
	}
	if legacy != nil {
		policy, err := legacy.Policy(roomID)
		if err != nil {
			return BackupScheduleResult{}, err
		}
		if policy.Enabled && input.TaskID == "" && !input.CreateNew {
			if len(candidates) > 0 {
				return BackupScheduleResult{}, &FieldError{Fields: map[string]string{"taskId": "存在旧快照策略和定时备份任务，请选择要修改的计划"}}
			}
			if input.Mode != nil {
				return BackupScheduleResult{}, &FieldError{Fields: map[string]string{"mode": "旧计划不支持选择备份方式，请先暂停旧计划，再添加定时备份"}}
			}
			if input.Schedule != nil || input.Timezone != nil || input.Name != nil || input.ClearRetention {
				return BackupScheduleResult{}, &FieldError{Fields: map[string]string{"schedule": "当前使用旧间隔快照策略；修改 Cron 前请先暂停旧策略，再创建定时计划"}}
			}
			changed := false
			if input.Enabled != nil && policy.Enabled != *input.Enabled {
				policy.Enabled = *input.Enabled
				changed = true
			}
			if input.Keep != nil && policy.MaxSnapshots != *input.Keep {
				policy.MaxSnapshots = *input.Keep
				changed = true
			}
			if changed {
				policy, err = legacy.SavePolicy(roomID, backups.PolicyRequest{Enabled: policy.Enabled, IntervalMinute: policy.IntervalMinute, MaxSnapshots: policy.MaxSnapshots, NextRunAt: policy.NextRunAt})
				if err != nil {
					return BackupScheduleResult{}, err
				}
			}
			return BackupScheduleResult{Source: "legacy", Policy: &policy, EffectiveEnabled: policy.Enabled, NextRunAt: policy.NextRunAt}, nil
		}
	}
	if len(candidates) == 0 && input.TaskID == "" && !input.CreateNew && input.Enabled != nil && !*input.Enabled {
		return BackupScheduleResult{Source: "automation", EffectiveEnabled: false}, nil
	}
	// The group and task are one database transaction. Invalid schedules and
	// revision conflicts cannot leave an enabled orphan group behind.
	tx := s.store.db.Begin()
	if tx.Error != nil {
		return BackupScheduleResult{}, tx.Error
	}
	defer tx.Rollback()
	store := *s.store
	store.db = tx
	child := &Service{rooms: backupScheduleRoom{room}, store: &store, jobs: s.jobs, executor: s.executor, now: s.now, active: map[string]bool{}}
	groups, err := child.Groups(roomID)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	tasks, err = child.Tasks(roomID)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	candidates = nil
	for _, task := range tasks {
		if task.Action == ActionBackupCreate {
			candidates = append(candidates, task)
		}
	}
	var selected *Task
	if input.TaskID != "" {
		for i := range candidates {
			if candidates[i].ID == input.TaskID {
				selected = &candidates[i]
				break
			}
		}
		if selected == nil {
			return BackupScheduleResult{}, ErrTaskNotFound
		}
	} else if !input.CreateNew {
		if len(candidates) == 1 {
			selected = &candidates[0]
		} else if len(candidates) > 1 {
			return BackupScheduleResult{}, &FieldError{Fields: map[string]string{"taskId": "存在多个备份计划，请先列出计划并指定 taskId"}}
		}
	}
	defaults := DefaultBackupSchedule()
	value := TaskInput{Name: defaults.Name, Enabled: true, Schedule: defaults.Schedule, Timezone: defaults.Timezone, Action: ActionBackupCreate, Parameters: map[string]any{"keep": defaults.Keep, "mode": defaults.Mode}, TimeoutSeconds: 600, RetryInterval: 60}
	if selected != nil {
		if input.ExpectedRevision != "" && input.ExpectedRevision != selected.Revision {
			return BackupScheduleResult{}, ErrRevisionConflict
		}
		value = TaskInput{GroupID: selected.GroupID, Name: selected.Name, Description: selected.Description, Enabled: selected.Enabled, Schedule: selected.Schedule, Timezone: selected.Timezone, Action: ActionBackupCreate, WorldIDs: selected.WorldIDs, Parameters: selected.Parameters, TimeoutSeconds: selected.TimeoutSeconds, RetryTimes: selected.RetryTimes, RetryInterval: selected.RetryInterval, Dependencies: selected.Dependencies, ExpectedRevision: selected.Revision}
	}
	if input.Enabled != nil {
		value.Enabled = *input.Enabled
	}
	if input.Name != nil {
		value.Name = *input.Name
	}
	if input.Schedule != nil {
		value.Schedule = *input.Schedule
	}
	if input.Timezone != nil {
		value.Timezone = *input.Timezone
	}
	if input.Keep != nil {
		if value.Parameters == nil {
			value.Parameters = map[string]any{}
		}
		value.Parameters["keep"] = *input.Keep
	}
	if input.Mode != nil {
		if value.Parameters == nil {
			value.Parameters = map[string]any{}
		}
		value.Parameters["mode"] = *input.Mode
	}
	if input.ClearRetention {
		delete(value.Parameters, "keep")
	}
	groupEnabled := false
	for _, group := range groups {
		if group.ID == value.GroupID {
			groupEnabled = group.Enabled
		}
	}
	if value.GroupID == "" || value.Enabled && !groupEnabled {
		value.GroupID = ""
		for _, group := range groups {
			if group.Name == "room-backups" && group.Enabled {
				value.GroupID = group.ID
				break
			}
		}
		if value.GroupID == "" {
			name := "room-backups"
			for _, group := range groups {
				if group.Name == name {
					name = "room-backups-" + uuid.NewString()[:8]
					break
				}
			}
			group, err := child.CreateGroup(roomID, GroupInput{Name: name, Type: "custom", Enabled: true})
			if err != nil {
				return BackupScheduleResult{}, err
			}
			value.GroupID = group.ID
		}
	}
	var saved Task
	if selected == nil {
		saved, err = child.CreateTask(roomID, value)
	} else {
		saved, err = child.UpdateTask(roomID, selected.ID, value)
	}
	if err != nil {
		return BackupScheduleResult{}, err
	}
	if err = tx.Commit().Error; err != nil {
		return BackupScheduleResult{}, err
	}
	// Read back after commit so completion represents persisted effective state.
	saved, err = s.Task(roomID, saved.ID)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	group, err := s.store.Group(roomID, saved.GroupID)
	if err != nil {
		return BackupScheduleResult{}, err
	}
	result := BackupScheduleResult{Source: "automation", Task: &saved, Created: selected == nil, EffectiveEnabled: saved.Enabled && group.Enabled}
	if result.EffectiveEnabled {
		result.NextRunAt = saved.NextRunAt
		if saved.NextRunAt != nil {
			zone, err := time.LoadLocation(saved.Timezone)
			if err != nil {
				return BackupScheduleResult{}, fmt.Errorf("invalid persisted time zone: %w", err)
			}
			result.NextRunLocal = saved.NextRunAt.In(zone).Format("2006-01-02 15:04:05 -07:00")
		}
	}
	return result, nil
}
