package automation

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

type groupRecord struct {
	ID          string `gorm:"type:char(36);primary_key"`
	RoomID      string `gorm:"type:varchar(255);not null;unique_index:idx_automation_group_room_name;index"`
	Name        string `gorm:"type:varchar(80);not null;unique_index:idx_automation_group_room_name"`
	Description string `gorm:"type:varchar(300);not null"`
	Type        string `gorm:"type:varchar(20);not null;default:'custom'"`
	Enabled     bool   `gorm:"not null;index"`
	Revision    string `gorm:"type:char(36);not null"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type taskRecord struct {
	ID             string `gorm:"type:char(36);primary_key"`
	RoomID         string `gorm:"type:varchar(255);not null;unique_index:idx_automation_task_room_name;index"`
	GroupID        string `gorm:"type:char(36);not null;index"`
	Name           string `gorm:"type:varchar(80);not null;unique_index:idx_automation_task_room_name"`
	Description    string `gorm:"type:varchar(300);not null"`
	Enabled        bool   `gorm:"not null;index"`
	Schedule       string `gorm:"type:varchar(128);not null"`
	Timezone       string `gorm:"type:varchar(64);not null"`
	Action         string `gorm:"type:varchar(64);not null;index"`
	WorldIDs       string `gorm:"type:text;not null"`
	Parameters     string `gorm:"type:text;not null"`
	TimeoutSeconds int    `gorm:"not null"`
	RetryTimes     int    `gorm:"not null;default:0"`
	RetryInterval  int    `gorm:"not null;default:60"`
	Dependencies   string `gorm:"type:text;not null;default:'[]'"`
	LastRunAt      *time.Time
	LastStatus     string `gorm:"type:varchar(20)"`
	LastJobID      string `gorm:"type:char(36)"`
	Revision       string `gorm:"type:char(36);not null"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type runRecord struct {
	ID         string `gorm:"type:char(36);primary_key"`
	TaskID     string `gorm:"type:char(36);not null;index"`
	TaskName   string `gorm:"type:varchar(80);not null"`
	GroupID    string `gorm:"type:char(36);not null;index"`
	GroupName  string `gorm:"type:varchar(80);not null"`
	RoomID     string `gorm:"type:varchar(255);not null;index"`
	Action     string `gorm:"type:varchar(64);not null;index"`
	Trigger    string `gorm:"type:varchar(20);not null"`
	Status     string `gorm:"type:varchar(20);not null;index"`
	JobID      string `gorm:"type:char(36);index"`
	Output     string `gorm:"type:text"`
	Error      string `gorm:"type:text"`
	StartedAt  *time.Time
	FinishedAt *time.Time
	DurationMs int64
	RetryCount int
	CreatedAt  time.Time `gorm:"not null;index"`
}

type Store struct {
	db          *gorm.DB
	groupsTable string
	tasksTable  string
	runsTable   string
	now         func() time.Time
}

func NewStore(db *gorm.DB, prefix string) *Store {
	prefix = strings.TrimSpace(prefix)
	return &Store{db: db, groupsTable: prefix + "automation_group", tasksTable: prefix + "automation_task", runsTable: prefix + "automation_run", now: time.Now}
}

func (s *Store) Migrate() error {
	for _, migration := range []struct {
		table string
		model interface{}
	}{{s.groupsTable, &groupRecord{}}, {s.tasksTable, &taskRecord{}}, {s.runsTable, &runRecord{}}} {
		if err := s.db.Table(migration.table).AutoMigrate(migration.model).Error; err != nil {
			return fmt.Errorf("migrate %s: %w", migration.table, err)
		}
	}
	return nil
}

func (s *Store) Groups(roomID string) ([]Group, error) {
	var records []groupRecord
	if err := s.db.Table(s.groupsTable).Where("room_id = ?", roomID).Order("name ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	groups := make([]Group, 0, len(records))
	for _, record := range records {
		var count int
		if err := s.db.Table(s.tasksTable).Where("group_id = ?", record.ID).Count(&count).Error; err != nil {
			return nil, err
		}
		groups = append(groups, groupFromRecord(record, count))
	}
	return groups, nil
}

func (s *Store) Group(roomID, groupID string) (Group, error) {
	var record groupRecord
	result := s.db.Table(s.groupsTable).Where("room_id = ? AND id = ?", roomID, groupID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Group{}, ErrGroupNotFound
	}
	if result.Error != nil {
		return Group{}, result.Error
	}
	var count int
	if err := s.db.Table(s.tasksTable).Where("group_id = ?", record.ID).Count(&count).Error; err != nil {
		return Group{}, err
	}
	return groupFromRecord(record, count), nil
}

func (s *Store) CreateGroup(group Group) (Group, error) {
	record := groupRecord{ID: group.ID, RoomID: group.RoomID, Name: group.Name, Description: group.Description, Type: group.Type, Enabled: group.Enabled, Revision: group.Revision, CreatedAt: group.CreatedAt, UpdatedAt: group.UpdatedAt}
	if err := s.db.Table(s.groupsTable).Create(&record).Error; err != nil {
		return Group{}, err
	}
	return groupFromRecord(record, 0), nil
}

func (s *Store) UpdateGroup(group Group, expectedRevision string) (Group, error) {
	result := s.db.Table(s.groupsTable).Where("room_id = ? AND id = ? AND revision = ?", group.RoomID, group.ID, expectedRevision).Updates(map[string]interface{}{
		"name": group.Name, "description": group.Description, "type": group.Type, "enabled": group.Enabled, "revision": group.Revision, "updated_at": group.UpdatedAt,
	})
	if result.Error != nil {
		return Group{}, result.Error
	}
	if result.RowsAffected == 0 {
		if _, err := s.Group(group.RoomID, group.ID); err != nil {
			return Group{}, err
		}
		return Group{}, ErrRevisionConflict
	}
	return s.Group(group.RoomID, group.ID)
}

func (s *Store) DeleteGroup(roomID, groupID string) error {
	var count int
	if err := s.db.Table(s.tasksTable).Where("room_id = ? AND group_id = ?", roomID, groupID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return ErrGroupNotEmpty
	}
	result := s.db.Table(s.groupsTable).Where("room_id = ? AND id = ?", roomID, groupID).Delete(&groupRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrGroupNotFound
	}
	return nil
}

func (s *Store) Tasks(roomID string) ([]Task, error) {
	var records []taskRecord
	if err := s.db.Table(s.tasksTable).Where("room_id = ?", roomID).Order("name ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	return s.tasksFromRecords(records)
}

func (s *Store) ScheduledTasks() ([]Task, error) {
	var records []taskRecord
	if err := s.db.Table(s.tasksTable+" AS task").Select("task.*").Joins("JOIN "+s.groupsTable+" AS grp ON grp.id = task.group_id").Where("task.enabled = ? AND grp.enabled = ?", true, true).Find(&records).Error; err != nil {
		return nil, err
	}
	return s.tasksFromRecords(records)
}

func (s *Store) Task(roomID, taskID string) (Task, error) {
	var record taskRecord
	result := s.db.Table(s.tasksTable).Where("room_id = ? AND id = ?", roomID, taskID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Task{}, ErrTaskNotFound
	}
	if result.Error != nil {
		return Task{}, result.Error
	}
	return s.taskFromRecord(record)
}

func (s *Store) TaskByID(taskID string) (Task, error) {
	var record taskRecord
	result := s.db.Table(s.tasksTable).Where("id = ?", taskID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Task{}, ErrTaskNotFound
	}
	if result.Error != nil {
		return Task{}, result.Error
	}
	return s.taskFromRecord(record)
}

func (s *Store) CreateTask(task Task) (Task, error) {
	record, err := recordFromTask(task)
	if err != nil {
		return Task{}, err
	}
	if err := s.db.Table(s.tasksTable).Create(&record).Error; err != nil {
		return Task{}, err
	}
	return s.taskFromRecord(record)
}

func (s *Store) UpdateTask(task Task, expectedRevision string) (Task, error) {
	record, err := recordFromTask(task)
	if err != nil {
		return Task{}, err
	}
	result := s.db.Table(s.tasksTable).Where("room_id = ? AND id = ? AND revision = ?", task.RoomID, task.ID, expectedRevision).Updates(map[string]interface{}{
		"group_id": record.GroupID, "name": record.Name, "description": record.Description, "enabled": record.Enabled,
		"schedule": record.Schedule, "timezone": record.Timezone, "action": record.Action, "world_ids": record.WorldIDs,
		"parameters": record.Parameters, "timeout_seconds": record.TimeoutSeconds, "retry_times": record.RetryTimes,
		"retry_interval": record.RetryInterval, "dependencies": record.Dependencies, "revision": record.Revision, "updated_at": record.UpdatedAt,
	})
	if result.Error != nil {
		return Task{}, result.Error
	}
	if result.RowsAffected == 0 {
		if _, err := s.Task(task.RoomID, task.ID); err != nil {
			return Task{}, err
		}
		return Task{}, ErrRevisionConflict
	}
	return s.Task(task.RoomID, task.ID)
}

func (s *Store) DeleteTask(roomID, taskID string) error {
	result := s.db.Table(s.tasksTable).Where("room_id = ? AND id = ?", roomID, taskID).Delete(&taskRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrTaskNotFound
	}
	return nil
}

func (s *Store) DeleteRoomTasks(roomID string) error {
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	rollback := func(err error) error {
		tx.Rollback()
		return err
	}
	if err := tx.Table(s.tasksTable).Where("room_id = ?", roomID).Delete(&taskRecord{}).Error; err != nil {
		return rollback(err)
	}
	if err := tx.Table(s.groupsTable).Where("room_id = ?", roomID).Delete(&groupRecord{}).Error; err != nil {
		return rollback(err)
	}
	return tx.Commit().Error
}

func (s *Store) CreateRun(run Run) (Run, error) {
	record := runRecord{ID: run.ID, TaskID: run.TaskID, TaskName: run.TaskName, GroupID: run.GroupID, GroupName: run.GroupName, RoomID: run.RoomID, Action: string(run.Action), Trigger: string(run.Trigger), Status: string(run.Status), JobID: run.JobID, Output: run.Output, Error: run.Error, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, DurationMs: run.DurationMs, RetryCount: run.RetryCount, CreatedAt: run.CreatedAt.UTC()}
	if err := s.db.Table(s.runsTable).Create(&record).Error; err != nil {
		return Run{}, err
	}
	return runFromRecord(record), nil
}

func (s *Store) AttachJob(runID, jobID string) error {
	return s.db.Table(s.runsTable).Where("id = ?", runID).Update("job_id", jobID).Error
}

func (s *Store) StartRun(runID string, startedAt time.Time) error {
	return s.db.Table(s.runsTable).Where("id = ?", runID).Updates(map[string]interface{}{"status": string(RunRunning), "started_at": startedAt.UTC()}).Error
}

func (s *Store) FinishRun(runID string, status RunStatus, output, errorMessage string, retryCount int, finishedAt time.Time) error {
	var record runRecord
	if err := s.db.Table(s.runsTable).Where("id = ?", runID).First(&record).Error; err != nil {
		return err
	}
	start := record.CreatedAt
	if record.StartedAt != nil {
		start = *record.StartedAt
	}
	duration := finishedAt.Sub(start).Milliseconds()
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	if err := tx.Table(s.runsTable).Where("id = ?", runID).Updates(map[string]interface{}{"status": string(status), "output": output, "error": errorMessage, "finished_at": finishedAt.UTC(), "duration_ms": duration, "retry_count": retryCount}).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Table(s.tasksTable).Where("id = ?", record.TaskID).Updates(map[string]interface{}{"last_run_at": finishedAt.UTC(), "last_status": string(status), "last_job_id": record.JobID}).Error; err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit().Error
}

func (s *Store) RecoverRuns() error {
	now := s.now().UTC()
	return s.db.Table(s.runsTable).Where("status IN (?)", []string{string(RunQueued), string(RunRunning)}).Updates(map[string]interface{}{
		"status": string(RunFailed), "error": "服务重启中断了自动化运行", "finished_at": now,
	}).Error
}

func (s *Store) Runs(roomID string, filter RunFilter) (RunList, error) {
	query := s.db.Table(s.runsTable).Where("room_id = ?", roomID)
	if filter.TaskID != "" {
		query = query.Where("task_id = ?", filter.TaskID)
	}
	if filter.GroupID != "" {
		query = query.Where("group_id = ?", filter.GroupID)
	}
	if filter.Status != "" {
		query = query.Where("status = ?", filter.Status)
	}
	if filter.StartAt != nil {
		query = query.Where("created_at >= ?", filter.StartAt.UTC())
	}
	if filter.EndAt != nil {
		query = query.Where("created_at < ?", filter.EndAt.UTC())
	}
	var total int
	if err := query.Count(&total).Error; err != nil {
		return RunList{}, err
	}
	var records []runRecord
	if err := query.Order("created_at DESC, id DESC").Limit(filter.Limit).Offset(filter.Offset).Find(&records).Error; err != nil {
		return RunList{}, err
	}
	items := make([]Run, 0, len(records))
	for _, record := range records {
		items = append(items, runFromRecord(record))
	}
	return RunList{Items: items, Total: total, Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *Store) Run(roomID, runID string) (Run, error) {
	var record runRecord
	result := s.db.Table(s.runsTable).Where("room_id = ? AND id = ?", roomID, runID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Run{}, ErrRunNotFound
	}
	if result.Error != nil {
		return Run{}, result.Error
	}
	return runFromRecord(record), nil
}

func (s *Store) ClearRuns(roomID string, before time.Time, taskID string, status RunStatus) (int, error) {
	query := s.db.Table(s.runsTable).Where("room_id = ? AND created_at < ?", roomID, before.UTC())
	if taskID != "" {
		query = query.Where("task_id = ?", taskID)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	result := query.Delete(&runRecord{})
	return int(result.RowsAffected), result.Error
}

func (s *Store) Stats(roomID string, since time.Time) (Stats, error) {
	var records []runRecord
	if err := s.db.Table(s.runsTable).Where("room_id = ? AND created_at >= ?", roomID, since.UTC()).Order("created_at ASC").Find(&records).Error; err != nil {
		return Stats{}, err
	}
	stats := Stats{}
	daily := make(map[string]*DailyStat)
	var durationTotal int64
	var durationCount int64
	for _, record := range records {
		stats.Total++
		date := record.CreatedAt.UTC().Format("2006-01-02")
		if daily[date] == nil {
			daily[date] = &DailyStat{Date: date}
		}
		switch RunStatus(record.Status) {
		case RunSucceeded:
			stats.Succeeded++
			daily[date].Succeeded++
		case RunFailed:
			stats.Failed++
			daily[date].Failed++
		case RunCanceled:
			stats.Canceled++
			daily[date].Failed++
		case RunSkipped:
			stats.Skipped++
			daily[date].Skipped++
		}
		if record.FinishedAt != nil && record.DurationMs >= 0 {
			durationTotal += record.DurationMs
			durationCount++
		}
	}
	terminal := stats.Succeeded + stats.Failed + stats.Canceled
	if terminal > 0 {
		stats.SuccessRate = float64(stats.Succeeded) / float64(terminal)
	}
	if durationCount > 0 {
		stats.AverageDurationMs = durationTotal / durationCount
	}
	for date := since.UTC(); !date.After(s.now().UTC()); date = date.AddDate(0, 0, 1) {
		key := date.Format("2006-01-02")
		if daily[key] == nil {
			daily[key] = &DailyStat{Date: key}
		}
		stats.Daily = append(stats.Daily, *daily[key])
	}
	return stats, nil
}

func (s *Store) ImportDocument(roomID string, document Document, replace bool, now time.Time) (ImportResult, error) {
	tx := s.db.Begin()
	if tx.Error != nil {
		return ImportResult{}, tx.Error
	}
	rollback := func(err error) (ImportResult, error) {
		tx.Rollback()
		return ImportResult{}, err
	}
	if replace {
		if err := tx.Table(s.tasksTable).Where("room_id = ?", roomID).Delete(&taskRecord{}).Error; err != nil {
			return rollback(err)
		}
		if err := tx.Table(s.groupsTable).Where("room_id = ?", roomID).Delete(&groupRecord{}).Error; err != nil {
			return rollback(err)
		}
	}
	groupIDs := make(map[string]string, len(document.Groups))
	for _, item := range document.Groups {
		id := uuid.NewString()
		groupIDs[item.Key] = id
		record := groupRecord{ID: id, RoomID: roomID, Name: strings.TrimSpace(item.Name), Description: strings.TrimSpace(item.Description), Type: normalizeGroupType(item.Type), Enabled: item.Enabled, Revision: uuid.NewString(), CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
		if err := tx.Table(s.groupsTable).Create(&record).Error; err != nil {
			return rollback(err)
		}
	}
	taskIDs := make(map[string]string, len(document.Tasks))
	for index, item := range document.Tasks {
		key := strings.TrimSpace(item.Key)
		if key == "" {
			key = fmt.Sprintf("task-%d", index)
		}
		taskIDs[key] = uuid.NewString()
	}
	for index, item := range document.Tasks {
		worldIDs, err := json.Marshal(item.WorldIDs)
		if err != nil {
			return rollback(err)
		}
		parameters, err := json.Marshal(item.Parameters)
		if err != nil {
			return rollback(err)
		}
		dependencyIDs := make([]string, 0, len(item.Dependencies))
		for _, dependency := range item.Dependencies {
			dependencyIDs = append(dependencyIDs, taskIDs[dependency])
		}
		dependencies, err := json.Marshal(dependencyIDs)
		if err != nil {
			return rollback(err)
		}
		key := strings.TrimSpace(item.Key)
		if key == "" {
			key = fmt.Sprintf("task-%d", index)
		}
		retryInterval := item.RetryInterval
		if retryInterval == 0 {
			retryInterval = 60
		}
		record := taskRecord{ID: taskIDs[key], RoomID: roomID, GroupID: groupIDs[item.GroupKey], Name: strings.TrimSpace(item.Name), Description: strings.TrimSpace(item.Description), Enabled: item.Enabled, Schedule: strings.Join(strings.Fields(item.Schedule), " "), Timezone: item.Timezone, Action: string(item.Action), WorldIDs: string(worldIDs), Parameters: string(parameters), TimeoutSeconds: item.TimeoutSeconds, RetryTimes: item.RetryTimes, RetryInterval: retryInterval, Dependencies: string(dependencies), Revision: uuid.NewString(), CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
		if err := tx.Table(s.tasksTable).Create(&record).Error; err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit().Error; err != nil {
		return ImportResult{}, err
	}
	return ImportResult{GroupsCreated: len(document.Groups), TasksCreated: len(document.Tasks)}, nil
}

func (s *Store) tasksFromRecords(records []taskRecord) ([]Task, error) {
	items := make([]Task, 0, len(records))
	for _, record := range records {
		task, err := s.taskFromRecord(record)
		if err != nil {
			return nil, err
		}
		items = append(items, task)
	}
	return items, nil
}

func (s *Store) taskFromRecord(record taskRecord) (Task, error) {
	var worldIDs []string
	var parameters map[string]interface{}
	var dependencies []string
	if err := json.Unmarshal([]byte(record.WorldIDs), &worldIDs); err != nil {
		return Task{}, err
	}
	if err := json.Unmarshal([]byte(record.Parameters), &parameters); err != nil {
		return Task{}, err
	}
	if record.Dependencies != "" {
		if err := json.Unmarshal([]byte(record.Dependencies), &dependencies); err != nil {
			return Task{}, err
		}
	}
	var group groupRecord
	if err := s.db.Table(s.groupsTable).Where("id = ?", record.GroupID).First(&group).Error; err != nil {
		return Task{}, err
	}
	return Task{ID: record.ID, RoomID: record.RoomID, GroupID: record.GroupID, GroupName: group.Name, Name: record.Name, Description: record.Description, Enabled: record.Enabled, Schedule: record.Schedule, Timezone: record.Timezone, Action: Action(record.Action), WorldIDs: worldIDs, Parameters: parameters, TimeoutSeconds: record.TimeoutSeconds, RetryTimes: record.RetryTimes, RetryInterval: record.RetryInterval, Dependencies: dependencies, LastRunAt: utcPointer(record.LastRunAt), LastStatus: RunStatus(record.LastStatus), LastJobID: record.LastJobID, Revision: record.Revision, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC()}, nil
}

func recordFromTask(task Task) (taskRecord, error) {
	worldIDs, err := json.Marshal(task.WorldIDs)
	if err != nil {
		return taskRecord{}, err
	}
	parameters, err := json.Marshal(task.Parameters)
	if err != nil {
		return taskRecord{}, err
	}
	dependencies, err := json.Marshal(task.Dependencies)
	if err != nil {
		return taskRecord{}, err
	}
	return taskRecord{ID: task.ID, RoomID: task.RoomID, GroupID: task.GroupID, Name: task.Name, Description: task.Description, Enabled: task.Enabled, Schedule: task.Schedule, Timezone: task.Timezone, Action: string(task.Action), WorldIDs: string(worldIDs), Parameters: string(parameters), TimeoutSeconds: task.TimeoutSeconds, RetryTimes: task.RetryTimes, RetryInterval: task.RetryInterval, Dependencies: string(dependencies), LastRunAt: task.LastRunAt, LastStatus: string(task.LastStatus), LastJobID: task.LastJobID, Revision: task.Revision, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt}, nil
}

func groupFromRecord(record groupRecord, taskCount int) Group {
	return Group{ID: record.ID, RoomID: record.RoomID, Name: record.Name, Description: record.Description, Type: normalizeGroupType(record.Type), Enabled: record.Enabled, TaskCount: taskCount, Revision: record.Revision, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC()}
}

func runFromRecord(record runRecord) Run {
	return Run{ID: record.ID, TaskID: record.TaskID, TaskName: record.TaskName, GroupID: record.GroupID, GroupName: record.GroupName, RoomID: record.RoomID, Action: Action(record.Action), Trigger: Trigger(record.Trigger), Status: RunStatus(record.Status), JobID: record.JobID, Output: record.Output, Error: record.Error, StartedAt: utcPointer(record.StartedAt), FinishedAt: utcPointer(record.FinishedAt), DurationMs: record.DurationMs, RetryCount: record.RetryCount, CreatedAt: record.CreatedAt.UTC()}
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}
