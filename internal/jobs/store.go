package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

var (
	ErrNotFound      = errors.New("job not found")
	ErrNotCancelable = errors.New("job cannot be canceled")
	ErrInvalidTarget = errors.New("job target is invalid")
	ErrInvalidStatus = errors.New("job status is invalid")
)

type jobRecord struct {
	ID              string    `gorm:"primary_key;type:char(36)"`
	Kind            string    `gorm:"type:varchar(64);index;not null"`
	Status          string    `gorm:"type:varchar(24);index;not null"`
	Outcome         string    `gorm:"type:varchar(24);not null"`
	RoomID          string    `gorm:"type:varchar(255);index"`
	WorldID         string    `gorm:"type:varchar(255)"`
	Progress        int       `gorm:"not null"`
	Message         string    `gorm:"type:text"`
	ProgressDetail  string    `gorm:"type:text"`
	CurrentBytes    int64     `gorm:"not null;default:0"`
	TotalBytes      int64     `gorm:"not null;default:0"`
	BytesPerSecond  int64     `gorm:"not null;default:0"`
	ErrorCode       string    `gorm:"type:varchar(64)"`
	ErrorMessage    string    `gorm:"type:text"`
	CancelRequested bool      `gorm:"not null"`
	CreatedAt       time.Time `gorm:"index;not null"`
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

type targetRecord struct {
	ID             int64  `gorm:"primary_key;AUTO_INCREMENT"`
	JobID          string `gorm:"type:char(36);index;not null"`
	TargetID       string `gorm:"type:varchar(255);not null"`
	Name           string `gorm:"type:varchar(255);not null"`
	Status         string `gorm:"type:varchar(24);not null"`
	Message        string `gorm:"type:text"`
	WarningCode    string `gorm:"type:varchar(64)"`
	WarningMessage string `gorm:"type:text"`
	ErrorCode      string `gorm:"type:varchar(64)"`
	ErrorMessage   string `gorm:"type:text"`
	StartedAt      *time.Time
	FinishedAt     *time.Time
}

type eventRecord struct {
	ID        int64     `gorm:"primary_key;AUTO_INCREMENT"`
	JobID     string    `gorm:"type:char(36);index;not null"`
	Type      string    `gorm:"type:varchar(64);not null"`
	Data      string    `gorm:"type:text;not null"`
	CreatedAt time.Time `gorm:"index;not null"`
}

type Store struct {
	db           *gorm.DB
	jobsTable    string
	targetsTable string
	eventsTable  string
	now          func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	prefix := strings.TrimSpace(tablePrefix)
	return &Store{
		db:           db,
		jobsTable:    prefix + "job",
		targetsTable: prefix + "job_target",
		eventsTable:  prefix + "job_event",
		now:          time.Now,
	}
}

func (s *Store) Migrate() error {
	for _, migration := range []struct {
		table string
		model interface{}
	}{
		{s.jobsTable, &jobRecord{}},
		{s.targetsTable, &targetRecord{}},
		{s.eventsTable, &eventRecord{}},
	} {
		if err := s.db.Table(migration.table).AutoMigrate(migration.model).Error; err != nil {
			return fmt.Errorf("migrate %s: %w", migration.table, err)
		}
	}
	if err := s.db.Table(s.targetsTable).AddUniqueIndex("idx_"+s.targetsTable+"_job_target", "job_id", "target_id").Error; err != nil {
		return fmt.Errorf("index job targets: %w", err)
	}
	return nil
}

func (s *Store) Create(kind, roomID, worldID string, targets []TargetSpec) (Job, Event, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" || len(kind) > 64 {
		return Job{}, Event{}, fmt.Errorf("invalid job kind")
	}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		if strings.TrimSpace(target.ID) == "" || strings.TrimSpace(target.Name) == "" || seen[target.ID] {
			return Job{}, Event{}, ErrInvalidTarget
		}
		seen[target.ID] = true
	}
	now := s.now().UTC()
	record := jobRecord{
		ID: uuid.NewString(), Kind: kind, Status: string(StatusQueued), Outcome: string(OutcomePending),
		RoomID: roomID, WorldID: worldID, Progress: 0, CreatedAt: now,
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return Job{}, Event{}, tx.Error
	}
	if err := tx.Table(s.jobsTable).Create(&record).Error; err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	for _, target := range targets {
		targetRecord := targetRecord{JobID: record.ID, TargetID: target.ID, Name: target.Name, Status: string(StatusQueued)}
		if err := tx.Table(s.targetsTable).Create(&targetRecord).Error; err != nil {
			tx.Rollback()
			return Job{}, Event{}, err
		}
	}
	job, err := s.getTx(tx, record.ID)
	if err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	event, err := s.appendEventTx(tx, "job.created", job)
	if err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	if err := tx.Commit().Error; err != nil {
		return Job{}, Event{}, err
	}
	return job, event, nil
}

func (s *Store) MarkRunning(jobID string) (Job, Event, error) {
	now := s.now().UTC()
	tx := s.db.Begin()
	result := tx.Table(s.jobsTable).Where("id = ? AND status = ?", jobID, StatusQueued).Updates(map[string]interface{}{
		"status": StatusRunning, "started_at": now, "message": "任务正在执行",
	})
	if result.Error != nil {
		tx.Rollback()
		return Job{}, Event{}, result.Error
	}
	if result.RowsAffected != 1 {
		tx.Rollback()
		return Job{}, Event{}, ErrInvalidStatus
	}
	if err := tx.Table(s.targetsTable).Where("job_id = ? AND status = ?", jobID, StatusQueued).Updates(map[string]interface{}{
		"status": StatusRunning, "started_at": now,
	}).Error; err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	return s.commitEvent(tx, "job.running", jobID)
}

func (s *Store) UpdateProgress(jobID string, progress int, message string) (Job, Event, error) {
	return s.UpdateProgressDetail(jobID, ProgressUpdate{Progress: progress, Message: message})
}

func (s *Store) UpdateProgressDetail(jobID string, update ProgressUpdate) (Job, Event, error) {
	update.Message = strings.TrimSpace(update.Message)
	if update.Progress < 0 || update.Progress >= 100 || update.Message == "" || len(update.Message) > 1000 ||
		update.CurrentBytes < 0 || update.TotalBytes < 0 || update.BytesPerSecond < 0 ||
		update.TotalBytes > 0 && update.CurrentBytes > update.TotalBytes {
		return Job{}, Event{}, ErrInvalidStatus
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return Job{}, Event{}, tx.Error
	}
	var current jobRecord
	if err := tx.Table(s.jobsTable).Where("id = ?", jobID).First(&current).Error; err != nil {
		tx.Rollback()
		if gorm.IsRecordNotFoundError(err) {
			return Job{}, Event{}, ErrNotFound
		}
		return Job{}, Event{}, err
	}
	if Status(current.Status) != StatusRunning {
		tx.Rollback()
		return Job{}, Event{}, ErrInvalidStatus
	}
	if update.Progress < current.Progress {
		if update.Detail == nil || len(update.Detail.Items) == 0 && len(update.Detail.Worlds) == 0 {
			tx.Rollback()
			job, err := s.Get(jobID)
			return job, Event{}, err
		}
		update.Progress = current.Progress
	}
	var previous ProgressDetail
	_ = json.Unmarshal([]byte(current.ProgressDetail), &previous)
	detail := ""
	var next ProgressDetail
	if update.Detail != nil {
		next = *update.Detail
	}
	if len(next.Items) == 0 {
		next.Items = previous.Items
	}
	if len(next.Worlds) == 0 {
		next.Worlds = previous.Worlds
	}
	if update.Detail != nil || len(next.Items) > 0 || len(next.Worlds) > 0 {
		encoded, err := json.Marshal(next)
		if err != nil {
			tx.Rollback()
			return Job{}, Event{}, err
		}
		detail = string(encoded)
	}
	if err := tx.Table(s.jobsTable).Where("id = ? AND status = ?", jobID, StatusRunning).Updates(map[string]interface{}{
		"progress": update.Progress, "message": update.Message,
		"progress_detail": detail,
		"current_bytes":   update.CurrentBytes, "total_bytes": update.TotalBytes, "bytes_per_second": update.BytesPerSecond,
	}).Error; err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	return s.commitEvent(tx, "job.progress", jobID)
}

func (s *Store) RecordTarget(jobID string, result TargetResult) (Job, Event, error) {
	if result.Status != StatusSucceeded && result.Status != StatusFailed && result.Status != StatusCanceled {
		return Job{}, Event{}, ErrInvalidStatus
	}
	now := s.now().UTC()
	updates := map[string]interface{}{
		"status": result.Status, "message": result.Message, "finished_at": now,
		"warning_code": "", "warning_message": "",
		"error_code": "", "error_message": "",
	}
	if result.Warning != nil {
		updates["warning_code"] = result.Warning.Code
		updates["warning_message"] = result.Warning.Message
	}
	if result.Error != nil {
		updates["error_code"] = result.Error.Code
		updates["error_message"] = result.Error.Message
	}
	tx := s.db.Begin()
	changed := tx.Table(s.targetsTable).Where("job_id = ? AND target_id = ? AND status = ?", jobID, result.TargetID, StatusRunning).Updates(updates)
	if changed.Error != nil {
		tx.Rollback()
		return Job{}, Event{}, changed.Error
	}
	if changed.RowsAffected != 1 {
		tx.Rollback()
		return Job{}, Event{}, ErrInvalidTarget
	}
	var total, finished int
	if err := tx.Table(s.targetsTable).Where("job_id = ?", jobID).Count(&total).Error; err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	if err := tx.Table(s.targetsTable).Where("job_id = ? AND status IN (?)", jobID, []Status{StatusSucceeded, StatusFailed, StatusCanceled}).Count(&finished).Error; err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	progress := 100
	if total > 0 {
		progress = finished * 100 / total
	}
	if err := tx.Table(s.jobsTable).Where("id = ?", jobID).UpdateColumn(
		"progress", gorm.Expr("CASE WHEN progress < ? THEN ? ELSE progress END", progress, progress),
	).Error; err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	return s.commitEvent(tx, "job.target.completed", jobID)
}

func (s *Store) Complete(jobID string, runnerErr error, canceled bool) (Job, Event, error) {
	tx := s.db.Begin()
	job, err := s.getTx(tx, jobID)
	if err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	if job.Status != StatusRunning {
		tx.Rollback()
		return Job{}, Event{}, ErrInvalidStatus
	}
	succeeded, failed, canceledTargets := targetCounts(job.Targets)
	warnings := targetWarningCount(job.Targets)
	status := StatusSucceeded
	outcome := OutcomeFull
	errorCode, errorMessage := "", ""
	message := "任务执行成功"
	if canceled || canceledTargets > 0 {
		status = StatusCanceled
		outcome = completionOutcome(succeeded, failed+canceledTargets)
		errorCode, errorMessage = "JOB_CANCELED", "任务已取消"
		message = "任务已取消"
	} else if runnerErr != nil || failed > 0 {
		status = StatusFailed
		outcome = completionOutcome(succeeded, failed)
		if outcome == OutcomePartial {
			errorCode, errorMessage = "PARTIAL_FAILURE", "部分目标执行失败"
			message = "任务部分成功"
		} else {
			errorCode, errorMessage = "JOB_FAILED", "任务执行失败"
			message = "任务执行失败"
		}
		if runnerErr != nil {
			errorMessage = runnerErr.Error()
		} else if outcome == OutcomeNone {
			for _, target := range job.Targets {
				if target.Status == StatusFailed && target.Error != nil {
					errorCode, errorMessage = target.Error.Code, target.Error.Message
					break
				}
			}
		}
	} else if warnings > 0 {
		message = fmt.Sprintf("任务执行成功，%d 个目标存在警告", warnings)
	}
	now := s.now().UTC()
	updates := map[string]interface{}{
		"status": status, "outcome": outcome, "progress": 100, "message": message,
		"error_code": errorCode, "error_message": errorMessage, "finished_at": now,
		"current_bytes": 0, "total_bytes": 0, "bytes_per_second": 0,
	}
	if err := tx.Table(s.jobsTable).Where("id = ? AND status = ?", jobID, StatusRunning).Updates(updates).Error; err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	return s.commitEvent(tx, "job.completed", jobID)
}

func (s *Store) RequestCancel(jobID string) (Job, Event, error) {
	tx := s.db.Begin()
	result := tx.Table(s.jobsTable).Where("id = ? AND status IN (?)", jobID, []Status{StatusQueued, StatusRunning}).UpdateColumn("cancel_requested", true)
	if result.Error != nil {
		tx.Rollback()
		return Job{}, Event{}, result.Error
	}
	if result.RowsAffected != 1 {
		var count int
		if err := tx.Table(s.jobsTable).Where("id = ?", jobID).Count(&count).Error; err != nil {
			tx.Rollback()
			return Job{}, Event{}, err
		}
		tx.Rollback()
		if count == 0 {
			return Job{}, Event{}, ErrNotFound
		}
		return Job{}, Event{}, ErrNotCancelable
	}
	return s.commitEvent(tx, "job.cancel.requested", jobID)
}

func (s *Store) RecoverInterrupted() ([]Event, error) {
	var records []jobRecord
	if err := s.db.Table(s.jobsTable).Where("status IN (?)", []Status{StatusQueued, StatusRunning}).Find(&records).Error; err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(records))
	for _, record := range records {
		tx := s.db.Begin()
		now := s.now().UTC()
		if err := tx.Table(s.targetsTable).Where("job_id = ? AND status IN (?)", record.ID, []Status{StatusQueued, StatusRunning}).Updates(map[string]interface{}{
			"status": StatusFailed, "error_code": "SERVER_RESTARTED", "error_message": "服务重启中断了任务", "finished_at": now,
		}).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
		interrupted, err := s.getTx(tx, record.ID)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		succeeded, failed, canceled := targetCounts(interrupted.Targets)
		outcome := completionOutcome(succeeded, failed+canceled)
		if err := tx.Table(s.jobsTable).Where("id = ?", record.ID).Updates(map[string]interface{}{
			"status": StatusFailed, "outcome": outcome, "progress": 100, "message": "任务被服务重启中断",
			"error_code": "SERVER_RESTARTED", "error_message": "服务重启中断了任务", "finished_at": now,
			"current_bytes": 0, "total_bytes": 0, "bytes_per_second": 0,
		}).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
		_, event, err := s.commitEvent(tx, "job.interrupted", record.ID)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func (s *Store) Get(jobID string) (Job, error) {
	return s.getTx(s.db, jobID)
}

func (s *Store) List(filter ListFilter) ([]Job, int, error) {
	query := s.db.Table(s.jobsTable)
	if filter.Status != "" {
		query = query.Where("status = ?", filter.Status)
	}
	if strings.TrimSpace(filter.Kind) != "" {
		query = query.Where("kind = ?", strings.TrimSpace(filter.Kind))
	}
	var total int
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	var records []jobRecord
	if err := query.Order("created_at DESC").Limit(limit).Offset(offset).Find(&records).Error; err != nil {
		return nil, 0, err
	}
	result := make([]Job, 0, len(records))
	for _, record := range records {
		job, err := s.jobFromRecord(s.db, record)
		if err != nil {
			return nil, 0, err
		}
		result = append(result, job)
	}
	return result, total, nil
}

func (s *Store) EventsAfter(afterID int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var records []eventRecord
	if err := s.db.Table(s.eventsTable).Where("id > ?", afterID).Order("id ASC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(records))
	for _, record := range records {
		var job Job
		if err := json.Unmarshal([]byte(record.Data), &job); err != nil {
			return nil, fmt.Errorf("decode job event %d: %w", record.ID, err)
		}
		events = append(events, Event{ID: record.ID, JobID: record.JobID, Type: record.Type, Data: job, CreatedAt: record.CreatedAt})
	}
	return events, nil
}

func (s *Store) EventWindow() (EventWindow, error) {
	var first, last sql.NullInt64
	if err := s.db.Table(s.eventsTable).Select("MIN(id), MAX(id)").Row().Scan(&first, &last); err != nil {
		return EventWindow{}, fmt.Errorf("read job event window: %w", err)
	}
	return EventWindow{FirstID: first.Int64, LastID: last.Int64}, nil
}

func (s *Store) Prune(policy RetentionPolicy) (RetentionResult, error) {
	if policy.EventMaxAge <= 0 || policy.EventLimit <= 0 || policy.JobMaxAge <= 0 || policy.JobLimit <= 0 {
		return RetentionResult{}, errors.New("job retention policy is invalid")
	}
	now := s.now().UTC()
	tx := s.db.Begin()
	if tx.Error != nil {
		return RetentionResult{}, tx.Error
	}
	rollback := func(err error) (RetentionResult, error) {
		tx.Rollback()
		return RetentionResult{}, err
	}
	result := RetentionResult{}
	deletedEvents := tx.Table(s.eventsTable).Where("created_at < ?", now.Add(-policy.EventMaxAge)).Delete(&eventRecord{})
	if deletedEvents.Error != nil {
		return rollback(deletedEvents.Error)
	}
	result.EventsDeleted += deletedEvents.RowsAffected

	var eventCount int
	if err := tx.Table(s.eventsTable).Count(&eventCount).Error; err != nil {
		return rollback(err)
	}
	if excess := eventCount - policy.EventLimit; excess > 0 {
		var boundary eventRecord
		if err := tx.Table(s.eventsTable).Select("id").Order("id ASC").Offset(excess - 1).Limit(1).First(&boundary).Error; err != nil {
			return rollback(err)
		}
		deleted := tx.Table(s.eventsTable).Where("id <= ?", boundary.ID).Delete(&eventRecord{})
		if deleted.Error != nil {
			return rollback(deleted.Error)
		}
		result.EventsDeleted += deleted.RowsAffected
	}

	terminal := []Status{StatusSucceeded, StatusFailed, StatusCanceled}
	var terminalRecords []jobRecord
	if err := tx.Table(s.jobsTable).Select("id, finished_at").Where("status IN (?)", terminal).
		Order("COALESCE(finished_at, created_at) DESC").Find(&terminalRecords).Error; err != nil {
		return rollback(err)
	}
	deleteIDs := make(map[string]struct{})
	cutoff := now.Add(-policy.JobMaxAge)
	for index, record := range terminalRecords {
		if index >= policy.JobLimit || record.FinishedAt != nil && record.FinishedAt.Before(cutoff) {
			deleteIDs[record.ID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(deleteIDs))
	for id := range deleteIDs {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += 400 {
		end := min(start+400, len(ids))
		batch := ids[start:end]
		deleted := tx.Table(s.targetsTable).Where("job_id IN (?)", batch).Delete(&targetRecord{})
		if deleted.Error != nil {
			return rollback(deleted.Error)
		}
		result.TargetsDeleted += deleted.RowsAffected
		deleted = tx.Table(s.eventsTable).Where("job_id IN (?)", batch).Delete(&eventRecord{})
		if deleted.Error != nil {
			return rollback(deleted.Error)
		}
		result.EventsDeleted += deleted.RowsAffected
		deleted = tx.Table(s.jobsTable).Where("id IN (?) AND status IN (?)", batch, terminal).Delete(&jobRecord{})
		if deleted.Error != nil {
			return rollback(deleted.Error)
		}
		result.JobsDeleted += deleted.RowsAffected
	}
	if err := tx.Commit().Error; err != nil {
		return RetentionResult{}, err
	}
	return result, nil
}

func (s *Store) commitEvent(tx *gorm.DB, eventType, jobID string) (Job, Event, error) {
	job, err := s.getTx(tx, jobID)
	if err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	event, err := s.appendEventTx(tx, eventType, job)
	if err != nil {
		tx.Rollback()
		return Job{}, Event{}, err
	}
	if err := tx.Commit().Error; err != nil {
		return Job{}, Event{}, err
	}
	return job, event, nil
}

func (s *Store) appendEventTx(tx *gorm.DB, eventType string, job Job) (Event, error) {
	data, err := json.Marshal(job)
	if err != nil {
		return Event{}, err
	}
	record := eventRecord{JobID: job.ID, Type: eventType, Data: string(data), CreatedAt: s.now().UTC()}
	if err := tx.Table(s.eventsTable).Create(&record).Error; err != nil {
		return Event{}, err
	}
	return Event{ID: record.ID, JobID: job.ID, Type: eventType, Data: job, CreatedAt: record.CreatedAt}, nil
}

func (s *Store) getTx(tx *gorm.DB, jobID string) (Job, error) {
	var record jobRecord
	result := tx.Table(s.jobsTable).Where("id = ?", jobID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Job{}, ErrNotFound
	}
	if result.Error != nil {
		return Job{}, result.Error
	}
	return s.jobFromRecord(tx, record)
}

func (s *Store) jobFromRecord(tx *gorm.DB, record jobRecord) (Job, error) {
	var records []targetRecord
	if err := tx.Table(s.targetsTable).Where("job_id = ?", record.ID).Order("id ASC").Find(&records).Error; err != nil {
		return Job{}, err
	}
	targets := make([]Target, 0, len(records))
	for _, target := range records {
		item := Target{
			ID: target.ID, TargetID: target.TargetID, Name: target.Name, Status: Status(target.Status), Message: target.Message,
			StartedAt: target.StartedAt, FinishedAt: target.FinishedAt,
		}
		if target.WarningCode != "" || target.WarningMessage != "" {
			item.Warning = &Error{Code: target.WarningCode, Message: target.WarningMessage}
		}
		if target.ErrorCode != "" || target.ErrorMessage != "" {
			item.Error = &Error{Code: target.ErrorCode, Message: target.ErrorMessage}
		}
		targets = append(targets, item)
	}
	job := Job{
		ID: record.ID, Kind: record.Kind, Status: Status(record.Status), Outcome: Outcome(record.Outcome), RoomID: record.RoomID,
		WorldID: record.WorldID, Progress: record.Progress, Message: record.Message, CancelRequested: record.CancelRequested,
		CreatedAt: record.CreatedAt, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt, Targets: targets,
	}
	if record.CurrentBytes > 0 || record.TotalBytes > 0 || record.BytesPerSecond > 0 {
		job.Transfer = &TransferProgress{
			CurrentBytes: record.CurrentBytes, TotalBytes: record.TotalBytes, BytesPerSecond: record.BytesPerSecond,
		}
	}
	if record.ProgressDetail != "" {
		var detail ProgressDetail
		if json.Unmarshal([]byte(record.ProgressDetail), &detail) == nil {
			job.ProgressDetail = &detail
		}
	}
	if record.ErrorCode != "" || record.ErrorMessage != "" {
		job.Error = &Error{Code: record.ErrorCode, Message: record.ErrorMessage}
	}
	return job, nil
}

func targetCounts(targets []Target) (succeeded, failed, canceled int) {
	for _, target := range targets {
		switch target.Status {
		case StatusSucceeded:
			succeeded++
		case StatusFailed:
			failed++
		case StatusCanceled:
			canceled++
		}
	}
	return
}

func targetWarningCount(targets []Target) int {
	count := 0
	for _, target := range targets {
		if target.Warning != nil {
			count++
		}
	}
	return count
}

func completionOutcome(succeeded, unsuccessful int) Outcome {
	if succeeded > 0 && unsuccessful > 0 {
		return OutcomePartial
	}
	if succeeded > 0 {
		return OutcomeFull
	}
	return OutcomeNone
}
