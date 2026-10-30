package worldquery

import (
	"encoding/json"
	"time"

	"github.com/jinzhu/gorm"
)

// Separate projection prevents query callers from reading administrator Lua.
type record struct {
	ID        string    `gorm:"primary_key;size:80"`
	Principal string    `gorm:"size:128;index"`
	RoomID    string    `gorm:"size:512;index"`
	WorldID   string    `gorm:"size:512"`
	Identity  string    `gorm:"type:text"`
	Prefab    string    `gorm:"size:80"`
	Pending   bool      `gorm:"index"`
	CreatedAt time.Time `gorm:"index"`
	Payload   string    `gorm:"type:text"`
}

type Store struct {
	db    *gorm.DB
	table string
}

func NewStore(db *gorm.DB, prefix string) *Store {
	return &Store{db: db, table: prefix + "world_query_run"}
}
func (s *Store) Migrate() error { return s.db.Table(s.table).AutoMigrate(&record{}).Error }

func (s *Store) save(rec record, r Result) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > 16384 {
		return ErrInvalid
	}
	rec.Payload = string(b)
	rec.Pending = r.Status == "uncertain"
	return s.db.Table(s.table).Save(&rec).Error
}
func (s *Store) get(id string) (record, Result, error) {
	var rec record
	err := s.db.Table(s.table).Where("id = ?", id).First(&rec).Error
	if gorm.IsRecordNotFoundError(err) {
		return rec, Result{}, ErrNotFound
	}
	if err != nil {
		return rec, Result{}, err
	}
	var r Result
	err = json.Unmarshal([]byte(rec.Payload), &r)
	return rec, r, err
}
func (s *Store) pending(room, world string, now time.Time) (record, Result, error) {
	var rec record
	err := s.db.Table(s.table).Where("room_id = ? AND world_id = ? AND pending = ? AND created_at > ?", room, world, true, now.Add(-2*time.Minute)).Order("created_at desc").First(&rec).Error
	if gorm.IsRecordNotFoundError(err) {
		return rec, Result{}, ErrNotFound
	}
	if err != nil {
		return rec, Result{}, err
	}
	var r Result
	err = json.Unmarshal([]byte(rec.Payload), &r)
	return rec, r, err
}
func (s *Store) prune(now time.Time) error {
	if err := s.db.Table(s.table).Where("created_at < ?", now.Add(-7*24*time.Hour)).Delete(&record{}).Error; err != nil {
		return err
	}
	// Reserve one slot using a boundary, not a 1999-parameter IN clause:
	// SQLite builds may limit a statement to 999 bound variables.
	var boundary record
	err := s.db.Table(s.table).Select("id, created_at").Order("created_at desc, id desc").Offset(1998).First(&boundary).Error
	if gorm.IsRecordNotFoundError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.db.Table(s.table).Where("created_at < ? OR (created_at = ? AND id < ?)", boundary.CreatedAt, boundary.CreatedAt, boundary.ID).Delete(&record{}).Error
}
