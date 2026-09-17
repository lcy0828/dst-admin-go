package chatlogs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dont/internal/rooms"
	"github.com/jinzhu/gorm"
)

const parserVersion = 1

var ErrSyncPending = errors.New("聊天历史正在分批补齐")

func sameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(*right)
}

// Existing source identity is stable across parser upgrades. Correct its
// interpretation rather than inserting another copy of its text.
func (s *Store) correctSourceTime(tx *gorm.DB, source sourceRecord, entry parsedEntry) error {
	var event eventRecord
	if err := tx.Table(s.eventsTable).Where("id = ?", source.EventID).First(&event).Error; err != nil {
		return err
	}
	if event.Fingerprint != entryIdentity(entry) {
		return fmt.Errorf("聊天来源 %s 正文与已保存记录冲突，保留原记录", source.ID)
	}
	if source.TimeVersion > entry.timeVersion {
		return nil
	}
	if source.TimeVersion == entry.timeVersion && sameTime(source.OccurredAt, entry.OccurredAt) && source.TimeEstimated == entry.TimeEstimated {
		return nil
	}
	if err := tx.Table(s.sourcesTable).Where("id = ?", source.ID).Updates(map[string]interface{}{
		"occurred_at": entry.OccurredAt, "time_version": entry.timeVersion, "time_estimated": entry.TimeEstimated,
	}).Error; err != nil {
		return err
	}
	return s.reconcileEventSource(tx, source.EventID)
}

type HistoryHealth struct {
	UnavailableGenerations int      `json:"unavailableGenerations"`
	ParseProblems          []string `json:"parseProblems"`
	PendingGenerations     int      `json:"pendingGenerations"`
	ParseErrors            int      `json:"parseErrors"`
	UncertainTimes         int      `json:"uncertainTimes"`
}

func (s *Store) historyHealth(roomID string) (HistoryHealth, error) {
	var health HistoryHealth
	if err := s.db.Table(s.generationsTable).Where("room_id = ? AND unavailable = ? AND (parser_version < ? OR caught_up = ?)", roomID, false, parserVersion, false).Count(&health.PendingGenerations).Error; err != nil {
		return health, err
	}
	if err := s.db.Table(s.generationsTable).Where("room_id = ? AND unavailable = ?", roomID, true).Count(&health.UnavailableGenerations).Error; err != nil {
		return health, err
	}
	var problems []generationRecord
	if err := s.db.Table(s.generationsTable).Where("room_id = ? AND parse_errors > 0", roomID).Limit(5).Find(&problems).Error; err != nil {
		return health, err
	}
	for _, problem := range problems {
		health.ParseProblems = append(health.ParseProblems, problem.WorldID+" / "+problem.FileName+": "+problem.LastParseProblem)
	}
	var counts struct{ Errors int }
	if err := s.db.Table(s.generationsTable).Select("COALESCE(SUM(parse_errors), 0) AS errors").Where("room_id = ?", roomID).Scan(&counts).Error; err != nil {
		return health, err
	}
	health.ParseErrors = counts.Errors
	if err := s.db.Table(s.eventsTable).Where("room_id = ? AND (occurred_at IS NULL OR time_estimated = ?)", roomID, true).Count(&health.UncertainTimes).Error; err != nil {
		return health, err
	}
	return health, nil
}

// RepairRoom is run by an explicit cancellable job. Each bounded batch commits
// its own checkpoint. A subsequent job resumes after cancellation or restart.
func (s *Service) RepairRoom(ctx context.Context, roomID string) (SyncResult, error) {
	if s.store == nil {
		return SyncResult{}, errors.New("聊天历史存储未启用")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	release, _, err := s.acquireSync(ctx, "repair:"+roomID, true)
	if err != nil {
		return SyncResult{}, err
	}
	defer release()
	// Recheck completed generations explicitly; interrupted generations retain their checkpoint.
	releaseBatch, _, err := s.acquireSync(ctx, roomID, true)
	if err != nil {
		return SyncResult{}, err
	}
	err = s.store.db.Table(s.store.generationsTable).Where("room_id = ? AND parser_version >= ?", roomID, parserVersion).Updates(map[string]interface{}{"parser_version": 0, "repair_cursor": 0}).Error
	releaseBatch()
	if err != nil {
		return SyncResult{}, err
	}
	var total SyncResult
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		batchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		result, _, err := s.syncRoom(batchCtx, roomID, true)
		cancel()
		total.Imported += result.Imported
		if err != nil {
			return total, err
		}
		total.Problems = result.Problems
		if !result.Pending {
			total.HistoryHealth, err = s.store.historyHealth(roomID)
			if err != nil {
				return total, err
			}
			if len(result.Problems) > 0 {
				return total, errors.New(result.Problems[0].Message)
			}
			return total, nil
		}
		// Yield between batches; nothing remains running when the job is canceled.
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return total, ctx.Err()
		case <-timer.C:
		}
	}
}

// Missing archives never erase saved messages or remain falsely "catching up".
func (s *Store) observeGenerations(roomID, worldID string, ids []string) error {
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	if err := tx.Table(s.generationsTable).Where("room_id = ? AND world_id = ?", roomID, worldID).Update("unavailable", true).Error; err != nil {
		tx.Rollback()
		return err
	}
	if len(ids) > 0 {
		if err := tx.Table(s.generationsTable).Where("room_id = ? AND world_id = ? AND generation_id IN (?)", roomID, worldID, ids).Update("unavailable", false).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit().Error
}

func (s *Service) RepairTarget(roomID string) (rooms.Room, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return room, err
	}
	if !room.Managed {
		return room, ErrRoomNotManaged
	}
	return room, nil
}
