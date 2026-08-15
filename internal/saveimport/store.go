package saveimport

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type importRecord struct {
	ID            string    `gorm:"primary_key;type:char(36)"`
	Name          string    `gorm:"type:varchar(128);not null"`
	SourceName    string    `gorm:"type:varchar(255);not null"`
	ArtifactName  string    `gorm:"type:varchar(255);not null"`
	Status        string    `gorm:"type:varchar(24);index;not null"`
	Size          int64     `gorm:"not null"`
	SHA256        string    `gorm:"type:char(64)"`
	ManifestJSON  string    `gorm:"type:text"`
	ErrorCode     string    `gorm:"type:varchar(64)"`
	ErrorMessage  string    `gorm:"type:text"`
	CreatedAt     time.Time `gorm:"index;not null"`
	UpdatedAt     time.Time `gorm:"not null"`
	AppliedAt     *time.Time
	ApplyPhase    string `gorm:"type:varchar(24)"`
	ApplyMode     string `gorm:"type:varchar(16)"`
	ApplyRoomID   string `gorm:"type:varchar(128)"`
	ApplyTarget   string `gorm:"type:varchar(255)"`
	ApplyStaging  string `gorm:"type:varchar(255)"`
	ApplyRollback string `gorm:"type:varchar(255)"`
}

const (
	applyPhasePublishing = "publishing"
	applyPhaseCommitted  = "committed"
	applyPhaseApplied    = "applied"
)

type Store struct {
	db    *gorm.DB
	table string
	now   func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	return &Store{db: db, table: strings.TrimSpace(tablePrefix) + "save_import", now: time.Now}
}

func (s *Store) Migrate() error {
	if err := s.db.Table(s.table).AutoMigrate(&importRecord{}).Error; err != nil {
		return fmt.Errorf("migrate save imports: %w", err)
	}
	return nil
}

func (s *Store) Create(record importRecord) (Session, error) {
	if err := s.db.Table(s.table).Create(&record).Error; err != nil {
		return Session{}, err
	}
	return sessionFromRecord(record)
}

func (s *Store) Get(id string) (Session, error) {
	var record importRecord
	result := s.db.Table(s.table).Where("id = ?", id).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Session{}, ErrImportNotFound
	}
	if result.Error != nil {
		return Session{}, result.Error
	}
	return sessionFromRecord(record)
}

func (s *Store) List() ([]Session, error) {
	var records []importRecord
	if err := s.db.Table(s.table).Order("created_at desc").Find(&records).Error; err != nil {
		return nil, err
	}
	items := make([]Session, 0, len(records))
	for _, record := range records {
		item, err := sessionFromRecord(record)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) MarkAnalyzing(id string) error {
	now := s.now().UTC()
	result := s.db.Table(s.table).Where("id = ? AND status IN (?)", id, []Status{StatusUploaded, StatusReady, StatusInvalid, StatusApplied}).Updates(map[string]interface{}{
		"status": StatusAnalyzing, "error_code": "", "error_message": "", "updated_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImportBusy
	}
	return nil
}

func (s *Store) MarkAnalysisFailed(id, code, message string) error {
	var record importRecord
	if err := s.db.Table(s.table).Where("id = ?", id).First(&record).Error; err != nil {
		if gorm.IsRecordNotFoundError(err) {
			return ErrImportNotFound
		}
		return err
	}
	status := StatusUploaded
	if strings.TrimSpace(record.ManifestJSON) != "" {
		status = StatusReady
	}
	now := s.now().UTC()
	return s.db.Table(s.table).Where("id = ?", id).Updates(map[string]interface{}{
		"status": status, "error_code": code, "error_message": message, "updated_at": now,
	}).Error
}

func (s *Store) SaveManifest(id, sha256 string, manifest Manifest) (Session, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return Session{}, err
	}
	now := s.now().UTC()
	result := s.db.Table(s.table).Where("id = ?", id).Updates(map[string]interface{}{
		"status": StatusReady, "sha256": sha256, "manifest_json": string(encoded),
		"error_code": "", "error_message": "", "updated_at": now,
	})
	if result.Error != nil {
		return Session{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Session{}, ErrImportNotFound
	}
	return s.Get(id)
}

func (s *Store) MarkInvalid(id, code, message string) error {
	now := s.now().UTC()
	result := s.db.Table(s.table).Where("id = ?", id).Updates(map[string]interface{}{
		"status": StatusInvalid, "error_code": code, "error_message": message, "updated_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImportNotFound
	}
	return nil
}

func (s *Store) BeginApply(id string, mode ApplyMode, roomID, target, staging, rollback string) error {
	now := s.now().UTC()
	result := s.db.Table(s.table).Where("id = ? AND status IN (?)", id, []Status{StatusReady, StatusApplied}).Updates(map[string]interface{}{
		"status": StatusApplying, "error_code": "", "error_message": "", "updated_at": now,
		"apply_phase": applyPhasePublishing, "apply_mode": mode, "apply_room_id": roomID,
		"apply_target": target, "apply_staging": staging, "apply_rollback": rollback,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImportNotReady
	}
	return nil
}

func (s *Store) MarkApplyCommitted(id string) error {
	result := s.db.Table(s.table).Where("id = ? AND status = ? AND apply_phase = ?", id, StatusApplying, applyPhasePublishing).Updates(map[string]interface{}{
		"apply_phase": applyPhaseCommitted, "updated_at": s.now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImportNotReady
	}
	return nil
}

func (s *Store) MarkApplyFailed(id, code, message string) error {
	now := s.now().UTC()
	return s.db.Table(s.table).Where("id = ?", id).Updates(map[string]interface{}{
		"status": StatusReady, "error_code": code, "error_message": message, "updated_at": now,
		"apply_phase": "", "apply_mode": "", "apply_room_id": "", "apply_target": "",
		"apply_staging": "", "apply_rollback": "",
	}).Error
}

func (s *Store) MarkRecoveryBlocked(id, code, message string) error {
	result := s.db.Table(s.table).Where("id = ? AND (status = ? OR apply_phase <> '')", id, StatusApplying).Updates(map[string]interface{}{
		"error_code": code, "error_message": message, "updated_at": s.now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImportNotReady
	}
	return nil
}

func (s *Store) MarkApplied(id string) (Session, error) {
	now := s.now().UTC()
	result := s.db.Table(s.table).Where("id = ? AND status = ? AND apply_phase = ?", id, StatusApplying, applyPhaseCommitted).Updates(map[string]interface{}{
		"status": StatusApplied, "apply_phase": applyPhaseApplied,
		"error_code": "", "error_message": "", "applied_at": now, "updated_at": now,
	})
	if result.Error != nil {
		return Session{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Session{}, ErrImportNotReady
	}
	return s.Get(id)
}

func (s *Store) ClearApplyJournal(id string) error {
	result := s.db.Table(s.table).Where("id = ? AND status = ? AND apply_phase IN (?)", id, StatusApplied, []string{applyPhaseCommitted, applyPhaseApplied}).Updates(map[string]interface{}{
		"apply_phase": "", "apply_mode": "", "apply_room_id": "", "apply_target": "",
		"apply_staging": "", "apply_rollback": "", "updated_at": s.now().UTC(),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImportNotReady
	}
	return nil
}

func (s *Store) RecoveryRecords() ([]importRecord, error) {
	var records []importRecord
	if err := s.db.Table(s.table).
		Where("status IN (?) OR apply_phase <> ''", []Status{StatusAnalyzing, StatusApplying}).
		Order("created_at asc").Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

func (s *Store) Delete(id string) error {
	result := s.db.Table(s.table).Where("id = ?", id).Delete(&importRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrImportNotFound
	}
	return nil
}

func sessionFromRecord(record importRecord) (Session, error) {
	value := Session{
		ID: record.ID, Name: record.Name, SourceName: record.SourceName, Status: Status(record.Status),
		Size: record.Size, SHA256: record.SHA256, ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, AppliedAt: record.AppliedAt,
	}
	if strings.TrimSpace(record.ManifestJSON) != "" {
		var manifest Manifest
		if err := json.Unmarshal([]byte(record.ManifestJSON), &manifest); err != nil {
			return Session{}, fmt.Errorf("decode save import manifest: %w", err)
		}
		value.Manifest = &manifest
	}
	return value, nil
}
