package roomprovision

import (
	"errors"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type operationRecord struct {
	ID               string    `gorm:"primary_key;type:char(36)"`
	RoomID           string    `gorm:"type:varchar(255);index;not null"`
	RoomName         string    `gorm:"type:varchar(128);not null"`
	TopologyRevision string    `gorm:"type:varchar(128);not null"`
	AppliedRevision  string    `gorm:"type:varchar(128)"`
	LeaseID          string    `gorm:"type:char(36)"`
	FencingToken     uint64    `gorm:"not null"`
	Phase            string    `gorm:"type:varchar(32);index;not null"`
	Status           string    `gorm:"type:varchar(32);index;not null"`
	Failure          string    `gorm:"type:text"`
	SourceJobID      string    `gorm:"type:char(36);index"`
	CreatedAt        time.Time `gorm:"index;not null"`
	UpdatedAt        time.Time `gorm:"not null"`
}

type stepRecord struct {
	ID                   string    `gorm:"primary_key;type:varchar(128)"`
	OperationID          string    `gorm:"type:char(36);index;not null"`
	WorldID              string    `gorm:"type:varchar(255);index;not null"`
	WorldName            string    `gorm:"type:varchar(128);not null"`
	SourceTargetID       string    `gorm:"type:varchar(128);index"`
	SourceInstallationID string    `gorm:"type:varchar(64)"`
	TargetID             string    `gorm:"type:varchar(128);index;not null"`
	InstallationID       string    `gorm:"type:varchar(64);not null"`
	Cluster              string    `gorm:"type:varchar(64);not null"`
	Shard                string    `gorm:"type:varchar(64);not null"`
	MigrationID          string    `gorm:"type:varchar(128);index"`
	Phase                string    `gorm:"type:varchar(32);index;not null"`
	Size                 int64     `gorm:"not null"`
	SHA256               string    `gorm:"type:char(64)"`
	WasRunning           bool      `gorm:"not null;default:false"`
	RuntimeRestored      bool      `gorm:"not null;default:false"`
	Failure              string    `gorm:"type:text"`
	UpdatedAt            time.Time `gorm:"not null"`
}

type Store struct {
	db             *gorm.DB
	operationTable string
	stepTable      string
	now            func() time.Time
}

func NewStore(db *gorm.DB, prefix string) *Store {
	prefix = strings.TrimSpace(prefix)
	return &Store{db: db, operationTable: prefix + "room_provision_operation", stepTable: prefix + "room_provision_step", now: time.Now}
}

func (s *Store) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("room provision database is required")
	}
	if err := s.db.Table(s.operationTable).AutoMigrate(&operationRecord{}).Error; err != nil {
		return err
	}
	return s.db.Table(s.stepTable).AutoMigrate(&stepRecord{}).Error
}

func (s *Store) Create(value Operation, steps []Step) (Operation, error) {
	tx := s.db.Begin()
	if tx.Error != nil {
		return Operation{}, tx.Error
	}
	operation := operationRecordFrom(value)
	if err := tx.Table(s.operationTable).Create(&operation).Error; err != nil {
		tx.Rollback()
		return Operation{}, err
	}
	for _, value := range steps {
		step := stepRecordFrom(value)
		if err := tx.Table(s.stepTable).Create(&step).Error; err != nil {
			tx.Rollback()
			return Operation{}, err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return Operation{}, err
	}
	return s.Get(value.ID)
}

func (s *Store) Get(id string) (Operation, error) {
	var record operationRecord
	result := s.db.Table(s.operationTable).Where("id = ?", id).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Operation{}, ErrNotFound
	}
	if result.Error != nil {
		return Operation{}, result.Error
	}
	value := operationFromRecord(record)
	var steps []stepRecord
	if err := s.db.Table(s.stepTable).Where("operation_id = ?", id).Order("world_id ASC").Find(&steps).Error; err != nil {
		return Operation{}, err
	}
	value.Steps = make([]Step, 0, len(steps))
	for _, step := range steps {
		value.Steps = append(value.Steps, stepFromRecord(step))
	}
	return value, nil
}

func (s *Store) List(roomID string) ([]Operation, error) {
	var records []operationRecord
	query := s.db.Table(s.operationTable)
	if strings.TrimSpace(roomID) != "" {
		query = query.Where("room_id = ?", roomID)
	}
	if err := query.Order("created_at DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	values := make([]Operation, 0, len(records))
	for _, record := range records {
		value, err := s.Get(record.ID)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func (s *Store) Active() ([]Operation, error) {
	var records []operationRecord
	if err := s.db.Table(s.operationTable).Where("status IN (?)", []string{string(StatusRunning), string(StatusRecoveryRequired)}).Order("created_at ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	values := make([]Operation, 0, len(records))
	for _, record := range records {
		value, err := s.Get(record.ID)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func (s *Store) SaveOperation(value Operation) (Operation, error) {
	value.UpdatedAt = s.now().UTC()
	record := operationRecordFrom(value)
	result := s.db.Table(s.operationTable).Where("id = ?", value.ID).Updates(map[string]interface{}{
		"applied_revision": record.AppliedRevision, "lease_id": record.LeaseID, "fencing_token": record.FencingToken,
		"phase": record.Phase, "status": record.Status, "failure": record.Failure, "updated_at": record.UpdatedAt,
	})
	if result.Error != nil {
		return Operation{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Operation{}, ErrNotFound
	}
	return s.Get(value.ID)
}

func (s *Store) SaveStep(value Step) (Step, error) {
	value.UpdatedAt = s.now().UTC()
	record := stepRecordFrom(value)
	result := s.db.Table(s.stepTable).Where("id = ? AND operation_id = ?", value.ID, value.OperationID).Updates(map[string]interface{}{
		"migration_id": record.MigrationID, "phase": record.Phase, "size": record.Size,
		"sha256": record.SHA256, "was_running": record.WasRunning, "runtime_restored": record.RuntimeRestored,
		"failure": record.Failure, "updated_at": record.UpdatedAt,
	})
	if result.Error != nil {
		return Step{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Step{}, ErrNotFound
	}
	var saved stepRecord
	if err := s.db.Table(s.stepTable).Where("id = ?", value.ID).First(&saved).Error; err != nil {
		return Step{}, err
	}
	return stepFromRecord(saved), nil
}

func operationRecordFrom(value Operation) operationRecord {
	return operationRecord{
		ID: value.ID, RoomID: value.RoomID, RoomName: value.RoomName, TopologyRevision: value.TopologyRevision,
		AppliedRevision: value.AppliedRevision, LeaseID: value.LeaseID, FencingToken: value.FencingToken,
		Phase: value.Phase, Status: string(value.Status), Failure: value.Failure, SourceJobID: value.SourceJobID,
		CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
	}
}

func operationFromRecord(value operationRecord) Operation {
	return Operation{
		ID: value.ID, RoomID: value.RoomID, RoomName: value.RoomName, TopologyRevision: value.TopologyRevision,
		AppliedRevision: value.AppliedRevision, LeaseID: value.LeaseID, FencingToken: value.FencingToken,
		Phase: value.Phase, Status: Status(value.Status), Failure: value.Failure, SourceJobID: value.SourceJobID,
		CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
	}
}

func stepRecordFrom(value Step) stepRecord {
	return stepRecord{
		ID: value.ID, OperationID: value.OperationID, WorldID: value.WorldID, WorldName: value.WorldName,
		SourceTargetID: value.SourceTargetID, SourceInstallationID: value.SourceInstallationID,
		TargetID: value.TargetID, InstallationID: value.InstallationID, Cluster: value.Cluster, Shard: value.Shard,
		MigrationID: value.MigrationID, Phase: value.Phase, Size: value.Size, SHA256: value.SHA256,
		WasRunning: value.WasRunning, RuntimeRestored: value.RuntimeRestored,
		Failure: value.Failure, UpdatedAt: value.UpdatedAt.UTC(),
	}
}

func stepFromRecord(value stepRecord) Step {
	return Step{
		ID: value.ID, OperationID: value.OperationID, WorldID: value.WorldID, WorldName: value.WorldName,
		SourceTargetID: value.SourceTargetID, SourceInstallationID: value.SourceInstallationID,
		TargetID: value.TargetID, InstallationID: value.InstallationID, Cluster: value.Cluster, Shard: value.Shard,
		MigrationID: value.MigrationID, Phase: value.Phase, Size: value.Size, SHA256: value.SHA256,
		WasRunning: value.WasRunning, RuntimeRestored: value.RuntimeRestored,
		Failure: value.Failure, UpdatedAt: value.UpdatedAt.UTC(),
	}
}
