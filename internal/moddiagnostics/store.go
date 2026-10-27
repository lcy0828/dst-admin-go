package moddiagnostics

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jinzhu/gorm"
)

type baselineRecord struct {
	ID         uint64    `gorm:"primary_key"`
	ReleaseID  string    `gorm:"type:varchar(128);not null;unique_index:idx_mod_baseline_release_world"`
	RoomID     string    `gorm:"type:varchar(255);not null;unique_index:idx_mod_baseline_release_world;index:idx_mod_baseline_world"`
	WorldID    string    `gorm:"type:varchar(255);not null;unique_index:idx_mod_baseline_release_world;index:idx_mod_baseline_world"`
	CapturedAt time.Time `gorm:"not null"`
	Payload    string    `gorm:"type:text;not null"`
}

type Store struct {
	db    *gorm.DB
	table string
	mu    sync.Mutex
}

func NewStore(db *gorm.DB, prefix string) *Store {
	return &Store{db: db, table: strings.TrimSpace(prefix) + "mod_diagnostic_baseline"}
}
func (s *Store) Migrate() error { return s.db.Table(s.table).AutoMigrate(&baselineRecord{}).Error }

func (s *Store) SaveOnce(b Baseline) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if len(raw) > 512*1024 {
		return errors.New("mod diagnostic baseline exceeds limit")
	}
	var found baselineRecord
	query := s.db.Table(s.table).Where("release_id = ? AND room_id = ? AND world_id = ?", b.ReleaseID, b.Observation.RoomID, b.Observation.WorldID)
	if err := query.First(&found).Error; err == nil {
		return nil
	} else if !gorm.IsRecordNotFoundError(err) {
		return err
	}
	record := baselineRecord{ReleaseID: b.ReleaseID, RoomID: b.Observation.RoomID, WorldID: b.Observation.WorldID, CapturedAt: b.CapturedAt, Payload: string(raw)}
	if err := s.db.Table(s.table).Create(&record).Error; err != nil {
		return err
	}
	// Five observations per world, pruned on writes; no retention poller.
	var old []baselineRecord
	if err := s.db.Table(s.table).Where("room_id = ? AND world_id = ?", record.RoomID, record.WorldID).Order("id DESC").Limit(5).Offset(5).Find(&old).Error; err != nil {
		return err
	}
	for _, item := range old {
		if err := s.db.Table(s.table).Where("id = ?", item.ID).Delete(&baselineRecord{}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Latest(roomID, worldID string) (*Baseline, error) {
	var record baselineRecord
	err := s.db.Table(s.table).Where("room_id = ? AND world_id = ?", roomID, worldID).Order("id DESC").First(&record).Error
	if gorm.IsRecordNotFoundError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal([]byte(record.Payload), &b); err != nil {
		return nil, fmt.Errorf("decode mod diagnostic baseline: %w", err)
	}
	return &b, nil
}
