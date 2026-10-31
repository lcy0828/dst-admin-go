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
	TargetID         string    `gorm:"type:varchar(160);index"`
	AgentID          string    `gorm:"type:varchar(128);index"`
	OperationID      string    `gorm:"type:varchar(128);index"`
	OperationKey     string    `gorm:"type:varchar(128);index"`
	LeaseID          string    `gorm:"type:varchar(128);index"`
	FencingToken     uint64    `gorm:"not null;default:0"`
	TopologyRevision string    `gorm:"type:varchar(128)"`
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
	if !filter.From.IsZero() {
		query = query.Where("occurred_at >= ?", filter.From.UTC())
	}
	if !filter.To.IsZero() {
		query = query.Where("occurred_at <= ?", filter.To.UTC())
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

func (s *Store) FirstSuccessfulStart() (*Event, error) {
	// Examine evidence in bounded batches. A failed first attempt must not hide
	// a later successful start, and a single loaded world must not complete a
	// grouped start. Historical evidence predating accepted requests retains its
	// old meaning. This reads audit records only; it never changes the Runtime.
	var firstRequest eventRecord
	err := s.db.Table(s.table).Where("type IN (?)", []EventType{EventStartRequested, EventRestartRequested}).
		Order("occurred_at ASC, id ASC").First(&firstRequest).Error
	if err != nil && !gorm.IsRecordNotFoundError(err) {
		return nil, err
	}
	var cursor eventRecord
	for {
		query := s.db.Table(s.table).Where("type = ? OR previous_state = ?", string(EventRunning), "running")
		if cursor.ID != 0 {
			query = query.Where("occurred_at > ? OR (occurred_at = ? AND id > ?)", cursor.OccurredAt, cursor.OccurredAt, cursor.ID)
		}
		var candidates []eventRecord
		if err := query.Order("occurred_at ASC, id ASC").Limit(128).Find(&candidates).Error; err != nil {
			return nil, err
		}
		for _, candidate := range candidates {
			complete, err := s.startScopeComplete(candidate, firstRequest)
			if err != nil {
				return nil, err
			}
			if complete {
				event := eventFromRecord(candidate)
				return &event, nil
			}
		}
		if len(candidates) < 128 {
			return nil, nil
		}
		cursor = candidates[len(candidates)-1]
	}
}

func (s *Store) startScopeComplete(candidate, firstRequest eventRecord) (bool, error) {
	var requested eventRecord
	err := s.db.Table(s.table).Where("room_id = ? AND world_id = ? AND type IN (?) AND (occurred_at < ? OR (occurred_at = ? AND id <= ?))",
		candidate.RoomID, candidate.WorldID, []EventType{EventStartRequested, EventRestartRequested}, candidate.OccurredAt, candidate.OccurredAt, candidate.ID).
		Order("occurred_at DESC, id DESC").First(&requested).Error
	if gorm.IsRecordNotFoundError(err) {
		// Do not use an unrelated world's observation to complete a newer start.
		return firstRequest.ID == 0 || candidate.OccurredAt.Before(firstRequest.OccurredAt) ||
			(candidate.OccurredAt.Equal(firstRequest.OccurredAt) && candidate.ID < firstRequest.ID), nil
	}
	if err != nil {
		return false, err
	}
	if requested.JobID == "" {
		return true, nil // Legacy requests did not carry a grouped operation ID.
	}
	var scope []eventRecord
	if err := s.db.Table(s.table).Where("room_id = ? AND job_id = ? AND type IN (?)", requested.RoomID, requested.JobID,
		[]EventType{EventStartRequested, EventRestartRequested}).Find(&scope).Error; err != nil {
		return false, err
	}
	for _, target := range scope {
		var observed eventRecord
		err := s.db.Table(s.table).Where("room_id = ? AND world_id = ? AND occurred_at >= ? AND (occurred_at < ? OR (occurred_at = ? AND id <= ?)) AND type IN (?)",
			target.RoomID, target.WorldID, target.OccurredAt, candidate.OccurredAt, candidate.OccurredAt, candidate.ID,
			[]EventType{EventSessionStarted, EventRunning, EventFailed, EventStopped, EventUnexpectedExit}).
			Order("occurred_at DESC, id DESC").First(&observed).Error
		if gorm.IsRecordNotFoundError(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		// A later stop/failure invalidates an earlier load. For legacy evidence
		// the candidate itself may establish the state immediately before exit.
		if observed.Type != string(EventRunning) && observed.RuntimeState != "running" &&
			!(observed.ID == candidate.ID && observed.PreviousState == "running") {
			return false, nil
		}
	}
	return true, nil
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

func (s *Store) AnnotateAction(event Event) error {
	if event.JobID == "" || event.RoomID == "" || event.WorldID == "" || event.Action == "" {
		return nil
	}
	var record eventRecord
	result := s.db.Table(s.table).
		Where("room_id = ? AND world_id = ? AND job_id = ? AND action = ?", event.RoomID, event.WorldID, event.JobID, event.Action).
		Order("occurred_at DESC, id DESC").First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return nil
	}
	if result.Error != nil {
		return result.Error
	}
	return s.db.Table(s.table).Where("id = ?", record.ID).Updates(map[string]interface{}{
		"target_id": event.TargetID, "agent_id": event.AgentID, "operation_id": event.OperationID,
		"operation_key": event.OperationKey, "lease_id": event.LeaseID, "fencing_token": event.FencingToken,
		"topology_revision": event.TopologyRevision,
	}).Error
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
		TargetID: event.TargetID, AgentID: event.AgentID, OperationID: event.OperationID, OperationKey: event.OperationKey,
		LeaseID: event.LeaseID, FencingToken: event.FencingToken, TopologyRevision: event.TopologyRevision,
		ExpectedObserved: event.ExpectedObserved, OccurredAt: event.OccurredAt.UTC(),
	}
}

func eventFromRecord(record eventRecord) Event {
	return Event{
		ID: record.ID, RoomID: record.RoomID, WorldID: record.WorldID, RoomDirectory: record.RoomDirectory,
		WorldDirectory: record.WorldDirectory, Type: EventType(record.Type), Action: record.Action, Source: Source(record.Source),
		PreviousState: record.PreviousState, RuntimeState: record.RuntimeState, ReasonCode: record.ReasonCode,
		Message: record.Message, JobID: record.JobID, RequestID: record.RequestID, ExpectedExit: record.ExpectedExit,
		TargetID: record.TargetID, AgentID: record.AgentID, OperationID: record.OperationID, OperationKey: record.OperationKey,
		LeaseID: record.LeaseID, FencingToken: record.FencingToken, TopologyRevision: record.TopologyRevision,
		ExpectedObserved: record.ExpectedObserved, OccurredAt: record.OccurredAt.UTC(),
	}
}
