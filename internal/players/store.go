package players

import (
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type playerRecord struct {
	RoomID          string `gorm:"type:varchar(255);not null;unique_index:idx_player_room_user;index"`
	UserID          string `gorm:"type:varchar(128);not null;unique_index:idx_player_room_user"`
	WorldID         string `gorm:"type:varchar(255);not null;index"`
	WorldName       string `gorm:"type:varchar(128);not null"`
	Name            string `gorm:"type:varchar(256);not null;index"`
	Prefab          string `gorm:"type:varchar(128);index"`
	Online          bool   `gorm:"not null;index"`
	Admin           bool   `gorm:"not null"`
	Age             int
	NetID           string `gorm:"type:varchar(128)"`
	Performance     int
	HealthPercent   *float64
	HungerPercent   *float64
	SanityPercent   *float64
	Temperature     *float64
	Moisture        *float64
	FirstSeenAt     time.Time `gorm:"not null"`
	LastSeenAt      time.Time `gorm:"not null;index"`
	StatusChangedAt time.Time `gorm:"not null"`
	LastRefreshedAt time.Time `gorm:"not null;index"`
}

type banRecord struct {
	RoomID    string     `gorm:"type:varchar(255);not null;unique_index:idx_player_ban_room_user;index"`
	UserID    string     `gorm:"type:varchar(128);not null;unique_index:idx_player_ban_room_user"`
	Reason    string     `gorm:"type:varchar(300);not null"`
	Duration  string     `gorm:"type:varchar(16);not null"`
	CreatedAt time.Time  `gorm:"not null"`
	UpdatedAt time.Time  `gorm:"not null"`
	ExpiresAt *time.Time `gorm:"index"`
}

type Store struct {
	db        *gorm.DB
	table     string
	bansTable string
	now       func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	prefix := strings.TrimSpace(tablePrefix)
	return &Store{db: db, table: prefix + "player", bansTable: prefix + "player_ban", now: time.Now}
}

func (s *Store) Migrate() error {
	if err := s.db.Table(s.table).AutoMigrate(&playerRecord{}).Error; err != nil {
		return fmt.Errorf("migrate players: %w", err)
	}
	if err := s.db.Table(s.bansTable).AutoMigrate(&banRecord{}).Error; err != nil {
		return fmt.Errorf("migrate player bans: %w", err)
	}
	return nil
}

func (s *Store) ReplaceWorldSnapshot(roomID, worldID, worldName string, observations []Observation, observedAt time.Time) error {
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	rollback := func(err error) error {
		tx.Rollback()
		return err
	}
	seen := make(map[string]bool, len(observations))
	for _, observation := range observations {
		seen[observation.ID] = true
		var existing playerRecord
		result := tx.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, observation.ID).First(&existing)
		if result.Error != nil && !gorm.IsRecordNotFoundError(result.Error) {
			return rollback(result.Error)
		}
		if gorm.IsRecordNotFoundError(result.Error) {
			record := playerRecord{
				RoomID: roomID, UserID: observation.ID, WorldID: worldID, WorldName: worldName,
				Name: observation.Name, Prefab: observation.Prefab, Online: true, Admin: observation.Admin,
				Age: observation.Age, NetID: observation.NetID, Performance: observation.Performance,
				HealthPercent: observation.HealthPercent, HungerPercent: observation.HungerPercent,
				SanityPercent: observation.SanityPercent, Temperature: observation.Temperature, Moisture: observation.Moisture,
				FirstSeenAt: observedAt, LastSeenAt: observedAt, StatusChangedAt: observedAt, LastRefreshedAt: observedAt,
			}
			if err := tx.Table(s.table).Create(&record).Error; err != nil {
				return rollback(err)
			}
			continue
		}
		statusChanged := existing.StatusChangedAt
		if !existing.Online || existing.WorldID != worldID {
			statusChanged = observedAt
		}
		updates := map[string]interface{}{
			"world_id": worldID, "world_name": worldName, "name": observation.Name, "prefab": observation.Prefab,
			"online": true, "admin": observation.Admin, "age": observation.Age, "net_id": observation.NetID,
			"performance": observation.Performance, "health_percent": observation.HealthPercent,
			"hunger_percent": observation.HungerPercent, "sanity_percent": observation.SanityPercent,
			"temperature": observation.Temperature, "moisture": observation.Moisture,
			"last_seen_at": observedAt, "last_refreshed_at": observedAt, "status_changed_at": statusChanged,
		}
		if err := tx.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, observation.ID).Updates(updates).Error; err != nil {
			return rollback(err)
		}
	}
	var online []playerRecord
	if err := tx.Table(s.table).Where("room_id = ? AND world_id = ? AND online = ?", roomID, worldID, true).Find(&online).Error; err != nil {
		return rollback(err)
	}
	for _, record := range online {
		if seen[record.UserID] {
			continue
		}
		if err := tx.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, record.UserID).Updates(map[string]interface{}{
			"online": false, "status_changed_at": observedAt, "last_refreshed_at": observedAt,
		}).Error; err != nil {
			return rollback(err)
		}
	}
	return tx.Commit().Error
}

func (s *Store) MarkWorldOffline(roomID, worldID string, observedAt time.Time) error {
	return s.db.Table(s.table).Where("room_id = ? AND world_id = ? AND online = ?", roomID, worldID, true).Updates(map[string]interface{}{
		"online": false, "status_changed_at": observedAt, "last_refreshed_at": observedAt,
	}).Error
}

func (s *Store) MarkPlayerOffline(roomID, userID string, observedAt time.Time) error {
	return s.db.Table(s.table).Where("room_id = ? AND user_id = ? AND online = ?", roomID, userID, true).Updates(map[string]interface{}{
		"online": false, "status_changed_at": observedAt, "last_refreshed_at": observedAt,
	}).Error
}

func (s *Store) Get(roomID, userID string) (Player, error) {
	var record playerRecord
	result := s.db.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, userID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Player{}, ErrPlayerNotFound
	}
	if result.Error != nil {
		return Player{}, result.Error
	}
	return playerFromRecord(record), nil
}

func (s *Store) List(roomID string, filter ListFilter) ([]Player, int, error) {
	query := s.db.Table(s.table).Where("room_id = ?", roomID)
	if filter.Status == "online" {
		query = query.Where("online = ?", true)
	} else if filter.Status == "offline" {
		query = query.Where("online = ?", false)
	}
	if filter.WorldID != "" {
		query = query.Where("world_id = ?", filter.WorldID)
	}
	if filter.Prefab != "" {
		query = query.Where("prefab = ?", filter.Prefab)
	}
	if filter.Query != "" {
		like := "%" + escapeLike(strings.ToLower(filter.Query)) + "%"
		query = query.Where("(LOWER(name) LIKE ? ESCAPE '\\' OR LOWER(user_id) LIKE ? ESCAPE '\\' OR LOWER(net_id) LIKE ? ESCAPE '\\')", like, like, like)
	}
	var total int
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var records []playerRecord
	if err := query.Order("online DESC, last_seen_at DESC, name ASC").Limit(filter.Limit).Offset(filter.Offset).Find(&records).Error; err != nil {
		return nil, 0, err
	}
	items := make([]Player, 0, len(records))
	for _, record := range records {
		items = append(items, playerFromRecord(record))
	}
	return items, total, nil
}

func (s *Store) Counts(roomID string) (int, int, *time.Time, error) {
	var total, online int
	base := s.db.Table(s.table).Where("room_id = ?", roomID)
	if err := base.Count(&total).Error; err != nil {
		return 0, 0, nil, err
	}
	if err := s.db.Table(s.table).Where("room_id = ? AND online = ?", roomID, true).Count(&online).Error; err != nil {
		return 0, 0, nil, err
	}
	var record playerRecord
	result := s.db.Table(s.table).Where("room_id = ?", roomID).Order("last_refreshed_at DESC").First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return total, online, nil, nil
	}
	if result.Error != nil {
		return 0, 0, nil, result.Error
	}
	refreshed := record.LastRefreshedAt.UTC()
	return total, online, &refreshed, nil
}

func (s *Store) SaveBan(value Ban) error {
	record := banRecord{
		RoomID: value.RoomID, UserID: value.PlayerID, Reason: value.Reason, Duration: value.Duration,
		CreatedAt: value.CreatedAt.UTC(), UpdatedAt: s.now().UTC(), ExpiresAt: utcPointer(value.ExpiresAt),
	}
	var existing banRecord
	result := s.db.Table(s.bansTable).Where("room_id = ? AND user_id = ?", value.RoomID, value.PlayerID).First(&existing)
	if gorm.IsRecordNotFoundError(result.Error) {
		return s.db.Table(s.bansTable).Create(&record).Error
	}
	if result.Error != nil {
		return result.Error
	}
	return s.db.Table(s.bansTable).Where("room_id = ? AND user_id = ?", value.RoomID, value.PlayerID).Updates(map[string]interface{}{
		"reason": record.Reason, "duration": record.Duration, "created_at": record.CreatedAt,
		"updated_at": record.UpdatedAt, "expires_at": record.ExpiresAt,
	}).Error
}

func (s *Store) DeleteBan(roomID, playerID string) error {
	return s.db.Table(s.bansTable).Where("room_id = ? AND user_id = ?", roomID, playerID).Delete(&banRecord{}).Error
}

func (s *Store) Bans(roomID string) (map[string]Ban, error) {
	var records []banRecord
	if err := s.db.Table(s.bansTable).Where("room_id = ?", roomID).Find(&records).Error; err != nil {
		return nil, err
	}
	values := make(map[string]Ban, len(records))
	for _, record := range records {
		value := banFromRecord(record)
		values[value.PlayerID] = value
	}
	return values, nil
}

func (s *Store) ExpiredBans(now time.Time) ([]Ban, error) {
	var records []banRecord
	if err := s.db.Table(s.bansTable).Where("expires_at IS NOT NULL AND expires_at <= ?", now.UTC()).Order("expires_at ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	values := make([]Ban, 0, len(records))
	for _, record := range records {
		values = append(values, banFromRecord(record))
	}
	return values, nil
}

func banFromRecord(record banRecord) Ban {
	return Ban{
		RoomID: record.RoomID, PlayerID: record.UserID, Reason: record.Reason, Duration: record.Duration,
		CreatedAt: record.CreatedAt.UTC(), ExpiresAt: utcPointer(record.ExpiresAt),
	}
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func playerFromRecord(record playerRecord) Player {
	return Player{
		ID: record.UserID, RoomID: record.RoomID, WorldID: record.WorldID, WorldName: record.WorldName,
		Name: record.Name, Prefab: record.Prefab, Online: record.Online, Admin: record.Admin, Age: record.Age,
		NetID: record.NetID, Performance: record.Performance, HealthPercent: record.HealthPercent,
		HungerPercent: record.HungerPercent, SanityPercent: record.SanityPercent,
		Temperature: record.Temperature, Moisture: record.Moisture,
		FirstSeenAt: record.FirstSeenAt.UTC(), LastSeenAt: record.LastSeenAt.UTC(),
		StatusChangedAt: record.StatusChangedAt.UTC(), LastRefreshedAt: record.LastRefreshedAt.UTC(),
	}
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "%", "\\%")
	return strings.ReplaceAll(value, "_", "\\_")
}
