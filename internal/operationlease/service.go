package operationlease

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

var (
	ErrBusy         = errors.New("room operation lease is held by another operation")
	ErrInvalidInput = errors.New("room operation lease input is invalid")
	ErrLeaseLost    = errors.New("room operation lease is no longer current")
)

type Lease struct {
	RoomID       string    `json:"roomId"`
	LeaseID      string    `json:"leaseId"`
	OperationKey string    `json:"operationKey"`
	FencingToken uint64    `json:"fencingToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

type leaseRecord struct {
	RoomID       string    `gorm:"type:varchar(255);primary_key"`
	LeaseID      string    `gorm:"type:char(36);not null;index"`
	OperationKey string    `gorm:"type:varchar(128);not null;index"`
	FencingToken uint64    `gorm:"not null"`
	ExpiresAt    time.Time `gorm:"not null;index"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Service struct {
	db    *gorm.DB
	table string
	now   func() time.Time
}

func NewService(db *gorm.DB, tablePrefix string) *Service {
	return &Service{db: db, table: strings.TrimSpace(tablePrefix) + "room_operation_lease", now: time.Now}
}

func (s *Service) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("room operation lease database is required")
	}
	if err := s.db.Table(s.table).AutoMigrate(&leaseRecord{}).Error; err != nil {
		return fmt.Errorf("migrate room operation leases: %w", err)
	}
	return nil
}

func (s *Service) Acquire(ctx context.Context, roomID, operationKey string, ttl time.Duration) (Lease, error) {
	roomID, operationKey = strings.TrimSpace(roomID), strings.TrimSpace(operationKey)
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if roomID == "" || len(roomID) > 255 || operationKey == "" || len(operationKey) > 128 ||
		strings.ContainsAny(roomID+operationKey, "\x00\r\n") || ttl < 30*time.Second || ttl > 10*time.Minute {
		return Lease{}, ErrInvalidInput
	}
	for attempt := 0; attempt < 3; attempt++ {
		lease, retry, err := s.acquireOnce(roomID, operationKey, ttl)
		if !retry {
			return lease, err
		}
		if err := ctx.Err(); err != nil {
			return Lease{}, err
		}
	}
	return Lease{}, ErrBusy
}

func (s *Service) acquireOnce(roomID, operationKey string, ttl time.Duration) (Lease, bool, error) {
	now := s.now().UTC()
	tx := s.db.Begin()
	if tx.Error != nil {
		return Lease{}, false, tx.Error
	}
	var current leaseRecord
	result := tx.Table(s.table).Where("room_id = ?", roomID).First(&current)
	if gorm.IsRecordNotFoundError(result.Error) {
		record := leaseRecord{
			RoomID: roomID, LeaseID: uuid.NewString(), OperationKey: operationKey, FencingToken: 1,
			ExpiresAt: now.Add(ttl), CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Table(s.table).Create(&record).Error; err != nil {
			tx.Rollback()
			return Lease{}, true, nil
		}
		if err := tx.Commit().Error; err != nil {
			return Lease{}, false, err
		}
		return leaseFromRecord(record), false, nil
	}
	if result.Error != nil {
		tx.Rollback()
		return Lease{}, false, result.Error
	}
	if current.ExpiresAt.After(now) {
		if current.OperationKey != operationKey {
			tx.Rollback()
			return Lease{}, false, ErrBusy
		}
		if err := tx.Commit().Error; err != nil {
			return Lease{}, false, err
		}
		return leaseFromRecord(current), false, nil
	}
	next := leaseRecord{
		RoomID: roomID, LeaseID: uuid.NewString(), OperationKey: operationKey,
		FencingToken: current.FencingToken + 1, ExpiresAt: now.Add(ttl), CreatedAt: current.CreatedAt, UpdatedAt: now,
	}
	changed := tx.Table(s.table).
		Where("room_id = ? AND fencing_token = ? AND expires_at <= ?", roomID, current.FencingToken, now).
		Updates(map[string]interface{}{
			"lease_id": next.LeaseID, "operation_key": next.OperationKey, "fencing_token": next.FencingToken,
			"expires_at": next.ExpiresAt, "updated_at": next.UpdatedAt,
		})
	if changed.Error != nil {
		tx.Rollback()
		return Lease{}, false, changed.Error
	}
	if changed.RowsAffected != 1 {
		tx.Rollback()
		return Lease{}, true, nil
	}
	if err := tx.Commit().Error; err != nil {
		return Lease{}, false, err
	}
	return leaseFromRecord(next), false, nil
}

func (s *Service) Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if ttl < 30*time.Second || ttl > 10*time.Minute {
		return Lease{}, ErrInvalidInput
	}
	now := s.now().UTC()
	expiresAt := now.Add(ttl)
	result := s.db.Table(s.table).Where(
		"room_id = ? AND lease_id = ? AND fencing_token = ? AND operation_key = ? AND expires_at > ?",
		lease.RoomID, lease.LeaseID, lease.FencingToken, lease.OperationKey, now,
	).Updates(map[string]interface{}{"expires_at": expiresAt, "updated_at": now})
	if result.Error != nil {
		return Lease{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Lease{}, ErrLeaseLost
	}
	lease.ExpiresAt = expiresAt
	return lease, nil
}

func (s *Service) Release(lease Lease) error {
	now := s.now().UTC()
	result := s.db.Table(s.table).Where(
		"room_id = ? AND lease_id = ? AND fencing_token = ? AND operation_key = ?",
		lease.RoomID, lease.LeaseID, lease.FencingToken, lease.OperationKey,
	).Updates(map[string]interface{}{"expires_at": now, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func leaseFromRecord(record leaseRecord) Lease {
	return Lease{
		RoomID: record.RoomID, LeaseID: record.LeaseID, OperationKey: record.OperationKey,
		FencingToken: record.FencingToken, ExpiresAt: record.ExpiresAt.UTC(),
	}
}
