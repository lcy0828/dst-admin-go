package performance

import (
	"context"
	"dont/internal/runtimeaudit"
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	"dont/internal/rooms"
)

type Catalog interface {
	List() ([]rooms.Room, error)
	Room(string) (rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}
type EventSource interface {
	List(string, runtimeaudit.ListFilter) (runtimeaudit.List, error)
}

type Source interface {
	Samples(context.Context, string) ([]Input, error)
}
type Service struct {
	events        EventSource
	store         *Store
	rooms         Catalog
	source        Source
	mu            sync.Mutex
	collectMu     sync.Mutex
	previous      map[string]Input
	due, pruneAt  map[string]time.Time
	wake          chan struct{}
	cachedRooms   []rooms.Room
	refreshAt     time.Time
	globalPruneAt time.Time
	now           func() time.Time
}

func New(store *Store, rooms Catalog, source Source) *Service {
	return &Service{store: store, rooms: rooms, source: source, previous: map[string]Input{}, due: map[string]time.Time{}, pruneAt: map[string]time.Time{}, wake: make(chan struct{}, 1), now: time.Now}
}
func (s *Service) ConfigureEvents(source EventSource) { s.events = source }
func (s *Service) room(id string) error {
	r, err := s.rooms.Room(id)
	if err != nil {
		return err
	}
	if !r.Managed {
		return rooms.ErrRoomNotManaged
	}
	return nil
}
func (s *Service) Settings(room string) (Settings, error) {
	if err := s.room(room); err != nil {
		return Settings{}, err
	}
	return s.store.Settings(room)
}
func (s *Service) UpdateSettings(room string, v Settings) (Settings, error) {
	if err := s.room(room); err != nil {
		return v, err
	}
	value, err := s.store.SaveSettings(room, v)
	if err == nil {
		s.mu.Lock()
		delete(s.due, room)
		for key, in := range s.previous {
			if in.RoomID == room {
				delete(s.previous, key)
			}
		}
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return value, err
}
func (s *Service) Report(room string, q Query) (Report, error) {
	if err := s.room(room); err != nil {
		return Report{}, err
	}
	worlds, err := s.rooms.Worlds(room)
	if err != nil {
		return Report{}, err
	}
	if q.WorldID != "" {
		found := false
		for _, w := range worlds {
			if w.ID == q.WorldID {
				found = true
			}
		}
		if !found {
			return Report{}, rooms.ErrWorldNotFound
		}
	}
	report, err := s.store.Report(room, q, len(worlds), s.now().UTC())
	if err != nil {
		return report, err
	}
	report.Events = []RuntimeEvent{}
	if s.events != nil {
		events, err := s.events.List(room, runtimeaudit.ListFilter{WorldID: q.WorldID, From: report.From, To: report.To, Limit: 100})
		if err != nil {
			report.EventsWarning = "EVENTS_UNAVAILABLE"
		} else {
			names := map[string]string{}
			for _, w := range worlds {
				names[w.ID] = w.Name
			}
			report.EventsTruncated = events.Total > len(events.Items)
			slices.Reverse(events.Items)
			exits := map[string]int{}
			for _, e := range events.Items {
				report.Events = append(report.Events, RuntimeEvent{ID: e.ID, WorldID: e.WorldID, WorldName: names[e.WorldID], At: e.OccurredAt, Type: string(e.Type), Source: string(e.Source), ReasonCode: e.ReasonCode, JobID: e.JobID})
				if e.Type == runtimeaudit.EventUnexpectedExit {
					exits[e.WorldID]++
				}
			}
			for i := range report.Worlds {
				if n := exits[report.Worlds[i].WorldID]; n > 0 {
					report.Worlds[i].Findings = append(report.Worlds[i].Findings, Finding{Code: "UNEXPECTED_EXIT_RECORDED", Severity: "warning", Value: float64(n)})
				}
			}
		}
	}
	return report, nil
}
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.collectDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
	}
}
func (s *Service) collectDue(ctx context.Context) {
	s.collectMu.Lock()
	defer s.collectMu.Unlock()
	now := s.now().UTC()
	if !now.Before(s.globalPruneAt) {
		if err := s.store.PruneExpired(now); err != nil {
			log.Printf("[Performance] retention: %v", err)
		}
		s.globalPruneAt = now.Add(10 * time.Minute)
	}
	if !now.Before(s.refreshAt) {
		items, err := s.rooms.List()
		if err == nil {
			s.cachedRooms = items
		}
		s.refreshAt = now.Add(time.Minute)
	}
	items := s.cachedRooms
	count := 0
	active := map[string]bool{}
	for _, r := range items {
		if r.Managed {
			active[r.ID] = true
		}
	}
	s.mu.Lock()
	for key, in := range s.previous {
		if !active[in.RoomID] {
			delete(s.previous, key)
		}
	}
	for room := range s.due {
		if !active[room] {
			delete(s.due, room)
			delete(s.pruneAt, room)
		}
	}
	s.mu.Unlock()
	for _, r := range items {
		if !r.Managed || ctx.Err() != nil {
			continue
		}
		s.mu.Lock()
		due := s.due[r.ID]
		s.mu.Unlock()
		if now.Before(due) {
			continue
		}
		if count >= 8 {
			break
		}
		count++
		c, err := s.store.Settings(r.ID)
		if err != nil {
			continue
		}
		s.mu.Lock()
		s.due[r.ID] = now.Add(time.Duration(c.IntervalSeconds) * time.Second)
		prune := s.pruneAt[r.ID]
		s.mu.Unlock()
		if now.After(prune) {
			if err := s.store.Prune(r.ID, c, now); err != nil {
				log.Printf("[Performance] prune: %v", err)
			}
			s.mu.Lock()
			s.pruneAt[r.ID] = now.Add(10 * time.Minute)
			s.mu.Unlock()
		}
		if !c.Enabled {
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		inputs, err := s.source.Samples(readCtx, r.ID)
		cancel()
		if err != nil {
			s.mu.Lock()
			for key, old := range s.previous {
				if old.RoomID == r.ID {
					delete(s.previous, key)
				}
			}
			s.mu.Unlock()
			continue
		}
		// A settings edit made during I/O invalidates the complete batch.
		current, err := s.store.Settings(r.ID)
		if err != nil || current.Revision != c.Revision || !current.Enabled {
			continue
		}
		present := map[string]bool{}
		for _, in := range inputs {
			if in.RoomID == r.ID {
				present[in.WorldID] = true
			}
		}
		s.mu.Lock()
		for key, in := range s.previous {
			if in.RoomID == r.ID && !present[in.WorldID] {
				delete(s.previous, key)
			}
		}
		s.mu.Unlock()
		for _, in := range inputs {
			if in.RoomID != r.ID || in.WorldID == "" || in.At.IsZero() || in.At.After(now.Add(time.Minute)) {
				continue
			}
			key := r.ID + "/" + in.WorldID
			s.mu.Lock()
			old, exists := s.previous[key]
			s.mu.Unlock()
			if exists && in.InstanceID == old.InstanceID && (in.At.Before(old.At) || in.At.Equal(old.At) || in.State != "running" && in.State == old.State) {
				continue
			}
			var previous *Input
			if exists {
				previous = &old
			}
			point := score(previous, in, time.Duration(c.IntervalSeconds)*time.Second)
			s.mu.Lock()
			if err := s.store.appendCurrent(point, c.Revision); err != nil {
				s.mu.Unlock()
				if !errors.Is(err, ErrConflict) {
					log.Printf("[Performance] append: %v", err)
				}
				continue
			}
			if len(s.previous) < 4096 || exists {
				s.previous[key] = in
			}
			s.mu.Unlock()
		}
	}
}
