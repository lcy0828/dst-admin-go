package distributedbackup

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type setRecord struct {
	ID                    string `gorm:"primary_key;type:char(36)"`
	RoomID                string `gorm:"type:varchar(255);index;not null"`
	RoomName              string `gorm:"type:varchar(128);not null"`
	Name                  string `gorm:"type:varchar(128);not null"`
	Kind                  string `gorm:"type:varchar(24);index;not null"`
	Mode                  string `gorm:"type:varchar(32);not null"`
	ManifestVersion       int    `gorm:"not null"`
	TopologyRevision      string `gorm:"type:varchar(128);index;not null"`
	SharedSHA256          string `gorm:"type:char(64)"`
	Status                string `gorm:"type:varchar(24);index;not null"`
	Size                  int64  `gorm:"not null"`
	ContentSize           int64  `gorm:"not null"`
	FileCount             int    `gorm:"not null"`
	OriginalRunningWorlds string `gorm:"type:text;not null"`
	ManifestSHA256        string `gorm:"type:char(64)"`
	Failure               string `gorm:"type:text"`
	SourceJobID           string `gorm:"type:char(36);index"`
	VerifiedAt            *time.Time
	CreatedAt             time.Time `gorm:"index;not null"`
	UpdatedAt             time.Time `gorm:"not null"`
}

type partRecord struct {
	ID               string `gorm:"primary_key;type:varchar(128)"`
	SetID            string `gorm:"type:char(36);index;not null"`
	RoomID           string `gorm:"type:varchar(255);index;not null"`
	WorldID          string `gorm:"type:varchar(255);index;not null"`
	WorldName        string `gorm:"type:varchar(128);not null"`
	WorldRole        string `gorm:"type:varchar(24);not null"`
	TargetID         string `gorm:"type:varchar(128);index;not null"`
	InstallationID   string `gorm:"type:varchar(64);not null"`
	Cluster          string `gorm:"type:varchar(64);not null"`
	Shard            string `gorm:"type:varchar(64);not null"`
	TopologyRevision string `gorm:"type:varchar(128);not null"`
	FileName         string `gorm:"type:varchar(255);not null"`
	Status           string `gorm:"type:varchar(24);index;not null"`
	Size             int64  `gorm:"not null"`
	ContentSize      int64  `gorm:"not null"`
	FileCount        int    `gorm:"not null"`
	SHA256           string `gorm:"type:char(64)"`
	SharedSHA256     string `gorm:"type:char(64)"`
	Failure          string `gorm:"type:text"`
	VerifiedAt       *time.Time
	CreatedAt        time.Time `gorm:"not null"`
	UpdatedAt        time.Time `gorm:"not null"`
}

type operationRecord struct {
	ID                    string    `gorm:"primary_key;type:char(36)"`
	SetID                 string    `gorm:"type:char(36);index"`
	ProtectionSetID       string    `gorm:"type:char(36);index"`
	RoomID                string    `gorm:"type:varchar(255);index;not null"`
	Kind                  string    `gorm:"type:varchar(24);index;not null"`
	Phase                 string    `gorm:"type:varchar(32);index;not null"`
	Status                string    `gorm:"type:varchar(32);index;not null"`
	TopologyRevision      string    `gorm:"type:varchar(128);not null"`
	LeaseID               string    `gorm:"type:char(36)"`
	FencingToken          uint64    `gorm:"not null"`
	OriginalRunningWorlds string    `gorm:"type:text;not null"`
	Failure               string    `gorm:"type:text"`
	CreatedAt             time.Time `gorm:"index;not null"`
	UpdatedAt             time.Time `gorm:"not null"`
}

type Store struct {
	db             *gorm.DB
	setTable       string
	partTable      string
	operationTable string
	now            func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	prefix := strings.TrimSpace(tablePrefix)
	return &Store{db: db, setTable: prefix + "backup_set", partTable: prefix + "backup_part", operationTable: prefix + "backup_operation", now: time.Now}
}

func (s *Store) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("distributed backup database is required")
	}
	if err := s.db.Table(s.setTable).AutoMigrate(&setRecord{}).Error; err != nil {
		return fmt.Errorf("migrate backup sets: %w", err)
	}
	if err := s.db.Table(s.partTable).AutoMigrate(&partRecord{}).Error; err != nil {
		return fmt.Errorf("migrate backup parts: %w", err)
	}
	if err := s.db.Table(s.operationTable).AutoMigrate(&operationRecord{}).Error; err != nil {
		return fmt.Errorf("migrate backup operations: %w", err)
	}
	return nil
}

func (s *Store) CreateSet(value Set, parts []Part) (Set, error) {
	tx := s.db.Begin()
	if tx.Error != nil {
		return Set{}, tx.Error
	}
	record, err := setRecordFrom(value)
	if err != nil {
		tx.Rollback()
		return Set{}, err
	}
	if err := tx.Table(s.setTable).Create(&record).Error; err != nil {
		tx.Rollback()
		return Set{}, err
	}
	for _, part := range parts {
		item := partRecordFrom(part)
		if err := tx.Table(s.partTable).Create(&item).Error; err != nil {
			tx.Rollback()
			return Set{}, err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return Set{}, err
	}
	return s.GetSet(value.ID)
}

func (s *Store) GetSet(id string) (Set, error) {
	var record setRecord
	result := s.db.Table(s.setTable).Where("id = ?", id).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Set{}, ErrNotFound
	}
	if result.Error != nil {
		return Set{}, result.Error
	}
	value, err := setFromRecord(record)
	if err != nil {
		return Set{}, err
	}
	var records []partRecord
	if err := s.db.Table(s.partTable).Where("set_id = ?", id).Order("world_id ASC").Find(&records).Error; err != nil {
		return Set{}, err
	}
	value.Parts = make([]Part, 0, len(records))
	for _, item := range records {
		value.Parts = append(value.Parts, partFromRecord(item))
	}
	return value, nil
}

func (s *Store) ListSets(roomID string) ([]Set, error) {
	var records []setRecord
	query := s.db.Table(s.setTable)
	if strings.TrimSpace(roomID) != "" {
		query = query.Where("room_id = ?", roomID)
	}
	if err := query.Order("created_at DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	values := make([]Set, 0, len(records))
	for _, record := range records {
		value, err := s.GetSet(record.ID)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func (s *Store) SaveSet(value Set) (Set, error) {
	record, err := setRecordFrom(value)
	if err != nil {
		return Set{}, err
	}
	updates := map[string]interface{}{
		"shared_sha256": record.SharedSHA256, "status": record.Status, "size": record.Size,
		"content_size": record.ContentSize, "file_count": record.FileCount, "manifest_sha256": record.ManifestSHA256,
		"failure": record.Failure, "verified_at": record.VerifiedAt, "updated_at": record.UpdatedAt,
	}
	result := s.db.Table(s.setTable).Where("id = ?", value.ID).Updates(updates)
	if result.Error != nil {
		return Set{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Set{}, ErrNotFound
	}
	return s.GetSet(value.ID)
}

func (s *Store) SavePart(value Part) (Part, error) {
	value.UpdatedAt = s.now().UTC()
	record := partRecordFrom(value)
	updates := map[string]interface{}{
		"status": record.Status, "size": record.Size, "content_size": record.ContentSize, "file_count": record.FileCount,
		"sha256": record.SHA256, "shared_sha256": record.SharedSHA256, "failure": record.Failure,
		"verified_at": record.VerifiedAt, "updated_at": record.UpdatedAt,
	}
	result := s.db.Table(s.partTable).Where("id = ? AND set_id = ?", value.ID, value.SetID).Updates(updates)
	if result.Error != nil {
		return Part{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Part{}, ErrNotFound
	}
	var saved partRecord
	if err := s.db.Table(s.partTable).Where("id = ?", value.ID).First(&saved).Error; err != nil {
		return Part{}, err
	}
	return partFromRecord(saved), nil
}

func (s *Store) CreateOperation(value Operation) (Operation, error) {
	record, err := operationRecordFrom(value)
	if err != nil {
		return Operation{}, err
	}
	if err := s.db.Table(s.operationTable).Create(&record).Error; err != nil {
		return Operation{}, err
	}
	return operationFromRecord(record)
}

func (s *Store) SaveOperation(value Operation) (Operation, error) {
	value.UpdatedAt = s.now().UTC()
	record, err := operationRecordFrom(value)
	if err != nil {
		return Operation{}, err
	}
	updates := map[string]interface{}{
		"set_id": record.SetID, "protection_set_id": record.ProtectionSetID, "phase": record.Phase, "status": record.Status,
		"topology_revision": record.TopologyRevision, "lease_id": record.LeaseID, "fencing_token": record.FencingToken,
		"original_running_worlds": record.OriginalRunningWorlds, "failure": record.Failure, "updated_at": record.UpdatedAt,
	}
	result := s.db.Table(s.operationTable).Where("id = ?", value.ID).Updates(updates)
	if result.Error != nil {
		return Operation{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Operation{}, ErrNotFound
	}
	return s.Operation(value.ID)
}

func (s *Store) Operation(id string) (Operation, error) {
	var record operationRecord
	result := s.db.Table(s.operationTable).Where("id = ?", id).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Operation{}, ErrNotFound
	}
	if result.Error != nil {
		return Operation{}, result.Error
	}
	return operationFromRecord(record)
}

func (s *Store) ActiveOperations() ([]Operation, error) {
	var records []operationRecord
	if err := s.db.Table(s.operationTable).Where("status IN (?)", []string{string(OperationRunning), string(OperationRecoveryRequired)}).Order("created_at ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	values := make([]Operation, 0, len(records))
	for _, record := range records {
		value, err := operationFromRecord(record)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func setRecordFrom(value Set) (setRecord, error) {
	running, err := json.Marshal(value.OriginalRunningWorlds)
	if err != nil {
		return setRecord{}, err
	}
	return setRecord{
		ID: value.ID, RoomID: value.RoomID, RoomName: value.RoomName, Name: value.Name, Kind: value.Kind, Mode: value.Mode,
		ManifestVersion: value.ManifestVersion, TopologyRevision: value.TopologyRevision, SharedSHA256: value.SharedSHA256,
		Status: string(value.Status), Size: value.Size, ContentSize: value.ContentSize, FileCount: value.FileCount,
		OriginalRunningWorlds: string(running), ManifestSHA256: value.ManifestSHA256, Failure: value.Failure,
		SourceJobID: value.SourceJobID, VerifiedAt: value.VerifiedAt, CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
	}, nil
}

func setFromRecord(record setRecord) (Set, error) {
	var running []string
	if err := json.Unmarshal([]byte(record.OriginalRunningWorlds), &running); err != nil {
		return Set{}, err
	}
	return Set{
		ID: record.ID, RoomID: record.RoomID, RoomName: record.RoomName, Name: record.Name, Kind: record.Kind, Mode: record.Mode,
		ManifestVersion: record.ManifestVersion, TopologyRevision: record.TopologyRevision, SharedSHA256: record.SharedSHA256,
		Status: Status(record.Status), Size: record.Size, ContentSize: record.ContentSize, FileCount: record.FileCount,
		OriginalRunningWorlds: running, ManifestSHA256: record.ManifestSHA256, Failure: record.Failure, SourceJobID: record.SourceJobID,
		VerifiedAt: record.VerifiedAt, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}, nil
}

func partRecordFrom(value Part) partRecord {
	return partRecord{
		ID: value.ID, SetID: value.SetID, RoomID: value.RoomID, WorldID: value.WorldID, WorldName: value.WorldName,
		WorldRole: value.WorldRole, TargetID: value.TargetID, InstallationID: value.InstallationID, Cluster: value.Cluster,
		Shard: value.Shard, TopologyRevision: value.TopologyRevision, FileName: value.FileName, Status: string(value.Status),
		Size: value.Size, ContentSize: value.ContentSize, FileCount: value.FileCount, SHA256: value.SHA256,
		SharedSHA256: value.SharedSHA256, Failure: value.Failure, VerifiedAt: value.VerifiedAt,
		CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
	}
}

func partFromRecord(record partRecord) Part {
	return Part{
		ID: record.ID, SetID: record.SetID, RoomID: record.RoomID, WorldID: record.WorldID, WorldName: record.WorldName,
		WorldRole: record.WorldRole, TargetID: record.TargetID, InstallationID: record.InstallationID, Cluster: record.Cluster,
		Shard: record.Shard, TopologyRevision: record.TopologyRevision, FileName: record.FileName, Status: PartStatus(record.Status),
		Size: record.Size, ContentSize: record.ContentSize, FileCount: record.FileCount, SHA256: record.SHA256,
		SharedSHA256: record.SharedSHA256, Failure: record.Failure, VerifiedAt: record.VerifiedAt,
		CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
}

func operationRecordFrom(value Operation) (operationRecord, error) {
	running, err := json.Marshal(value.OriginalRunningWorlds)
	if err != nil {
		return operationRecord{}, err
	}
	return operationRecord{
		ID: value.ID, SetID: value.SetID, ProtectionSetID: value.ProtectionSetID, RoomID: value.RoomID, Kind: value.Kind,
		Phase: value.Phase, Status: string(value.Status), TopologyRevision: value.TopologyRevision, LeaseID: value.LeaseID,
		FencingToken: value.FencingToken, OriginalRunningWorlds: string(running), Failure: value.Failure,
		CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
	}, nil
}

func operationFromRecord(record operationRecord) (Operation, error) {
	var running []string
	if err := json.Unmarshal([]byte(record.OriginalRunningWorlds), &running); err != nil {
		return Operation{}, err
	}
	return Operation{
		ID: record.ID, SetID: record.SetID, ProtectionSetID: record.ProtectionSetID, RoomID: record.RoomID,
		Kind: record.Kind, Phase: record.Phase, Status: OperationStatus(record.Status), TopologyRevision: record.TopologyRevision,
		LeaseID: record.LeaseID, FencingToken: record.FencingToken, OriginalRunningWorlds: running,
		Failure: record.Failure, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}, nil
}
