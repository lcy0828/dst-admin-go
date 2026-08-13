package runtimeaudit

import (
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type eventRecord struct {
	ID               uint64    `gorm:"primary_key"`
	RoomID           string    `gorm:"type:varchar(255);not null;index:idx_runtime_audit_room_time"`
	WorldID          string    `gorm:"type:varchar(255);not null;index:idx_runtime_audit_world_time"`
	RoomDirectory    string    `gorm:"type:varchar(128);not null"`
	WorldDirectory   string    `gorm:"type:varchar(128);not null"`
	Type             string    `gorm:"type:varchar(40);not null;index"`
	Action           string    `gorm:"type:varchar(32)"`
	Source           string    `gorm:"type:varchar(32);not null"`
	PreviousState    string    `gorm:"type:varchar(32)"`
	RuntimeState     string    `gorm:"type:varchar(32)"`
	ReasonCode       string    `gorm:"type:varchar(64)"`
	Message          string    `gorm:"type:text"`
	JobID            string    `gorm:"type:varchar(64);index"`
	RequestID        string    `gorm:"type:varchar(128);index"`
	ExpectedExit     bool      `gorm:"not null"`
	ExpectedObserved bool      `gorm:"not null"`
	OccurredAt       time.Time `gorm:"not null;index:idx_runtime_audit_room_time;index:idx_runtime_audit_world_time"`
}

type Store struct {
	db    *gorm.DB
	table string
}

func NewStore(db *gorm.DB, prefix string) *Store {
	return &Store{db: db, table: strings.TrimSpace(prefix) + "shard_runtime_event"}
}

func (s *Store) Migrate() error {
	if err := s.db.Table(s.table).AutoMigrate(&eventRecord{}).Error; err != nil {
		return fmt.Errorf("migrate shard runtime events: %w", err)
	}
	return nil
}

func (s *Store) Append(event Event) (Event, error) {
	record := recordFromEvent(event)
	if err := s.db.Table(s.table).Create(&record).Error; err != nil {
		return Event{}, err
	}
	return eventFromRecord(record), nil
}

func (s *Store) List(roomID string, filter ListFilter) (List, error) {
	query := s.db.Table(s.table).Where("room_id = ?", roomID)
	if filter.WorldID != "" {
		query = query.Where("world_id = ?", filter.WorldID)
	}
	if filter.Type != "" {
		query = query.Where("type = ?", filter.Type)
	}
	var total int
	if err := query.Count(&total).Error; err != nil {
		return List{}, err
	}
	var records []eventRecord
	if err := query.Order("occurred_at DESC, id DESC").Limit(filter.Limit).Find(&records).Error; err != nil {
		return List{}, err
	}
	items := make([]Event, 0, len(records))
	for _, record := range records {
		items = append(items, eventFromRecord(record))
	}
	return List{Items: items, Total: total}, nil
}

func (s *Store) LatestExit(roomID, worldID string) (*Event, error) {
	var record eventRecord
	err := s.db.Table(s.table).
		Where("room_id = ? AND world_id = ? AND type IN (?)", roomID, worldID, []EventType{EventStopped, EventUnexpectedExit}).
		Order("occurred_at DESC, id DESC").First(&record).Error
	if gorm.IsRecordNotFoundError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	event := eventFromRecord(record)
	return &event, nil
}

func (s *Store) ConsumeExpectedExit(roomID, worldID string, since time.Time) (*Event, error) {
	tx := s.db.Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	var record eventRecord
	err := tx.Table(s.table).
		Where("room_id = ? AND world_id = ? AND expected_exit = ? AND expected_observed = ? AND occurred_at >= ?", roomID, worldID, true, false, since.UTC()).
		Order("occurred_at DESC, id DESC").First(&record).Error
	if gorm.IsRecordNotFoundError(err) {
		tx.Rollback()
		return nil, nil
	}
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Table(s.table).Where("id = ? AND expected_observed = ?", record.ID, false).UpdateColumn("expected_observed", true).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	record.ExpectedObserved = true
	event := eventFromRecord(record)
	return &event, nil
}

func recordFromEvent(event Event) eventRecord {
	return eventRecord{
		ID: event.ID, RoomID: event.RoomID, WorldID: event.WorldID, RoomDirectory: event.RoomDirectory,
		WorldDirectory: event.WorldDirectory, Type: string(event.Type), Action: event.Action, Source: string(event.Source),
		PreviousState: event.PreviousState, RuntimeState: event.RuntimeState, ReasonCode: event.ReasonCode,
		Message: event.Message, JobID: event.JobID, RequestID: event.RequestID, ExpectedExit: event.ExpectedExit,
		ExpectedObserved: event.ExpectedObserved, OccurredAt: event.OccurredAt.UTC(),
	}
}

func eventFromRecord(record eventRecord) Event {
	return Event{
		ID: record.ID, RoomID: record.RoomID, WorldID: record.WorldID, RoomDirectory: record.RoomDirectory,
		WorldDirectory: record.WorldDirectory, Type: EventType(record.Type), Action: record.Action, Source: Source(record.Source),
		PreviousState: record.PreviousState, RuntimeState: record.RuntimeState, ReasonCode: record.ReasonCode,
		Message: record.Message, JobID: record.JobID, RequestID: record.RequestID, ExpectedExit: record.ExpectedExit,
		ExpectedObserved: record.ExpectedObserved, OccurredAt: record.OccurredAt.UTC(),
	}
}
