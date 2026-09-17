package configuration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"dont/internal/runtimedriver"
	"dont/shared"

	"github.com/jinzhu/gorm"
)

var ErrConfigurationStateNotFound = errors.New("configuration state was not found")

type configurationStateRecord struct {
	ID               string `gorm:"primary_key;type:char(64)"`
	RoomID           string `gorm:"type:varchar(255);index;not null"`
	WorldID          string `gorm:"type:varchar(255);index"`
	Scope            string `gorm:"type:varchar(32);index;not null"`
	ObservedSnapshot string `gorm:"type:text"`
	ObservedRevision string `gorm:"type:char(64)"`
	DesiredFiles     string `gorm:"type:text"`
	DesiredRevision  string `gorm:"type:char(64)"`
	Converged        bool   `gorm:"not null;default:false"`
	LastError        string `gorm:"type:text"`
	ObservedAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type StoredConfigurationState struct {
	RoomID           string
	WorldID          string
	Scope            string
	Observed         runtimedriver.ConfigurationSnapshot
	ObservedRevision string
	ObservedAt       *time.Time
}

func (s StoredConfigurationState) ObservedSnapshot() (runtimedriver.ConfigurationSnapshot, bool) {
	if len(s.Observed.Result.Files) > 0 {
		value := s.Observed
		value.Result.Files = cloneConfigurationFiles(value.Result.Files)
		return value, true
	}
	return runtimedriver.ConfigurationSnapshot{}, false
}

type ConfigurationStateRepository interface {
	Observe(string, string, string, runtimedriver.ConfigurationSnapshot, string, time.Time) (StoredConfigurationState, error)
	Get(string, string, string) (StoredConfigurationState, error)
}

type StateStore struct {
	db    *gorm.DB
	table string
	now   func() time.Time
	mu    sync.Mutex
}

func NewStateStore(db *gorm.DB, tablePrefix string) *StateStore {
	return &StateStore{db: db, table: strings.TrimSpace(tablePrefix) + "configuration_state", now: time.Now}
}

func (s *StateStore) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("configuration state database is required")
	}
	if err := s.db.Table(s.table).AutoMigrate(&configurationStateRecord{}).Error; err != nil {
		return fmt.Errorf("migrate %s: %w", s.table, err)
	}
	return nil
}

func (s *StateStore) Observe(roomID, worldID, scope string, snapshot runtimedriver.ConfigurationSnapshot, revision string, observedAt time.Time) (StoredConfigurationState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return StoredConfigurationState{}, err
	}
	if observedAt.IsZero() {
		observedAt = s.now().UTC()
	} else {
		observedAt = observedAt.UTC()
	}
	now := s.now().UTC()
	record, found, err := s.record(roomID, worldID, scope)
	if err != nil {
		return StoredConfigurationState{}, err
	}
	if !found {
		record = configurationStateRecord{
			ID: stateID(roomID, worldID, scope), RoomID: roomID, WorldID: worldID, Scope: scope, CreatedAt: now,
		}
	}
	record.ObservedSnapshot = string(payload)
	record.ObservedRevision = revision
	record.ObservedAt = &observedAt
	// DesiredFiles was used by older releases as a second configuration source.
	// Clear it as soon as the Runtime has supplied a real disk observation.
	record.DesiredFiles = ""
	record.DesiredRevision = ""
	record.Converged = true
	record.LastError = ""
	record.UpdatedAt = now
	if err := s.db.Table(s.table).Save(&record).Error; err != nil {
		return StoredConfigurationState{}, err
	}
	return decodeConfigurationState(record)
}

func (s *StateStore) Get(roomID, worldID, scope string) (StoredConfigurationState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found, err := s.record(roomID, worldID, scope)
	if err != nil {
		return StoredConfigurationState{}, err
	}
	if !found {
		return StoredConfigurationState{}, ErrConfigurationStateNotFound
	}
	return decodeConfigurationState(record)
}

func (s *StateStore) record(roomID, worldID, scope string) (configurationStateRecord, bool, error) {
	var record configurationStateRecord
	result := s.db.Table(s.table).Where("id = ?", stateID(roomID, worldID, scope)).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return configurationStateRecord{}, false, nil
	}
	if result.Error != nil {
		return configurationStateRecord{}, false, result.Error
	}
	return record, true, nil
}

func stateID(roomID, worldID, scope string) string {
	sum := sha256.Sum256([]byte(roomID + "\x00" + worldID + "\x00" + scope))
	return hex.EncodeToString(sum[:])
}

func decodeConfigurationState(record configurationStateRecord) (StoredConfigurationState, error) {
	result := StoredConfigurationState{
		RoomID: record.RoomID, WorldID: record.WorldID, Scope: record.Scope,
		ObservedRevision: record.ObservedRevision, ObservedAt: record.ObservedAt,
	}
	if record.ObservedSnapshot != "" {
		if err := json.Unmarshal([]byte(record.ObservedSnapshot), &result.Observed); err != nil {
			return StoredConfigurationState{}, err
		}
	}
	return result, nil
}

func cloneConfigurationFiles(values []shared.RuntimeConfigurationFile) []shared.RuntimeConfigurationFile {
	result := make([]shared.RuntimeConfigurationFile, len(values))
	for index, value := range values {
		result[index] = value
		result[index].Data = append([]byte(nil), value.Data...)
	}
	return result
}
