package notifications

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

const feedLimit = 10000

type feedRecord struct {
	ID        uint64    `gorm:"primary_key;AUTO_INCREMENT"`
	EventKey  string    `gorm:"type:char(64);unique_index"`
	Kind      string    `gorm:"type:varchar(64);index"`
	RoomID    string    `gorm:"type:varchar(255);index"`
	Category  string    `gorm:"type:varchar(32)"`
	Value     string    `gorm:"type:text"`
	CreatedAt time.Time `gorm:"index"`
}

type Feed struct {
	Items            []Event           `json:"items"`
	NextCursor       string            `json:"nextCursor"`
	HasMore          bool              `json:"hasMore"`
	HistoryTruncated bool              `json:"historyTruncated"`
	RetentionDays    int               `json:"retentionDays"`
	Collection       map[string]string `json:"collection,omitempty"`
	SupportedKinds   []string          `json:"supportedKinds"`
}

type feedCursor struct {
	Sequence uint64 `json:"sequence"`
	Filter   string `json:"filter"`
}

// Record facts even without a delivery channel. MCP consumers can resume from
// their own cursor; delivery retries never produce another event in this feed.
func (s *Store) appendEvent(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Transaction(func(tx *gorm.DB) error { return s.appendEventTx(tx, event) })
}

func (s *Store) appendEventTx(tx *gorm.DB, event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if len(data) > 32768 {
		return ErrInvalid
	}
	hash := sha256.Sum256([]byte(event.Kind + "\x00" + event.ID))
	row := feedRecord{EventKey: hex.EncodeToString(hash[:]), Kind: event.Kind, RoomID: event.RoomID, Category: event.Category, Value: string(data), CreatedAt: time.Now().UTC()}

	if err := tx.Table(s.channelsTable+"_event").Where("event_key = ?", row.EventKey).FirstOrCreate(&row).Error; err != nil {
		return err
	}
	var boundary []feedRecord
	if err := tx.Table(s.channelsTable + "_event").Select("id").Order("id DESC").Offset(feedLimit - 1).Limit(1).Find(&boundary).Error; err != nil {
		return err
	}
	q := tx.Table(s.channelsTable+"_event").Select("id").Where("created_at < ?", time.Now().UTC().Add(-30*24*time.Hour))
	if len(boundary) > 0 {
		q = q.Or("id < ?", boundary[0].ID)
	}
	var expired []feedRecord
	if err := q.Order("id ASC").Limit(512).Find(&expired).Error; err != nil {
		return err
	}
	ids := make([]uint64, 0, len(expired))
	for _, r := range expired {
		ids = append(ids, r.ID)
	}
	if len(ids) > 0 {
		return tx.Table(s.channelsTable+"_event").Where("id IN (?)", ids).Delete(&feedRecord{}).Error
	}
	return nil
}

func (s *Service) Feed(after, kinds string, limit int) (Feed, error) {
	return s.feed("", after, kinds, limit, false)
}

// RoomFeed filters in SQL before pagination. A room cursor cannot read another
// room or the privileged system feed. Tail seeds a new subscriber without replay.
func (s *Service) RoomFeed(room, after, kinds string, limit int, tail bool) (Feed, error) {
	if room == "" || len(room) > 255 {
		return Feed{}, ErrInvalid
	}
	if err := s.recoverFacts(); err != nil {
		return Feed{}, err
	}
	s.expirePendingChanges()
	page, err := s.feed(room, after, kinds, limit, tail)
	if err == nil && (kinds == "" || strings.Contains(kinds, "game.available")) {
		s.watchGame(room)
	}
	if err == nil && (kinds == "" || strings.Contains(kinds, "runtime.alert")) {
		s.watchPerformance(room)
	}
	if err == nil {
		page.Collection = s.collection(room)
	}
	return page, err
}

func (s *Service) feed(room, after, kinds string, limit int, tail bool) (Feed, error) {
	result := Feed{Items: []Event{}, RetentionDays: 30, SupportedKinds: append([]string{}, EventKinds...)}
	if limit < 1 || limit > 100 || len(after) > 1024 || len(kinds) > 512 {
		return result, ErrInvalid
	}
	filter := []string{}
	if kinds != "" {
		filter = strings.Split(kinds, ",")
		for _, kind := range filter {
			if !slices.Contains(EventKinds, kind) {
				return result, ErrInvalid
			}
		}
		slices.Sort(filter)
		filter = slices.Compact(filter)
	}
	filterKey := strings.Join(filter, ",")
	if room != "" {
		filterKey = "room:" + room + "\x00" + filterKey
	}
	hash := sha256.Sum256([]byte(filterKey))
	cursor := feedCursor{Filter: hex.EncodeToString(hash[:])}
	if after != "" {
		data, err := base64.RawURLEncoding.DecodeString(after)
		var saved feedCursor
		if err != nil || json.Unmarshal(data, &saved) != nil || saved.Filter != cursor.Filter {
			return result, ErrInvalid
		}
		cursor = saved
	}
	// One snapshot fixes the upper bound while messages are being published.
	err := s.store.db.Transaction(func(tx *gorm.DB) error {
		table := s.store.channelsTable + "_event"
		var bounds struct {
			First uint64
			Last  uint64
		}
		if err := tx.Table(table).Select("COALESCE(MIN(id),0) AS first, COALESCE(MAX(id),0) AS last").Scan(&bounds).Error; err != nil {
			return err
		}
		if cursor.Sequence > bounds.Last {
			if room == "" {
				return ErrInvalid
			}
			cursor.Sequence = 0
			result.HistoryTruncated = true
		}
		if tail && after == "" {
			cursor.Sequence = bounds.Last
		}
		// Expiry is a read constraint as well as cleanup. An idle instance
		// must not replay months-old messages simply because no event arrived.
		var tail []feedRecord
		if err := tx.Table(table).Select("id").Order("id DESC").Offset(feedLimit - 1).Limit(1).Find(&tail).Error; err != nil {
			return err
		}
		firstID := uint64(0)
		if len(tail) > 0 {
			firstID = tail[0].ID
		}
		cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
		var retained struct{ First uint64 }
		if err := tx.Table(table).Select("COALESCE(MIN(id),0) AS first").Where("created_at >= ? AND id >= ?", cutoff, firstID).Scan(&retained).Error; err != nil {
			return err
		}
		result.HistoryTruncated = result.HistoryTruncated || retained.First > 1 && cursor.Sequence < retained.First-1 || retained.First == 0 && cursor.Sequence < bounds.Last
		q := tx.Table(table).Where("id > ? AND id <= ? AND id >= ? AND created_at >= ?", cursor.Sequence, bounds.Last, firstID, cutoff)
		if room != "" {
			q = q.Where("room_id = ?", room)
		}
		if len(filter) > 0 {
			q = q.Where("kind IN (?)", filter)
		}
		var rows []feedRecord
		if err := q.Order("id ASC").Limit(limit + 1).Find(&rows).Error; err != nil {
			return err
		}
		result.HasMore = len(rows) > limit
		if result.HasMore {
			rows = rows[:limit]
		}
		for _, row := range rows {
			var event Event
			if err := json.Unmarshal([]byte(row.Value), &event); err != nil {
				return err
			}
			result.Items = append(result.Items, event)
			cursor.Sequence = row.ID
		}
		if !result.HasMore && bounds.Last > cursor.Sequence {
			cursor.Sequence = bounds.Last
		}
		return nil
	})
	data, _ := json.Marshal(cursor)
	result.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	return result, err
}

// RecentRoom is a bounded read of already recorded facts, not a game query.
func (s *Service) RecentRoom(room string, limit int) ([]Event, error) {
	if room == "" || len(room) > 255 || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	if err := s.recoverFacts(); err != nil {
		return nil, err
	}
	s.expirePendingChanges()
	var rows []feedRecord
	err := s.store.db.Transaction(func(tx *gorm.DB) error {
		table := s.store.channelsTable + "_event"
		var boundary []feedRecord
		if err := tx.Table(table).Select("id").Order("id DESC").Offset(feedLimit - 1).Limit(1).Find(&boundary).Error; err != nil {
			return err
		}
		first := uint64(0)
		if len(boundary) > 0 {
			first = boundary[0].ID
		}
		return tx.Table(table).Where("room_id = ? AND category <> ? AND id >= ? AND created_at >= ?", room, "", first, time.Now().UTC().Add(-30*24*time.Hour)).Order("id DESC").Limit(limit).Find(&rows).Error
	})
	items := make([]Event, 0, len(rows))
	for _, row := range rows {
		var event Event
		if e := json.Unmarshal([]byte(row.Value), &event); e != nil {
			return nil, e
		}
		if event.Category != "" {
			items = append(items, event)
		}
	}
	return items, err
}
