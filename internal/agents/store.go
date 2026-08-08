package agents

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type agentRecord struct {
	ID            string `gorm:"type:varchar(128);primary_key"`
	Hostname      string `gorm:"type:varchar(255);not null"`
	OS            string `gorm:"type:varchar(64);not null;index"`
	Arch          string `gorm:"type:varchar(64);not null"`
	Version       string `gorm:"type:varchar(128);not null"`
	IPAddresses   string `gorm:"type:text;not null"`
	Status        string `gorm:"type:varchar(16);not null;index"`
	LastHeartbeat time.Time
	LastReportAt  *time.Time
	Capabilities  string `gorm:"type:text;not null"`
	Metrics       string `gorm:"type:text;not null"`
	Details       string `gorm:"type:text;not null"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type commandRecord struct {
	ID         string `gorm:"type:char(36);primary_key"`
	AgentID    string `gorm:"type:varchar(128);not null;index"`
	AgentName  string `gorm:"type:varchar(255);not null"`
	Action     string `gorm:"type:varchar(64);not null;index"`
	Status     string `gorm:"type:varchar(20);not null;index"`
	JobID      string `gorm:"type:char(36);index"`
	RemoteID   string `gorm:"type:varchar(128)"`
	Output     string `gorm:"type:text"`
	Error      string `gorm:"type:text"`
	ExitCode   *int
	StartedAt  *time.Time
	FinishedAt *time.Time
	DurationMs int64
	CreatedAt  time.Time `gorm:"not null;index"`
}

type securityRecord struct {
	ID          string `gorm:"type:varchar(32);primary_key"`
	Fingerprint string `gorm:"type:varchar(64);not null"`
	RotatedAt   time.Time
}

type Store struct {
	db            *gorm.DB
	agentsTable   string
	commandsTable string
	securityTable string
	now           func() time.Time
}

func NewStore(db *gorm.DB, prefix string) *Store {
	prefix = strings.TrimSpace(prefix)
	return &Store{db: db, agentsTable: prefix + "agent", commandsTable: prefix + "agent_command", securityTable: prefix + "agent_security", now: time.Now}
}

func (s *Store) Migrate() error {
	for _, migration := range []struct {
		table string
		model interface{}
	}{{s.agentsTable, &agentRecord{}}, {s.commandsTable, &commandRecord{}}, {s.securityTable, &securityRecord{}}} {
		if err := s.db.Table(migration.table).AutoMigrate(migration.model).Error; err != nil {
			return fmt.Errorf("migrate %s: %w", migration.table, err)
		}
	}
	return nil
}

func (s *Store) Sync(snapshots []TransportSnapshot) (bool, error) {
	now := s.now().UTC()
	seen := make(map[string]bool, len(snapshots))
	changed := false
	tx := s.db.Begin()
	if tx.Error != nil {
		return false, tx.Error
	}
	rollback := func(err error) (bool, error) { tx.Rollback(); return false, err }
	for _, snapshot := range snapshots {
		seen[snapshot.ID] = true
		var existing agentRecord
		result := tx.Table(s.agentsTable).Where("id = ?", snapshot.ID).First(&existing)
		if result.Error != nil && !gorm.IsRecordNotFoundError(result.Error) {
			return rollback(result.Error)
		}
		record, err := recordFromSnapshot(snapshot, now)
		if err != nil {
			return rollback(err)
		}
		if gorm.IsRecordNotFoundError(result.Error) {
			record.CreatedAt = now
			if err := tx.Table(s.agentsTable).Create(&record).Error; err != nil {
				return rollback(err)
			}
			changed = true
			continue
		}
		record.CreatedAt = existing.CreatedAt
		if existing.Status != record.Status || !existing.LastHeartbeat.Equal(record.LastHeartbeat) || existing.Details != record.Details || existing.Capabilities != record.Capabilities {
			changed = true
		}
		if err := tx.Table(s.agentsTable).Where("id = ?", snapshot.ID).Updates(map[string]interface{}{
			"hostname": record.Hostname, "os": record.OS, "arch": record.Arch, "version": record.Version,
			"ip_addresses": record.IPAddresses, "status": record.Status, "last_heartbeat": record.LastHeartbeat,
			"last_report_at": record.LastReportAt, "capabilities": record.Capabilities, "metrics": record.Metrics,
			"details": record.Details, "updated_at": now,
		}).Error; err != nil {
			return rollback(err)
		}
	}
	var online []agentRecord
	query := tx.Table(s.agentsTable).Where("status = ?", StatusOnline)
	if len(seen) > 0 {
		ids := make([]string, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		query = query.Where("id NOT IN (?)", ids)
	}
	if err := query.Find(&online).Error; err != nil {
		return rollback(err)
	}
	if len(online) > 0 {
		changed = true
		ids := make([]string, 0, len(online))
		for _, record := range online {
			ids = append(ids, record.ID)
		}
		if err := tx.Table(s.agentsTable).Where("id IN (?)", ids).Updates(map[string]interface{}{"status": StatusOffline, "updated_at": now}).Error; err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit().Error; err != nil {
		return false, err
	}
	return changed, nil
}

func (s *Store) Agents() ([]Agent, error) {
	var records []agentRecord
	if err := s.db.Table(s.agentsTable).Order("status DESC, hostname ASC, id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	items := make([]Agent, 0, len(records))
	for _, record := range records {
		item, err := agentFromRecord(record)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) Agent(id string) (Agent, error) {
	var record agentRecord
	result := s.db.Table(s.agentsTable).Where("id = ?", id).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Agent{}, ErrAgentNotFound
	}
	if result.Error != nil {
		return Agent{}, result.Error
	}
	return agentFromRecord(record)
}

func (s *Store) DeleteAgent(id string) error {
	result := s.db.Table(s.agentsTable).Where("id = ? AND status = ?", id, StatusOffline).Delete(&agentRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		if agent, err := s.Agent(id); err == nil && agent.Status == StatusOnline {
			return ErrAgentOnline
		}
		return ErrAgentNotFound
	}
	return nil
}

func (s *Store) CreateCommand(command Command) error {
	record := commandRecord{ID: command.ID, AgentID: command.AgentID, AgentName: command.AgentName, Action: string(command.Action), Status: string(command.Status), CreatedAt: command.CreatedAt.UTC()}
	return s.db.Table(s.commandsTable).Create(&record).Error
}

func (s *Store) AttachJob(id, jobID string) error {
	return s.db.Table(s.commandsTable).Where("id = ?", id).Update("job_id", jobID).Error
}

func (s *Store) StartCommand(id string, startedAt time.Time) error {
	return s.db.Table(s.commandsTable).Where("id = ? AND status = ?", id, CommandQueued).Updates(map[string]interface{}{"status": CommandRunning, "started_at": startedAt.UTC()}).Error
}

func (s *Store) SetRemoteID(id, remoteID string) error {
	return s.db.Table(s.commandsTable).Where("id = ?", id).Update("remote_id", remoteID).Error
}

func (s *Store) FinishCommand(id string, status CommandStatus, result ExecutionResult, errorMessage string, finishedAt time.Time) error {
	var record commandRecord
	if err := s.db.Table(s.commandsTable).Where("id = ?", id).First(&record).Error; err != nil {
		return err
	}
	start := record.CreatedAt
	if record.StartedAt != nil {
		start = *record.StartedAt
	}
	exitCode := result.ExitCode
	return s.db.Table(s.commandsTable).Where("id = ?", id).Updates(map[string]interface{}{
		"status": status, "remote_id": result.RemoteID, "output": result.Output, "error": errorMessage,
		"exit_code": &exitCode, "finished_at": finishedAt.UTC(), "duration_ms": finishedAt.Sub(start).Milliseconds(),
	}).Error
}

func (s *Store) RecoverCommands() error {
	now := s.now().UTC()
	return s.db.Table(s.commandsTable).Where("status IN (?)", []CommandStatus{CommandQueued, CommandRunning}).Updates(map[string]interface{}{
		"status": CommandFailed, "error": "服务重启中断了 Agent 命令", "finished_at": now,
	}).Error
}

func (s *Store) Commands(filter CommandFilter) (CommandList, error) {
	query := s.db.Table(s.commandsTable)
	if filter.AgentID != "" {
		query = query.Where("agent_id = ?", filter.AgentID)
	}
	if filter.Status != "" {
		query = query.Where("status = ?", filter.Status)
	}
	if filter.Query != "" {
		pattern := "%" + filter.Query + "%"
		query = query.Where("(action LIKE ? OR agent_name LIKE ? OR output LIKE ? OR error LIKE ?)", pattern, pattern, pattern, pattern)
	}
	if filter.StartAt != nil {
		query = query.Where("created_at >= ?", filter.StartAt.UTC())
	}
	if filter.EndAt != nil {
		query = query.Where("created_at < ?", filter.EndAt.UTC())
	}
	var total int
	if err := query.Count(&total).Error; err != nil {
		return CommandList{}, err
	}
	var records []commandRecord
	if err := query.Order("created_at DESC, id DESC").Limit(filter.Limit).Offset(filter.Offset).Find(&records).Error; err != nil {
		return CommandList{}, err
	}
	items := make([]Command, 0, len(records))
	for _, record := range records {
		items = append(items, commandFromRecord(record))
	}
	return CommandList{Items: items, Total: total, Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *Store) Command(id string) (Command, error) {
	var record commandRecord
	result := s.db.Table(s.commandsTable).Where("id = ?", id).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Command{}, ErrCommandNotFound
	}
	if result.Error != nil {
		return Command{}, result.Error
	}
	return commandFromRecord(record), nil
}

func (s *Store) Security() (securityRecord, error) {
	var record securityRecord
	result := s.db.Table(s.securityTable).Where("id = ?", "primary").First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return securityRecord{}, nil
	}
	return record, result.Error
}

func (s *Store) SaveSecurity(fingerprint string, rotatedAt time.Time) error {
	record := securityRecord{ID: "primary", Fingerprint: fingerprint, RotatedAt: rotatedAt.UTC()}
	var count int
	if err := s.db.Table(s.securityTable).Where("id = ?", record.ID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return s.db.Table(s.securityTable).Create(&record).Error
	}
	return s.db.Table(s.securityTable).Where("id = ?", record.ID).Updates(map[string]interface{}{"fingerprint": record.Fingerprint, "rotated_at": record.RotatedAt}).Error
}

func recordFromSnapshot(snapshot TransportSnapshot, now time.Time) (agentRecord, error) {
	ipAddresses, err := json.Marshal(snapshot.IPAddresses)
	if err != nil {
		return agentRecord{}, err
	}
	capabilities, err := json.Marshal(snapshot.Capabilities)
	if err != nil {
		return agentRecord{}, err
	}
	metrics, err := json.Marshal(snapshot.Metrics)
	if err != nil {
		return agentRecord{}, err
	}
	details, err := json.Marshal(snapshot.Details)
	if err != nil {
		return agentRecord{}, err
	}
	status := snapshot.Status
	if status != StatusOffline {
		status = StatusOnline
	}
	return agentRecord{ID: snapshot.ID, Hostname: snapshot.Hostname, OS: snapshot.OS, Arch: snapshot.Arch, Version: snapshot.Version, IPAddresses: string(ipAddresses), Status: string(status), LastHeartbeat: snapshot.LastHeartbeat.UTC(), LastReportAt: utcPointer(snapshot.LastReportAt), Capabilities: string(capabilities), Metrics: string(metrics), Details: string(details), UpdatedAt: now}, nil
}

func agentFromRecord(record agentRecord) (Agent, error) {
	var ips []string
	if err := json.Unmarshal([]byte(record.IPAddresses), &ips); err != nil {
		return Agent{}, err
	}
	var capabilities []string
	if err := json.Unmarshal([]byte(record.Capabilities), &capabilities); err != nil {
		return Agent{}, err
	}
	var metrics Metrics
	if err := json.Unmarshal([]byte(record.Metrics), &metrics); err != nil {
		return Agent{}, err
	}
	var details map[string]interface{}
	if err := json.Unmarshal([]byte(record.Details), &details); err != nil {
		return Agent{}, err
	}
	return Agent{ID: record.ID, Hostname: record.Hostname, OS: record.OS, Arch: record.Arch, Version: record.Version, IPAddresses: ips, Status: Status(record.Status), LastHeartbeat: record.LastHeartbeat.UTC(), LastReportAt: utcPointer(record.LastReportAt), Capabilities: capabilities, Metrics: metrics, Details: details, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC()}, nil
}

func commandFromRecord(record commandRecord) Command {
	return Command{ID: record.ID, AgentID: record.AgentID, AgentName: record.AgentName, Action: Action(record.Action), Status: CommandStatus(record.Status), JobID: record.JobID, RemoteID: record.RemoteID, Output: record.Output, Error: record.Error, ExitCode: record.ExitCode, StartedAt: utcPointer(record.StartedAt), FinishedAt: utcPointer(record.FinishedAt), DurationMs: record.DurationMs, CreatedAt: record.CreatedAt.UTC()}
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
