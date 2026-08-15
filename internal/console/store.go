package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

var ErrRunNotFound = errors.New("command run not found")

var ErrDefinitionNotFound = errors.New("command definition not found")

type runRecord struct {
	ID               string    `gorm:"primary_key;type:char(36)"`
	RoomID           string    `gorm:"type:varchar(255);index;not null"`
	WorldID          string    `gorm:"type:varchar(255);index;not null"`
	Mode             string    `gorm:"type:varchar(16);not null"`
	CommandID        string    `gorm:"type:varchar(64)"`
	Name             string    `gorm:"type:varchar(128);not null"`
	Risk             string    `gorm:"type:varchar(16);not null"`
	Arguments        string    `gorm:"type:text"`
	RawCommand       string    `gorm:"type:text"`
	Status           string    `gorm:"type:varchar(16);index;not null"`
	Message          string    `gorm:"type:text"`
	ErrorCode        string    `gorm:"type:varchar(64)"`
	ErrorMessage     string    `gorm:"type:text"`
	CreatedAt        time.Time `gorm:"index;not null"`
	FinishedAt       *time.Time
	TransportOutcome string `gorm:"type:varchar(16)"`
	ExecutionOutcome string `gorm:"type:varchar(16)"`
	OperationID      string `gorm:"type:varchar(80);index"`
	OperationKey     string `gorm:"type:varchar(80)"`
	TargetID         string `gorm:"type:varchar(255);index"`
	AgentID          string `gorm:"type:varchar(255);index"`
	TopologyRevision string `gorm:"type:varchar(80)"`
	ObservedAt       *time.Time
}

type definitionRecord struct {
	ID          string    `gorm:"primary_key;type:char(36)"`
	Name        string    `gorm:"type:varchar(80);not null"`
	Description string    `gorm:"type:varchar(300);not null"`
	Category    string    `gorm:"type:varchar(80);index;not null"`
	Script      string    `gorm:"type:text;not null"`
	Parameters  string    `gorm:"type:text;not null"`
	CreatedAt   time.Time `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"not null"`
}

type Store struct {
	db               *gorm.DB
	runsTable        string
	definitionsTable string
	now              func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	prefix := strings.TrimSpace(tablePrefix)
	return &Store{
		db: db, runsTable: prefix + "command_run", definitionsTable: prefix + "command_definition", now: time.Now,
	}
}

func (s *Store) Migrate() error {
	if err := s.db.Table(s.runsTable).AutoMigrate(&runRecord{}).Error; err != nil {
		return fmt.Errorf("migrate command runs: %w", err)
	}
	if err := s.db.Table(s.definitionsTable).AutoMigrate(&definitionRecord{}).Error; err != nil {
		return fmt.Errorf("migrate command definitions: %w", err)
	}
	return nil
}

func (s *Store) Create(run Run) (Run, error) {
	arguments, err := json.Marshal(run.Arguments)
	if err != nil {
		return Run{}, fmt.Errorf("encode command arguments: %w", err)
	}
	run.ID = uuid.NewString()
	run.Status = RunSending
	run.LogQuery = run.ID
	run.CreatedAt = s.now().UTC()
	record := recordFromRun(run, string(arguments))
	if err := s.db.Table(s.runsTable).Create(&record).Error; err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) Complete(runID string, sendErr error) (Run, error) {
	return s.CompleteDelivery(runID, Delivery{TransportOutcome: "sent", ExecutionOutcome: "unknown"}, sendErr)
}

func (s *Store) CompleteDelivery(runID string, delivery Delivery, sendErr error) (Run, error) {
	now := s.now().UTC()
	message := strings.TrimSpace(delivery.Message)
	if message == "" {
		message = "分片控制台已接收命令；DST 业务执行结果未知"
	}
	updates := map[string]interface{}{
		"status": RunSent, "message": message, "finished_at": now, "error_code": "", "error_message": "",
		"transport_outcome": delivery.TransportOutcome, "execution_outcome": delivery.ExecutionOutcome,
		"operation_id": delivery.OperationID, "operation_key": delivery.OperationKey, "target_id": delivery.TargetID,
		"agent_id": delivery.AgentID, "topology_revision": delivery.TopologyRevision, "observed_at": delivery.ObservedAt,
	}
	if sendErr != nil {
		updates["status"] = RunFailed
		updates["message"] = "命令发送失败"
		updates["error_code"] = "COMMAND_SEND_FAILED"
		updates["error_message"] = sendErr.Error()
		updates["transport_outcome"] = "failed"
		updates["execution_outcome"] = "none"
	}
	result := s.db.Table(s.runsTable).Where("id = ? AND status = ?", runID, RunSending).Updates(updates)
	if result.Error != nil {
		return Run{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Run{}, ErrRunNotFound
	}
	return s.Get(runID)
}

func (s *Store) Get(runID string) (Run, error) {
	var record runRecord
	result := s.db.Table(s.runsTable).Where("id = ?", runID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Run{}, ErrRunNotFound
	}
	if result.Error != nil {
		return Run{}, result.Error
	}
	return runFromRecord(record)
}

func (s *Store) List(filter ListFilter) ([]Run, int, error) {
	query := s.db.Table(s.runsTable)
	if filter.RoomID != "" {
		query = query.Where("room_id = ?", filter.RoomID)
	}
	if filter.WorldID != "" {
		query = query.Where("world_id = ?", filter.WorldID)
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
	var records []runRecord
	if err := query.Order("created_at DESC").Limit(limit).Offset(offset).Find(&records).Error; err != nil {
		return nil, 0, err
	}
	runs := make([]Run, 0, len(records))
	for _, record := range records {
		run, err := runFromRecord(record)
		if err != nil {
			return nil, 0, err
		}
		runs = append(runs, run)
	}
	return runs, total, nil
}

func (s *Store) DeleteRuns(filter ListFilter) (int64, error) {
	query := s.db.Table(s.runsTable)
	if filter.RoomID != "" {
		query = query.Where("room_id = ?", filter.RoomID)
	}
	if filter.WorldID != "" {
		query = query.Where("world_id = ?", filter.WorldID)
	}
	result := query.Delete(&runRecord{})
	return result.RowsAffected, result.Error
}

func (s *Store) CreateDefinition(definition Definition) (Definition, error) {
	parameters, err := json.Marshal(definition.Parameters)
	if err != nil {
		return Definition{}, fmt.Errorf("encode command parameters: %w", err)
	}
	now := s.now().UTC()
	definition.ID = uuid.NewString()
	record := definitionRecord{
		ID: definition.ID, Name: definition.Name, Description: definition.Description,
		Category: definition.Category, Script: definition.Script, Parameters: string(parameters),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.db.Table(s.definitionsTable).Create(&record).Error; err != nil {
		return Definition{}, err
	}
	return definitionFromRecord(record)
}

func (s *Store) UpdateDefinition(definition Definition) (Definition, error) {
	parameters, err := json.Marshal(definition.Parameters)
	if err != nil {
		return Definition{}, fmt.Errorf("encode command parameters: %w", err)
	}
	updates := map[string]interface{}{
		"name": definition.Name, "description": definition.Description, "category": definition.Category,
		"script": definition.Script, "parameters": string(parameters), "updated_at": s.now().UTC(),
	}
	result := s.db.Table(s.definitionsTable).Where("id = ?", definition.ID).Updates(updates)
	if result.Error != nil {
		return Definition{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Definition{}, ErrDefinitionNotFound
	}
	return s.Definition(definition.ID)
}

func (s *Store) DeleteDefinition(id string) error {
	result := s.db.Table(s.definitionsTable).Where("id = ?", id).Delete(&definitionRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrDefinitionNotFound
	}
	return nil
}

func (s *Store) Definition(id string) (Definition, error) {
	var record definitionRecord
	result := s.db.Table(s.definitionsTable).Where("id = ?", id).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Definition{}, ErrDefinitionNotFound
	}
	if result.Error != nil {
		return Definition{}, result.Error
	}
	return definitionFromRecord(record)
}

func (s *Store) Definitions() ([]Definition, error) {
	var records []definitionRecord
	if err := s.db.Table(s.definitionsTable).Order("category ASC, name ASC, id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	definitions := make([]Definition, 0, len(records))
	for _, record := range records {
		definition, err := definitionFromRecord(record)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func definitionFromRecord(record definitionRecord) (Definition, error) {
	parameters := make([]Parameter, 0)
	if record.Parameters != "" && record.Parameters != "null" {
		if err := json.Unmarshal([]byte(record.Parameters), &parameters); err != nil {
			return Definition{}, fmt.Errorf("decode command parameters: %w", err)
		}
	}
	return Definition{
		ID: record.ID, Name: record.Name, Description: record.Description, Category: record.Category,
		Risk: RiskCritical, Parameters: parameters, Script: record.Script, IsBuiltin: false,
	}, nil
}

func recordFromRun(run Run, arguments string) runRecord {
	return runRecord{
		ID: run.ID, RoomID: run.RoomID, WorldID: run.WorldID, Mode: run.Mode, CommandID: run.CommandID,
		Name: run.Name, Risk: string(run.Risk), Arguments: arguments, RawCommand: run.RawCommand,
		Status: string(run.Status), Message: run.Message, ErrorCode: run.ErrorCode, ErrorMessage: run.ErrorMessage,
		CreatedAt: run.CreatedAt, FinishedAt: run.FinishedAt, TransportOutcome: run.TransportOutcome,
		ExecutionOutcome: run.ExecutionOutcome, OperationID: run.OperationID, OperationKey: run.OperationKey,
		TargetID: run.TargetID, AgentID: run.AgentID, TopologyRevision: run.TopologyRevision, ObservedAt: run.ObservedAt,
	}
}

func runFromRecord(record runRecord) (Run, error) {
	arguments := make(map[string]interface{})
	if record.Arguments != "" && record.Arguments != "null" {
		if err := json.Unmarshal([]byte(record.Arguments), &arguments); err != nil {
			return Run{}, fmt.Errorf("decode command arguments: %w", err)
		}
	}
	return Run{
		ID: record.ID, RoomID: record.RoomID, WorldID: record.WorldID, Mode: record.Mode, CommandID: record.CommandID,
		Name: record.Name, Risk: Risk(record.Risk), Arguments: arguments, RawCommand: record.RawCommand,
		Status: RunStatus(record.Status), Message: record.Message, ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage,
		LogQuery: record.ID, CreatedAt: record.CreatedAt, FinishedAt: record.FinishedAt,
		TransportOutcome: record.TransportOutcome, ExecutionOutcome: record.ExecutionOutcome,
		OperationID: record.OperationID, OperationKey: record.OperationKey, TargetID: record.TargetID,
		AgentID: record.AgentID, TopologyRevision: record.TopologyRevision, ObservedAt: record.ObservedAt,
	}, nil
}
