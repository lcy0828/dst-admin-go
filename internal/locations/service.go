package locations

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"dont/internal/console"
	"dont/internal/landmarks"
	"dont/internal/operationlease"
	"dont/internal/players"
	"dont/internal/rooms"
	"dont/internal/worldquery"
	"github.com/google/uuid"
)

type Runtime interface {
	LocationIdentity(context.Context, string, string) (console.LocationIdentity, error)
	ExecuteLocation(context.Context, string, string, string, map[string]any) (console.Run, error)
	Run(string) (console.Run, error)
	RefreshRun(context.Context, string, string) (console.Run, error)
}
type Players interface {
	Resolve(string, string, int) (players.Resolution, error)
	OnlineWorld(context.Context, string, string) (rooms.World, error)
}
type Catalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}
type LeaseService interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

var playerIDPattern = regexp.MustCompile(`^KU_[A-Za-z0-9_-]{3,64}$`)

type Service struct {
	landmarks interface {
		Query(context.Context, worldquery.Principal, worldquery.Request) (worldquery.Result, error)
	}
	leases    LeaseService
	store     *Store
	runtime   Runtime
	rooms     Catalog
	players   Players
	binding   func(context.Context, string, string) (string, error)
	mapSource func(string, string, string, string, float64, float64) (string, error)
	mu        sync.Mutex
	busy      map[string]bool
	limits    map[string][]time.Time
	slots     chan struct{}
}

func New(store *Store, runtime Runtime, rooms Catalog) *Service {
	return &Service{store: store, runtime: runtime, rooms: rooms, busy: map[string]bool{}, limits: map[string][]time.Time{}, slots: make(chan struct{}, 2)}
}
func (s *Service) Configure(p Players, binding func(context.Context, string, string) (string, error)) {
	s.players = p
	s.binding = binding
}
func (s *Service) ConfigureLeases(leases LeaseService) { s.leases = leases }
func (s *Service) ConfigureLandmarks(q interface {
	Query(context.Context, worldquery.Principal, worldquery.Request) (worldquery.Result, error)
}) {
	s.landmarks = q
}
func (s *Service) ConfigureMapSource(f func(string, string, string, string, float64, float64) (string, error)) {
	s.mapSource = f
}

func (s *Service) Binding(ctx context.Context, room, world string) (string, error) {
	if s.binding == nil {
		return "", ErrUnavailable
	}
	return s.binding(ctx, room, world)
}
func (s *Service) List(room, world, search string, offset int) ([]Location, error) {
	if _, e := s.rooms.Room(room); e != nil {
		return nil, e
	}
	return s.store.List(room, world, search, offset)
}
func (s *Service) Get(room, id string) (Location, error) { return s.store.Get(room, id) }
func (s *Service) Delete(room, id string, revision int64) error {
	return s.store.Delete(room, id, revision)
}
func (s *Service) resolve(ctx context.Context, room, query, world string) (string, string, error) {
	if s.players == nil || len(query) > 256 || query == "" {
		return "", "", ErrInvalid
	}
	id := query
	if !strings.HasPrefix(id, "KU_") {
		r, e := s.players.Resolve(room, query, 0)
		if e != nil {
			return "", "", e
		}
		if r.Status != "resolved" || len(r.Items) != 1 {
			return "", "", ErrConflict
		}
		id = r.Items[0].ID
	}
	if !playerIDPattern.MatchString(id) {
		return "", "", ErrInvalid
	}
	w, e := s.players.OnlineWorld(ctx, room, id)
	if e != nil {
		return "", "", ErrUnavailable
	}
	if world != "" && world != w.ID {
		return "", "", ErrConflict
	}
	return id, w.ID, nil
}

// A single non-queueing gate covers real prechecks, not ordinary list reads.
func (s *Service) gate(actor, room, world string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := room + "/" + world
	now := time.Now()
	for k, times := range s.limits {
		fresh := times[:0]
		for _, t := range times {
			if now.Sub(t) < 10*time.Second {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) == 0 {
			delete(s.limits, k)
		} else {
			s.limits[k] = fresh
		}
	}
	if s.busy[key] || len(s.limits[key]) >= 4 {
		return nil, ErrBusy
	}
	ak := "actor:" + actor
	var recent int
	for _, t := range s.limits[ak] {
		if now.Sub(t) < 2*time.Second {
			recent++
		}
	}
	if recent >= 2 || len(s.limits) >= 2048 {
		return nil, ErrBusy
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	s.busy[key] = true
	s.limits[key] = append(s.limits[key], now)
	s.limits[ak] = append(s.limits[ak], now)
	return func() { s.mu.Lock(); delete(s.busy, key); s.mu.Unlock(); <-s.slots }, nil
}
func caller(actor, room, world string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(actor+"\x00"+room+"\x00"+world)))
}
func effect(run console.Run, id, player string) *Effect {
	if run.Status != console.RunSucceeded || run.ExecutionOutcome != "confirmed" || run.Output == nil || run.Output.Truncated {
		return nil
	}
	var result *Effect
	for _, line := range strings.Split(run.Output.Text, "\n") {
		if !strings.HasPrefix(line, "[DST-ADMIN-TELEPORT-V1] ") {
			continue
		}
		if result != nil {
			return nil
		}
		var e Effect
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "[DST-ADMIN-TELEPORT-V1] ")), &e) != nil || e.OperationID != id || e.PlayerID != player || e.ObservedAt <= 0 {
			return nil
		}
		if e.Point != nil && !e.Point.Valid() || e.Before != nil && !e.Before.Valid() || e.After != nil && !e.After.Valid() {
			return nil
		}
		if e.State == "confirmed" && (e.Point == nil || e.Before == nil || e.After == nil || (e.Point.X-e.After.X)*(e.Point.X-e.After.X)+(e.Point.Z-e.After.Z)*(e.Point.Z-e.After.Z) > .25) {
			return nil
		}
		result = &e
	}
	return result
}
func (s *Service) Save(ctx context.Context, actor, room, id string, in SaveInput) (Location, error) {
	if actor == "" {
		return Location{}, ErrDenied
	}
	if _, _, e := cleanName(in.Name); e != nil {
		return Location{}, e
	}
	var old Location
	var e error
	if id != "" {
		old, e = s.store.Get(room, id)
		if e != nil {
			return Location{}, e
		}
		if in.Revision != old.Revision {
			return Location{}, ErrConflict
		}
		if in.WorldID == "" {
			in.WorldID = old.WorldID
		}
	}
	if in.PlayerID == "" && in.X == nil && in.Z == nil && in.MapID == "" && id != "" && in.WorldID == old.WorldID {
		old.Name = in.Name
		old.UpdatedBy = actor
		return s.store.Save(old, in.Revision)
	}
	if in.PlayerID != "" && (in.X != nil || in.Z != nil || in.MapID != "") {
		return Location{}, ErrInvalid
	}
	if in.PlayerID != "" {
		in.PlayerID, in.WorldID, e = s.resolve(ctx, room, in.PlayerID, in.WorldID)
		if e != nil {
			return Location{}, e
		}
	}
	if _, e = s.rooms.World(room, in.WorldID); e != nil {
		return Location{}, e
	}
	release, e := s.gate(actor, room, in.WorldID)
	if e != nil {
		return Location{}, e
	}
	defer release()
	identity, e := s.runtime.LocationIdentity(ctx, room, in.WorldID)
	if e != nil {
		return Location{}, e
	}
	binding, e := s.Binding(ctx, room, in.WorldID)
	if e != nil {
		return Location{}, e
	}
	p := Location{ID: id, RoomID: room, WorldID: in.WorldID, Name: in.Name, SessionID: identity.SessionID, BindingRevision: binding, Source: "coordinates", SourceTime: time.Now().UTC(), CreatedBy: actor, UpdatedBy: actor}
	if in.PlayerID != "" {
		op := uuid.NewString()
		run, e := s.runtime.ExecuteLocation(ctx, room, in.WorldID, uuid.NewString(), map[string]any{"phase": "capture", "operationId": op, "playerId": in.PlayerID, "caller": caller(actor, room, in.WorldID), "sessionId": identity.SessionID})
		if e != nil {
			return Location{}, e
		}
		v := effect(run, op, in.PlayerID)
		if v == nil || v.State != "captured" || v.Point == nil || v.SessionID != identity.SessionID {
			return Location{}, ErrUnavailable
		}
		p.X, p.Z = v.Point.X, v.Point.Z
		p.Source = "player"
		p.SourceRef = run.ID
		p.SourceTime = time.Unix(v.ObservedAt, 0).UTC()
	} else {
		if in.X == nil || in.Z == nil {
			return Location{}, ErrInvalid
		}
		p.X, p.Z = *in.X, *in.Z
		if in.MapID != "" {
			if s.mapSource == nil {
				return Location{}, ErrUnavailable
			}
			source, err := s.mapSource(room, in.WorldID, in.MapID, identity.SessionID, p.X, p.Z)
			if err != nil {
				return Location{}, ErrOldWorld
			}
			p.Source = "map"
			p.SourceRef = source
			if _, at, ok := strings.Cut(source, "@"); ok {
				if stamp, err := time.Parse(time.RFC3339, at); err == nil {
					p.SourceTime = stamp
				}
			}
		}
	}
	return s.store.Save(p, in.Revision)
}
func (s *Service) Prepare(ctx context.Context, actor, room string, in PrepareInput) (Preview, error) {
	if actor == "" {
		return Preview{}, ErrDenied
	}
	landmarkID := landmarks.ID(in.LocationName)
	if len(in.LandmarkRef) > 100 || in.LandmarkRef != "" && (landmarkID == "" || in.LocationID != "") {
		return Preview{}, ErrInvalid
	}
	destinations := 0
	for _, v := range []bool{in.LocationID != "", in.LocationName != "", in.TargetPlayerID != "", in.X != nil || in.Z != nil} {
		if v {
			destinations++
		}
	}
	if destinations != 1 {
		return Preview{}, ErrInvalid
	}
	id, world, e := s.resolve(ctx, room, in.PlayerID, in.WorldID)
	if e != nil {
		return Preview{}, e
	}
	release, e := s.gate(actor, room, world)
	if e != nil {
		return Preview{}, e
	}
	defer release()
	identity, e := s.runtime.LocationIdentity(ctx, room, world)
	if e != nil {
		return Preview{}, e
	}
	binding, e := s.Binding(ctx, room, world)
	if e != nil {
		return Preview{}, e
	}
	p := Preview{ID: uuid.NewString(), Actor: actor, RoomID: room, WorldID: world, PlayerID: id, Identity: identity.Instance, BindingRevision: binding, State: "preparing", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(60 * time.Second)}
	var saved *Location
	if in.LocationID == "" && in.LocationName != "" && in.LandmarkRef == "" {
		// Preserve saved-name semantics, including common names and aliases.
		// Only a missing saved name permits a common landmark lookup.
		l, err := s.store.Resolve(room, world, in.LocationName)
		if err == nil {
			saved = &l
		} else if !errors.Is(err, ErrNotFound) || landmarkID == "" {
			return Preview{}, err
		}
	}
	if saved == nil && in.LocationID == "" && landmarkID != "" {
		if s.landmarks == nil {
			return Preview{}, ErrUnavailable
		}
		result, err := s.landmarks.Query(ctx, worldquery.Principal{ID: actor, RoomID: room}, worldquery.Request{RoomID: room, WorldID: world, QueryID: "landmarks"})
		if err != nil || !result.Complete || result.Status != "succeeded" {
			return Preview{RoomID: room, WorldID: world, PlayerID: id, State: "rejected", Reason: "LANDMARK_SCAN_INCOMPLETE"}, nil
		}
		var data landmarks.Data
		if json.Unmarshal(result.Data, &data) != nil {
			return Preview{}, ErrUnavailable
		}
		choices := []landmarks.Point{}
		for _, point := range data.Items {
			if point.Valid() && point.LandmarkID == landmarkID && (in.LandmarkRef == "" || point.ID == in.LandmarkRef) {
				choices = append(choices, point)
			}
		}
		if len(choices) == 0 {
			return Preview{RoomID: room, WorldID: world, PlayerID: id, LocationName: landmarks.Name(landmarkID), State: "rejected", Reason: "LANDMARK_NOT_FOUND"}, nil
		}
		if len(choices) > 1 {
			return Preview{RoomID: room, WorldID: world, PlayerID: id, LocationName: landmarks.Name(landmarkID), State: "needs_selection", Candidates: choices}, nil
		}
		p.LocationName = landmarks.Name(landmarkID)
		p.X, p.Z = choices[0].X, choices[0].Z
	} else if in.LocationID != "" || in.LocationName != "" {
		var l Location
		if saved != nil {
			l = *saved
		} else if in.LocationID != "" {
			l, e = s.store.Get(room, in.LocationID)
		} else {
			l, e = s.store.Resolve(room, world, in.LocationName)
		}
		if e != nil {
			return Preview{}, e
		}
		if l.WorldID != world {
			return Preview{}, ErrConflict
		}
		if l.SessionID != identity.SessionID {
			return Preview{}, ErrOldWorld
		}
		p.LocationID, p.LocationName, p.LocationRevision = l.ID, l.Name, l.ExecutionRevision
		p.X, p.Z = l.X, l.Z
		p.NeedsReview = l.BindingRevision != binding
	} else if in.TargetPlayerID != "" {
		p.TargetPlayerID, _, e = s.resolve(ctx, room, in.TargetPlayerID, world)
		if e != nil {
			return Preview{}, e
		}
	} else {
		if in.X == nil || in.Z == nil || !(Point{*in.X, *in.Z}).Valid() {
			return Preview{}, ErrInvalid
		}
		p.X, p.Z = *in.X, *in.Z
	}
	p.RunID = uuid.NewString()
	if e = s.store.insertPreview(p); e != nil {
		return Preview{}, e
	}
	run, e := s.runtime.ExecuteLocation(ctx, room, world, p.RunID, map[string]any{"phase": "prepare", "operationId": p.ID, "playerId": id, "caller": caller(actor, room, world), "sessionId": identity.SessionID, "x": p.X, "z": p.Z, "targetPlayerId": p.TargetPlayerID})
	p.Effect = effect(run, p.ID, p.PlayerID)
	p.State = "rejected"
	p.Reason = "PRECHECK_UNAVAILABLE"
	if e == nil && p.Effect != nil && p.Effect.State == "prepared" && p.Effect.Point != nil && p.Effect.SessionID == identity.SessionID {
		p.State = "prepared"
		p.Reason = ""
		p.X, p.Z = p.Effect.Point.X, p.Effect.Point.Z
		p.PlayerName = p.Effect.PlayerName
		p.ExpiresAt = time.Unix(p.Effect.ExpiresAt, 0).UTC()
		if p.ExpiresAt.After(time.Now().Add(61*time.Second)) || !p.ExpiresAt.After(time.Now()) {
			p.State = "rejected"
			p.Reason = "CLOCK_MISMATCH"
		}
	} else if p.Effect != nil {
		p.Reason = p.Effect.Reason
	}
	p.ServerTime = time.Now().UTC()
	if se := s.store.finish(p); se != nil {
		return Preview{}, se
	}
	return p, nil
}
func (s *Service) GetPreview(ctx context.Context, actor, room, id string, refresh bool) (Preview, error) {
	p, e := s.store.preview(room, actor, id)
	if e != nil {
		return p, e
	}
	if (p.State == "prepared" || p.State == "preparing") && !p.ExpiresAt.After(time.Now()) {
		ok, e := s.store.transition(p.ID, p.State, "expired", p.RunID)
		if e != nil {
			return p, e
		}
		if ok {
			p.State = "expired"
		} else {
			return s.store.preview(room, actor, id)
		}
	}
	if refresh && (p.State == "dispatching" || p.State == "uncertain") && p.RunID != "" {
		run, e := s.runtime.RefreshRun(ctx, room, p.RunID)
		if e == nil {
			p = s.result(p, run)
			if e = s.store.finish(p); e != nil {
				if errors.Is(e, ErrConflict) {
					return s.store.preview(room, actor, id)
				}
				return p, e
			}
		}
	}
	return p, nil
}
func (s *Service) result(p Preview, run console.Run) Preview {
	p.Effect = effect(run, p.ID, p.PlayerID)
	p.State = "uncertain"
	p.Reason = "RESULT_UNCONFIRMED"
	if p.Effect != nil && (p.Effect.State == "confirmed" || p.Effect.State == "rejected" || p.Effect.State == "uncertain") {
		p.State = p.Effect.State
		p.Reason = p.Effect.Reason
	} else if run.Status == console.RunFailed && !run.MayHaveExecuted && run.ExecutionOutcome == "none" {
		p.State = "rejected"
		p.Reason = "DISPATCH_REJECTED"
	}
	p.ServerTime = time.Now().UTC()
	return p
}
func (s *Service) Execute(ctx context.Context, actor, room, id string) (Preview, error) {
	p, e := s.GetPreview(ctx, actor, room, id, false)
	if e != nil {
		return p, e
	}
	if p.State != "prepared" {
		return p, nil
	}
	// Claim before dispatch or cancellation, persist run identity before any I/O.
	runID := uuid.NewString()
	claimed, e := s.store.transition(p.ID, "prepared", "dispatching", runID)
	if e != nil {
		return p, e
	}
	if !claimed {
		return s.GetPreview(ctx, actor, room, id, false)
	}
	p.State = "dispatching"
	p.RunID = runID
	reject := func(reason string) (Preview, error) {
		p.State = "rejected"
		p.Reason = reason
		p.Effect = nil
		p.ServerTime = time.Now().UTC()
		return p, s.store.finish(p)
	}
	if !p.ExpiresAt.After(time.Now()) {
		return reject("PREVIEW_EXPIRED")
	}
	if s.leases != nil {
		lease, err := s.leases.Acquire(ctx, room, "location.teleport:"+p.ID, 45*time.Second)
		if err != nil {
			return reject("WORLD_MAINTENANCE")
		}
		defer s.leases.Release(lease)
		ctx = operationlease.WithBorrowedLeases(ctx, lease)
	}
	identity, e := s.runtime.LocationIdentity(ctx, room, p.WorldID)
	if e != nil || identity.Instance != p.Identity {
		return reject("WORLD_CHANGED")
	}
	binding, e := s.Binding(ctx, room, p.WorldID)
	if e != nil || binding != p.BindingRevision {
		return reject("WORLD_CHANGED")
	}
	if p.LocationID != "" {
		l, e := s.store.Get(room, p.LocationID)
		if e != nil || l.ExecutionRevision != p.LocationRevision {
			return reject("LOCATION_CHANGED")
		}
	}
	_, _, e = s.resolve(ctx, room, p.PlayerID, p.WorldID)
	if e != nil {
		return reject("PLAYER_MOVING_WORLD")
	}
	run, e := s.runtime.ExecuteLocation(ctx, room, p.WorldID, p.RunID, map[string]any{"phase": "execute", "operationId": p.ID, "playerId": p.PlayerID, "caller": caller(actor, room, p.WorldID), "sessionId": identity.SessionID, "targetPlayerId": p.TargetPlayerID, "notAfter": p.ExpiresAt.Unix()})
	if e != nil {
		p.State = "uncertain"
		p.Reason = "RESULT_UNCONFIRMED"
	} else {
		p = s.result(p, run)
	}
	if e = s.store.finish(p); e != nil {
		return p, e
	}
	return p, nil
}
func (s *Service) Cancel(ctx context.Context, actor, room, id string) (Preview, error) {
	p, e := s.GetPreview(ctx, actor, room, id, false)
	if e != nil {
		return p, e
	}
	if p.State != "prepared" {
		return p, nil
	}
	won, e := s.store.transition(p.ID, "prepared", "cancelled", p.RunID)
	if e != nil {
		return p, e
	}
	if !won {
		return s.GetPreview(ctx, actor, room, id, false)
	}
	p.State = "cancelled"
	p.Effect = nil
	// The durable CAS already prevents dispatch. Best-effort removal only
	// releases the Runtime reservation, allowing a new choice without waiting
	// sixty seconds. It cannot move a player; lost cleanup expires naturally.
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _ = s.runtime.ExecuteLocation(cleanupCtx, room, p.WorldID, uuid.NewString(), map[string]any{"phase": "cancel", "operationId": p.ID, "playerId": p.PlayerID, "caller": caller(actor, room, p.WorldID)})
	return p, nil
}
