package players

import (
	"encoding/json"
	"time"

	"github.com/jinzhu/gorm"
)

type playerWorldSession struct {
	RoomID     string `gorm:"primary_key;type:varchar(255)"`
	WorldID    string `gorm:"primary_key;type:varchar(255)"`
	SessionID  string `gorm:"type:varchar(128);not null"`
	ObservedAt time.Time
}

type playerArchive struct {
	ID         uint   `gorm:"primary_key"`
	RoomID     string `gorm:"type:varchar(255);index"`
	WorldID    string `gorm:"type:varchar(255)"`
	UserID     string `gorm:"type:varchar(128)"`
	Name       string `gorm:"type:varchar(256)"`
	Prefab     string `gorm:"type:varchar(128)"`
	NetID      string `gorm:"type:varchar(128)"`
	ArchivedAt time.Time
	Record     string `gorm:"type:text"`
}

func (s *Store) migrateSessions() error {
	if err := s.db.Table(s.table + "_session").AutoMigrate(&playerWorldSession{}).Error; err != nil {
		return err
	}
	return s.db.Table(s.table + "_history").AutoMigrate(&playerArchive{}).Error
}

// Only accepted runtime snapshots may establish a save session. A missing
// snapshot, process restart, pause or failed request cannot reset player data.
func (s *Store) ObserveWorldSession(roomID, worldID, sessionID string, at time.Time) error {
	if sessionID == "" || at.IsZero() {
		return nil
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback()
	if _, err := s.observeWorldSessionTx(tx, roomID, worldID, sessionID, at); err != nil {
		return err
	}
	return tx.Commit().Error
}

func (s *Store) observeWorldSessionTx(tx *gorm.DB, roomID, worldID, sessionID string, at time.Time) (bool, error) {
	if sessionID == "" || at.IsZero() {
		return true, nil
	}
	var existing playerWorldSession
	result := tx.Table(s.table+"_session").Where("room_id = ? AND world_id = ?", roomID, worldID).First(&existing)
	if result.Error != nil && !gorm.IsRecordNotFoundError(result.Error) {
		return false, result.Error
	}
	// Worldstate and player samples can finish out of order. Same-session
	// samples still reconcile players, but an older session cannot roll back.
	if result.Error == nil && existing.SessionID != sessionID && !at.After(existing.ObservedAt) {
		return false, nil
	}
	if result.Error == nil && existing.SessionID == sessionID && !at.After(existing.ObservedAt) {
		return true, nil
	}
	if result.Error != nil || existing.SessionID != sessionID {
		var records []playerRecord
		if err := tx.Table(s.table).Where("room_id = ? AND world_id = ?", roomID, worldID).Find(&records).Error; err != nil {
			return false, err
		}
		for _, record := range records {
			// Older installations have no session evidence. Keep these records in
			// history rather than relabeling their attributes as belonging to this save.
			record.Online = false
			states := decodeFieldStates(record.FieldStates)
			for field := range states {
				markFieldStale(states, field)
			}
			record.FieldStates, _ = encodeFieldStates(states)
			data, err := json.Marshal(record)
			if err != nil {
				return false, err
			}
			archived := playerArchive{RoomID: roomID, WorldID: worldID, UserID: record.UserID, Name: record.Name, Prefab: record.Prefab, NetID: record.NetID, ArchivedAt: at, Record: string(data)}
			if err := tx.Table(s.table + "_history").Create(&archived).Error; err != nil {
				return false, err
			}
		}
		if len(records) > 0 {
			if err := tx.Table(s.table).Where("room_id = ? AND world_id = ?", roomID, worldID).Delete(&playerRecord{}).Error; err != nil {
				return false, err
			}
		}
	}
	record := playerWorldSession{RoomID: roomID, WorldID: worldID, SessionID: sessionID, ObservedAt: at.UTC()}
	if gorm.IsRecordNotFoundError(result.Error) {
		return true, tx.Table(s.table + "_session").Create(&record).Error
	}
	return true, tx.Table(s.table+"_session").Where("room_id = ? AND world_id = ?", roomID, worldID).Updates(map[string]interface{}{"session_id": sessionID, "observed_at": at.UTC()}).Error
}

func (s *Store) listHistory(roomID string, filter ListFilter) ([]Player, int, error) {
	query := s.db.Table(s.table+"_history").Where("room_id = ?", roomID)
	if filter.Status == "online" {
		return []Player{}, 0, nil
	}
	if filter.WorldID != "" {
		query = query.Where("world_id = ?", filter.WorldID)
	}
	if filter.Prefab != "" {
		query = query.Where("prefab = ?", filter.Prefab)
	}
	if filter.Query != "" {
		like := "%" + escapeLike(filter.Query) + "%"
		query = query.Where("(name LIKE ? ESCAPE '\\' OR user_id LIKE ? ESCAPE '\\' OR net_id LIKE ? ESCAPE '\\')", like, like, like)
	}
	var total int
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var records []playerArchive
	if err := query.Order("archived_at DESC, id DESC").Limit(filter.Limit).Offset(filter.Offset).Find(&records).Error; err != nil {
		return nil, 0, err
	}
	items := make([]Player, 0, len(records))
	for _, archived := range records {
		var record playerRecord
		if err := json.Unmarshal([]byte(archived.Record), &record); err != nil {
			return nil, 0, err
		}
		player := playerFromRecord(record)
		player.Historical = true
		player.HistoryID = archived.ID
		items = append(items, player)
	}
	return items, total, nil
}

func markDynamicFieldsStale(states FieldStates) {
	for _, field := range []string{"gameplayState", "age", "prefab", "netScore", "health", "healthMax", "hunger", "hungerMax", "sanity", "sanityMax", "healthPercent", "hungerPercent", "sanityPercent", "temperature", "moisture", "performance"} {
		markFieldStale(states, field)
	}
}

// Stale counts every stale presence; Counts retains the conservative online
// semantics relied on by unattended maintenance.
func (s *Store) StaleCount(roomID string) (int, error) {
	var records []playerRecord
	if err := s.db.Table(s.table).Select("field_states").Where("room_id = ?", roomID).Find(&records).Error; err != nil {
		return 0, err
	}
	count := 0
	for _, record := range records {
		if decodeFieldStates(record.FieldStates)["online"].Status == FreshnessStale {
			count++
		}
	}
	return count, nil
}
