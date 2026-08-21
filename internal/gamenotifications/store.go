package gamenotifications

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"dont/internal/rooms"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

type notificationRecord struct {
	ID            string `gorm:"primary_key;type:char(36)"`
	RoomID        string `gorm:"type:varchar(255);index;not null"`
	RoomName      string `gorm:"type:varchar(255);not null"`
	Message       string `gorm:"type:text;not null"`
	Source        string `gorm:"type:varchar(32);index;not null"`
	Status        string `gorm:"type:varchar(24);index;not null"`
	JobID         string `gorm:"type:char(36);index"`
	SuccessCount  int    `gorm:"not null"`
	FailureCount  int    `gorm:"not null"`
	SkippedCount  int    `gorm:"not null"`
	CanceledCount int    `gorm:"not null"`
	CreatedAt     time.Time
	CompletedAt   *time.Time
}

type deliveryRecord struct {
	ID               int64  `gorm:"primary_key;AUTO_INCREMENT"`
	NotificationID   string `gorm:"type:char(36);unique_index:idx_game_notification_world;not null"`
	WorldID          string `gorm:"type:varchar(255);unique_index:idx_game_notification_world;not null"`
	WorldName        string `gorm:"type:varchar(255);not null"`
	TargetID         string `gorm:"type:varchar(255);index"`
	AgentID          string `gorm:"type:varchar(255);index"`
	Status           string `gorm:"type:varchar(24);index;not null"`
	Message          string `gorm:"type:text"`
	ErrorCode        string `gorm:"type:varchar(64)"`
	ErrorMessage     string `gorm:"type:text"`
	TopologyRevision string `gorm:"type:varchar(80)"`
	SentAt           *time.Time
	ObservedAt       *time.Time
}

type policyRecord struct {
	RoomID           string `gorm:"primary_key;type:varchar(255)"`
	Enabled          bool   `gorm:"not null"`
	CountdownSeconds int    `gorm:"not null"`
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Store struct {
	db                 *gorm.DB
	notificationsTable string
	deliveriesTable    string
	policiesTable      string
	now                func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	prefix := strings.TrimSpace(tablePrefix)
	return &Store{
		db: db, notificationsTable: prefix + "game_notification",
		deliveriesTable: prefix + "game_notification_delivery",
		policiesTable:   prefix + "game_notification_policy", now: time.Now,
	}
}

func (s *Store) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("game notification database is required")
	}
	for _, migration := range []struct {
		table string
		model interface{}
	}{
		{s.notificationsTable, &notificationRecord{}},
		{s.deliveriesTable, &deliveryRecord{}},
		{s.policiesTable, &policyRecord{}},
	} {
		if err := s.db.Table(migration.table).AutoMigrate(migration.model).Error; err != nil {
			return fmt.Errorf("migrate %s: %w", migration.table, err)
		}
	}
	return nil
}

func (s *Store) RecoverInterrupted() error {
	var records []notificationRecord
	if err := s.db.Table(s.notificationsTable).
		Where("status IN (?)", []Status{StatusQueued, StatusSending}).
		Find(&records).Error; err != nil {
		return err
	}
	var combined error
	for _, record := range records {
		notification, err := s.Get(record.ID)
		if err != nil {
			combined = errors.Join(combined, err)
			continue
		}
		now := s.now().UTC()
		for _, delivery := range notification.Deliveries {
			if delivery.Status != DeliveryQueued {
				continue
			}
			delivery.Status = DeliveryFailed
			delivery.ErrorCode = "SERVICE_RESTARTED"
			delivery.ErrorMessage = "服务重启导致通知发送中断"
			delivery.SentAt = &now
			if err := s.RecordDelivery(notification.ID, delivery); err != nil {
				combined = errors.Join(combined, err)
			}
		}
		updated, err := s.Get(notification.ID)
		if err != nil {
			combined = errors.Join(combined, err)
			continue
		}
		success, failure, skipped, canceled := deliveryCounts(updated.Deliveries)
		if _, err := s.Complete(notification.ID, completionStatus(success, failure, skipped, canceled), success, failure, skipped, canceled); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	return combined
}

func (s *Store) Create(room rooms.Room, message string, source Source, worlds []rooms.World) (Notification, error) {
	now := s.now().UTC()
	record := notificationRecord{
		ID: uuid.NewString(), RoomID: room.ID, RoomName: room.Name, Message: message,
		Source: string(source), Status: string(StatusQueued), CreatedAt: now,
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return Notification{}, tx.Error
	}
	if err := tx.Table(s.notificationsTable).Create(&record).Error; err != nil {
		tx.Rollback()
		return Notification{}, err
	}
	for _, world := range worlds {
		delivery := deliveryRecord{
			NotificationID: record.ID, WorldID: world.ID, WorldName: world.Name,
			Status: string(DeliveryQueued),
		}
		if err := tx.Table(s.deliveriesTable).Create(&delivery).Error; err != nil {
			tx.Rollback()
			return Notification{}, err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return Notification{}, err
	}
	return s.Get(record.ID)
}

func (s *Store) AttachJob(notificationID, jobID string) error {
	result := s.db.Table(s.notificationsTable).Where("id = ?", notificationID).UpdateColumn("job_id", strings.TrimSpace(jobID))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) MarkSending(notificationID string) error {
	result := s.db.Table(s.notificationsTable).Where("id = ? AND status = ?", notificationID, StatusQueued).UpdateColumn("status", StatusSending)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RecordDelivery(notificationID string, delivery Delivery) error {
	updates := map[string]interface{}{
		"target_id": delivery.TargetID, "agent_id": delivery.AgentID, "status": delivery.Status,
		"message": delivery.Message, "error_code": delivery.ErrorCode, "error_message": delivery.ErrorMessage,
		"topology_revision": delivery.TopologyRevision, "sent_at": delivery.SentAt, "observed_at": delivery.ObservedAt,
	}
	result := s.db.Table(s.deliveriesTable).
		Where("notification_id = ? AND world_id = ? AND status = ?", notificationID, delivery.WorldID, DeliveryQueued).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Complete(notificationID string, status Status, success, failure, skipped, canceled int) (Notification, error) {
	now := s.now().UTC()
	result := s.db.Table(s.notificationsTable).Where("id = ?", notificationID).Updates(map[string]interface{}{
		"status": status, "success_count": success, "failure_count": failure,
		"skipped_count": skipped, "canceled_count": canceled, "completed_at": now,
	})
	if result.Error != nil {
		return Notification{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Notification{}, ErrNotFound
	}
	return s.Get(notificationID)
}

func (s *Store) Fail(notificationID, message string) error {
	values := []Delivery{}
	notification, err := s.Get(notificationID)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for _, current := range notification.Deliveries {
		if current.Status != DeliveryQueued {
			continue
		}
		current.Status = DeliveryFailed
		current.ErrorCode = "JOB_CREATE_FAILED"
		current.ErrorMessage = message
		current.SentAt = &now
		values = append(values, current)
	}
	for _, delivery := range values {
		if err := s.RecordDelivery(notificationID, delivery); err != nil {
			return err
		}
	}
	_, err = s.Complete(notificationID, StatusFailed, 0, len(values), 0, 0)
	return err
}

func (s *Store) Get(notificationID string) (Notification, error) {
	var record notificationRecord
	result := s.db.Table(s.notificationsTable).Where("id = ?", notificationID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Notification{}, ErrNotFound
	}
	if result.Error != nil {
		return Notification{}, result.Error
	}
	deliveries, err := s.deliveries([]string{record.ID})
	if err != nil {
		return Notification{}, err
	}
	return notificationFromRecord(record, deliveries[record.ID]), nil
}

func (s *Store) List(filter ListFilter) (List, error) {
	query := s.db.Table(s.notificationsTable)
	if roomID := strings.TrimSpace(filter.RoomID); roomID != "" {
		query = query.Where("room_id = ?", roomID)
	}
	var total int
	if err := query.Count(&total).Error; err != nil {
		return List{}, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	var records []notificationRecord
	if err := query.Order("created_at DESC, id DESC").Limit(limit).Offset(offset).Find(&records).Error; err != nil {
		return List{}, err
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	deliveries, err := s.deliveries(ids)
	if err != nil {
		return List{}, err
	}
	items := make([]Notification, 0, len(records))
	for _, record := range records {
		items = append(items, notificationFromRecord(record, deliveries[record.ID]))
	}
	return List{Items: items, Total: total, Limit: limit, Offset: offset}, nil
}

func (s *Store) deliveries(notificationIDs []string) (map[string][]Delivery, error) {
	grouped := make(map[string][]Delivery, len(notificationIDs))
	if len(notificationIDs) == 0 {
		return grouped, nil
	}
	var records []deliveryRecord
	if err := s.db.Table(s.deliveriesTable).Where("notification_id IN (?)", notificationIDs).Order("id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	for _, record := range records {
		grouped[record.NotificationID] = append(grouped[record.NotificationID], deliveryFromRecord(record))
	}
	return grouped, nil
}

func (s *Store) Policy(roomID string) (Policy, error) {
	var record policyRecord
	result := s.db.Table(s.policiesTable).Where("room_id = ?", roomID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Policy{RoomID: roomID, Enabled: true, CountdownSeconds: DefaultCountdownSecond, UpdatedAt: s.now().UTC()}, nil
	}
	if result.Error != nil {
		return Policy{}, result.Error
	}
	return policyFromRecord(record), nil
}

func (s *Store) SavePolicy(policy Policy) (Policy, error) {
	now := s.now().UTC()
	var current policyRecord
	result := s.db.Table(s.policiesTable).Where("room_id = ?", policy.RoomID).First(&current)
	if gorm.IsRecordNotFoundError(result.Error) {
		record := policyRecord{
			RoomID: policy.RoomID, Enabled: policy.Enabled, CountdownSeconds: policy.CountdownSeconds,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.db.Table(s.policiesTable).Create(&record).Error; err != nil {
			return Policy{}, err
		}
		return policyFromRecord(record), nil
	}
	if result.Error != nil {
		return Policy{}, result.Error
	}
	if err := s.db.Table(s.policiesTable).Where("room_id = ?", policy.RoomID).Updates(map[string]interface{}{
		"enabled": policy.Enabled, "countdown_seconds": policy.CountdownSeconds, "updated_at": now,
	}).Error; err != nil {
		return Policy{}, err
	}
	return s.Policy(policy.RoomID)
}

func notificationFromRecord(record notificationRecord, deliveries []Delivery) Notification {
	return Notification{
		ID: record.ID, RoomID: record.RoomID, RoomName: record.RoomName, Message: record.Message,
		Source: Source(record.Source), Status: Status(record.Status), JobID: record.JobID,
		SuccessCount: record.SuccessCount, FailureCount: record.FailureCount,
		SkippedCount: record.SkippedCount, CanceledCount: record.CanceledCount,
		CreatedAt: record.CreatedAt.UTC(), CompletedAt: utcTime(record.CompletedAt), Deliveries: deliveries,
	}
}

func deliveryFromRecord(record deliveryRecord) Delivery {
	return Delivery{
		ID: record.ID, NotificationID: record.NotificationID, WorldID: record.WorldID, WorldName: record.WorldName,
		TargetID: record.TargetID, AgentID: record.AgentID, Status: DeliveryStatus(record.Status), Message: record.Message,
		ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage, TopologyRevision: record.TopologyRevision,
		SentAt: utcTime(record.SentAt), ObservedAt: utcTime(record.ObservedAt),
	}
}

func policyFromRecord(record policyRecord) Policy {
	return Policy{
		RoomID: record.RoomID, Enabled: record.Enabled,
		CountdownSeconds: record.CountdownSeconds, UpdatedAt: record.UpdatedAt.UTC(),
	}
}

func utcTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

func deliveryCounts(values []Delivery) (success, failure, skipped, canceled int) {
	for _, value := range values {
		switch value.Status {
		case DeliverySucceeded:
			success++
		case DeliveryFailed:
			failure++
		case DeliverySkipped:
			skipped++
		case DeliveryCanceled:
			canceled++
		}
	}
	return success, failure, skipped, canceled
}
