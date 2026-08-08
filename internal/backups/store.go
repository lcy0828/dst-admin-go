package backups

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

var ErrBackupNotFound = errors.New("backup not found")

type backupRecord struct {
	ID              string `gorm:"primary_key;type:char(36)"`
	RoomID          string `gorm:"type:varchar(255);index;unique_index:idx_backup_room_file;not null"`
	Name            string `gorm:"type:varchar(128);not null"`
	Kind            string `gorm:"type:varchar(24);index;not null"`
	FileName        string `gorm:"type:varchar(255);unique_index:idx_backup_room_file;not null"`
	SourceName      string `gorm:"type:varchar(255)"`
	Size            int64  `gorm:"not null"`
	ContentSize     int64  `gorm:"not null"`
	FileCount       int    `gorm:"not null"`
	SHA256          string `gorm:"type:char(64);not null"`
	Status          string `gorm:"type:varchar(16);index;not null"`
	ValidationError string `gorm:"type:text"`
	VerifiedAt      *time.Time
	SourceJobID     string    `gorm:"type:char(36);index"`
	CreatedAt       time.Time `gorm:"index;not null"`
	UpdatedAt       time.Time `gorm:"not null"`
}

type policyRecord struct {
	RoomID         string     `gorm:"primary_key;type:varchar(255)"`
	Enabled        bool       `gorm:"not null"`
	IntervalMinute int        `gorm:"not null"`
	MaxSnapshots   int        `gorm:"not null"`
	NextRunAt      *time.Time `gorm:"index"`
	LastRunAt      *time.Time
	LastJobID      string    `gorm:"type:char(36)"`
	LastError      string    `gorm:"type:text"`
	UpdatedAt      time.Time `gorm:"not null"`
}

type Store struct {
	db          *gorm.DB
	backupTable string
	policyTable string
	now         func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	prefix := strings.TrimSpace(tablePrefix)
	return &Store{db: db, backupTable: prefix + "backup", policyTable: prefix + "backup_policy", now: time.Now}
}

func (s *Store) Migrate() error {
	if err := s.db.Table(s.backupTable).AutoMigrate(&backupRecord{}).Error; err != nil {
		return fmt.Errorf("migrate backups: %w", err)
	}
	if err := s.db.Table(s.policyTable).AutoMigrate(&policyRecord{}).Error; err != nil {
		return fmt.Errorf("migrate backup policies: %w", err)
	}
	return nil
}

func (s *Store) Create(value Backup) (Backup, error) {
	record := backupRecordFrom(value)
	if err := s.db.Table(s.backupTable).Create(&record).Error; err != nil {
		return Backup{}, err
	}
	return backupFromRecord(record), nil
}

func (s *Store) Get(id string) (Backup, error) {
	var record backupRecord
	result := s.db.Table(s.backupTable).Where("id = ?", id).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Backup{}, ErrBackupNotFound
	}
	if result.Error != nil {
		return Backup{}, result.Error
	}
	return backupFromRecord(record), nil
}

func (s *Store) List(roomID string) ([]Backup, error) {
	var records []backupRecord
	query := s.db.Table(s.backupTable)
	if roomID != "" {
		query = query.Where("room_id = ?", roomID)
	}
	if err := query.Order("created_at DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]Backup, 0, len(records))
	for _, record := range records {
		result = append(result, backupFromRecord(record))
	}
	return result, nil
}

func (s *Store) FindByFileName(roomID, fileName string) (Backup, error) {
	var record backupRecord
	result := s.db.Table(s.backupTable).Where("room_id = ? AND file_name = ?", roomID, fileName).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Backup{}, ErrBackupNotFound
	}
	if result.Error != nil {
		return Backup{}, result.Error
	}
	return backupFromRecord(record), nil
}

func (s *Store) Rename(id, name string) (Backup, error) {
	now := s.now().UTC()
	result := s.db.Table(s.backupTable).Where("id = ?", id).Updates(map[string]interface{}{"name": name, "updated_at": now})
	if result.Error != nil {
		return Backup{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Backup{}, ErrBackupNotFound
	}
	return s.Get(id)
}

func (s *Store) Delete(id string) error {
	result := s.db.Table(s.backupTable).Where("id = ?", id).Delete(&backupRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrBackupNotFound
	}
	return nil
}

func (s *Store) Policy(roomID string) (Policy, error) {
	var record policyRecord
	result := s.db.Table(s.policyTable).Where("room_id = ?", roomID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Policy{RoomID: roomID, IntervalMinute: 360, MaxSnapshots: 6, UpdatedAt: s.now().UTC()}, nil
	}
	if result.Error != nil {
		return Policy{}, result.Error
	}
	return policyFromRecord(record), nil
}

func (s *Store) SavePolicy(policy Policy) (Policy, error) {
	policy.UpdatedAt = s.now().UTC()
	record := policyRecordFrom(policy)
	var existing policyRecord
	result := s.db.Table(s.policyTable).Where("room_id = ?", policy.RoomID).First(&existing)
	if gorm.IsRecordNotFoundError(result.Error) {
		if err := s.db.Table(s.policyTable).Create(&record).Error; err != nil {
			return Policy{}, err
		}
	} else if result.Error != nil {
		return Policy{}, result.Error
	} else if err := s.db.Table(s.policyTable).Where("room_id = ?", policy.RoomID).Updates(map[string]interface{}{
		"enabled": policy.Enabled, "interval_minute": policy.IntervalMinute, "max_snapshots": policy.MaxSnapshots,
		"next_run_at": policy.NextRunAt, "last_run_at": policy.LastRunAt, "last_job_id": policy.LastJobID,
		"last_error": policy.LastError, "updated_at": policy.UpdatedAt,
	}).Error; err != nil {
		return Policy{}, err
	}
	return s.Policy(policy.RoomID)
}

func (s *Store) DuePolicies(now time.Time) ([]Policy, error) {
	var records []policyRecord
	if err := s.db.Table(s.policyTable).Where("enabled = ? AND next_run_at IS NOT NULL AND next_run_at <= ?", true, now.UTC()).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]Policy, 0, len(records))
	for _, record := range records {
		result = append(result, policyFromRecord(record))
	}
	return result, nil
}

func backupRecordFrom(value Backup) backupRecord {
	return backupRecord{ID: value.ID, RoomID: value.RoomID, Name: value.Name, Kind: string(value.Kind), FileName: value.FileName, SourceName: value.SourceName, Size: value.Size, ContentSize: value.ContentSize, FileCount: value.FileCount, SHA256: value.SHA256, Status: value.Status, ValidationError: value.ValidationError, VerifiedAt: value.VerifiedAt, SourceJobID: value.SourceJobID, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}

func backupFromRecord(record backupRecord) Backup {
	return Backup{ID: record.ID, RoomID: record.RoomID, Name: record.Name, Kind: Kind(record.Kind), FileName: record.FileName, SourceName: record.SourceName, Size: record.Size, ContentSize: record.ContentSize, FileCount: record.FileCount, SHA256: record.SHA256, Status: record.Status, ValidationError: record.ValidationError, VerifiedAt: record.VerifiedAt, SourceJobID: record.SourceJobID, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
}

func policyRecordFrom(value Policy) policyRecord {
	return policyRecord{RoomID: value.RoomID, Enabled: value.Enabled, IntervalMinute: value.IntervalMinute, MaxSnapshots: value.MaxSnapshots, NextRunAt: value.NextRunAt, LastRunAt: value.LastRunAt, LastJobID: value.LastJobID, LastError: value.LastError, UpdatedAt: value.UpdatedAt}
}

func policyFromRecord(record policyRecord) Policy {
	return Policy{RoomID: record.RoomID, Enabled: record.Enabled, IntervalMinute: record.IntervalMinute, MaxSnapshots: record.MaxSnapshots, NextRunAt: record.NextRunAt, LastRunAt: record.LastRunAt, LastJobID: record.LastJobID, LastError: record.LastError, UpdatedAt: record.UpdatedAt}
}
