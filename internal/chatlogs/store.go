package chatlogs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dont/internal/rooms"
	"dont/shared"

	"github.com/jinzhu/gorm"
)

type eventRecord struct {
	ID               string `gorm:"primary_key"`
	RoomID           string
	Kind             string
	AnnouncementType string
	PlayerID         string
	PlayerName       string
	Content          string `gorm:"type:text"`
	Fingerprint      string
	SourceTimestamp  string
	OccurredAt       *time.Time
	TimeEstimated    bool
	ObservedAt       time.Time
	PrimaryWorldID   string
	PrimaryWorldRole string
}

type sourceRecord struct {
	ID              string `gorm:"primary_key"`
	EventID         string
	RoomID          string
	WorldID         string
	WorldName       string
	WorldRole       string
	GenerationID    string
	SourceCursor    int64
	SourceTimestamp string
	OccurredAt      *time.Time
	ObservedAt      time.Time
	TimeVersion     int
	TimeEstimated   bool
}

type generationRecord struct {
	Key                int64 `gorm:"primary_key;AUTO_INCREMENT"`
	RoomID             string
	WorldID            string
	GenerationID       string
	FileName           string
	Archived           bool
	StartedAt          *time.Time
	StartedAtEstimated bool
	UpdatedAt          time.Time
	Size               int64
	Cursor             int64
	CaughtUp           bool
	LastSyncedAt       time.Time
	ParserVersion      int `gorm:"not null;default:0"`
	RepairCursor       int64
	ParseErrors        int
	LastParseProblem   string
	Unavailable        bool `gorm:"not null;default:false"`
}

type syncRecord struct {
	RoomID       string `gorm:"primary_key"`
	State        string
	LastError    string `gorm:"type:text"`
	LastSyncedAt *time.Time
	UpdatedAt    time.Time
}

type Store struct {
	db               *gorm.DB
	eventsTable      string
	sourcesTable     string
	generationsTable string
	syncTable        string
}

type GenerationState struct {
	Cursor        int64
	Size          int64
	CaughtUp      bool
	ParserVersion int
	RepairCursor  int64
}

type SyncState struct {
	State        string
	LastError    string
	LastSyncedAt *time.Time
}

type sourceGenerationRecord struct {
	SourceTimestamp    string
	OccurredAt         *time.Time
	TimeEstimated      bool
	TimeVersion        int
	WorldID            string
	WorldRole          string
	ObservedAt         time.Time
	StartedAtEstimated bool
}

func NewStore(db *gorm.DB, prefix string) *Store {
	prefix = strings.TrimSpace(prefix)
	return &Store{
		db: db, eventsTable: prefix + "chat_event", sourcesTable: prefix + "chat_event_source",
		generationsTable: prefix + "chat_log_generation", syncTable: prefix + "chat_log_sync",
	}
}

func (s *Store) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("chat log database is required")
	}
	for _, migration := range []struct {
		table string
		model interface{}
	}{
		{s.eventsTable, &eventRecord{}}, {s.sourcesTable, &sourceRecord{}},
		{s.generationsTable, &generationRecord{}}, {s.syncTable, &syncRecord{}},
	} {
		if err := s.db.Table(migration.table).AutoMigrate(migration.model).Error; err != nil {
			return fmt.Errorf("migrate chat history table %s: %w", migration.table, err)
		}
	}
	for _, index := range []struct {
		table, name string
		unique      bool
		columns     []string
	}{
		{s.eventsTable, "idx_chat_event_room_time", false, []string{"room_id", "occurred_at", "id"}},
		{s.eventsTable, "idx_chat_event_room_kind_time", false, []string{"room_id", "kind", "occurred_at"}},
		{s.eventsTable, "idx_chat_event_room_player_time", false, []string{"room_id", "player_id", "occurred_at"}},
		{s.eventsTable, "idx_chat_event_fingerprint_time", false, []string{"room_id", "fingerprint", "occurred_at"}},
		{s.sourcesTable, "idx_chat_source_event", false, []string{"event_id"}},
		{s.sourcesTable, "idx_chat_source_room_world", false, []string{"room_id", "world_id"}},
		{s.generationsTable, "idx_chat_generation_identity", true, []string{"room_id", "world_id", "generation_id"}},
	} {
		if s.db.NewScope(indexModel(index.table, s)).Dialect().HasIndex(index.table, index.name) {
			continue
		}
		query := s.db.Table(index.table)
		var err error
		if index.unique {
			err = query.AddUniqueIndex(index.name, index.columns...).Error
		} else {
			err = query.AddIndex(index.name, index.columns...).Error
		}
		if err != nil {
			return fmt.Errorf("create chat history index %s: %w", index.name, err)
		}
	}
	return nil
}

func indexModel(table string, store *Store) interface{} {
	switch table {
	case store.eventsTable:
		return eventRecord{}
	case store.sourcesTable:
		return sourceRecord{}
	case store.generationsTable:
		return generationRecord{}
	default:
		return syncRecord{}
	}
}

func (s *Store) Generation(roomID, worldID, generationID string) (GenerationState, error) {
	var record generationRecord
	result := s.db.Table(s.generationsTable).Where("room_id = ? AND world_id = ? AND generation_id = ?", roomID, worldID, generationID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return GenerationState{}, nil
	}
	if result.Error != nil {
		return GenerationState{}, result.Error
	}
	return GenerationState{Cursor: record.Cursor, Size: record.Size, CaughtUp: record.CaughtUp, ParserVersion: record.ParserVersion, RepairCursor: record.RepairCursor}, nil
}

// ReconcileGeneration removes an older estimated identity for the same
// immutable archive after a paired DST startup log becomes available.
func (s *Store) ReconcileGeneration(roomID string, world rooms.World, generation shared.RuntimeChatLogGeneration) error {
	if !generation.Archived || generation.StartedAtEstimated {
		return nil
	}
	var obsolete []generationRecord
	if err := s.db.Table(s.generationsTable).Where(
		"room_id = ? AND world_id = ? AND file_name = ? AND generation_id <> ? AND started_at_estimated = ?",
		roomID, world.ID, generation.FileName, generation.ID, true,
	).Find(&obsolete).Error; err != nil || len(obsolete) == 0 {
		return err
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			tx.Rollback()
			panic(recovered)
		}
	}()
	generationIDs := make([]string, 0, len(obsolete))
	for _, record := range obsolete {
		generationIDs = append(generationIDs, record.GenerationID)
	}
	var sources []sourceRecord
	if err := tx.Table(s.sourcesTable).Where(
		"room_id = ? AND world_id = ? AND generation_id IN (?)", roomID, world.ID, generationIDs,
	).Find(&sources).Error; err != nil {
		tx.Rollback()
		return err
	}
	eventIDs := make(map[string]bool, len(sources))
	for _, source := range sources {
		eventIDs[source.EventID] = true
	}
	if err := tx.Table(s.sourcesTable).Where(
		"room_id = ? AND world_id = ? AND generation_id IN (?)", roomID, world.ID, generationIDs,
	).Delete(&sourceRecord{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Table(s.generationsTable).Where(
		"room_id = ? AND world_id = ? AND generation_id IN (?)", roomID, world.ID, generationIDs,
	).Delete(&generationRecord{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	for eventID := range eventIDs {
		if err := s.reconcileEventSource(tx, eventID); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit().Error
}

func (s *Store) reconcileEventSource(tx *gorm.DB, eventID string) error {
	var sources []sourceGenerationRecord
	if err := tx.Table(s.sourcesTable+" AS chat_source").Select(
		"chat_source.*, chat_generation.started_at_estimated",
	).Joins(
		"JOIN "+s.generationsTable+" AS chat_generation ON chat_generation.room_id = chat_source.room_id AND chat_generation.world_id = chat_source.world_id AND chat_generation.generation_id = chat_source.generation_id",
	).Where("chat_source.event_id = ?", eventID).Scan(&sources).Error; err != nil {
		return err
	}
	if len(sources) == 0 {
		return tx.Table(s.eventsTable).Where("id = ?", eventID).Delete(&eventRecord{}).Error
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if (sources[i].OccurredAt != nil && !sources[i].TimeEstimated) != (sources[j].OccurredAt != nil && !sources[j].TimeEstimated) {
			return sources[i].OccurredAt != nil && !sources[i].TimeEstimated
		}
		left, right := sourcePriority(rooms.WorldRole(sources[i].WorldRole)), sourcePriority(rooms.WorldRole(sources[j].WorldRole))
		if left != right {
			return left < right
		}
		return sources[i].ObservedAt.Before(sources[j].ObservedAt)
	})
	primary := sources[0]
	return tx.Table(s.eventsTable).Where("id = ?", eventID).Updates(map[string]interface{}{
		"source_timestamp":   primary.SourceTimestamp,
		"occurred_at":        primary.OccurredAt,
		"time_estimated":     primary.TimeEstimated || primary.TimeVersion == 0 && primary.StartedAtEstimated,
		"primary_world_id":   primary.WorldID,
		"primary_world_role": primary.WorldRole,
	}).Error
}

func (s *Store) ImportGeneration(roomID string, world rooms.World, generation shared.RuntimeChatLogGeneration, entries []parsedEntry, cursor int64, observedAt time.Time) (int, error) {
	return s.importGeneration(roomID, world, generation, entries, cursor, observedAt, batchProgress{})
}

type batchProgress struct {
	version          int
	repair           bool
	parseErrors      int
	lastParseProblem string
}

func (s *Store) importGeneration(roomID string, world rooms.World, generation shared.RuntimeChatLogGeneration, entries []parsedEntry, cursor int64, observedAt time.Time, progress batchProgress) (int, error) {
	if cursor < 0 || cursor > generation.Size {
		return 0, errors.New("chat generation cursor is invalid")
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return 0, tx.Error
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			tx.Rollback()
			panic(recovered)
		}
	}()
	imported := 0
	for _, entry := range entries {
		created, err := s.importEntry(tx, roomID, world, generation.ID, entry, observedAt)
		if err != nil {
			tx.Rollback()
			return 0, err
		}
		if created {
			imported++
		}
	}
	var startedAt *time.Time
	if !generation.StartedAt.IsZero() {
		value := generation.StartedAt.UTC()
		startedAt = &value
	}
	query := tx.Table(s.generationsTable).Where("room_id = ? AND world_id = ? AND generation_id = ?", roomID, world.ID, generation.ID)
	var existing generationRecord
	found := true
	if err := query.First(&existing).Error; gorm.IsRecordNotFoundError(err) {
		found = false
	} else if err != nil {
		tx.Rollback()
		return 0, err
	}
	processedCursor := cursor
	if found {
		if existing.Cursor > cursor {
			cursor = existing.Cursor
		}
		if existing.Size > generation.Size {
			generation.Size = existing.Size
		}
		if existing.UpdatedAt.After(generation.UpdatedAt) {
			generation.UpdatedAt = existing.UpdatedAt
		}
		generation.Archived = generation.Archived || existing.Archived
		if existing.StartedAt != nil && (startedAt == nil || !existing.StartedAtEstimated && generation.StartedAtEstimated) {
			value := existing.StartedAt.UTC()
			startedAt = &value
		}
		if !existing.StartedAtEstimated {
			generation.StartedAtEstimated = false
		}
	}
	record := generationRecord{
		RoomID: roomID, WorldID: world.ID, GenerationID: generation.ID, FileName: generation.FileName,
		Archived: generation.Archived, StartedAt: startedAt, StartedAtEstimated: generation.StartedAtEstimated,
		UpdatedAt: generation.UpdatedAt.UTC(), Size: generation.Size, Cursor: cursor,
		CaughtUp: cursor >= generation.Size, LastSyncedAt: observedAt.UTC(),
		ParserVersion: existing.ParserVersion, RepairCursor: existing.RepairCursor, ParseErrors: existing.ParseErrors, LastParseProblem: existing.LastParseProblem,
	}
	if progress.repair {
		if existing.RepairCursor == 0 {
			record.ParseErrors = 0
			record.LastParseProblem = ""
		}
		record.RepairCursor = processedCursor
		if processedCursor >= generation.Size {
			record.ParserVersion = progress.version
			record.RepairCursor = 0
		}
	} else if progress.version > 0 && !found {
		record.ParserVersion = progress.version
	}
	record.ParseErrors += progress.parseErrors
	if progress.lastParseProblem != "" {
		record.LastParseProblem = progress.lastParseProblem
	}
	var saveErr error
	if found {
		record.Key = existing.Key
		saveErr = tx.Table(s.generationsTable).Save(&record).Error
	} else {
		saveErr = tx.Table(s.generationsTable).Create(&record).Error
	}
	if saveErr != nil {
		tx.Rollback()
		return 0, saveErr
	}
	if err := tx.Commit().Error; err != nil {
		return 0, err
	}
	return imported, nil
}

func (s *Store) importEntry(tx *gorm.DB, roomID string, world rooms.World, generationID string, entry parsedEntry, observedAt time.Time) (bool, error) {
	sourceID := chatSourceID(roomID, world.ID, generationID, entry.sourceCursor)
	var existing sourceRecord
	if err := tx.Table(s.sourcesTable).Where("id = ?", sourceID).First(&existing).Error; err == nil {
		if entry.timeVersion > 0 {
			if err := s.correctSourceTime(tx, existing, entry); err != nil {
				return false, err
			}
		}
		return false, nil
	} else if !gorm.IsRecordNotFoundError(err) {
		return false, err
	}
	fingerprint := entryIdentity(entry)
	event, matched, err := s.matchEvent(tx, roomID, world.ID, fingerprint, entry.OccurredAt)
	if err != nil {
		return false, err
	}
	if !matched {
		event = eventRecord{
			ID: sourceID, RoomID: roomID, Kind: string(entry.Kind), AnnouncementType: entry.AnnouncementType,
			PlayerID: entry.PlayerID, PlayerName: entry.PlayerName, Content: entry.Content, Fingerprint: fingerprint,
			SourceTimestamp: entry.SourceTimestamp, OccurredAt: entry.OccurredAt, TimeEstimated: entry.TimeEstimated, ObservedAt: observedAt.UTC(),
			PrimaryWorldID: world.ID, PrimaryWorldRole: string(world.Role),
		}
		if err := tx.Table(s.eventsTable).Create(&event).Error; err != nil {
			return false, err
		}
	} else if sourcePriority(world.Role) < sourcePriority(rooms.WorldRole(event.PrimaryWorldRole)) {
		updates := map[string]interface{}{
			"source_timestamp": entry.SourceTimestamp, "occurred_at": entry.OccurredAt,
			"time_estimated":   entry.TimeEstimated,
			"primary_world_id": world.ID, "primary_world_role": string(world.Role),
		}
		if err := tx.Table(s.eventsTable).Where("id = ?", event.ID).Updates(updates).Error; err != nil {
			return false, err
		}
	}
	source := sourceRecord{
		ID: sourceID, EventID: event.ID, RoomID: roomID, WorldID: world.ID, WorldName: world.Name,
		WorldRole: string(world.Role), GenerationID: generationID, SourceCursor: entry.sourceCursor,
		SourceTimestamp: entry.SourceTimestamp, OccurredAt: entry.OccurredAt, ObservedAt: observedAt.UTC(),
		TimeVersion: entry.timeVersion, TimeEstimated: entry.TimeEstimated,
	}
	if err := tx.Table(s.sourcesTable).Create(&source).Error; err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) matchEvent(tx *gorm.DB, roomID, worldID, fingerprint string, occurredAt *time.Time) (eventRecord, bool, error) {
	if occurredAt == nil {
		return eventRecord{}, false, nil
	}
	window := time.Duration(dedupWindow) * time.Second
	var candidates []eventRecord
	if err := tx.Table(s.eventsTable).Where(
		"room_id = ? AND fingerprint = ? AND occurred_at BETWEEN ? AND ?",
		roomID, fingerprint, occurredAt.Add(-window), occurredAt.Add(window),
	).Limit(8).Find(&candidates).Error; err != nil {
		return eventRecord{}, false, err
	}
	type match struct {
		event eventRecord
		delta time.Duration
	}
	matches := make([]match, 0, len(candidates))
	for _, candidate := range candidates {
		var count int
		if err := tx.Table(s.sourcesTable).Where("event_id = ? AND world_id = ?", candidate.ID, worldID).Count(&count).Error; err != nil {
			return eventRecord{}, false, err
		}
		if count != 0 || candidate.OccurredAt == nil {
			continue
		}
		delta := candidate.OccurredAt.Sub(*occurredAt)
		if delta < 0 {
			delta = -delta
		}
		matches = append(matches, match{event: candidate, delta: delta})
	}
	if len(matches) == 0 {
		return eventRecord{}, false, nil
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].delta < matches[j].delta })
	if len(matches) > 1 && matches[0].delta == matches[1].delta {
		return eventRecord{}, false, nil
	}
	return matches[0].event, true, nil
}

func (s *Store) MarkSync(roomID, state, message string, syncedAt *time.Time) error {
	now := time.Now().UTC()
	if syncedAt != nil {
		value := syncedAt.UTC()
		syncedAt = &value
	}
	var record syncRecord
	result := s.db.Table(s.syncTable).Where("room_id = ?", roomID).First(&record)
	if result.Error != nil && !gorm.IsRecordNotFoundError(result.Error) {
		return result.Error
	}
	if gorm.IsRecordNotFoundError(result.Error) {
		record = syncRecord{RoomID: roomID, State: state, LastError: message, LastSyncedAt: syncedAt, UpdatedAt: now}
		return s.db.Table(s.syncTable).Create(&record).Error
	}
	updates := map[string]interface{}{"state": state, "last_error": message, "updated_at": now}
	if syncedAt != nil {
		updates["last_synced_at"] = syncedAt
	}
	return s.db.Table(s.syncTable).Where("room_id = ?", roomID).Updates(updates).Error
}

func (s *Store) SyncState(roomID string) (SyncState, error) {
	var record syncRecord
	result := s.db.Table(s.syncTable).Where("room_id = ?", roomID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return SyncState{State: "pending"}, nil
	}
	if result.Error != nil {
		return SyncState{}, result.Error
	}
	return SyncState{State: record.State, LastError: record.LastError, LastSyncedAt: record.LastSyncedAt}, nil
}

func (s *Store) List(roomID string, filter Filter) (List, error) {
	base := s.db.Table(s.eventsTable+" AS chat_event").Where("chat_event.room_id = ?", roomID)
	if filter.WorldID != "" {
		base = base.Where("EXISTS (SELECT 1 FROM "+s.sourcesTable+" AS chat_source WHERE chat_source.event_id = chat_event.id AND chat_source.world_id = ?)", filter.WorldID)
	}
	if filter.Query != "" {
		like := "%" + escapeLike(filter.Query) + "%"
		base = base.Where("(chat_event.player_id LIKE ? ESCAPE '\\' OR chat_event.player_name LIKE ? ESCAPE '\\' OR chat_event.content LIKE ? ESCAPE '\\')", like, like, like)
	}
	counts := map[Kind]int{KindSay: 0, KindWhisper: 0, KindAnnouncement: 0}
	rows, err := base.Select("chat_event.kind, count(*)").Group("chat_event.kind").Rows()
	if err != nil {
		return List{}, err
	}
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			rows.Close()
			return List{}, err
		}
		counts[Kind(kind)] = count
	}
	rows.Close()
	query := base
	if filter.Kind != "" {
		query = query.Where("chat_event.kind = ?", string(filter.Kind))
	}
	var total int
	if err := query.Count(&total).Error; err != nil {
		return List{}, err
	}
	var records []eventRecord
	if err := query.Order("chat_event.occurred_at DESC, chat_event.observed_at DESC, chat_event.id DESC").Limit(filter.Limit).Offset(filter.Offset).Find(&records).Error; err != nil {
		return List{}, err
	}
	items, err := s.entries(records)
	if err != nil {
		return List{}, err
	}
	state, err := s.SyncState(roomID)
	if err != nil {
		return List{}, err
	}
	health, err := s.historyHealth(roomID)
	if err != nil {
		return List{}, err
	}
	if state.State == "ready" && health.PendingGenerations > 0 {
		state.State = "pending"
	}
	if state.State == "ready" && (health.ParseErrors > 0 || health.UncertainTimes > 0 || health.UnavailableGenerations > 0) {
		state.State = "review"
	}
	return List{
		HistoryHealth: health,
		Items:         items, Total: total, Counts: counts, Limit: filter.Limit, Offset: filter.Offset,
		HistoryAvailable: true, SyncState: state.State, SyncMessage: state.LastError, LastSyncedAt: state.LastSyncedAt,
	}, nil
}

func (s *Store) entries(records []eventRecord) ([]Entry, error) {
	if len(records) == 0 {
		return []Entry{}, nil
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	var sources []sourceRecord
	if err := s.db.Table(s.sourcesTable).Where("event_id IN (?)", ids).Order("observed_at ASC").Find(&sources).Error; err != nil {
		return nil, err
	}
	byEvent := make(map[string][]Source, len(records))
	for _, source := range sources {
		byEvent[source.EventID] = append(byEvent[source.EventID], Source{
			WorldID: source.WorldID, WorldName: source.WorldName, WorldRole: rooms.WorldRole(source.WorldRole),
		})
	}
	items := make([]Entry, 0, len(records))
	for _, record := range records {
		entry := Entry{
			ID: record.ID, Kind: Kind(record.Kind), AnnouncementType: record.AnnouncementType,
			PlayerID: record.PlayerID, PlayerName: record.PlayerName, Content: record.Content,
			SourceTimestamp: record.SourceTimestamp, OccurredAt: record.OccurredAt, TimeEstimated: record.TimeEstimated, Sources: byEvent[record.ID],
		}
		sortSources(entry.Sources)
		items = append(items, entry)
	}
	return items, nil
}

func chatSourceID(roomID, worldID, generationID string, cursor int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", roomID, worldID, generationID, cursor)))
	return hex.EncodeToString(sum[:16])
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "%", "\\%")
	return strings.ReplaceAll(value, "_", "\\_")
}
