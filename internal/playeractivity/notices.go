package playeractivity

import (
	"encoding/json"
	"log"
	"time"
)

// ConfigureNotices is configured once, before Run. The pending bit commits with
// the source event and cursor. A failed projection is retried on the next normal
// collection round, never by re-executing a game command or adding a poller.
func (s *Service) ConfigureNotices(eligible func(Event) bool, observer func([]Event) error) {
	s.store.noticeEligible = eligible
	s.noticeObserver = observer
	s.noticeDue = map[string]uint64{}
	s.store.noticeQueued = func(room, world string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		key := room + "/" + world
		if len(s.noticeDue) < 4096 || s.noticeDue[key] > 0 {
			s.noticeDue[key]++
		} else {
			delete(s.noticeDue, key)
		}
	}
}

func (s *Service) publishNotices(room, world string) {
	if s.noticeObserver == nil {
		return
	}
	key := room + "/" + world
	s.mu.Lock()
	generation, known := s.noticeDue[key]
	s.mu.Unlock()
	if known && generation == 0 {
		return
	}
	settle := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.noticeDue[key] == generation && (len(s.noticeDue) < 4096 || known) {
			s.noticeDue[key] = 0
		}
	}
	var rows []eventRecord
	// Existing retention/quota pruning also bounds pending records. New installs
	// do not backfill previously collected player data into notification feeds.
	if err := s.store.db.Table(s.store.events).Where("room_id = ? AND world_id = ? AND notice_pending = ?", room, world, true).Order("occurred_at ASC, id ASC").Limit(64).Find(&rows).Error; err != nil {
		log.Printf("[Activity] read pending notices: %v", err)
		return
	}
	if len(rows) == 0 {
		settle()
		return
	}
	events := make([]Event, 0, len(rows))
	ids := make([]string, 0, len(rows))
	cutoff := s.store.now().Add(-24 * time.Hour)
	for _, row := range rows {
		var e Event
		if row.OccurredAt.After(cutoff) {
			if err := json.Unmarshal([]byte(row.Value), &e); err != nil {
				log.Printf("[Activity] invalid pending notice: %v", err)
				return
			}
			events = append(events, e)
		}
		ids = append(ids, row.ID)
	}
	if len(events) > 0 {
		if err := s.noticeObserver(events); err != nil {
			return
		}
	}
	if err := s.store.db.Table(s.store.events).Where("id IN (?)", ids).UpdateColumn("notice_pending", false).Error; err != nil {
		log.Printf("[Activity] acknowledge notices: %v", err)
		return
	}
	if len(rows) < 64 {
		settle()
	}
}
