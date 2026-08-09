package structuredlogs

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type entryRecord struct {
	ID              int64 `gorm:"primary_key;AUTO_INCREMENT"`
	RoomID          string
	WorldID         string
	WorldName       string
	Type            string
	Content         string `gorm:"type:text"`
	RawContent      string `gorm:"type:text"`
	RuleID          string
	RuleName        string
	SourceCursor    int64
	SourceTimestamp string
	OccurredAt      *time.Time
	ObservedAt      time.Time
}

type ruleRecord struct {
	Key         int64  `gorm:"primary_key;AUTO_INCREMENT"`
	ID          string `gorm:"unique_index:idx_structured_rule_room_id"`
	RoomID      string `gorm:"unique_index:idx_structured_rule_room_id"`
	Name        string
	Description string
	LogType     string
	Pattern     string `gorm:"type:text"`
	Regex       bool
	Enabled     bool
	Priority    int
	MatchMode   string
	TailPattern string `gorm:"type:text"`
	BuiltIn     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type refreshRecord struct {
	RoomID      string `gorm:"primary_key"`
	WorldID     string `gorm:"primary_key"`
	RefreshedAt time.Time
}

type Store struct {
	db           *gorm.DB
	entriesTable string
	rulesTable   string
	refreshTable string
	now          func() time.Time
}

func NewStore(db *gorm.DB, prefix string) *Store {
	prefix = strings.TrimSpace(prefix)
	return &Store{
		db: db, entriesTable: prefix + "structured_log", rulesTable: prefix + "structured_log_rule",
		refreshTable: prefix + "structured_log_refresh", now: time.Now,
	}
}

func (s *Store) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("structured log database is required")
	}
	if err := s.db.Table(s.entriesTable).AutoMigrate(&entryRecord{}).Error; err != nil {
		return fmt.Errorf("migrate structured logs: %w", err)
	}
	if err := s.db.Table(s.rulesTable).AutoMigrate(&ruleRecord{}).Error; err != nil {
		return fmt.Errorf("migrate structured log rules: %w", err)
	}
	if err := s.db.Table(s.refreshTable).AutoMigrate(&refreshRecord{}).Error; err != nil {
		return fmt.Errorf("migrate structured log refresh state: %w", err)
	}
	for _, index := range []struct {
		table, name string
		columns     []string
	}{
		{s.entriesTable, "idx_structured_log_room_world", []string{"room_id", "world_id"}},
		{s.entriesTable, "idx_structured_log_room_type", []string{"room_id", "type"}},
		{s.entriesTable, "idx_structured_log_observed", []string{"observed_at"}},
		{s.rulesTable, "idx_structured_rule_room_priority", []string{"room_id", "priority"}},
	} {
		if !s.db.NewScope(entryRecord{}).Dialect().HasIndex(index.table, index.name) {
			if err := s.db.Table(index.table).AddIndex(index.name, index.columns...).Error; err != nil {
				return fmt.Errorf("create structured log index %s: %w", index.name, err)
			}
		}
	}
	return nil
}

func (s *Store) ReplaceWorldSnapshot(roomID, worldID, worldName string, entries []Entry, observedAt time.Time) error {
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer func() {
		if recoverValue := recover(); recoverValue != nil {
			tx.Rollback()
			panic(recoverValue)
		}
	}()
	if err := tx.Table(s.entriesTable).Where("room_id = ? AND world_id = ?", roomID, worldID).Delete(&entryRecord{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	for _, entry := range entries {
		record := entryRecord{
			RoomID: roomID, WorldID: worldID, WorldName: worldName, Type: string(entry.Type), Content: entry.Content,
			RawContent: entry.RawContent, RuleID: entry.RuleID, RuleName: entry.RuleName, SourceCursor: entry.SourceCursor,
			SourceTimestamp: entry.SourceTimestamp, OccurredAt: entry.OccurredAt, ObservedAt: observedAt,
		}
		if err := tx.Table(s.entriesTable).Create(&record).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	refresh := refreshRecord{RoomID: roomID, WorldID: worldID, RefreshedAt: observedAt}
	if err := tx.Table(s.refreshTable).Where("room_id = ? AND world_id = ?", roomID, worldID).Assign(refresh).FirstOrCreate(&refresh).Error; err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit().Error
}

func (s *Store) ClearWorldSnapshot(roomID, worldID string) (int64, error) {
	tx := s.db.Begin()
	if tx.Error != nil {
		return 0, tx.Error
	}
	defer func() {
		if recoverValue := recover(); recoverValue != nil {
			tx.Rollback()
			panic(recoverValue)
		}
	}()
	deleted := tx.Table(s.entriesTable).Where("room_id = ? AND world_id = ?", roomID, worldID).Delete(&entryRecord{})
	if deleted.Error != nil {
		tx.Rollback()
		return 0, deleted.Error
	}
	if err := tx.Table(s.refreshTable).Where("room_id = ? AND world_id = ?", roomID, worldID).Delete(&refreshRecord{}).Error; err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Commit().Error; err != nil {
		return 0, err
	}
	return deleted.RowsAffected, nil
}

func (s *Store) List(roomID string, filter ListFilter) ([]Entry, int, error) {
	query := s.db.Table(s.entriesTable).Where("room_id = ?", roomID)
	if filter.WorldID != "" {
		query = query.Where("world_id = ?", filter.WorldID)
	}
	if filter.Type != "" {
		query = query.Where("type = ?", string(filter.Type))
	}
	if filter.Query != "" {
		like := "%" + escapeLike(filter.Query) + "%"
		query = query.Where("(content LIKE ? ESCAPE '\\' OR raw_content LIKE ? ESCAPE '\\')", like, like)
	}
	var total int
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var records []entryRecord
	if err := query.Order("id DESC").Limit(filter.Limit).Offset(filter.Offset).Find(&records).Error; err != nil {
		return nil, 0, err
	}
	items := make([]Entry, 0, len(records))
	for _, record := range records {
		items = append(items, entryFromRecord(record))
	}
	return items, total, nil
}

func (s *Store) Counts(roomID, worldID string) (map[LogType]int, *time.Time, error) {
	entries := s.db.Table(s.entriesTable).Select("type, count(*) AS count").Where("room_id = ?", roomID)
	refreshes := s.db.Table(s.refreshTable).Where("room_id = ?", roomID)
	if worldID != "" {
		entries = entries.Where("world_id = ?", worldID)
		refreshes = refreshes.Where("world_id = ?", worldID)
	}
	rows, err := entries.Group("type").Rows()
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	counts := make(map[LogType]int)
	for rows.Next() {
		var value string
		var count int
		if err := rows.Scan(&value, &count); err != nil {
			return nil, nil, err
		}
		counts[LogType(value)] = count
	}
	var record refreshRecord
	result := refreshes.Order("refreshed_at DESC").First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return counts, nil, nil
	}
	if result.Error != nil {
		return nil, nil, result.Error
	}
	value := record.RefreshedAt
	return counts, &value, nil
}

func (s *Store) Rules(roomID string) ([]Rule, error) {
	var records []ruleRecord
	if err := s.db.Table(s.rulesTable).Where("room_id = ?", roomID).Order("priority DESC, name ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	rules := make([]Rule, 0, len(records))
	for _, record := range records {
		rules = append(rules, ruleFromRecord(record))
	}
	return rules, nil
}

func (s *Store) CreateRule(rule Rule) (Rule, error) {
	now := s.now().UTC()
	rule.CreatedAt, rule.UpdatedAt = now, now
	record := ruleToRecord(rule)
	if err := s.db.Table(s.rulesTable).Create(&record).Error; err != nil {
		return Rule{}, err
	}
	return ruleFromRecord(record), nil
}

func (s *Store) Rule(roomID, ruleID string) (Rule, error) {
	var record ruleRecord
	if err := s.db.Table(s.rulesTable).Where("room_id = ? AND id = ?", roomID, ruleID).First(&record).Error; err != nil {
		if gorm.IsRecordNotFoundError(err) {
			return Rule{}, ErrRuleNotFound
		}
		return Rule{}, err
	}
	return ruleFromRecord(record), nil
}

func (s *Store) UpdateRule(rule Rule) (Rule, error) {
	rule.UpdatedAt = s.now().UTC()
	record := ruleToRecord(rule)
	result := s.db.Table(s.rulesTable).Where("room_id = ? AND id = ?", rule.RoomID, rule.ID).Updates(map[string]interface{}{
		"name": rule.Name, "description": rule.Description, "log_type": string(rule.LogType), "pattern": rule.Pattern,
		"regex": rule.Regex, "enabled": rule.Enabled, "priority": rule.Priority, "match_mode": string(rule.MatchMode),
		"tail_pattern": rule.TailPattern, "updated_at": rule.UpdatedAt,
	})
	if result.Error != nil {
		return Rule{}, result.Error
	}
	if result.RowsAffected == 0 {
		return Rule{}, ErrRuleNotFound
	}
	return ruleFromRecord(record), nil
}

func (s *Store) DeleteRule(roomID, ruleID string) error {
	rule, err := s.Rule(roomID, ruleID)
	if err != nil {
		return err
	}
	if rule.BuiltIn {
		return ErrBuiltInRule
	}
	return s.db.Table(s.rulesTable).Where("room_id = ? AND id = ?", roomID, ruleID).Delete(&ruleRecord{}).Error
}

func entryFromRecord(record entryRecord) Entry {
	return Entry{ID: record.ID, RoomID: record.RoomID, WorldID: record.WorldID, WorldName: record.WorldName, Type: LogType(record.Type), Content: record.Content, RawContent: record.RawContent, RuleID: record.RuleID, RuleName: record.RuleName, SourceCursor: record.SourceCursor, SourceTimestamp: record.SourceTimestamp, OccurredAt: record.OccurredAt, ObservedAt: record.ObservedAt}
}

func ruleToRecord(rule Rule) ruleRecord {
	return ruleRecord{ID: rule.ID, RoomID: rule.RoomID, Name: rule.Name, Description: rule.Description, LogType: string(rule.LogType), Pattern: rule.Pattern, Regex: rule.Regex, Enabled: rule.Enabled, Priority: rule.Priority, MatchMode: string(rule.MatchMode), TailPattern: rule.TailPattern, BuiltIn: rule.BuiltIn, CreatedAt: rule.CreatedAt, UpdatedAt: rule.UpdatedAt}
}

func ruleFromRecord(record ruleRecord) Rule {
	matchMode := MatchMode(record.MatchMode)
	if matchMode == "" {
		matchMode = MatchModeSingle
	}
	return Rule{ID: record.ID, RoomID: record.RoomID, Name: record.Name, Description: record.Description, LogType: LogType(record.LogType), Pattern: record.Pattern, Regex: record.Regex, Enabled: record.Enabled, Priority: record.Priority, MatchMode: matchMode, TailPattern: record.TailPattern, BuiltIn: record.BuiltIn, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "%", "\\%")
	return strings.ReplaceAll(value, "_", "\\_")
}
