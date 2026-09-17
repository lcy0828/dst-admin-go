package modcontrol

import (
	"context"
	"strings"

	"dont/internal/mods"

	"github.com/jinzhu/gorm"
)

// This stores only the editor mode, never a second copy of game configuration.
type configurationModeRecord struct {
	RoomID string `gorm:"primary_key;type:varchar(255)"`
	ModID  string `gorm:"primary_key;type:varchar(32)"`
	Mode   string `gorm:"type:varchar(16);not null"`
}

type ConfigurationModeStore struct {
	db    *gorm.DB
	table string
}

func NewConfigurationModeStore(db *gorm.DB, prefix string) *ConfigurationModeStore {
	return &ConfigurationModeStore{db: db, table: strings.TrimSpace(prefix) + "mod_configuration_mode"}
}

func (s *ConfigurationModeStore) Migrate() error {
	return s.db.Table(s.table).AutoMigrate(&configurationModeRecord{}).Error
}

func (s *Service) ConfigureConfigurationModes(store *ConfigurationModeStore) {
	s.configurationModes = store
}

func (s *Service) SetConfigurationMode(_ context.Context, roomID, modID, mode string) error {
	if s.configurationModes == nil || !mods.ValidID(modID) || (mode != "shared" && mode != "separate") {
		return mods.ErrInvalidRequest
	}
	if _, err := s.source.rooms.Room(roomID); err != nil {
		return err
	}
	record := configurationModeRecord{RoomID: roomID, ModID: modID, Mode: mode}
	return s.configurationModes.db.Table(s.configurationModes.table).Save(&record).Error
}

func (s *Service) applyConfigurationModes(profile *mods.RoomModProfile) error {
	if s.configurationModes == nil {
		return nil
	}
	var records []configurationModeRecord
	if err := s.configurationModes.db.Table(s.configurationModes.table).Where("room_id = ?", profile.RoomID).Find(&records).Error; err != nil {
		return err
	}
	modes := make(map[string]string, len(records))
	for _, record := range records {
		modes[record.ModID] = record.Mode
	}
	for index := range profile.Items {
		if modes[profile.Items[index].ModID] == "separate" {
			profile.Items[index].ConfigurationMode = "separate"
		}
	}
	return nil
}
