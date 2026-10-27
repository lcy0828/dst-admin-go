package playeractivity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

type configRecord struct {
	RoomID   string `gorm:"primary_key;type:varchar(255)"`
	Revision string `gorm:"type:varchar(80)"`
	Value    string `gorm:"type:text"`
}
type eventRecord struct {
	ID         string    `gorm:"primary_key;type:char(64)"`
	RoomID     string    `gorm:"type:varchar(255);index:idx_activity_room_time;index:idx_activity_user_time"`
	UserID     string    `gorm:"type:varchar(128);index:idx_activity_user_time"`
	WorldID    string    `gorm:"type:varchar(255);index"`
	Kind       string    `gorm:"type:varchar(64);index"`
	OccurredAt time.Time `gorm:"index:idx_activity_room_time;index:idx_activity_user_time"`
	SizeBytes  int64
	Value      string `gorm:"type:text"`
}
type cursorRecord struct {
	ID              string `gorm:"primary_key;type:char(64)"`
	RoomID          string `gorm:"type:varchar(255);index"`
	WorldID         string `gorm:"type:varchar(255)"`
	Scope           string `gorm:"type:varchar(512)"`
	Generation      int64
	Producer        string `gorm:"type:varchar(128)"`
	AppliedRevision string `gorm:"type:varchar(80)"`
	Dropped         int64
	GapCount        int64
	SourceUpdatedAt *time.Time
	LastCollectedAt *time.Time
	LastEventAt     *time.Time
	State           string `gorm:"type:varchar(32)"`
	Error           string `gorm:"type:varchar(256)"`
}
type Store struct {
	db                       *gorm.DB
	configs, events, cursors string
	now                      func() time.Time
}

func NewStore(db *gorm.DB, prefix string) *Store {
	return &Store{db: db, configs: prefix + "player_activity_config", events: prefix + "player_activity_event", cursors: prefix + "player_activity_cursor", now: time.Now}
}
func (s *Store) Migrate() error {
	for _, v := range []struct {
		name  string
		model any
	}{{s.configs, &configRecord{}}, {s.events, &eventRecord{}}, {s.cursors, &cursorRecord{}}} {
		if err := s.db.Table(v.name).AutoMigrate(v.model).Error; err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) Settings(room string) (Settings, error) {
	var row configRecord
	err := s.db.Table(s.configs).Where("room_id = ?", room).First(&row).Error
	if gorm.IsRecordNotFoundError(err) {
		return Defaults(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	var c Settings
	if json.Unmarshal([]byte(row.Value), &c) != nil || !c.valid() {
		return Settings{}, fmt.Errorf("invalid persisted activity settings")
	}
	return c, nil
}
func (s *Store) SaveSettings(room string, p SettingsPatch) (Settings, error) {
	current, err := s.Settings(room)
	if err != nil {
		return Settings{}, err
	}
	if p.Revision != current.Revision {
		return Settings{}, ErrConflict
	}
	for _, v := range []struct {
		to   *bool
		from *bool
	}{{&current.Enabled, p.Enabled}, {&current.Positions, p.Positions}, {&current.Actions, p.Actions}, {&current.Combat, p.Combat}} {
		if v.from != nil {
			*v.to = *v.from
		}
	}
	for _, v := range []struct {
		to   *int
		from *int
	}{{&current.PositionIntervalSeconds, p.PositionIntervalSeconds}, {&current.MinimumDistance, p.MinimumDistance}, {&current.CombatIntervalSeconds, p.CombatIntervalSeconds}, {&current.FlushIntervalSeconds, p.FlushIntervalSeconds}, {&current.SyncIntervalSeconds, p.SyncIntervalSeconds}, {&current.RetentionDays, p.RetentionDays}, {&current.MaximumMiB, p.MaximumMiB}} {
		if v.from != nil {
			*v.to = *v.from
		}
	}
	if !current.valid() {
		return Settings{}, ErrInvalid
	}
	current.Revision = uuid.NewString()
	data, _ := json.Marshal(current)
	if p.Revision == "default" {
		err = s.db.Table(s.configs).Create(&configRecord{RoomID: room, Revision: current.Revision, Value: string(data)}).Error
		if err != nil {
			if existing, readErr := s.Settings(room); readErr == nil && existing.Revision != "default" {
				return Settings{}, ErrConflict
			}
			return Settings{}, err
		}
	} else {
		r := s.db.Table(s.configs).Where("room_id = ? AND revision = ?", room, p.Revision).Updates(map[string]any{"revision": current.Revision, "value": string(data)})
		if r.Error != nil {
			return Settings{}, r.Error
		}
		if r.RowsAffected != 1 {
			return Settings{}, ErrConflict
		}
	}
	return current, nil
}
func identity(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
func (s *Store) cursor(room, world string) (cursorRecord, error) {
	row := cursorRecord{ID: identity(room, world), RoomID: room, WorldID: world}
	err := s.db.Table(s.cursors).Where("id = ?", row.ID).First(&row).Error
	if gorm.IsRecordNotFoundError(err) {
		return row, nil
	}
	return row, err
}
func (s *Store) saveCursor(tx *gorm.DB, row cursorRecord) error {
	return tx.Table(s.cursors).Save(&row).Error
}
func (s *Store) ingest(row cursorRecord, events []Event) error {
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback()
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	existing := []eventRecord{}
	if len(ids) > 0 {
		for start := 0; start < len(ids); start += 400 {
			end := start + 400
			if end > len(ids) {
				end = len(ids)
			}
			var batch []eventRecord
			if err := tx.Table(s.events).Select("id").Where("id IN (?)", ids[start:end]).Find(&batch).Error; err != nil {
				return err
			}
			existing = append(existing, batch...)
		}
	}
	seen := map[string]bool{}
	for _, e := range existing {
		seen[e.ID] = true
	}
	for _, e := range events {
		if seen[e.ID] {
			continue
		}
		value, err := json.Marshal(e)
		if err != nil {
			return err
		}
		record := eventRecord{ID: e.ID, RoomID: e.RoomID, UserID: e.UserID, WorldID: e.WorldID, Kind: e.Kind, OccurredAt: e.OccurredAt, Value: string(value), SizeBytes: int64(len(value) + 256)}
		if err = tx.Table(s.events).Create(&record).Error; err != nil {
			return err
		}
		seen[e.ID] = true
	}
	if err := s.saveCursor(tx, row); err != nil {
		return err
	}
	return tx.Commit().Error
}
func (s *Store) prune(room string, c Settings) error {
	var rows []eventRecord
	if err := s.db.Table(s.events).Select("id, size_bytes, occurred_at").Where("room_id = ? AND occurred_at < ?", room, s.now().UTC().Add(-time.Duration(c.RetentionDays)*24*time.Hour)).Order("occurred_at ASC").Limit(2048).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		var totals struct{ Bytes int64 }
		if err := s.db.Table(s.events).Select("COALESCE(SUM(size_bytes), 0) AS bytes").Where("room_id = ?", room).Scan(&totals).Error; err != nil {
			return err
		}
		excess := totals.Bytes - int64(c.MaximumMiB)*(1<<20)
		if excess <= 0 {
			return nil
		}
		if err := s.db.Table(s.events).Select("id,size_bytes,occurred_at").Where("room_id = ?", room).Order("occurred_at ASC, id ASC").Limit(2048).Find(&rows).Error; err != nil {
			return err
		}
		chosen := 0
		for _, row := range rows {
			if excess <= 0 {
				break
			}
			excess -= row.SizeBytes
			chosen++
		}
		rows = rows[:chosen]
	}
	ids := []string{}
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	for start := 0; start < len(ids); start += 400 {
		end := start + 400
		if end > len(ids) {
			end = len(ids)
		}
		if err := s.db.Table(s.events).Where("room_id = ? AND id IN (?)", room, ids[start:end]).Delete(&eventRecord{}).Error; err != nil {
			return err
		}
	}
	return nil
}
