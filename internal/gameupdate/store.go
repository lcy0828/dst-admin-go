package gameupdate

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

var ErrRunNotFound = errors.New("game update run not found")

type runRecord struct {
	JobID         string    `gorm:"primary_key;type:char(36)"`
	Status        string    `gorm:"type:varchar(16);index;not null"`
	BeforeVersion string    `gorm:"type:varchar(64)"`
	AfterVersion  string    `gorm:"type:varchar(64)"`
	CleanCache    bool      `gorm:"not null"`
	Log           string    `gorm:"type:text"`
	ErrorMessage  string    `gorm:"type:text"`
	StartedAt     time.Time `gorm:"index;not null"`
	FinishedAt    *time.Time
}

type Store struct {
	db    *gorm.DB
	table string
	now   func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	return &Store{db: db, table: strings.TrimSpace(tablePrefix) + "game_update_run", now: time.Now}
}

func (s *Store) Migrate() error {
	if err := s.db.Table(s.table).AutoMigrate(&runRecord{}).Error; err != nil {
		return fmt.Errorf("migrate game update runs: %w", err)
	}
	now := s.now().UTC()
	if err := s.db.Table(s.table).Where("status = ?", "running").Updates(map[string]interface{}{
		"status": "failed", "error_message": "服务重启导致更新状态中断，请重新检查版本后决定是否重试", "finished_at": now,
	}).Error; err != nil {
		return fmt.Errorf("recover game update runs: %w", err)
	}
	return nil
}

func (s *Store) Begin(jobID, before string, cleanCache bool) (Run, error) {
	run := Run{JobID: jobID, Status: "running", BeforeVersion: before, CleanCache: cleanCache, StartedAt: s.now().UTC()}
	record := recordFromRun(run)
	if err := s.db.Table(s.table).Create(&record).Error; err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) Complete(jobID, after, logText string, runErr error) (Run, error) {
	now := s.now().UTC()
	updates := map[string]interface{}{"status": "succeeded", "after_version": after, "log": logText, "error_message": "", "finished_at": now}
	if runErr != nil {
		updates["status"] = "failed"
		updates["error_message"] = runErr.Error()
	}
	result := s.db.Table(s.table).Where("job_id = ? AND status = ?", jobID, "running").Updates(updates)
	if result.Error != nil {
		return Run{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Run{}, ErrRunNotFound
	}
	return s.Get(jobID)
}

func (s *Store) Get(jobID string) (Run, error) {
	var record runRecord
	result := s.db.Table(s.table).Where("job_id = ?", jobID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Run{}, ErrRunNotFound
	}
	if result.Error != nil {
		return Run{}, result.Error
	}
	return runFromRecord(record), nil
}

func recordFromRun(run Run) runRecord {
	return runRecord{JobID: run.JobID, Status: run.Status, BeforeVersion: run.BeforeVersion, AfterVersion: run.AfterVersion, CleanCache: run.CleanCache, Log: run.Log, ErrorMessage: run.ErrorMessage, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt}
}

func runFromRecord(record runRecord) Run {
	return Run{JobID: record.JobID, Status: record.Status, BeforeVersion: record.BeforeVersion, AfterVersion: record.AfterVersion, CleanCache: record.CleanCache, Log: record.Log, ErrorMessage: record.ErrorMessage, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt}
}
