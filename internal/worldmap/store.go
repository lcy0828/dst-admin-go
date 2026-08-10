package worldmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

var ErrMapNotFound = errors.New("map not found")

type mapRecord struct {
	ID              string    `gorm:"primary_key;type:char(36)"`
	RoomID          string    `gorm:"type:varchar(255);index;not null"`
	WorldID         string    `gorm:"type:varchar(255);index;not null"`
	SessionID       string    `gorm:"type:text;not null"`
	SessionLabel    string    `gorm:"type:varchar(255);not null"`
	Status          string    `gorm:"type:varchar(16);index;not null"`
	Stage           string    `gorm:"type:varchar(32)"`
	LayersJSON      string    `gorm:"type:text;not null"`
	Width           int       `gorm:"not null"`
	Height          int       `gorm:"not null"`
	FeatureCount    int       `gorm:"not null"`
	WarningCount    int       `gorm:"not null"`
	SourceSHA256    string    `gorm:"type:char(64)"`
	RendererVersion string    `gorm:"type:varchar(128)"`
	Log             string    `gorm:"type:text"`
	ErrorMessage    string    `gorm:"type:text"`
	SourceJobID     string    `gorm:"type:char(36);index;not null"`
	CreatedAt       time.Time `gorm:"index;not null"`
	FinishedAt      *time.Time
}

type Store struct {
	db    *gorm.DB
	table string
	now   func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	return &Store{db: db, table: strings.TrimSpace(tablePrefix) + "world_map", now: time.Now}
}

func (s *Store) Migrate() error {
	if err := s.db.Table(s.table).AutoMigrate(&mapRecord{}).Error; err != nil {
		return fmt.Errorf("migrate world maps: %w", err)
	}
	now := s.now().UTC()
	if err := s.db.Table(s.table).Where("status = ?", "running").Updates(map[string]interface{}{
		"status": "failed", "stage": "interrupted", "error_message": "服务重启导致地图生成中断，可重新生成", "finished_at": now,
	}).Error; err != nil {
		return fmt.Errorf("recover world maps: %w", err)
	}
	return nil
}

func (s *Store) Begin(value Map) (Map, error) {
	value.Status = "running"
	value.Stage = "snapshot"
	value.CreatedAt = s.now().UTC()
	record, err := recordFromMap(value)
	if err != nil {
		return Map{}, err
	}
	if err := s.db.Table(s.table).Create(&record).Error; err != nil {
		return Map{}, err
	}
	return value, nil
}

func (s *Store) Stage(id, stage string) error {
	result := s.db.Table(s.table).Where("id = ? AND status = ?", id, "running").Update("stage", stage)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrMapNotFound
	}
	return nil
}

func (s *Store) Complete(id, status, stage, logText, errorMessage string, value Map) (Map, error) {
	now := s.now().UTC()
	result := s.db.Table(s.table).Where("id = ? AND status = ?", id, "running").Updates(map[string]interface{}{
		"status": status, "stage": stage, "log": logText, "error_message": errorMessage,
		"width": value.Width, "height": value.Height, "feature_count": value.FeatureCount,
		"warning_count": value.WarningCount, "source_sha256": value.SourceSHA256,
		"renderer_version": value.RendererVersion, "finished_at": now,
	})
	if result.Error != nil {
		return Map{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Map{}, ErrMapNotFound
	}
	return s.Get(id)
}

func (s *Store) Get(id string) (Map, error) {
	var record mapRecord
	result := s.db.Table(s.table).Where("id = ?", id).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Map{}, ErrMapNotFound
	}
	if result.Error != nil {
		return Map{}, result.Error
	}
	return mapFromRecord(record)
}

func (s *Store) List(roomID string) ([]Map, error) {
	var records []mapRecord
	if err := s.db.Table(s.table).Where("room_id = ?", roomID).Order("created_at DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]Map, 0, len(records))
	for _, record := range records {
		value, err := mapFromRecord(record)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func (s *Store) Successful(roomID, worldID string) ([]Map, error) {
	return s.ByStatus(roomID, worldID, "succeeded")
}

func (s *Store) ByStatus(roomID, worldID, status string) ([]Map, error) {
	var records []mapRecord
	if err := s.db.Table(s.table).Where("room_id = ? AND world_id = ? AND status = ?", roomID, worldID, status).Order("created_at DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]Map, 0, len(records))
	for _, record := range records {
		value, err := mapFromRecord(record)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func (s *Store) Delete(id string) error {
	result := s.db.Table(s.table).Where("id = ?", id).Delete(&mapRecord{})
	return result.Error
}

func recordFromMap(value Map) (mapRecord, error) {
	layers, err := json.Marshal(value.Layers)
	if err != nil {
		return mapRecord{}, err
	}
	return mapRecord{
		ID: value.ID, RoomID: value.RoomID, WorldID: value.WorldID, SessionID: value.SessionID,
		SessionLabel: value.SessionLabel, Status: value.Status, Stage: value.Stage, LayersJSON: string(layers),
		Width: value.Width, Height: value.Height, Log: value.Log, ErrorMessage: value.ErrorMessage,
		FeatureCount: value.FeatureCount, WarningCount: value.WarningCount, SourceSHA256: value.SourceSHA256,
		RendererVersion: value.RendererVersion,
		SourceJobID:     value.SourceJobID, CreatedAt: value.CreatedAt, FinishedAt: value.FinishedAt,
	}, nil
}

func mapFromRecord(record mapRecord) (Map, error) {
	var layers []Layer
	if err := json.Unmarshal([]byte(record.LayersJSON), &layers); err != nil {
		return Map{}, err
	}
	return Map{
		ID: record.ID, RoomID: record.RoomID, WorldID: record.WorldID, SessionID: record.SessionID,
		SessionLabel: record.SessionLabel, Status: record.Status, Stage: record.Stage, Layers: layers,
		Width: record.Width, Height: record.Height, Log: record.Log, ErrorMessage: record.ErrorMessage,
		FeatureCount: record.FeatureCount, WarningCount: record.WarningCount, SourceSHA256: record.SourceSHA256,
		RendererVersion: record.RendererVersion,
		SourceJobID:     record.SourceJobID, CreatedAt: record.CreatedAt, FinishedAt: record.FinishedAt,
	}, nil
}
