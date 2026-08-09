package announcements

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type announcementRecord struct {
	ID          uint64 `gorm:"primary_key;AUTO_INCREMENT"`
	Title       string `gorm:"type:varchar(200);not null"`
	Content     string `gorm:"type:text;not null"`
	PublishTime time.Time
	ExpireTime  time.Time
	Target      string `gorm:"type:varchar(16);not null"`
	Important   bool   `gorm:"not null"`
	UpdatedAt   time.Time
}

type Store struct {
	db    *gorm.DB
	table string
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	return &Store{db: db, table: strings.TrimSpace(tablePrefix) + "announcement"}
}

func (s *Store) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("announcement database is required")
	}
	if err := s.db.Table(s.table).AutoMigrate(&announcementRecord{}).Error; err != nil {
		return fmt.Errorf("migrate announcements: %w", err)
	}
	return nil
}

func (s *Store) List() ([]Announcement, error) {
	var records []announcementRecord
	if err := s.db.Table(s.table).Order("important DESC, publish_time DESC, id DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	items := make([]Announcement, 0, len(records))
	for _, record := range records {
		items = append(items, announcementFromRecord(record))
	}
	return items, nil
}

func (s *Store) Get(id uint64) (Announcement, error) {
	var record announcementRecord
	if err := s.db.Table(s.table).Where("id = ?", id).First(&record).Error; err != nil {
		if gorm.IsRecordNotFoundError(err) {
			return Announcement{}, ErrNotFound
		}
		return Announcement{}, err
	}
	return announcementFromRecord(record), nil
}

func (s *Store) Create(value Announcement) (Announcement, error) {
	record := announcementToRecord(value)
	if err := s.db.Table(s.table).Create(&record).Error; err != nil {
		return Announcement{}, err
	}
	return announcementFromRecord(record), nil
}

func (s *Store) Update(value Announcement) (Announcement, error) {
	result := s.db.Table(s.table).Where("id = ?", value.ID).Updates(map[string]interface{}{
		"title": value.Title, "content": value.Content, "expire_time": value.ExpireTime,
		"target": string(value.Target), "important": value.Important, "updated_at": value.UpdatedAt,
	})
	if result.Error != nil {
		return Announcement{}, result.Error
	}
	if result.RowsAffected == 0 {
		return Announcement{}, ErrNotFound
	}
	return s.Get(value.ID)
}

func (s *Store) Delete(id uint64) error {
	result := s.db.Table(s.table).Where("id = ?", id).Delete(&announcementRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func announcementToRecord(value Announcement) announcementRecord {
	return announcementRecord{
		ID: value.ID, Title: value.Title, Content: value.Content, PublishTime: value.PublishTime,
		ExpireTime: value.ExpireTime, Target: string(value.Target), Important: value.Important, UpdatedAt: value.UpdatedAt,
	}
}

func announcementFromRecord(record announcementRecord) Announcement {
	return Announcement{
		ID: record.ID, Title: record.Title, Content: record.Content, PublishTime: record.PublishTime,
		ExpireTime: record.ExpireTime, Target: Target(record.Target), Important: record.Important, UpdatedAt: record.UpdatedAt,
	}
}
