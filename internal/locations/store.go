package locations

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

type Store struct {
	db                  *gorm.DB
	locations, previews string
	mu                  sync.Mutex
}

func NewStore(db *gorm.DB, prefix string) *Store {
	return &Store{db: db, locations: strings.TrimSpace(prefix) + "game_location", previews: strings.TrimSpace(prefix) + "teleport_preview"}
}
func (s *Store) Migrate() error {
	for table, value := range map[string]any{s.locations: &Location{}, s.previews: &Preview{}} {
		if e := s.db.Table(table).AutoMigrate(value).Error; e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) List(room, world, search string, offset int) ([]Location, error) {
	if offset < 0 || offset > 10000 || len(search) > 192 {
		return nil, ErrInvalid
	}
	q := s.db.Table(s.locations).Where("deleted_at IS NULL").Where("room_id = ?", room)
	if world != "" {
		q = q.Where("world_id = ?", world)
	}
	if search != "" {
		_, key, e := cleanName(search)
		if e != nil {
			return nil, e
		}
		q = q.Where("name_key LIKE ? ESCAPE '\\'", "%"+strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(key)+"%")
	}
	rows := []Location{}
	e := q.Order("world_id, name_key, id").Limit(50).Offset(offset).Find(&rows).Error
	return rows, e
}
func (s *Store) Get(room, id string) (Location, error) {
	var p Location
	e := s.db.Table(s.locations).Where("deleted_at IS NULL").Where("room_id = ? AND id = ?", room, id).First(&p).Error
	if gorm.IsRecordNotFoundError(e) {
		e = ErrNotFound
	}
	return p, e
}
func (s *Store) Resolve(room, world, name string) (Location, error) {
	_, key, e := cleanName(name)
	if e != nil {
		return Location{}, e
	}
	var rows []Location
	q := s.db.Table(s.locations).Where("deleted_at IS NULL").Where("room_id = ? AND name_key = ?", room, key)
	if world != "" {
		q = q.Where("world_id = ?", world)
	}
	if e = q.Limit(2).Find(&rows).Error; e != nil {
		return Location{}, e
	}
	if len(rows) == 0 {
		return Location{}, ErrNotFound
	}
	if len(rows) != 1 {
		return Location{}, ErrConflict
	}
	return rows[0], nil
}
func (s *Store) Save(p Location, revision int64) (Location, error) {
	name, key, e := cleanName(p.Name)
	if e != nil || !p.Point().Valid() || p.RoomID == "" || p.WorldID == "" || p.SessionID == "" {
		return Location{}, ErrInvalid
	}
	p.Name, p.NameKey = name, key
	scope := fmt.Sprintf("%x", sha256.Sum256([]byte(p.RoomID+"\x00"+p.WorldID+"\x00"+key)))
	p.NameScope = &scope
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := s.db.Begin()
	if tx.Error != nil {
		return Location{}, tx.Error
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if e = tx.Table(s.locations).Where("deleted_at IS NOT NULL AND deleted_at < ?", now.Add(-30*24*time.Hour)).Unscoped().Delete(&Location{}).Error; e != nil {
		return Location{}, e
	}
	if p.ID == "" {
		p.ID = uuid.NewString()
		p.Revision = 1
		p.ExecutionRevision = 1
		p.CreatedAt = now
		var n int
		if e = tx.Table(s.locations).Where("deleted_at IS NULL").Where("room_id = ? AND world_id = ?", p.RoomID, p.WorldID).Count(&n).Error; e != nil {
			return Location{}, e
		}
		if n >= 100 {
			return Location{}, ErrLimit
		}
	} else {
		var old Location
		if e = tx.Table(s.locations).Where("deleted_at IS NULL").Where("room_id = ? AND id = ?", p.RoomID, p.ID).First(&old).Error; e != nil {
			return Location{}, ErrNotFound
		}
		if old.Revision != revision {
			return Location{}, ErrConflict
		}
		p.Revision = old.Revision + 1
		p.ExecutionRevision = old.ExecutionRevision
		p.CreatedAt, p.CreatedBy = old.CreatedAt, old.CreatedBy
		if old.WorldID != p.WorldID {
			var count int
			if e = tx.Table(s.locations).Where("deleted_at IS NULL AND room_id = ? AND world_id = ?", p.RoomID, p.WorldID).Count(&count).Error; e != nil {
				return Location{}, e
			}
			if count >= 100 {
				return Location{}, ErrLimit
			}
		}
		if old.X != p.X || old.Z != p.Z || old.WorldID != p.WorldID || old.SessionID != p.SessionID || old.BindingRevision != p.BindingRevision {
			p.ExecutionRevision++
		}
	}
	var n int
	if e = tx.Table(s.locations).Where("deleted_at IS NULL").Where("room_id = ? AND world_id = ? AND name_key = ? AND id <> ?", p.RoomID, p.WorldID, p.NameKey, p.ID).Count(&n).Error; e != nil {
		return Location{}, e
	}
	if n > 0 {
		return Location{}, ErrConflict
	}
	p.UpdatedAt = now
	if revision == 0 {
		e = tx.Table(s.locations).Create(&p).Error
	} else {
		e = tx.Table(s.locations).Save(&p).Error
	}
	if e != nil {
		return Location{}, e
	}
	if e = tx.Commit().Error; e != nil {
		return Location{}, e
	}
	return p, nil
}
func (s *Store) Delete(room, id string, revision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.db.Table(s.locations).Where("deleted_at IS NULL AND room_id = ? AND id = ? AND revision = ?", room, id, revision).Updates(map[string]any{"deleted_at": time.Now().UTC(), "name_scope": nil, "execution_revision": gorm.Expr("execution_revision + 1"), "revision": gorm.Expr("revision + 1")})
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) preview(room, actor, id string) (Preview, error) {
	var p Preview
	e := s.db.Table(s.previews).Where("room_id = ? AND actor = ? AND id = ?", room, actor, id).First(&p).Error
	if gorm.IsRecordNotFoundError(e) {
		e = ErrNotFound
	}
	p.ServerTime = time.Now().UTC()
	if p.EffectJSON != "" {
		_ = json.Unmarshal([]byte(p.EffectJSON), &p.Effect)
	}
	return p, e
}
func (s *Store) insertPreview(p Preview) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	// Demand-driven bounded retention. Never turn an uncertain historical result
	// into success; expired Runtime permits cannot execute again.
	if e := s.db.Table(s.previews).Where("created_at < ?", now.Add(-7*24*time.Hour)).Delete(&Preview{}).Error; e != nil {
		return e
	}
	var n int
	if e := s.db.Table(s.previews).Where("room_id = ? AND world_id = ? AND state IN (?) AND expires_at > ?", p.RoomID, p.WorldID, []string{"preparing", "prepared", "dispatching", "uncertain"}, now).Count(&n).Error; e != nil {
		return e
	}
	if n >= 16 {
		return ErrBusy
	}
	if e := s.db.Table(s.previews).Where("room_id = ? AND player_id = ? AND state IN (?) AND expires_at > ?", p.RoomID, p.PlayerID, []string{"preparing", "prepared", "dispatching", "uncertain"}, now).Count(&n).Error; e != nil {
		return e
	}
	if n > 0 {
		return ErrBusy
	}
	var total int
	if e := s.db.Table(s.previews).Count(&total).Error; e != nil {
		return e
	}
	if total >= 2000 {
		return ErrLimit
	}
	return s.db.Table(s.previews).Create(&p).Error
}
func (s *Store) transition(id, from, to, run string) (bool, error) {
	q := s.db.Table(s.previews).Where("id = ? AND state = ?", id, from).Updates(map[string]any{"state": to, "run_id": run, "effect_json": "", "reason": ""})
	return q.RowsAffected == 1, q.Error
}
func (s *Store) finish(p Preview) error {
	raw, _ := json.Marshal(p.Effect)
	allowed := []string{"preparing", "dispatching", "uncertain"}
	if p.State == "prepared" {
		allowed = []string{"preparing"}
	}
	q := s.db.Table(s.previews).Where("id = ? AND state IN (?)", p.ID, allowed).Updates(map[string]any{"state": p.State, "reason": p.Reason, "run_id": p.RunID, "effect_json": string(raw), "expires_at": p.ExpiresAt, "x": p.X, "z": p.Z, "player_name": p.PlayerName, "needs_review": p.NeedsReview})
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

// Archive is called only after a successful room/world deletion. Location
// persistence is not part of restoring or starting game saves.
func (s *Store) Archive(room, world string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer tx.Rollback()
	q := tx.Table(s.locations).Where("room_id = ? AND deleted_at IS NULL", room)
	if world != "" {
		q = q.Where("world_id = ?", world)
	}
	if e := q.Updates(map[string]any{"deleted_at": time.Now().UTC(), "name_scope": nil, "execution_revision": gorm.Expr("execution_revision + 1")}).Error; e != nil {
		return e
	}
	q = tx.Table(s.previews).Where("room_id = ? AND state IN (?)", room, []string{"preparing", "prepared"})
	if world != "" {
		q = q.Where("world_id = ?", world)
	}
	if e := q.Updates(map[string]any{"state": "expired", "reason": "WORLD_CHANGED"}).Error; e != nil {
		return e
	}
	return tx.Commit().Error
}
