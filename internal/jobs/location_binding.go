package jobs

import (
	"fmt"
	"strings"

	"github.com/jinzhu/gorm"
)

// LocationBinding reads existing durable maintenance evidence. Restore/import
// never depends on the optional location store being available.
func (s *Store) LocationBinding(room, world string) (string, error) {
	var rows []jobRecord
	kinds := []string{"backup.restore", "backup-set.restore", "save-import.apply", "room.regenerate", "room.rollback", "rooms.regenerate", "rooms.rollback", "room.provision"}
	e := s.db.Table(s.jobsTable).Select("id, status, kind").Where("(room_id = ? OR room_id = '') AND (world_id = ? OR world_id = '') AND kind IN (?)", room, world, kinds).Order("created_at DESC").Limit(16).Find(&rows).Error
	if e != nil && !gorm.IsRecordNotFoundError(e) {
		return "", e
	}
	ids := []string{"maintenance-v1"}
	for _, r := range rows {
		if r.Status == string(StatusQueued) || r.Status == string(StatusRunning) {
			return "", fmt.Errorf("world maintenance is active")
		}
		ids = append(ids, r.ID+":"+string(r.Status))
	}
	return strings.Join(ids, "|"), nil
}
