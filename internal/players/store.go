package players

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type playerRecord struct {
	RoomID           string `gorm:"type:varchar(255);not null;unique_index:idx_player_room_user;index"`
	UserID           string `gorm:"type:varchar(128);not null;unique_index:idx_player_room_user"`
	WorldID          string `gorm:"type:varchar(255);not null;index"`
	WorldName        string `gorm:"type:varchar(128);not null"`
	Name             string `gorm:"type:varchar(256);not null;index"`
	Prefab           string `gorm:"type:varchar(128);index"`
	Online           bool   `gorm:"not null;index"`
	Admin            bool   `gorm:"not null"`
	Age              int
	NetID            string `gorm:"type:varchar(128)"`
	NetScore         *int
	Performance      *int
	HealthPercent    *float64
	HungerPercent    *float64
	SanityPercent    *float64
	Temperature      *float64
	Moisture         *float64
	FieldStates      string    `gorm:"type:text"`
	PresenceConflict bool      `gorm:"not null;default:false;index"`
	ObservedWorldIDs string    `gorm:"type:text"`
	FirstSeenAt      time.Time `gorm:"not null"`
	LastSeenAt       time.Time `gorm:"not null;index"`
	StatusChangedAt  time.Time `gorm:"not null"`
	LastRefreshedAt  time.Time `gorm:"not null;index"`
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
	return s.ReplaceRoomSnapshots(roomID, []worldSnapshot{{
		WorldID: worldID, WorldName: worldName, Observations: observations, ObservedAt: observedAt,
	}})
}

type worldSnapshot struct {
	WorldID      string
	WorldName    string
	Observations []Observation
	History      []Observation
	ObservedAt   time.Time
	Source       DataSource
	Stopped      bool
}

type snapshotCandidate struct {
	worldSnapshot
	observation Observation
}

func (s *Store) ReplaceRoomSnapshots(roomID string, snapshots []worldSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	rollback := func(err error) error {
		tx.Rollback()
		return err
	}
	var current []playerRecord
	if err := tx.Table(s.table).Where("room_id = ?", roomID).Find(&current).Error; err != nil {
		return rollback(err)
	}
	existingByID := make(map[string]playerRecord, len(current))
	for _, record := range current {
		existingByID[record.UserID] = record
	}
	winners := make(map[string]snapshotCandidate)
	candidates := make(map[string][]snapshotCandidate)
	for _, snapshot := range snapshots {
		for _, observation := range snapshot.Observations {
			candidate := snapshotCandidate{worldSnapshot: snapshot, observation: observation}
			candidates[observation.ID] = append(candidates[observation.ID], candidate)
			winner, exists := winners[observation.ID]
			if !exists || snapshotCandidateWins(candidate, winner, existingByID[observation.ID]) {
				winners[observation.ID] = candidate
			}
		}
	}
	for playerID, candidate := range winners {
		observedWorldIDs := presenceWorldIDs(candidate, candidates[playerID])
		if err := s.upsertSnapshotPlayer(tx, roomID, candidate, observedWorldIDs); err != nil {
			return rollback(err)
		}
	}
	for _, snapshot := range snapshots {
		if snapshot.Stopped {
			if err := s.markStoppedWorldPlayersOffline(tx, roomID, snapshot); err != nil {
				return rollback(err)
			}
			if _, err := s.mergeWorldHistoryTx(tx, roomID, snapshot.WorldID, snapshot.WorldName, snapshot.History, snapshot.ObservedAt); err != nil {
				return rollback(err)
			}
			continue
		}
		if err := s.markMissingSnapshotPlayersOffline(tx, roomID, snapshot, winners); err != nil {
			return rollback(err)
		}
		if _, err := s.mergeWorldHistoryTx(tx, roomID, snapshot.WorldID, snapshot.WorldName, snapshot.History, snapshot.ObservedAt); err != nil {
			return rollback(err)
		}
	}
	return tx.Commit().Error
}

func (s *Store) markStoppedWorldPlayersOffline(tx *gorm.DB, roomID string, snapshot worldSnapshot) error {
	var online []playerRecord
	if err := tx.Table(s.table).Where("room_id = ? AND world_id = ? AND online = ?", roomID, snapshot.WorldID, true).Find(&online).Error; err != nil {
		return err
	}
	for _, record := range online {
		fieldStates := decodeFieldStates(record.FieldStates)
		observedAt := snapshot.ObservedAt.UTC()
		fieldStates["online"] = FieldState{Source: SourceNativeLog, ObservedAt: &observedAt, Status: FreshnessStale}
		encoded, err := encodeFieldStates(fieldStates)
		if err != nil {
			return err
		}
		if err := tx.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, record.UserID).Updates(map[string]interface{}{
			"online": false, "status_changed_at": snapshot.ObservedAt, "field_states": encoded,
			"presence_conflict": false, "observed_world_ids": encodeWorldIDs([]string{record.WorldID}),
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func snapshotCandidateWins(candidate, current snapshotCandidate, existing playerRecord) bool {
	if !candidate.ObservedAt.Equal(current.ObservedAt) {
		return candidate.ObservedAt.After(current.ObservedAt)
	}
	if existing.Online {
		if candidate.WorldID == existing.WorldID && current.WorldID != existing.WorldID {
			return true
		}
		if current.WorldID == existing.WorldID && candidate.WorldID != existing.WorldID {
			return false
		}
	}
	return candidate.WorldID < current.WorldID
}

func (s *Store) upsertSnapshotPlayer(tx *gorm.DB, roomID string, candidate snapshotCandidate, observedWorldIDs []string) error {
	observation := candidate.observation
	encodedWorldIDs := encodeWorldIDs(observedWorldIDs)
	presenceConflict := len(observedWorldIDs) > 1
	var existing playerRecord
	result := tx.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, observation.ID).First(&existing)
	if result.Error != nil && !gorm.IsRecordNotFoundError(result.Error) {
		return result.Error
	}
	if gorm.IsRecordNotFoundError(result.Error) {
		fieldStates, err := encodeFieldStates(observation.Fields)
		if err != nil {
			return err
		}
		record := playerRecord{
			RoomID: roomID, UserID: observation.ID, WorldID: candidate.WorldID, WorldName: candidate.WorldName,
			Name: observation.Name, Prefab: observation.Prefab, Online: true, Admin: observation.Admin,
			Age: observation.Age, NetID: observation.NetID, NetScore: observation.NetScore,
			HealthPercent: observation.HealthPercent, HungerPercent: observation.HungerPercent,
			SanityPercent: observation.SanityPercent, Temperature: observation.Temperature, Moisture: observation.Moisture,
			FieldStates: fieldStates, PresenceConflict: presenceConflict, ObservedWorldIDs: encodedWorldIDs,
			FirstSeenAt: candidate.ObservedAt, LastSeenAt: candidate.ObservedAt, StatusChangedAt: candidate.ObservedAt, LastRefreshedAt: candidate.ObservedAt,
		}
		return tx.Table(s.table).Create(&record).Error
	}
	statusChanged := existing.StatusChangedAt
	if !existing.Online || existing.WorldID != candidate.WorldID {
		statusChanged = candidate.ObservedAt
	}
	fieldStates := decodeFieldStates(existing.FieldStates)
	mergeStoredFieldStates(fieldStates, observation.Fields, existing, observation)
	encodedFieldStates, err := encodeFieldStates(fieldStates)
	if err != nil {
		return err
	}
	updates := map[string]interface{}{
		"world_id": candidate.WorldID, "world_name": candidate.WorldName, "name": observation.Name,
		"online": true, "admin": observation.Admin, "age": observation.Age, "net_id": observation.NetID,
		"performance": nil, "field_states": encodedFieldStates,
		"presence_conflict": presenceConflict, "observed_world_ids": encodedWorldIDs,
		"last_seen_at": candidate.ObservedAt, "last_refreshed_at": candidate.ObservedAt, "status_changed_at": statusChanged,
	}
	setObservedMetric(updates, "net_score", observation.NetScore)
	setObservedMetric(updates, "health_percent", observation.HealthPercent)
	setObservedMetric(updates, "hunger_percent", observation.HungerPercent)
	setObservedMetric(updates, "sanity_percent", observation.SanityPercent)
	setObservedMetric(updates, "temperature", observation.Temperature)
	setObservedMetric(updates, "moisture", observation.Moisture)
	if observation.Prefab != "" {
		updates["prefab"] = observation.Prefab
	}
	if observation.Age == 0 && existing.Age > 0 {
		delete(updates, "age")
	}
	if observation.NetID == "" && existing.NetID != "" {
		delete(updates, "net_id")
	}
	return tx.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, observation.ID).Updates(updates).Error
}

func (s *Store) markMissingSnapshotPlayersOffline(tx *gorm.DB, roomID string, snapshot worldSnapshot, winners map[string]snapshotCandidate) error {
	var online []playerRecord
	if err := tx.Table(s.table).Where("room_id = ? AND world_id = ? AND online = ?", roomID, snapshot.WorldID, true).Find(&online).Error; err != nil {
		return err
	}
	for _, record := range online {
		if _, observed := winners[record.UserID]; observed {
			continue
		}
		fieldStates := decodeFieldStates(record.FieldStates)
		source := snapshot.Source
		if source == "" {
			source = SourceNativeLog
		}
		fieldStates["online"] = liveField(source, snapshot.ObservedAt)
		encodedFieldStates, err := encodeFieldStates(fieldStates)
		if err != nil {
			return err
		}
		if err := tx.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, record.UserID).Updates(map[string]interface{}{
			"online": false, "status_changed_at": snapshot.ObservedAt, "last_refreshed_at": snapshot.ObservedAt,
			"presence_conflict": false, "observed_world_ids": encodeWorldIDs([]string{record.WorldID}),
			"field_states": encodedFieldStates,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) MergeWorldHistory(roomID, worldID, worldName string, observations []Observation, observedAt time.Time) (int, error) {
	tx := s.db.Begin()
	if tx.Error != nil {
		return 0, tx.Error
	}
	rollback := func(err error) (int, error) {
		tx.Rollback()
		return 0, err
	}
	inserted, err := s.mergeWorldHistoryTx(tx, roomID, worldID, worldName, observations, observedAt)
	if err != nil {
		return rollback(err)
	}
	if err := tx.Commit().Error; err != nil {
		return 0, err
	}
	return inserted, nil
}

func (s *Store) mergeWorldHistoryTx(tx *gorm.DB, roomID, worldID, worldName string, observations []Observation, observedAt time.Time) (int, error) {
	inserted := 0
	for _, observation := range observations {
		var existing playerRecord
		result := tx.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, observation.ID).First(&existing)
		if result.Error == nil {
			continue
		}
		if !gorm.IsRecordNotFoundError(result.Error) {
			return 0, result.Error
		}
		record := playerRecord{
			RoomID: roomID, UserID: observation.ID, WorldID: worldID, WorldName: worldName,
			Name: observation.Name, Prefab: observation.Prefab, Online: false, Admin: observation.Admin,
			Age: observation.Age, NetID: observation.NetID,
			HealthPercent: observation.HealthPercent, HungerPercent: observation.HungerPercent,
			SanityPercent: observation.SanityPercent, Temperature: observation.Temperature, Moisture: observation.Moisture,
			PresenceConflict: false, ObservedWorldIDs: encodeWorldIDs([]string{worldID}),
			FirstSeenAt: observedAt, LastSeenAt: observedAt, StatusChangedAt: observedAt, LastRefreshedAt: observedAt,
		}
		fieldStates, err := encodeFieldStates(observation.Fields)
		if err != nil {
			return 0, err
		}
		record.FieldStates = fieldStates
		if err := tx.Table(s.table).Create(&record).Error; err != nil {
			return 0, err
		}
		inserted++
	}
	return inserted, nil
}

func (s *Store) MarkWorldOffline(roomID, worldID string, observedAt time.Time) error {
	return s.db.Table(s.table).Where("room_id = ? AND world_id = ? AND online = ?", roomID, worldID, true).Updates(map[string]interface{}{
		"online": false, "status_changed_at": observedAt, "last_refreshed_at": observedAt,
		"presence_conflict": false,
	}).Error
}

func (s *Store) MarkPlayerOffline(roomID, userID string, observedAt time.Time) error {
	return s.db.Table(s.table).Where("room_id = ? AND user_id = ? AND online = ?", roomID, userID, true).Updates(map[string]interface{}{
		"online": false, "status_changed_at": observedAt, "last_refreshed_at": observedAt,
		"presence_conflict": false,
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
		NetID: record.NetID, NetScore: record.NetScore, Performance: record.Performance, HealthPercent: record.HealthPercent,
		HungerPercent: record.HungerPercent, SanityPercent: record.SanityPercent,
		Temperature: record.Temperature, Moisture: record.Moisture,
		FirstSeenAt: record.FirstSeenAt.UTC(), LastSeenAt: record.LastSeenAt.UTC(),
		StatusChangedAt: record.StatusChangedAt.UTC(), LastRefreshedAt: record.LastRefreshedAt.UTC(),
		Fields: decodeFieldStates(record.FieldStates), PresenceConflict: record.PresenceConflict,
		ObservedWorldIDs: decodeWorldIDs(record.ObservedWorldIDs, record.WorldID),
	}
}

func presenceWorldIDs(winner snapshotCandidate, candidates []snapshotCandidate) []string {
	latest := winner.ObservedAt
	for _, candidate := range candidates {
		if candidate.ObservedAt.After(latest) {
			latest = candidate.ObservedAt
		}
	}
	cutoff := latest.Add(-15 * time.Second)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.ObservedAt.Before(cutoff) || strings.TrimSpace(candidate.WorldID) == "" {
			continue
		}
		seen[candidate.WorldID] = true
	}
	if len(seen) == 0 {
		seen[winner.WorldID] = true
	}
	values := make([]string, 0, len(seen))
	for worldID := range seen {
		values = append(values, worldID)
	}
	sort.Strings(values)
	return values
}

func encodeWorldIDs(values []string) string {
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func decodeWorldIDs(encoded, fallback string) []string {
	values := []string{}
	if strings.TrimSpace(encoded) != "" {
		_ = json.Unmarshal([]byte(encoded), &values)
	}
	if len(values) == 0 && strings.TrimSpace(fallback) != "" {
		values = []string{fallback}
	}
	return values
}

func setObservedMetric(updates map[string]interface{}, column string, value interface{}) {
	switch typed := value.(type) {
	case *int:
		if typed != nil {
			updates[column] = typed
		}
	case *float64:
		if typed != nil {
			updates[column] = typed
		}
	}
}

func encodeFieldStates(values FieldStates) (string, error) {
	if values == nil {
		values = FieldStates{}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode player field states: %w", err)
	}
	return string(encoded), nil
}

func decodeFieldStates(encoded string) FieldStates {
	values := make(FieldStates)
	if strings.TrimSpace(encoded) != "" {
		_ = json.Unmarshal([]byte(encoded), &values)
	}
	return values
}

func mergeStoredFieldStates(current, incoming FieldStates, existing playerRecord, observation Observation) {
	for field, state := range incoming {
		if state.Status == FreshnessUnavailable && storedMetricAvailable(existing, field) {
			previous := current[field]
			if previous.ObservedAt == nil {
				instant := existing.LastRefreshedAt.UTC()
				previous = FieldState{Source: state.Source, ObservedAt: &instant}
			}
			previous.Status = FreshnessStale
			current[field] = previous
			continue
		}
		current[field] = state
	}
	for field, available := range map[string]bool{
		"netScore": observation.NetScore != nil, "healthPercent": observation.HealthPercent != nil,
		"hungerPercent": observation.HungerPercent != nil, "sanityPercent": observation.SanityPercent != nil,
		"temperature": observation.Temperature != nil, "moisture": observation.Moisture != nil,
	} {
		if available {
			continue
		}
		if previous, exists := current[field]; exists && previous.Status == FreshnessLive && storedMetricAvailable(existing, field) {
			previous.Status = FreshnessStale
			current[field] = previous
		}
	}
}

func storedMetricAvailable(record playerRecord, field string) bool {
	switch field {
	case "netScore":
		return record.NetScore != nil
	case "healthPercent":
		return record.HealthPercent != nil
	case "hungerPercent":
		return record.HungerPercent != nil
	case "sanityPercent":
		return record.SanityPercent != nil
	case "temperature":
		return record.Temperature != nil
	case "moisture":
		return record.Moisture != nil
	default:
		return false
	}
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "%", "\\%")
	return strings.ReplaceAll(value, "_", "\\_")
}
