package playeractivity

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type pageCursor struct {
	At     time.Time `json:"at"`
	ID     string    `json:"id"`
	Filter string    `json:"filter"`
}

func (s *Service) validateQuery(room string, q Query) error {
	if err := s.room(room); err != nil {
		return err
	}
	if q.Limit < 0 || q.Limit > 500 || !safeKind(q.Kind) || q.UserID != "" && !actorPattern.MatchString(q.UserID) || len(q.Before) > 1024 {
		return ErrInvalid
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.From.Before(q.To) {
		return ErrInvalid
	}
	if q.WorldID != "" {
		worlds, err := s.rooms.Worlds(room)
		if err != nil {
			return err
		}
		found := false
		for _, w := range worlds {
			if w.ID == q.WorldID {
				found = true
			}
		}
		if !found {
			return ErrNotFound
		}
	}
	return nil
}
func (s *Store) filtered(room string, q Query, c Settings) *gorm.DB {
	cutoff := s.now().UTC().Add(-time.Duration(c.RetentionDays) * 24 * time.Hour)
	if q.From.After(cutoff) {
		cutoff = q.From
	}
	db := s.db.Table(s.events).Where("room_id = ? AND occurred_at >= ?", room, cutoff)
	if !q.To.IsZero() {
		db = db.Where("occurred_at <= ?", q.To)
	}
	if q.UserID != "" {
		db = db.Where("(user_id = ? OR kind IN (?))", q.UserID, []string{"collection.gap", "collection.instance"})
	}
	if q.WorldID != "" {
		db = db.Where("world_id = ?", q.WorldID)
	}
	if q.Kind != "" {
		if strings.HasSuffix(q.Kind, ".") {
			db = db.Where("kind LIKE ?", q.Kind+"%")
		} else {
			db = db.Where("kind = ?", q.Kind)
		}
	}
	return db
}
func (s *Service) Events(room string, q Query) (Page, error) {
	result := Page{Items: []Event{}, Limit: q.Limit}
	if result.Limit == 0 {
		result.Limit = 100
	}
	if err := s.validateQuery(room, q); err != nil {
		return result, err
	}
	c, err := s.store.Settings(room)
	if err != nil {
		return result, err
	}
	fingerprint := identity(room, q.UserID, q.WorldID, q.Kind, q.From.UTC().Format(time.RFC3339Nano), q.To.UTC().Format(time.RFC3339Nano))
	db := s.store.filtered(room, q, c)
	if q.Before != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Before)
		if err != nil {
			return result, ErrInvalid
		}
		var cursor pageCursor
		if json.Unmarshal(raw, &cursor) != nil || cursor.At.IsZero() || len(cursor.ID) != 64 || cursor.Filter != fingerprint {
			return result, ErrInvalid
		}
		db = db.Where("(occurred_at < ? OR (occurred_at = ? AND id < ?))", cursor.At, cursor.At, cursor.ID)
	}
	var rows []eventRecord
	if err := db.Order("occurred_at DESC, id DESC").Limit(result.Limit + 1).Find(&rows).Error; err != nil {
		return result, err
	}
	result.HasMore = len(rows) > result.Limit
	if result.HasMore {
		rows = rows[:result.Limit]
	}
	for _, row := range rows {
		var event Event
		if err := json.Unmarshal([]byte(row.Value), &event); err != nil {
			return result, err
		}
		result.Items = append(result.Items, event)
	}
	if result.HasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		raw, _ := json.Marshal(pageCursor{last.OccurredAt, last.ID, fingerprint})
		result.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return result, nil
}

// Trajectory returns sampled positions and collection boundaries. Missing samples
// are not interpolated; consumers must break lines across world/instance/gaps.
func (s *Service) Trajectory(room string, q Query) (Page, error) {
	if q.UserID == "" || q.Kind != "" {
		return Page{}, ErrInvalid
	}
	if err := s.validateQuery(room, q); err != nil {
		return Page{}, err
	}
	// Include action positions as well as periodic samples and explicit gaps.
	return s.Events(room, q)
}
func (s *Service) Combat(room string, q Query) (CombatResult, error) {
	result := CombatResult{Items: []CombatSummary{}}
	if q.Before != "" || q.Kind != "" {
		return result, ErrInvalid
	}
	if err := s.validateQuery(room, q); err != nil {
		return result, err
	}
	c, err := s.store.Settings(room)
	if err != nil {
		return result, err
	}
	q.Kind = "combat."
	var rows []eventRecord
	if err := s.store.filtered(room, q, c).Order("occurred_at DESC, id DESC").Limit(2001).Find(&rows).Error; err != nil {
		return result, err
	}
	result.Truncated = len(rows) > 2000
	if result.Truncated {
		rows = rows[:2000]
	}
	result.ScannedEvents = len(rows)
	indices := map[string]int{}
	for _, row := range rows {
		var event Event
		if err := json.Unmarshal([]byte(row.Value), &event); err != nil {
			return result, err
		}
		direction := "outgoing"
		if event.Kind == "combat.received" {
			direction = "incoming"
		}
		target, _ := event.Fields["targetGuid"].(string)
		prefab, _ := event.Fields["targetPrefab"].(string)
		key := identity(event.WorldID, event.ProducerInstanceID, event.UserID, target, prefab, direction)
		i, found := indices[key]
		if !found {
			i = len(result.Items)
			indices[key] = i
			epic, _ := event.Fields["epic"].(bool)
			result.Items = append(result.Items, CombatSummary{UserID: event.UserID, Direction: direction, WorldID: event.WorldID, ProducerInstanceID: event.ProducerInstanceID, TargetGUID: target, TargetPrefab: prefab, Epic: epic, FirstAt: event.OccurredAt, LastAt: event.OccurredAt})
		}
		item := &result.Items[i]
		if event.OccurredAt.Before(item.FirstAt) {
			item.FirstAt = event.OccurredAt
		}
		if event.OccurredAt.After(item.LastAt) {
			item.LastAt = event.OccurredAt
		}
		number := func(k string) float64 {
			n, _ := event.Fields[k].(float64)
			if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
				return 0
			}
			return n
		}
		if event.Kind == "combat.kill" {
			item.Kills++
		} else {
			item.Hits += int64(number("hits"))
			item.UnknownDamageHits += int64(number("unknownDamageHits"))
			if number("knownDamageHits") > 0 {
				if item.Damage == nil {
					v := float64(0)
					item.Damage = &v
				}
				*item.Damage += number("damage")
			}
		}
	}
	return result, nil
}
