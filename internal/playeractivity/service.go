package playeractivity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/runtimefiles"
	"dont/shared"
)

type Catalog interface {
	List() ([]rooms.Room, error)
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}
type Source interface {
	ReadActivityArtifacts(context.Context, string, string, []string) (shared.RuntimeArtifactBundle, error)
	ActivityScope(context.Context, string, string) (string, error)
}
type Commander interface {
	ExecuteCommand(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error)
}
type activityTarget struct{ room, world string }
type Service struct {
	store       *Store
	rooms       Catalog
	source      Source
	commander   Commander
	mu          sync.Mutex
	due         map[string]time.Time
	active      map[string]bool
	wake        chan struct{}
	pruneAt     map[string]time.Time
	pruneEvents map[string]int
	targets     []activityTarget
	refreshAt   time.Time
	orphanAfter string
}

func New(store *Store, catalog Catalog, source Source, commander Commander) *Service {
	return &Service{store: store, rooms: catalog, source: source, commander: commander, due: map[string]time.Time{}, active: map[string]bool{}, wake: make(chan struct{}, 1), pruneAt: map[string]time.Time{}, pruneEvents: map[string]int{}}
}
func (s *Service) room(room string) error {
	r, err := s.rooms.Room(room)
	if err != nil || !r.Managed {
		return ErrNotFound
	}
	return nil
}
func (s *Service) Settings(room string) (Settings, error) {
	if err := s.room(room); err != nil {
		return Settings{}, err
	}
	return s.store.Settings(room)
}
func (s *Service) UpdateSettings(room string, patch SettingsPatch) (Settings, error) {
	if err := s.room(room); err != nil {
		return Settings{}, err
	}
	c, err := s.store.SaveSettings(room, patch)
	if err == nil {
		s.mu.Lock()
		s.due = map[string]time.Time{}
		delete(s.pruneAt, room)
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return c, err
}
func (s *Service) Run(ctx context.Context) {
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	for {
		s.collectDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.wake:
		}
	}
}
func (s *Service) collectDue(ctx context.Context) {

	type work struct {
		room, world string
		config      Settings
	}
	items := []work{}
	now := s.store.now()
	targets := s.targetSnapshot(now)
	configs := map[string]Settings{}
	for _, target := range targets {
		key := target.room + "/" + target.world
		s.mu.Lock()
		due := s.due[key]
		s.mu.Unlock()
		if now.Before(due) {
			continue
		}
		config, ok := configs[target.room]
		if !ok {
			var err error
			config, err = s.store.Settings(target.room)
			if err != nil {
				continue
			}
			configs[target.room] = config
		}
		items = append(items, work{target.room, target.world, config})
	}
	// Oldest due target first prevents one busy room from starving other nodes.
	sort.SliceStable(items, func(i, j int) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.due[items[i].room+"/"+items[i].world].Before(s.due[items[j].room+"/"+items[j].world])
	})
	if len(items) > 8 {
		items = items[:8]
	}
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for _, item := range items {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(item work) {
			defer wg.Done()
			defer func() { <-sem }()
			readCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			delay := s.collect(readCtx, item.room, item.world, item.config)
			s.mu.Lock()
			s.due[item.room+"/"+item.world] = s.store.now().Add(delay)
			s.mu.Unlock()
		}(item)
	}
	wg.Wait()
}

var actorPattern = regexp.MustCompile(`^KU_[A-Za-z0-9_-]{3,128}$`)
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
var kinds = map[string]bool{"position": true, "player.joined": true, "player.left": true, "player.death": true, "player.respawned": true, "action.pickup": true, "action.drop": true, "action.harvest": true, "action.eat": true, "action.build_item": true, "action.build_structure": true, "action.work": true, "combat.attack": true, "combat.received": true, "combat.kill": true}

func decode(data []byte, value any) error { return runtimefiles.DecodeJSONArtifact(data, value) }
func (s *Service) collect(ctx context.Context, room, world string, c Settings) time.Duration {
	key := room + "/" + world
	s.mu.Lock()
	if s.active[key] {
		s.mu.Unlock()
		return time.Second * 5
	}
	s.active[key] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.active, key); s.mu.Unlock() }()
	row, err := s.store.cursor(room, world)
	if err != nil {
		return time.Minute
	}
	persisted := row
	fail := func(state, code string) time.Duration {
		failure := persisted
		failure.State = state
		failure.Error = code
		_ = s.store.saveCursor(s.store.db, failure)
		return time.Minute
	}
	scope, err := s.source.ActivityScope(ctx, room, world)
	if err != nil {
		return fail("unavailable", "NODE_UNAVAILABLE")
	}
	bundle, err := s.source.ReadActivityArtifacts(ctx, room, world, nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fail("unavailable", "ACTIVITY_RUNTIME_UNAVAILABLE")
		}
		return fail("unavailable", "ACTIVITY_READ_FAILED")
	}
	if err = runtimefiles.ValidateArtifactBundle(shared.ArtifactPlayerActivity, bundle); err != nil {
		return fail("unavailable", "ACTIVITY_INVALID_BUNDLE")
	}
	var manifest Manifest
	found := false
	for _, a := range bundle.Artifacts {
		if a.Name == "activity-manifest.json" {
			found = true
			err = decode(a.Data, &manifest)
			break
		}
	}
	if !found || err != nil || !validManifest(manifest) {
		return fail("unavailable", "ACTIVITY_INVALID_MANIFEST")
	}
	boundaryChanged := row.Scope != scope || row.Generation > manifest.Generation
	if boundaryChanged {
		row.Generation = 0
		row.Scope = scope
		row.Dropped = 0
	}
	now := s.store.now().UTC()
	events := []Event{}
	systemEvent := func(kind, code string, fields map[string]any) {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["code"] = code
		event := Event{ID: identity(room, world, scope, manifest.ProducerInstanceID, kind, code, strconv.FormatInt(manifest.Generation, 10), strconv.FormatInt(manifest.Dropped, 10)), RoomID: room, WorldID: world, Kind: kind, ProducerInstanceID: manifest.ProducerInstanceID, SessionID: manifest.SessionID, OccurredAt: now, OccurredAtUnix: now.Unix(), ReceivedAt: now, Fields: fields}
		events = append(events, event)
	}
	if row.Producer != manifest.ProducerInstanceID || boundaryChanged {
		systemEvent("collection.instance", "RUNTIME_INSTANCE_CHANGED", nil)
		row.Producer = manifest.ProducerInstanceID
		row.Dropped = 0
	}
	if manifest.Dropped > row.Dropped {
		systemEvent("collection.gap", "NODE_EVENTS_DROPPED", map[string]any{"dropped": manifest.Dropped - row.Dropped})
		row.GapCount++
		row.Dropped = manifest.Dropped
	}
	slots := []Slot{}
	for _, slot := range manifest.Slots {
		if slot.Generation > row.Generation {
			slots = append(slots, slot)
		}
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Generation < slots[j].Generation })
	if len(slots) > 0 && slots[0].Generation > row.Generation+1 {
		systemEvent("collection.gap", "SPOOL_WINDOW_EXCEEDED", map[string]any{"missingSegments": slots[0].Generation - row.Generation - 1})
		row.GapCount++
		row.Generation = slots[0].Generation - 1
	}
	if len(slots) > 8 {
		slots = slots[:8]
	}
	if len(slots) > 0 {
		names := []string{}
		for _, slot := range slots {
			names = append(names, fmt.Sprintf("activity-%02d.json", slot.Slot))
		}
		segments, err := s.source.ReadActivityArtifacts(ctx, room, world, names)
		if err != nil {
			return fail("unavailable", "ACTIVITY_SEGMENT_READ_FAILED")
		}
		if runtimefiles.ValidateArtifactBundle(shared.ArtifactPlayerActivity, segments) != nil {
			return fail("unavailable", "ACTIVITY_INVALID_SEGMENT_BUNDLE")
		}
		byName := map[string][]byte{}
		for _, a := range segments.Artifacts {
			byName[a.Name] = a.Data
		}
		for _, slot := range slots {
			var segment Segment
			data := byName[fmt.Sprintf("activity-%02d.json", slot.Slot)]
			// DST may prepend its PersistentStorage header to the bounded JSON payload.
			if len(data) > 65536+256 || decode(data, &segment) != nil || !validSegment(segment, slot, now) {
				return fail("unavailable", "ACTIVITY_SEGMENT_CHANGED")
			}
			if slot.Generation > row.Generation+1 {
				systemEvent("collection.gap", "SEGMENT_SEQUENCE_GAP", map[string]any{"missingSegments": slot.Generation - row.Generation - 1})
				row.GapCount++
			}
			for _, event := range segment.Events {
				event.RoomID = room
				event.WorldID = world
				event.ProducerInstanceID = segment.ProducerInstanceID
				event.SessionID = segment.SessionID
				event.ID = identity(room, world, scope, segment.ProducerInstanceID, strconv.FormatInt(event.Sequence, 10))
				event.OccurredAt = time.Unix(event.OccurredAtUnix, 0).UTC()
				event.ReceivedAt = now
				events = append(events, event)
				if row.LastEventAt == nil || event.OccurredAt.After(*row.LastEventAt) {
					at := event.OccurredAt
					row.LastEventAt = &at
				}
			}
			row.Generation = slot.Generation
		}
	}
	after, err := s.source.ActivityScope(ctx, room, world)
	if err != nil || after != scope {
		return time.Second * 5
	}
	sourceAt := time.Unix(manifest.UpdatedAtUnix, 0).UTC()
	row.SourceUpdatedAt = &sourceAt
	row.AppliedRevision = manifest.Config.Revision
	row.LastCollectedAt = &now
	row.Error = ""
	row.State = "recording"
	if !manifest.Config.Enabled {
		row.State = "disabled"
	} else if manifest.PlayerCount == 0 {
		row.State = "idle"
	}
	if manifest.LastError != "" {
		row.State = "degraded"
		row.Error = "NODE_ACTIVITY_ERROR"
	}
	if now.Sub(sourceAt) > 2*time.Minute && (row.State == "recording" || row.State == "idle") {
		row.State = "inactive"
	}
	if err = s.store.ingest(row, events); err != nil {
		return time.Minute
	}
	s.maybePrune(room, c, len(events))
	if manifest.Config.Revision != c.Revision && s.commander != nil {
		raw, _ := json.Marshal(c)
		arguments := map[string]any{}
		_ = json.Unmarshal(raw, &arguments)
		id := "activity-" + identity(room, world, scope, manifest.ProducerInstanceID, c.Revision, strconv.FormatInt(now.Unix()/30, 10))[:48]
		receipt, err := s.commander.ExecuteCommand(ctx, room, world, dstruntime.CommandRequest{RequestID: id, Action: "activity.configure", Arguments: arguments})
		if err != nil || !receipt.OK {
			if receipt.Code != "ACTIVITY_BUSY" {
				row.Error = "ACTIVITY_CONFIG_APPLY_FAILED"
				row.State = "degraded"
				_ = s.store.saveCursor(s.store.db, row)
			}
			return time.Second * 30
		}
	}
	if !c.Enabled && row.AppliedRevision == c.Revision {
		return time.Minute
	}
	if manifest.PlayerCount == 0 && manifest.Config.Revision == c.Revision {
		return time.Second * 30
	}
	return time.Duration(c.SyncIntervalSeconds) * time.Second
}
func validManifest(m Manifest) bool {
	expected := m.Generation
	if expected > 64 {
		expected = 64
	}
	if int64(len(m.Slots)) != expected {
		return false
	}

	if m.SchemaVersion != 1 || !tokenPattern.MatchString(m.ProducerInstanceID) || len(m.SessionID) > 128 || len(m.ShardID) > 64 || m.Generation < 0 || m.Generation > 1e12 || len(m.Slots) > 64 || m.Dropped < 0 || m.PlayerCount < 0 || m.PlayerCount > 1024 || len(m.Config.Revision) < 1 || len(m.Config.Revision) > 80 || m.UpdatedAtUnix < 946684800 || m.UpdatedAtUnix > time.Now().Add(5*time.Minute).Unix() || m.Pending < 0 || m.Pending > 512 {
		return false
	}
	seen := map[int]bool{}
	gens := map[int64]bool{}
	for _, slot := range m.Slots {
		if slot.Slot < 1 || slot.Slot > 64 || seen[slot.Slot] || gens[slot.Generation] || slot.Generation < 1 || slot.Generation > m.Generation || slot.Generation < m.Generation-63 || int((slot.Generation-1)%64)+1 != slot.Slot || slot.LastSequence-slot.FirstSequence > 100000 || slot.FirstSequence < 1 || slot.LastSequence < slot.FirstSequence || !tokenPattern.MatchString(slot.ProducerInstanceID) || len(slot.SessionID) > 128 || len(slot.ShardID) > 64 {
			return false
		}
		seen[slot.Slot] = true
		gens[slot.Generation] = true
	}
	return true
}
func validSegment(v Segment, slot Slot, now time.Time) bool {
	if v.SchemaVersion != 1 || v.Generation != slot.Generation || v.ProducerInstanceID != slot.ProducerInstanceID || v.SessionID != slot.SessionID || v.ShardID != slot.ShardID || v.FirstSequence != slot.FirstSequence || v.LastSequence != slot.LastSequence || len(v.Events) < 1 || len(v.Events) > 128 {
		return false
	}
	previous := int64(0)
	for _, e := range v.Events {
		if e.Sequence <= previous || e.Sequence < v.FirstSequence || e.Sequence > v.LastSequence || e.OccurredAtUnix < 946684800 || e.OccurredAtUnix > now.Add(5*time.Minute).Unix() || !kinds[e.Kind] || !actorPattern.MatchString(e.UserID) || len(e.Name) > 128 || len(e.Prefab) > 80 || len(e.Fields) > 20 {
			return false
		}
		if e.Position != nil {
			for _, n := range []float64{e.Position.X, e.Position.Y, e.Position.Z} {
				if math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) > 1e9 {
					return false
				}
			}
		}
		for k, value := range e.Fields {
			if len(k) > 40 {
				return false
			}
			switch x := value.(type) {
			case string:
				if len(x) > 128 {
					return false
				}
			case float64:
				if math.IsNaN(x) || math.IsInf(x, 0) || math.Abs(x) > 1e15 {
					return false
				}
			case bool:
			default:
				return false
			}
		}
		previous = e.Sequence
	}
	return v.Events[0].Sequence == v.FirstSequence && previous == v.LastSequence
}

func (s *Service) Status(room string) (Status, error) {
	c, err := s.Settings(room)
	if err != nil {
		return Status{}, err
	}
	result := Status{Settings: c, Worlds: []WorldStatus{}}
	worlds, err := s.rooms.Worlds(room)
	if err != nil {
		return result, err
	}
	for _, w := range worlds {
		row, err := s.store.cursor(room, w.ID)
		if err != nil {
			return result, err
		}
		state := row.State
		if state == "" {
			state = "pending"
		}
		if row.SourceUpdatedAt != nil && s.store.now().Sub(*row.SourceUpdatedAt) > 2*time.Minute && (state == "recording" || state == "idle") {
			state = "inactive"
		}
		result.Worlds = append(result.Worlds, WorldStatus{WorldID: w.ID, WorldName: w.Name, State: state, DesiredRevision: c.Revision, AppliedRevision: row.AppliedRevision, SourceUpdatedAt: row.SourceUpdatedAt, Pending: row.AppliedRevision != c.Revision, LastCollectedAt: row.LastCollectedAt, LastEventAt: row.LastEventAt, Dropped: row.Dropped, GapCount: row.GapCount, Error: row.Error})
	}
	var counts struct {
		Count int64
		Bytes int64
	}
	err = s.store.db.Table(s.store.events).Select("COUNT(*) AS count, COALESCE(SUM(size_bytes),0) AS bytes").Where("room_id = ?", room).Scan(&counts).Error
	result.StoredEvents = counts.Count
	result.StoredBytes = counts.Bytes
	return result, err
}

func safeKind(kind string) bool {
	return kind == "" || kinds[kind] || kind == "collection.gap" || kind == "collection.instance" || strings.HasSuffix(kind, ".") && (kind == "combat." || kind == "action." || kind == "player." || kind == "collection.")
}

func (s *Service) maybePrune(room string, c Settings, added int) {
	s.mu.Lock()
	s.pruneEvents[room] += added
	due := s.store.now().Sub(s.pruneAt[room]) >= time.Minute || s.pruneEvents[room] >= 1024
	if due {
		s.pruneAt[room] = s.store.now()
		s.pruneEvents[room] = 0
	}
	s.mu.Unlock()
	if due {
		_ = s.store.prune(room, c)
	}
}

// Wake invalidates target discovery only on lifecycle/connection events. Idle
// timer ticks do not repeatedly enumerate room files or load world settings.
func (s *Service) Wake() {
	s.mu.Lock()
	s.refreshAt = time.Time{}
	s.due = map[string]time.Time{}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) targetSnapshot(now time.Time) []activityTarget {
	s.mu.Lock()
	if now.Before(s.refreshAt) {
		result := append([]activityTarget{}, s.targets...)
		s.mu.Unlock()
		return result
	}
	// Set the next refresh before reading. A concurrent Wake can invalidate it.
	s.refreshAt = now.Add(time.Minute)
	s.mu.Unlock()
	rooms, err := s.rooms.List()
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return append([]activityTarget{}, s.targets...)
	}
	targets := []activityTarget{}
	for _, room := range rooms {
		if !room.Managed {
			continue
		}
		worlds, err := s.rooms.Worlds(room.ID)
		if err != nil {
			continue
		}
		for _, w := range worlds {
			targets = append(targets, activityTarget{room.ID, w.ID})
		}
	}
	s.mu.Lock()
	s.targets = targets
	s.mu.Unlock()
	// Retention also applies after a room is removed from the active catalog.
	// Walk a bounded page of previously collected rooms, never their save files.

	var persisted []struct{ RoomID string }
	readPage := func() error {
		return s.store.db.Table(s.store.cursors).Select("DISTINCT room_id").Where("room_id > ?", s.orphanAfter).Order("room_id ASC").Limit(8).Scan(&persisted).Error
	}
	if err := readPage(); err == nil {
		if len(persisted) == 0 && s.orphanAfter != "" {
			s.orphanAfter = ""
			if err := readPage(); err != nil {
				return append([]activityTarget{}, targets...)
			}
		}
		for _, r := range persisted {
			if c, err := s.store.Settings(r.RoomID); err == nil {
				s.maybePrune(r.RoomID, c, 0)
			}
			s.orphanAfter = r.RoomID
		}
	}

	return append([]activityTarget{}, targets...)
}
