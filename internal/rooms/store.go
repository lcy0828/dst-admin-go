package rooms

import (
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type managedRoomRecord struct {
	RoomID        string    `gorm:"primary_key;type:varchar(255)"`
	DirectoryName string    `gorm:"type:varchar(255);unique_index;not null"`
	AdoptedAt     time.Time `gorm:"not null"`
}

type Store struct {
	db    *gorm.DB
	table string
	now   func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	return &Store{db: db, table: strings.TrimSpace(tablePrefix) + "managed_room", now: time.Now}
}

func (s *Store) Migrate() error {
	if err := s.db.Table(s.table).AutoMigrate(&managedRoomRecord{}).Error; err != nil {
		return fmt.Errorf("migrate managed rooms: %w", err)
	}
	return nil
}

func (s *Store) IsManaged(roomID string) (bool, error) {
	var count int
	if err := s.db.Table(s.table).Where("room_id = ?", roomID).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) Adopt(room Room) error {
	record := managedRoomRecord{RoomID: room.ID, DirectoryName: room.DirectoryName, AdoptedAt: s.now().UTC()}
	var existing managedRoomRecord
	result := s.db.Table(s.table).Where("room_id = ?", room.ID).First(&existing)
	if result.Error == nil {
		return nil
	}
	if !gorm.IsRecordNotFoundError(result.Error) {
		return result.Error
	}
	if err := s.db.Table(s.table).Create(&record).Error; err != nil {
		return fmt.Errorf("adopt room: %w", err)
	}
	return nil
}

func (s *Store) Unadopt(roomID string) error {
	if err := s.db.Table(s.table).Where("room_id = ?", roomID).Delete(&managedRoomRecord{}).Error; err != nil {
		return fmt.Errorf("unadopt room: %w", err)
	}
	return nil
}
