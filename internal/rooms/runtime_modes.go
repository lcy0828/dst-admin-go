package rooms

import (
	"errors"
	"fmt"
	"time"

	"dont/shared"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

var ErrRuntimePreferenceConflict = errors.New("world runtime preference changed")

// RuntimeModePreference describes a confirmed selection, not an installed package.
type RuntimeModePreference struct {
	RoomID    string                        `gorm:"primary_key;type:varchar(255)" json:"roomId"`
	WorldID   string                        `gorm:"primary_key;type:varchar(255)" json:"worldId"`
	Mode      shared.RuntimePerformanceMode `gorm:"type:varchar(32);not null" json:"mode"`
	Revision  string                        `gorm:"type:char(36);not null" json:"revision"`
	UpdatedAt time.Time                     `gorm:"not null" json:"updatedAt"`
}

func (s *Store) runtimeModesTable() string { return s.table + "_runtime_mode" }

func (s *Service) RuntimeModePreferences(roomID string) (map[string]RuntimeModePreference, error) {
	return s.store.RuntimeModePreferences(roomID)
}

func (s *Store) RuntimeModePreferences(roomID string) (map[string]RuntimeModePreference, error) {
	var values []RuntimeModePreference
	if err := s.db.Table(s.runtimeModesTable()).Where("room_id = ?", roomID).Find(&values).Error; err != nil {
		return nil, fmt.Errorf("read world runtime preferences: %w", err)
	}
	result := make(map[string]RuntimeModePreference, len(values))
	for _, value := range values {
		if mode, ok := shared.NormalizeRuntimePerformanceMode(value.Mode); !ok || value.Mode == "" || mode != value.Mode {
			return nil, fmt.Errorf("invalid saved runtime mode for world %s", value.WorldID)
		}
		result[value.WorldID] = value
	}
	return result, nil
}

// RememberRuntimeMode uses a revision check so a delayed result cannot replace
// a selection confirmed by a newer operation. An empty revision means absent.
func (s *Store) RememberRuntimeMode(roomID, worldID string, mode shared.RuntimePerformanceMode, revision string) error {
	if _, ok := shared.NormalizeRuntimePerformanceMode(mode); !ok || mode == "" || roomID == "" || worldID == "" {
		return errors.New("invalid world runtime preference")
	}
	value := RuntimeModePreference{RoomID: roomID, WorldID: worldID, Mode: mode, Revision: uuid.NewString(), UpdatedAt: s.now().UTC()}
	if revision != "" {
		result := s.db.Table(s.runtimeModesTable()).Where("room_id = ? AND world_id = ? AND revision = ?", roomID, worldID, revision).
			Updates(map[string]interface{}{"mode": mode, "revision": value.Revision, "updated_at": value.UpdatedAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRuntimePreferenceConflict
		}
		return nil
	}
	// INSERT OR IGNORE is supported by the application's SQLite database and
	// avoids a read/create race without overwriting an existing preference.
	result := s.db.Exec("INSERT OR IGNORE INTO "+s.runtimeModesTable()+" (room_id, world_id, mode, revision, updated_at) VALUES (?, ?, ?, ?, ?)",
		roomID, worldID, mode, value.Revision, value.UpdatedAt)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrRuntimePreferenceConflict
	}
	return nil
}

func (s *Store) ForgetWorldRuntimeMode(roomID, worldID string) error {
	return s.db.Table(s.runtimeModesTable()).Where("room_id = ? AND world_id = ?", roomID, worldID).Delete(&RuntimeModePreference{}).Error
}

func (s *Store) forgetRoomRuntimeModes(roomID string) error {
	return forgetRoomRuntimeModes(s.db, s.runtimeModesTable(), roomID)
}

func forgetRoomRuntimeModes(tx *gorm.DB, table, roomID string) error {
	return tx.Table(table).Where("room_id = ?", roomID).Delete(&RuntimeModePreference{}).Error
}
