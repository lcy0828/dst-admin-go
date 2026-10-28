package playeractivity

import (
	"fmt"
	"github.com/jinzhu/gorm"
	"time"
)

// A descriptor is tied to a placement and stream, never just a reused slot name.
// No raw player data is stored in diagnostics. Pending is evidence of uncertainty,
// not a claim that a publisher finished (or lost) its write.
type recoveryRecord struct {
	RangeCount    int64
	Compacted     bool
	ID            string `gorm:"primary_key;type:char(64)"`
	RoomID        string `gorm:"type:varchar(255);index"`
	WorldID       string `gorm:"type:varchar(255);index"`
	StreamID      string `gorm:"type:char(64);index"`
	Scope         string `gorm:"type:varchar(512)"`
	Generation    int64
	EndGeneration int64
	Slot          int
	Producer      string `gorm:"type:varchar(128)"`
	Session       string `gorm:"type:varchar(128)"`
	Shard         string `gorm:"type:varchar(64)"`
	FirstSequence int64
	LastSequence  int64
	Code          string `gorm:"type:varchar(64)"`
	Field         string `gorm:"type:varchar(128)"`
	Fingerprint   string `gorm:"type:char(64)"`
	State         string `gorm:"type:varchar(32);index"`
	Observations  int
	FirstSeenAt   time.Time `gorm:"index"`
	LastSeenAt    time.Time
	RetryAt       time.Time
	// Valid JSON/envelope/sequence structure is required before event isolation.
	MissingSequences string `gorm:"type:text"`
	TrustedEnvelope  bool
}

func descriptorID(row cursorRecord, slot Slot) string {
	return identity(row.RoomID, row.WorldID, row.Scope, row.StreamID, fmt.Sprint(slot.Generation), slot.ProducerInstanceID, slot.SessionID, slot.ShardID, fmt.Sprint(slot.FirstSequence), fmt.Sprint(slot.LastSequence))
}
func matchesDescriptor(r recoveryRecord, row cursorRecord, slot Slot) bool {
	return r.ID == descriptorID(row, slot) && r.Slot == slot.Slot
}
func (s *Store) pendingRecovery(room, world string) ([]recoveryRecord, error) {
	var rows []recoveryRecord
	err := s.db.Table(s.recoveries).Where("room_id = ? AND world_id = ? AND state IN (?)", room, world, []string{"pending", "isolated"}).Order("generation ASC").Limit(64).Find(&rows).Error
	return rows, err
}
func (s *Store) coverage(room, world string) (Coverage, error) {
	c := Coverage{WorldID: world, Complete: true}
	var rows []struct {
		State string
		Count int64
	}
	err := s.db.Table(s.recoveries).Select("state, SUM(CASE WHEN range_count > 0 THEN range_count ELSE 1 END) AS count").Where("room_id = ? AND world_id = ?", room, world).Group("state").Scan(&rows).Error
	for _, row := range rows {
		switch row.State {
		case "pending":
			c.PendingSegments += row.Count
		case "isolated":
			c.IsolatedSegments += row.Count
		case "expired":
			c.UnavailableRanges += row.Count
		case "resolved":
			c.RecoveredSegments += row.Count
		}
	}
	row, rowErr := s.cursor(room, world)
	if rowErr != nil {
		return c, rowErr
	}
	c.LegacyUnknown = row.LegacyCoverageBefore != nil
	c.Complete = c.PendingSegments+c.IsolatedSegments+c.UnavailableRanges == 0 && !c.LegacyUnknown
	return c, err
}
func (s *Service) coverage(room string, q Query) ([]Coverage, error) {
	worlds, err := s.rooms.Worlds(room)
	if err != nil {
		return nil, err
	}
	result := []Coverage{}
	for _, w := range worlds {
		if q.WorldID != "" && q.WorldID != w.ID {
			continue
		}
		c, err := s.store.coverage(room, w.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, nil
}
func (s *Store) diagnostics(room, world string) ([]Diagnostic, error) {
	result := []Diagnostic{}
	var rows []recoveryRecord
	err := s.db.Table(s.recoveries).Where("room_id = ? AND world_id = ? AND state <> ?", room, world, "resolved").Order("last_seen_at DESC").Limit(4).Find(&rows).Error
	for _, r := range rows {
		result = append(result, Diagnostic{Code: r.Code, Field: r.Field, Generation: r.Generation, Fingerprint: r.Fingerprint, FirstSeenAt: r.FirstSeenAt, State: r.State})
	}
	return result, err
}

// Resynchronize coalesces work in the existing scheduler. It never rewinds a
// cursor, issues gameplay commands, or declares an accepted request recovered.
func (s *Service) Resynchronize(room string) (SyncRequest, error) {
	if err := s.room(room); err != nil {
		return SyncRequest{}, err
	}
	worlds, err := s.rooms.Worlds(room)
	if err != nil {
		return SyncRequest{}, err
	}
	s.mu.Lock()
	for _, w := range worlds {
		key := room + "/" + w.ID
		delete(s.due, key)
		if !s.active[key] {
			s.retryNow[key] = true
		}
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return SyncRequest{Accepted: true, Status: "pending"}, nil
}

// Cleanup is bounded and shares the existing activity retention pass. Range IDs
// live with the events, so cleanup can never reconnect trajectories across a gap.
func (s *Store) pruneRecovery(room string, c Settings) error {
	if err := s.compactRecovery(room); err != nil {
		return err
	}
	cutoff := s.now().UTC().Add(-time.Duration(c.RetentionDays) * 24 * time.Hour)
	var cursors []cursorRecord
	if err := s.db.Table(s.cursors).Where("room_id = ? AND legacy_coverage_before < ?", room, cutoff).Limit(64).Find(&cursors).Error; err != nil {
		return err
	}
	for _, cursor := range cursors {
		var record []eventRecord
		if err := s.db.Table(s.events).Select("id").Where("room_id = ? AND world_id = ? AND occurred_at <= ?", room, cursor.WorldID, *cursor.LegacyCoverageBefore).Limit(1).Find(&record).Error; err != nil {
			return err
		}
		if len(record) == 0 {
			if err := s.db.Table(s.cursors).Where("id = ?", cursor.ID).Update("legacy_coverage_before", nil).Error; err != nil {
				return err
			}
		}
	}
	var rows []recoveryRecord
	if err := s.db.Table(s.recoveries).Select("id").Where("room_id = ? AND last_seen_at < ?", room, cutoff).Order("last_seen_at ASC").Limit(256).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return s.db.Table(s.recoveries).Where("room_id = ? AND id IN (?)", room, ids).Delete(&recoveryRecord{}).Error
}

// Keep old terminal diagnostics bounded without losing coverage or counts.
// Active descriptors are never compacted, and event range IDs remain intact.
func (s *Store) compactRecovery(room string) error {
	var worlds []struct {
		WorldID string
		Count   int64
	}
	if err := s.db.Table(s.recoveries).Select("world_id, COUNT(*) AS count").Where("room_id = ? AND state IN (?) AND (compacted = ? OR compacted IS NULL)", room, []string{"expired", "resolved"}, false).Group("world_id").Having("COUNT(*) > ?", 1024).Limit(64).Scan(&worlds).Error; err != nil {
		return err
	}
	for _, world := range worlds {
		var rows []recoveryRecord
		if err := s.db.Table(s.recoveries).Where("room_id = ? AND world_id = ? AND state IN (?) AND (compacted = ? OR compacted IS NULL)", room, world.WorldID, []string{"expired", "resolved"}, false).Order("last_seen_at ASC").Limit(128).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			continue
		}
		tx := s.db.Begin()
		if tx.Error != nil {
			return tx.Error
		}
		err := func() error {
			summaries := map[string]recoveryRecord{}
			for _, row := range rows {
				summary, ok := summaries[row.State]
				if !ok {
					id := identity(room, world.WorldID, "compacted", row.State)
					err := tx.Table(s.recoveries).Where("id = ?", id).First(&summary).Error
					if err != nil && !gorm.IsRecordNotFoundError(err) {
						return err
					}
					if gorm.IsRecordNotFoundError(err) {
						summary = recoveryRecord{ID: id, RoomID: room, WorldID: world.WorldID, State: row.State, Compacted: true, Code: "ACTIVITY_HISTORY_COMPACTED", FirstSeenAt: row.FirstSeenAt, LastSeenAt: row.LastSeenAt}
					}
				}
				count := row.RangeCount
				if count < 1 {
					count = 1
				}
				summary.RangeCount += count
				if row.FirstSeenAt.Before(summary.FirstSeenAt) {
					summary.FirstSeenAt = row.FirstSeenAt
				}
				if row.LastSeenAt.After(summary.LastSeenAt) {
					summary.LastSeenAt = row.LastSeenAt
				}
				summaries[row.State] = summary
			}
			for _, summary := range summaries {
				if err := tx.Table(s.recoveries).Save(&summary).Error; err != nil {
					return err
				}
			}
			ids := []string{}
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			return tx.Table(s.recoveries).Where("id IN (?)", ids).Delete(&recoveryRecord{}).Error
		}()
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit().Error; err != nil {
			return err
		}
	}
	return nil
}
