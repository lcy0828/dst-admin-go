package modupdates

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

type policyRecord struct {
	RoomID               string `gorm:"primary_key;type:varchar(128)"`
	AutoCheck            bool   `gorm:"not null"`
	AutoPrepare          bool   `gorm:"not null"`
	ApplyWhenEmpty       bool   `gorm:"not null"`
	RestartWithPlayers   bool   `gorm:"not null"`
	GameAnnouncement     bool   `gorm:"not null"`
	EmptyGraceSeconds    int    `gorm:"not null"`
	CheckIntervalMinutes int    `gorm:"not null"`
	Revision             string `gorm:"type:varchar(128);not null"`
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type stateRecord struct {
	RoomID               string `gorm:"primary_key;type:varchar(128)"`
	Status               string `gorm:"type:varchar(32);index;not null"`
	AvailableModIDsJSON  string `gorm:"type:text;not null"`
	PreparedPlanHash     string `gorm:"type:varchar(128)"`
	PublicationID        string `gorm:"type:varchar(128)"`
	OnlinePlayers        int
	StaleOnlinePlayers   int
	PresenceFresh        bool
	AnnouncementPlanHash string `gorm:"type:varchar(128)"`
	ErrorCode            string `gorm:"type:varchar(64)"`
	ErrorMessage         string `gorm:"type:text"`
	LastCheckedAt        *time.Time
	PreparedAt           *time.Time
	EmptySince           *time.Time
	NextCheckAt          *time.Time `gorm:"index"`
	NextActionAt         *time.Time `gorm:"index"`
	UpdatedAt            time.Time  `gorm:"index;not null"`
}

type Store struct {
	db          *gorm.DB
	policyTable string
	stateTable  string
	now         func() time.Time
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	prefix := strings.TrimSpace(tablePrefix)
	return &Store{db: db, policyTable: prefix + "mod_update_policy", stateTable: prefix + "mod_update_state", now: time.Now}
}

func (s *Store) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("mod update database is required")
	}
	if err := s.db.Table(s.policyTable).AutoMigrate(&policyRecord{}).Error; err != nil {
		return fmt.Errorf("migrate Mod update policies: %w", err)
	}
	if err := s.db.Table(s.stateTable).AutoMigrate(&stateRecord{}).Error; err != nil {
		return fmt.Errorf("migrate Mod update states: %w", err)
	}
	return nil
}

func (s *Store) Policy(roomID string) (Policy, error) {
	var record policyRecord
	result := s.db.Table(s.policyTable).Where("room_id = ?", roomID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return DefaultPolicy(roomID), nil
	}
	if result.Error != nil {
		return Policy{}, result.Error
	}
	return policyFromRecord(record), nil
}

func (s *Store) SavePolicy(roomID string, input PolicyInput) (Policy, error) {
	now := s.now().UTC()
	var current policyRecord
	result := s.db.Table(s.policyTable).Where("room_id = ?", roomID).First(&current)
	if result.Error != nil && !gorm.IsRecordNotFoundError(result.Error) {
		return Policy{}, result.Error
	}
	revision := uuid.NewString()
	if gorm.IsRecordNotFoundError(result.Error) {
		if strings.TrimSpace(input.ExpectedRevision) != "" {
			return Policy{}, ErrRevisionConflict
		}
		record := policyRecord{
			RoomID: roomID, AutoCheck: input.AutoCheck, AutoPrepare: input.AutoPrepare, ApplyWhenEmpty: input.ApplyWhenEmpty,
			RestartWithPlayers: false, GameAnnouncement: input.GameAnnouncement, EmptyGraceSeconds: input.EmptyGraceSeconds,
			CheckIntervalMinutes: input.CheckIntervalMinutes, Revision: revision, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.db.Table(s.policyTable).Create(&record).Error; err != nil {
			return Policy{}, err
		}
		return policyFromRecord(record), nil
	}
	if input.ExpectedRevision != current.Revision {
		return Policy{}, ErrRevisionConflict
	}
	updates := map[string]interface{}{
		"auto_check": input.AutoCheck, "auto_prepare": input.AutoPrepare, "apply_when_empty": input.ApplyWhenEmpty,
		"restart_with_players": false, "game_announcement": input.GameAnnouncement,
		"empty_grace_seconds": input.EmptyGraceSeconds, "check_interval_minutes": input.CheckIntervalMinutes,
		"revision": revision, "updated_at": now,
	}
	updated := s.db.Table(s.policyTable).Where("room_id = ? AND revision = ?", roomID, current.Revision).Updates(updates)
	if updated.Error != nil {
		return Policy{}, updated.Error
	}
	if updated.RowsAffected != 1 {
		return Policy{}, ErrRevisionConflict
	}
	return s.Policy(roomID)
}

func (s *Store) State(roomID string) (State, error) {
	var record stateRecord
	result := s.db.Table(s.stateTable).Where("room_id = ?", roomID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return DefaultState(roomID, s.now()), nil
	}
	if result.Error != nil {
		return State{}, result.Error
	}
	return stateFromRecord(record)
}

func (s *Store) SaveState(value State) (State, error) {
	value.AvailableModIDs = normalizedIDs(value.AvailableModIDs)
	encoded, err := json.Marshal(value.AvailableModIDs)
	if err != nil {
		return State{}, err
	}
	value.UpdatedAt = s.now().UTC()
	record := stateRecordFrom(value, string(encoded))
	var count int
	if err := s.db.Table(s.stateTable).Where("room_id = ?", value.RoomID).Count(&count).Error; err != nil {
		return State{}, err
	}
	if count == 0 {
		if err := s.db.Table(s.stateTable).Create(&record).Error; err != nil {
			return State{}, err
		}
	} else {
		updates := map[string]interface{}{
			"status": record.Status, "available_mod_ids_json": record.AvailableModIDsJSON,
			"prepared_plan_hash": record.PreparedPlanHash, "publication_id": record.PublicationID,
			"online_players": record.OnlinePlayers, "stale_online_players": record.StaleOnlinePlayers,
			"presence_fresh": record.PresenceFresh, "announcement_plan_hash": record.AnnouncementPlanHash,
			"error_code": record.ErrorCode, "error_message": record.ErrorMessage,
			"last_checked_at": record.LastCheckedAt, "prepared_at": record.PreparedAt, "empty_since": record.EmptySince,
			"next_check_at": record.NextCheckAt, "next_action_at": record.NextActionAt, "updated_at": record.UpdatedAt,
		}
		if err := s.db.Table(s.stateTable).Where("room_id = ?", value.RoomID).Updates(updates).Error; err != nil {
			return State{}, err
		}
	}
	return s.State(value.RoomID)
}

func (s *Store) Schedule(now time.Time) ([]DueItem, *time.Time, error) {
	var policies []policyRecord
	if err := s.db.Table(s.policyTable).Where("auto_check = ?", true).Order("room_id ASC").Find(&policies).Error; err != nil {
		return nil, nil, err
	}
	now = now.UTC()
	due := make([]DueItem, 0)
	var next *time.Time
	for _, record := range policies {
		state, err := s.State(record.RoomID)
		if err != nil {
			return nil, nil, err
		}
		if state.NextActionAt != nil && pendingApplyStatus(state.Status) {
			if !state.NextActionAt.After(now) {
				due = append(due, DueItem{RoomID: record.RoomID, Action: DueApply})
				continue
			}
			next = earlier(next, state.NextActionAt)
		}
		if state.NextCheckAt == nil || !state.NextCheckAt.After(now) {
			due = append(due, DueItem{RoomID: record.RoomID, Action: DueCheck})
			continue
		}
		next = earlier(next, state.NextCheckAt)
	}
	return due, next, nil
}

func policyFromRecord(record policyRecord) Policy {
	return Policy{
		RoomID: record.RoomID, AutoCheck: record.AutoCheck, AutoPrepare: record.AutoPrepare,
		ApplyWhenEmpty: record.ApplyWhenEmpty, RestartWithPlayers: false, GameAnnouncement: record.GameAnnouncement,
		EmptyGraceSeconds: record.EmptyGraceSeconds, CheckIntervalMinutes: record.CheckIntervalMinutes,
		Revision: record.Revision, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
}

func stateRecordFrom(value State, ids string) stateRecord {
	return stateRecord{
		RoomID: value.RoomID, Status: string(value.Status), AvailableModIDsJSON: ids,
		PreparedPlanHash: value.PreparedPlanHash, PublicationID: value.PublicationID,
		OnlinePlayers: value.OnlinePlayers, StaleOnlinePlayers: value.StaleOnlinePlayers, PresenceFresh: value.PresenceFresh,
		AnnouncementPlanHash: value.AnnouncementPlanHash, ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage,
		LastCheckedAt: utc(value.LastCheckedAt), PreparedAt: utc(value.PreparedAt), EmptySince: utc(value.EmptySince),
		NextCheckAt: utc(value.NextCheckAt), NextActionAt: utc(value.NextActionAt), UpdatedAt: value.UpdatedAt.UTC(),
	}
}

func stateFromRecord(record stateRecord) (State, error) {
	ids := []string{}
	if err := json.Unmarshal([]byte(record.AvailableModIDsJSON), &ids); err != nil {
		return State{}, fmt.Errorf("decode Mod update IDs: %w", err)
	}
	return State{
		RoomID: record.RoomID, Status: Status(record.Status), AvailableModIDs: normalizedIDs(ids),
		PreparedPlanHash: record.PreparedPlanHash, PublicationID: record.PublicationID,
		OnlinePlayers: record.OnlinePlayers, StaleOnlinePlayers: record.StaleOnlinePlayers, PresenceFresh: record.PresenceFresh,
		AnnouncementPlanHash: record.AnnouncementPlanHash, ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage,
		LastCheckedAt: utc(record.LastCheckedAt), PreparedAt: utc(record.PreparedAt), EmptySince: utc(record.EmptySince),
		NextCheckAt: utc(record.NextCheckAt), NextActionAt: utc(record.NextActionAt), UpdatedAt: record.UpdatedAt.UTC(),
	}, nil
}

func normalizedIDs(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func earlier(current, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.Before(*current) {
		value := candidate.UTC()
		return &value
	}
	return current
}

func pendingApplyStatus(status Status) bool {
	switch status {
	case StatusPrepared, StatusWaitingForPlayers, StatusScheduled, StatusBlocked:
		return true
	default:
		return false
	}
}

func utc(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}
