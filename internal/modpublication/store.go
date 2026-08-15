package modpublication

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type publicationRecord struct {
	ID                   string `gorm:"primary_key;type:varchar(128)"`
	IdempotencyKey       string `gorm:"type:varchar(260);unique_index;not null"`
	SourceJobID          string `gorm:"type:varchar(128);index"`
	RoomID               string `gorm:"type:varchar(128);index;not null"`
	Status               string `gorm:"type:varchar(32);index;not null"`
	Outcome              string `gorm:"type:varchar(16);not null"`
	TopologyRevision     string `gorm:"type:varchar(128);index;not null"`
	PlanHash             string `gorm:"type:char(64);index;not null"`
	PlanJSON             string `gorm:"type:text;not null"`
	ProtectionBackupJSON string `gorm:"type:text;not null"`
	FencesJSON           string `gorm:"type:text;not null"`
	CommitDecision       bool   `gorm:"index;not null"`
	RestartRequired      bool   `gorm:"not null"`
	ErrorCode            string `gorm:"type:varchar(64)"`
	ErrorMessage         string `gorm:"type:text"`
	CommitDecidedAt      *time.Time
	FinishedAt           *time.Time
	CreatedAt            time.Time `gorm:"index;not null"`
	UpdatedAt            time.Time `gorm:"not null"`
}

type targetRecord struct {
	ID             string `gorm:"primary_key;type:varchar(128)"`
	PublicationID  string `gorm:"type:varchar(128);index;not null"`
	TargetID       string `gorm:"type:varchar(128);index;not null"`
	InstallationID string `gorm:"type:varchar(128);index;not null"`
	Status         string `gorm:"type:varchar(32);index;not null"`
	CacheEnsured   bool   `gorm:"not null"`
	Prepared       bool   `gorm:"not null"`
	Published      bool   `gorm:"not null"`
	Completed      bool   `gorm:"not null"`
	RolledBack     bool   `gorm:"not null"`
	ErrorCode      string `gorm:"type:varchar(64)"`
	ErrorMessage   string `gorm:"type:text"`
	PreparedAt     *time.Time
	PublishedAt    *time.Time
	CompletedAt    *time.Time
	RolledBackAt   *time.Time
	CreatedAt      time.Time `gorm:"not null"`
	UpdatedAt      time.Time `gorm:"not null"`
}

type Store struct {
	db               *gorm.DB
	publicationTable string
	targetTable      string
	now              func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	prefix := strings.TrimSpace(tablePrefix)
	return &Store{db: db, publicationTable: prefix + "mod_publication", targetTable: prefix + "mod_publication_target", now: time.Now}
}

func (s *Store) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("mod publication database is required")
	}
	if err := s.db.Table(s.publicationTable).AutoMigrate(&publicationRecord{}).Error; err != nil {
		return fmt.Errorf("migrate mod publications: %w", err)
	}
	if err := s.db.Table(s.targetTable).AutoMigrate(&targetRecord{}).Error; err != nil {
		return fmt.Errorf("migrate mod publication targets: %w", err)
	}
	return nil
}

func (s *Store) Create(value Publication) (Publication, error) {
	if err := validatePublication(value); err != nil {
		return Publication{}, err
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return Publication{}, tx.Error
	}
	rollback := func(err error) (Publication, error) {
		tx.Rollback()
		return Publication{}, err
	}
	if value.SourceJobID != "" {
		var count int
		if err := tx.Table(s.publicationTable).Where("source_job_id = ?", value.SourceJobID).Count(&count).Error; err != nil {
			return rollback(err)
		}
		if count > 0 {
			return rollback(ErrIdempotencyConflict)
		}
	}
	record, err := publicationRecordFrom(value)
	if err != nil {
		return rollback(err)
	}
	if err := tx.Table(s.publicationTable).Create(&record).Error; err != nil {
		return rollback(err)
	}
	for _, target := range value.Targets {
		record := targetRecordFrom(value.ID, target, value.CreatedAt)
		if err := tx.Table(s.targetTable).Create(&record).Error; err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit().Error; err != nil {
		return Publication{}, err
	}
	return s.Get(value.ID)
}

func (s *Store) Get(id string) (Publication, error) {
	var record publicationRecord
	result := s.db.Table(s.publicationTable).Where("id = ?", id).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Publication{}, ErrNotFound
	}
	if result.Error != nil {
		return Publication{}, result.Error
	}
	value, err := publicationFromRecord(record)
	if err != nil {
		return Publication{}, err
	}
	var targets []targetRecord
	if err := s.db.Table(s.targetTable).Where("publication_id = ?", id).Order("target_id ASC, installation_id ASC").Find(&targets).Error; err != nil {
		return Publication{}, err
	}
	value.Targets = make([]TargetResult, 0, len(targets))
	for _, target := range targets {
		value.Targets = append(value.Targets, targetFromRecord(target))
	}
	if len(value.Targets) != len(value.Plan.Targets) {
		return Publication{}, ErrInvalidInput
	}
	return value, nil
}

func (s *Store) FindIdempotent(id, sourceJobID string) (Publication, error) {
	if id != "" {
		value, err := s.Get(id)
		if err == nil || !errors.Is(err, ErrNotFound) {
			return value, err
		}
	}
	if sourceJobID == "" {
		return Publication{}, ErrNotFound
	}
	var record publicationRecord
	result := s.db.Table(s.publicationTable).Where("source_job_id = ?", sourceJobID).Order("created_at ASC").First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Publication{}, ErrNotFound
	}
	if result.Error != nil {
		return Publication{}, result.Error
	}
	return s.Get(record.ID)
}

func (s *Store) List(roomID string, limit, offset int) ([]Publication, int, error) {
	roomID = strings.TrimSpace(roomID)
	if !validID(roomID) || limit < 1 || limit > 100 || offset < 0 {
		return nil, 0, ErrInvalidInput
	}
	query := s.db.Table(s.publicationTable).Where("room_id = ?", roomID)
	var total int
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var records []publicationRecord
	if err := query.Order("created_at DESC, id DESC").Limit(limit).Offset(offset).Find(&records).Error; err != nil {
		return nil, 0, err
	}
	values := make([]Publication, 0, len(records))
	for _, record := range records {
		value, err := s.Get(record.ID)
		if err != nil {
			return nil, 0, err
		}
		values = append(values, value)
	}
	return values, total, nil
}

func (s *Store) Save(value Publication) (Publication, error) {
	if err := validatePublication(value); err != nil {
		return Publication{}, err
	}
	record, err := publicationRecordFrom(value)
	if err != nil {
		return Publication{}, err
	}
	updates := map[string]interface{}{
		"status": record.Status, "outcome": record.Outcome, "protection_backup_json": record.ProtectionBackupJSON,
		"fences_json": record.FencesJSON, "commit_decision": record.CommitDecision, "restart_required": record.RestartRequired,
		"error_code": record.ErrorCode, "error_message": record.ErrorMessage, "commit_decided_at": record.CommitDecidedAt,
		"finished_at": record.FinishedAt, "updated_at": record.UpdatedAt,
	}
	result := s.db.Table(s.publicationTable).Where("id = ? AND plan_hash = ?", value.ID, value.Plan.PlanHash).Updates(updates)
	if result.Error != nil {
		return Publication{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Publication{}, ErrNotFound
	}
	return s.Get(value.ID)
}

func (s *Store) SaveTarget(publicationID string, value TargetResult) (TargetResult, error) {
	if !validID(publicationID) || !validID(value.TargetID) || !validID(value.InstallationID) || !validStatus(value.Status) {
		return TargetResult{}, ErrInvalidInput
	}
	value.UpdatedAt = s.now().UTC()
	record := targetRecordFrom(publicationID, value, value.UpdatedAt)
	updates := map[string]interface{}{
		"status": record.Status, "cache_ensured": record.CacheEnsured, "prepared": record.Prepared,
		"published": record.Published, "completed": record.Completed, "rolled_back": record.RolledBack,
		"error_code": record.ErrorCode, "error_message": record.ErrorMessage, "prepared_at": record.PreparedAt,
		"published_at": record.PublishedAt, "completed_at": record.CompletedAt, "rolled_back_at": record.RolledBackAt,
		"updated_at": record.UpdatedAt,
	}
	result := s.db.Table(s.targetTable).Where("id = ? AND publication_id = ?", record.ID, publicationID).Updates(updates)
	if result.Error != nil {
		return TargetResult{}, result.Error
	}
	if result.RowsAffected != 1 {
		return TargetResult{}, ErrNotFound
	}
	var saved targetRecord
	if err := s.db.Table(s.targetTable).Where("id = ?", record.ID).First(&saved).Error; err != nil {
		return TargetResult{}, err
	}
	return targetFromRecord(saved), nil
}

func (s *Store) DecideCommit(id string, decidedAt time.Time) (Publication, error) {
	decidedAt = decidedAt.UTC()
	result := s.db.Table(s.publicationTable).Where("id = ? AND commit_decision = ?", id, false).Updates(map[string]interface{}{
		"commit_decision": true, "status": string(StatusCommitted), "outcome": string(OutcomeFull),
		"commit_decided_at": &decidedAt, "updated_at": decidedAt,
	})
	if result.Error != nil {
		return Publication{}, result.Error
	}
	if result.RowsAffected == 0 {
		current, err := s.Get(id)
		if err != nil {
			return Publication{}, err
		}
		if !current.CommitDecision {
			return Publication{}, ErrConflict
		}
		return current, nil
	}
	return s.Get(id)
}

func (s *Store) Active() ([]Publication, error) {
	var records []publicationRecord
	terminal := []string{string(StatusSucceeded), string(StatusFailed), string(StatusRolledBack)}
	if err := s.db.Table(s.publicationTable).Where("status NOT IN (?)", terminal).Order("created_at ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	values := make([]Publication, 0, len(records))
	for _, record := range records {
		value, err := s.Get(record.ID)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func publicationRecordFrom(value Publication) (publicationRecord, error) {
	plan, err := json.Marshal(value.Plan)
	if err != nil {
		return publicationRecord{}, err
	}
	backups, err := json.Marshal(value.ProtectionBackupIDs)
	if err != nil {
		return publicationRecord{}, err
	}
	fences, err := json.Marshal(value.Fences)
	if err != nil {
		return publicationRecord{}, err
	}
	return publicationRecord{
		ID: value.ID, IdempotencyKey: publicationIdempotencyKey(value), SourceJobID: value.SourceJobID, RoomID: value.RoomID, Status: string(value.Status), Outcome: string(value.Outcome),
		TopologyRevision: value.Plan.TopologyRevision, PlanHash: value.Plan.PlanHash, PlanJSON: string(plan),
		ProtectionBackupJSON: string(backups), FencesJSON: string(fences), CommitDecision: value.CommitDecision,
		RestartRequired: value.RestartRequired, ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage,
		CommitDecidedAt: value.CommitDecidedAt, FinishedAt: value.FinishedAt,
		CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
	}, nil
}

func publicationFromRecord(record publicationRecord) (Publication, error) {
	var plan Plan
	var backups []string
	var fences []Fence
	if err := strictJSON(record.PlanJSON, &plan); err != nil {
		return Publication{}, fmt.Errorf("decode publication plan: %w", err)
	}
	if err := strictJSON(record.ProtectionBackupJSON, &backups); err != nil {
		return Publication{}, fmt.Errorf("decode publication backups: %w", err)
	}
	if err := strictJSON(record.FencesJSON, &fences); err != nil {
		return Publication{}, fmt.Errorf("decode publication fences: %w", err)
	}
	if err := validatePlan(plan); err != nil || plan.PlanHash != record.PlanHash || plan.TopologyRevision != record.TopologyRevision {
		return Publication{}, errors.Join(ErrInvalidInput, err)
	}
	value := Publication{
		ID: record.ID, SourceJobID: record.SourceJobID, RoomID: record.RoomID, Status: Status(record.Status), Outcome: Outcome(record.Outcome),
		Plan: plan, ProtectionBackupIDs: backups, Fences: fences, CommitDecision: record.CommitDecision,
		RestartRequired: record.RestartRequired, ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage,
		CommitDecidedAt: utcPointer(record.CommitDecidedAt), FinishedAt: utcPointer(record.FinishedAt),
		CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
	if err := validatePublication(value); err != nil {
		return Publication{}, err
	}
	return value, nil
}

func targetRecordFrom(publicationID string, value TargetResult, createdAt time.Time) targetRecord {
	return targetRecord{
		ID: targetResultID(publicationID, value.TargetID, value.InstallationID), PublicationID: publicationID,
		TargetID: value.TargetID, InstallationID: value.InstallationID, Status: string(value.Status),
		CacheEnsured: value.CacheEnsured, Prepared: value.Prepared, Published: value.Published,
		Completed: value.Completed, RolledBack: value.RolledBack, ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage,
		PreparedAt: value.PreparedAt, PublishedAt: value.PublishedAt, CompletedAt: value.CompletedAt, RolledBackAt: value.RolledBackAt,
		CreatedAt: createdAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
	}
}

func targetFromRecord(record targetRecord) TargetResult {
	return TargetResult{
		TargetID: record.TargetID, InstallationID: record.InstallationID, Status: Status(record.Status),
		CacheEnsured: record.CacheEnsured, Prepared: record.Prepared, Published: record.Published,
		Completed: record.Completed, RolledBack: record.RolledBack, ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage,
		PreparedAt: utcPointer(record.PreparedAt), PublishedAt: utcPointer(record.PublishedAt),
		CompletedAt: utcPointer(record.CompletedAt), RolledBackAt: utcPointer(record.RolledBackAt), UpdatedAt: record.UpdatedAt.UTC(),
	}
}

func validatePublication(value Publication) error {
	if !validID(value.ID) || value.SourceJobID != "" && !validID(value.SourceJobID) || value.RoomID != value.Plan.RoomID || !validStatus(value.Status) || !validOutcome(value.Outcome) {
		return ErrInvalidInput
	}
	return validatePlan(value.Plan)
}

func validStatus(value Status) bool {
	switch value {
	case StatusPreviewed, StatusPreparing, StatusPrepared, StatusPublishing, StatusCommitted, StatusCompleting, StatusSucceeded, StatusFailed, StatusRolledBack, StatusRecoveryRequired:
		return true
	default:
		return false
	}
}

func validOutcome(value Outcome) bool {
	return value == OutcomeFull || value == OutcomePartial || value == OutcomeNone
}

func targetResultID(publicationID, targetID, installationID string) string {
	return hashBytes([]byte(publicationID + "\x00" + targetID + "\x00" + installationID))
}

func publicationIdempotencyKey(value Publication) string {
	if value.SourceJobID != "" {
		return "job:" + value.SourceJobID
	}
	return "operation:" + value.ID
}

func strictJSON(value string, target interface{}) error {
	decoder := json.NewDecoder(io.LimitReader(bytes.NewBufferString(value), 128<<20))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrInvalidInput
		}
		return err
	}
	return nil
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
