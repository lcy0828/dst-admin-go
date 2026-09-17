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
	GameplayState    string `gorm:"type:varchar(32);index"`
	Online           bool   `gorm:"not null;index"`
	Admin            bool   `gorm:"not null"`
	Age              int
	NetID            string `gorm:"type:varchar(128)"`
	NetScore         *int
	Performance      *int
	HealthPercent    *float64
	HungerPercent    *float64
	SanityPercent    *float64
	Health           *float64
	HealthMax        *float64
	Hunger           *float64
	HungerMax        *float64
	Sanity           *float64
	SanityMax        *float64
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
	HistoryOnly  bool
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
	observedWorlds := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		if !snapshot.HistoryOnly {
			observedWorlds[snapshot.WorldID] = true
		}
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
		existing := existingByID[playerID]
		// A single-shard refresh after a player action also sees the cluster-wide
		// connection list. Its remote "loading" entry cannot replace another
		// shard's confirmed entity state when that shard was not sampled.
		if existing.Online && storedWorldConfirmed(existing) && existing.WorldID != candidate.WorldID &&
			!observedWorlds[existing.WorldID] && !candidateHasLocalPlayer(candidate) &&
			(candidate.observation.GameplayState == GameplayStateLoading || candidate.observation.GameplayState == GameplayStateSelectingCharacter) {
			continue
		}
		observedWorldIDs := presenceWorldIDs(candidate, candidates[playerID])
		if err := s.upsertSnapshotPlayer(tx, roomID, candidate, observedWorldIDs); err != nil {
			return rollback(err)
		}
	}
	for _, snapshot := range snapshots {
		if snapshot.HistoryOnly {
			if _, err := s.mergeWorldHistoryTx(tx, roomID, snapshot.WorldID, snapshot.WorldName, snapshot.History, snapshot.ObservedAt); err != nil {
				return rollback(err)
			}
			continue
		}
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
		markFieldStale(fieldStates, "gameplayState")
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
	candidateLocal := candidateHasLocalPlayer(candidate)
	currentLocal := candidateHasLocalPlayer(current)
	if candidateLocal != currentLocal {
		return candidateLocal
	}
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
	observation.Fields = copyFieldStates(observation.Fields)
	source := candidate.Source
	if source == "" {
		source = observation.Fields["online"].Source
	}
	if source == "" {
		source = SourceNativeLog
	}
	observation.Fields["lastSeenAt"] = liveField(source, candidate.ObservedAt)
	observation.Fields["world"] = FieldState{Source: source, Status: FreshnessUnavailable}
	if candidateHasLocalPlayer(candidate) || source == SourceNativeLog {
		observation.Fields["world"] = liveField(source, candidate.ObservedAt)
	}
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
			Name: observation.Name, Prefab: observation.Prefab, GameplayState: observation.GameplayState, Online: true, Admin: observation.Admin,
			Age: observation.Age, NetID: observation.NetID, NetScore: observation.NetScore,
			HealthPercent: observation.HealthPercent, HungerPercent: observation.HungerPercent,
			SanityPercent: observation.SanityPercent, Temperature: observation.Temperature, Moisture: observation.Moisture,
			Health: observation.Health, HealthMax: observation.HealthMax, Hunger: observation.Hunger, HungerMax: observation.HungerMax,
			Sanity: observation.Sanity, SanityMax: observation.SanityMax,
			FieldStates: fieldStates, PresenceConflict: presenceConflict, ObservedWorldIDs: encodedWorldIDs,
			FirstSeenAt: candidate.ObservedAt, LastSeenAt: candidate.ObservedAt, StatusChangedAt: candidate.ObservedAt, LastRefreshedAt: candidate.ObservedAt,
		}
		return tx.Table(s.table).Create(&record).Error
	}
	if candidate.ObservedAt.Before(existing.LastRefreshedAt) {
		return nil
	}
	statusChanged := existing.StatusChangedAt
	fieldStates := decodeFieldStates(existing.FieldStates)
	worldID, worldName := candidate.WorldID, candidate.WorldName
	if observation.Fields["world"].Status == FreshnessUnavailable && storedWorldConfirmed(existing) {
		worldID, worldName = existing.WorldID, existing.WorldName
		previous := storedWorldField(existing)
		previous.Status = FreshnessStale
		observation.Fields["world"] = previous
	}
	if !existing.Online || existing.WorldID != worldID {
		statusChanged = candidate.ObservedAt
	}
	lastSeen := candidate.ObservedAt
	if existing.LastSeenAt.After(lastSeen) {
		lastSeen = existing.LastSeenAt
		delete(observation.Fields, "lastSeenAt")
	}
	mergeStoredFieldStates(fieldStates, observation.Fields, existing, observation)
	encodedFieldStates, err := encodeFieldStates(fieldStates)
	if err != nil {
		return err
	}
	updates := map[string]interface{}{
		"world_id": worldID, "world_name": worldName, "name": observation.Name,
		"online": true, "admin": observation.Admin, "age": observation.Age, "net_id": observation.NetID,
		"performance": nil, "field_states": encodedFieldStates,
		"presence_conflict": presenceConflict, "observed_world_ids": encodedWorldIDs,
		"last_seen_at": lastSeen, "last_refreshed_at": candidate.ObservedAt, "status_changed_at": statusChanged,
	}
	setObservedMetric(updates, "net_score", observation.NetScore)
	setObservedMetric(updates, "health_percent", observation.HealthPercent)
	setObservedMetric(updates, "hunger_percent", observation.HungerPercent)
	setObservedMetric(updates, "sanity_percent", observation.SanityPercent)
	setObservedMetric(updates, "health", observation.Health)
	setObservedMetric(updates, "health_max", observation.HealthMax)
	setObservedMetric(updates, "hunger", observation.Hunger)
	setObservedMetric(updates, "hunger_max", observation.HungerMax)
	setObservedMetric(updates, "sanity", observation.Sanity)
	setObservedMetric(updates, "sanity_max", observation.SanityMax)
	setObservedMetric(updates, "temperature", observation.Temperature)
	setObservedMetric(updates, "moisture", observation.Moisture)
	if observation.Prefab != "" {
		updates["prefab"] = observation.Prefab
	}
	if observation.GameplayState != "" {
		updates["gameplay_state"] = observation.GameplayState
	}
	if observation.Age == 0 && existing.Age > 0 {
		delete(updates, "age")
	}
	if observation.NetID == "" && existing.NetID != "" {
		delete(updates, "net_id")
	}
	if existing.FirstSeenAt.IsZero() || candidate.ObservedAt.Before(existing.FirstSeenAt) {
		updates["first_seen_at"] = candidate.ObservedAt
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
		markFieldStale(fieldStates, "gameplayState")
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
		historyWorldID, historyWorldName := worldID, worldName
		if observation.HistoryWorldID != "" {
			historyWorldID, historyWorldName = observation.HistoryWorldID, observation.HistoryWorldName
		}
		var existing playerRecord
		result := tx.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, observation.ID).First(&existing)
		if result.Error == nil {
			if err := s.enrichHistoryPlayer(tx, existing, historyWorldID, historyWorldName, observation, observedAt); err != nil {
				return 0, err
			}
			continue
		}
		if !gorm.IsRecordNotFoundError(result.Error) {
			return 0, result.Error
		}
		record := playerRecord{
			RoomID: roomID, UserID: observation.ID, WorldID: historyWorldID, WorldName: historyWorldName,
			Name: observation.Name, Prefab: observation.Prefab, GameplayState: observation.GameplayState, Online: false, Admin: observation.Admin,
			Age: observation.Age, NetID: observation.NetID,
			HealthPercent: observation.HealthPercent, HungerPercent: observation.HungerPercent,
			SanityPercent: observation.SanityPercent, Temperature: observation.Temperature, Moisture: observation.Moisture,
			Health: observation.Health, HealthMax: observation.HealthMax, Hunger: observation.Hunger, HungerMax: observation.HungerMax,
			Sanity: observation.Sanity, SanityMax: observation.SanityMax,
			PresenceConflict: false, ObservedWorldIDs: encodeWorldIDs([]string{historyWorldID}),
			FirstSeenAt: observation.FirstSeenAt, LastSeenAt: observation.LastSeenAt,
			StatusChangedAt: observation.LastDisconnectedAt, LastRefreshedAt: time.Time{},
		}
		fieldStates, err := encodeFieldStates(observation.Fields)
		if err != nil {
			return 0, err
		}
		record.FieldStates = fieldStates
		if err := tx.Table(s.table).Create(&record).Error; err != nil {
			return 0, err
		}
		if err := s.enrichHistoryPlayer(tx, record, historyWorldID, historyWorldName, observation, observedAt); err != nil {
			return 0, err
		}
		inserted++
	}
	return inserted, nil
}

func (s *Store) MarkWorldOffline(roomID, worldID string, observedAt time.Time) error {
	return s.markRecordsOffline(roomID, "world_id = ?", worldID, observedAt)
}

// ObserveRuntimePresence updates only presence that has become invalid. It does
// not claim that a runtime status check produced a new player telemetry sample.
func (s *Store) ObserveRuntimePresence(roomID string, stopped, unconfirmed []string, observedAt time.Time) error {
	if len(stopped)+len(unconfirmed) == 0 {
		return nil
	}
	worldIDs := append(append([]string{}, stopped...), unconfirmed...)
	confirmedStopped := make(map[string]bool, len(stopped))
	for _, worldID := range stopped {
		confirmedStopped[worldID] = true
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback()
	var records []playerRecord
	if err := tx.Table(s.table).Where("room_id = ? AND world_id IN (?) AND online = ?", roomID, worldIDs, true).Find(&records).Error; err != nil {
		return err
	}
	for _, record := range records {
		states := decodeFieldStates(record.FieldStates)
		for field, state := range states {
			if state.Status == FreshnessLive {
				state.Status = FreshnessStale
				states[field] = state
			}
		}
		if _, exists := states["online"]; !exists {
			instant := record.LastRefreshedAt.UTC()
			states["online"] = FieldState{Source: SourceNativeLog, ObservedAt: &instant, Status: FreshnessStale}
		}
		if confirmedStopped[record.WorldID] {
			instant := observedAt.UTC()
			states["online"] = FieldState{Source: SourceNativeLog, ObservedAt: &instant, Status: FreshnessStale}
		}
		encoded, err := encodeFieldStates(states)
		if err != nil {
			return err
		}
		if !confirmedStopped[record.WorldID] && encoded == record.FieldStates {
			continue
		}
		updates := map[string]interface{}{"field_states": encoded}
		if confirmedStopped[record.WorldID] {
			updates["online"], updates["status_changed_at"] = false, observedAt.UTC()
			updates["presence_conflict"] = false
			updates["observed_world_ids"] = encodeWorldIDs([]string{record.WorldID})
		}
		if err := tx.Table(s.table).Where("room_id = ? AND user_id = ? AND world_id = ? AND online = ?", roomID, record.UserID, record.WorldID, true).Updates(updates).Error; err != nil {
			return err
		}
	}
	return tx.Commit().Error
}

func (s *Store) MarkWorldStale(roomID, worldID string) error {
	return s.ObserveRuntimePresence(roomID, nil, []string{worldID}, time.Time{})
}

func (s *Store) MarkPlayerOffline(roomID, userID string, observedAt time.Time) error {
	return s.markRecordsOffline(roomID, "user_id = ?", userID, observedAt)
}

func (s *Store) markRecordsOffline(roomID, fieldClause string, fieldValue interface{}, observedAt time.Time) error {
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	rollback := func(err error) error {
		tx.Rollback()
		return err
	}
	var records []playerRecord
	query := tx.Table(s.table).Where("room_id = ? AND online = ?", roomID, true).Where(fieldClause, fieldValue)
	if err := query.Find(&records).Error; err != nil {
		return rollback(err)
	}
	for _, record := range records {
		states := decodeFieldStates(record.FieldStates)
		states["online"] = liveField(SourceNativeLog, observedAt)
		markFieldStale(states, "gameplayState")
		encoded, err := encodeFieldStates(states)
		if err != nil {
			return rollback(err)
		}
		if err := tx.Table(s.table).Where("room_id = ? AND user_id = ?", roomID, record.UserID).Updates(map[string]interface{}{
			"online": false, "status_changed_at": observedAt, "last_refreshed_at": observedAt,
			"presence_conflict": false, "field_states": encoded,
		}).Error; err != nil {
			return rollback(err)
		}
	}
	return tx.Commit().Error
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

func (s *Store) Counts(roomID string) (int, int, int, *time.Time, error) {
	var total int
	base := s.db.Table(s.table).Where("room_id = ?", roomID)
	if err := base.Count(&total).Error; err != nil {
		return 0, 0, 0, nil, err
	}
	var onlineRecords []playerRecord
	if err := s.db.Table(s.table).Select("field_states").Where("room_id = ? AND online = ?", roomID, true).Find(&onlineRecords).Error; err != nil {
		return 0, 0, 0, nil, err
	}
	staleOnline := 0
	for _, record := range onlineRecords {
		if decodeFieldStates(record.FieldStates)["online"].Status != FreshnessLive {
			staleOnline++
		}
	}
	online := len(onlineRecords) - staleOnline
	var record playerRecord
	result := s.db.Table(s.table).Where("room_id = ?", roomID).Order("last_refreshed_at DESC").First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return total, online, staleOnline, nil, nil
	}
	if result.Error != nil {
		return 0, 0, 0, nil, result.Error
	}
	refreshed := record.LastRefreshedAt.UTC()
	return total, online, staleOnline, &refreshed, nil
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
	fields := decodeFieldStates(record.FieldStates)
	presence := fields["online"]
	if presence.Status == "" {
		presence.Status = FreshnessUnavailable
	}
	return Player{
		ID: record.UserID, RoomID: record.RoomID, WorldID: record.WorldID, WorldName: record.WorldName,
		WorldConfirmed: storedWorldConfirmed(record),
		Name:           record.Name, Prefab: record.Prefab, GameplayState: record.GameplayState, Online: record.Online, Admin: record.Admin, Age: record.Age,
		NetID: record.NetID, NetScore: record.NetScore, Performance: record.Performance, HealthPercent: record.HealthPercent,
		HungerPercent: record.HungerPercent, SanityPercent: record.SanityPercent,
		Health: record.Health, HealthMax: record.HealthMax, Hunger: record.Hunger, HungerMax: record.HungerMax,
		Sanity: record.Sanity, SanityMax: record.SanityMax,
		Temperature: record.Temperature, Moisture: record.Moisture,
		FirstSeenAt: record.FirstSeenAt.UTC(), LastSeenAt: record.LastSeenAt.UTC(),
		LastSeenSource:  fields["lastSeenAt"].Source,
		LastConnectedAt: fields["connected"].ObservedAt, LastDisconnectedAt: fields["disconnected"].ObservedAt,
		StatusChangedAt: record.StatusChangedAt.UTC(), LastRefreshedAt: record.LastRefreshedAt.UTC(),
		PresenceStatus: presence.Status, PresenceObservedAt: presence.ObservedAt,
		Fields: fields, PresenceConflict: record.PresenceConflict,
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
	hasLocalPlayer := false
	for _, candidate := range candidates {
		if !candidate.ObservedAt.Before(cutoff) && candidateHasLocalPlayer(candidate) {
			hasLocalPlayer = true
			break
		}
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.ObservedAt.Before(cutoff) || strings.TrimSpace(candidate.WorldID) == "" || hasLocalPlayer && !candidateHasLocalPlayer(candidate) {
			continue
		}
		seen[candidate.WorldID] = true
	}
	// TheNet:GetClientTable is cluster-wide. When no shard has a local player
	// entity yet, every shard may report the same selecting/loading client.
	if !hasLocalPlayer {
		seen = map[string]bool{winner.WorldID: true}
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

func candidateHasLocalPlayer(candidate snapshotCandidate) bool {
	switch candidate.observation.GameplayState {
	case GameplayStateAlive, GameplayStateDead, GameplayStateGhost, GameplayStateMigrating:
		return true
	}
	return candidate.observation.HealthPercent != nil || candidate.observation.HungerPercent != nil ||
		candidate.observation.SanityPercent != nil || candidate.observation.Temperature != nil || candidate.observation.Moisture != nil ||
		candidate.observation.Health != nil || candidate.observation.Hunger != nil || candidate.observation.Sanity != nil
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

func markFieldStale(states FieldStates, field string) {
	state, exists := states[field]
	if !exists || state.Status != FreshnessLive {
		return
	}
	state.Status = FreshnessStale
	states[field] = state
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
		"gameplayState": observation.GameplayState != "",
		"netScore":      observation.NetScore != nil, "healthPercent": observation.HealthPercent != nil,
		"hungerPercent": observation.HungerPercent != nil, "sanityPercent": observation.SanityPercent != nil,
		"health": observation.Health != nil, "healthMax": observation.HealthMax != nil,
		"hunger": observation.Hunger != nil, "hungerMax": observation.HungerMax != nil,
		"sanity": observation.Sanity != nil, "sanityMax": observation.SanityMax != nil,
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
	case "gameplayState":
		return record.GameplayState != ""
	case "netScore":
		return record.NetScore != nil
	case "healthPercent":
		return record.HealthPercent != nil
	case "hungerPercent":
		return record.HungerPercent != nil
	case "sanityPercent":
		return record.SanityPercent != nil
	case "health":
		return record.Health != nil
	case "healthMax":
		return record.HealthMax != nil
	case "hunger":
		return record.Hunger != nil
	case "hungerMax":
		return record.HungerMax != nil
	case "sanity":
		return record.Sanity != nil
	case "sanityMax":
		return record.SanityMax != nil
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
