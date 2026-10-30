package worldquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"github.com/google/uuid"
)

type bucket struct {
	tokens float64
	at     time.Time
}
type cached struct {
	value   Result
	expires time.Time
	size    int
}
type flight struct {
	done      chan struct{}
	principal string
	key       string
	result    Result
	err       error
}
type Service struct {
	rooms      RoomReader
	runtime    Runtime
	store      *Store
	now        func() time.Time
	mu         sync.Mutex
	active     map[string]*flight
	buckets    map[string]bucket
	scans      map[string]time.Time
	cache      map[string]cached
	cacheBytes int
	reads      map[string]bool
	slots      chan struct{}
}

func New(r RoomReader, runtime Runtime, store *Store) *Service {
	return &Service{rooms: r, runtime: runtime, store: store, now: time.Now, active: map[string]*flight{}, buckets: map[string]bucket{}, scans: map[string]time.Time{}, cache: map[string]cached{}, reads: map[string]bool{}, slots: make(chan struct{}, 2)}
}
func (s *Service) authorize(p Principal, q Request) error {
	if p.ID == "" || len(p.ID) > 128 {
		return ErrDenied
	}
	if p.RoomID != "" && p.RoomID != q.RoomID {
		return ErrDenied
	}
	if len(q.RoomID) > 512 || len(q.WorldID) > 512 {
		return ErrInvalid
	}
	if _, err := rooms.DecodeID(q.RoomID); err != nil {
		return ErrInvalid
	}
	if _, err := rooms.DecodeID(q.WorldID); err != nil {
		return ErrInvalid
	}
	if _, err := s.rooms.Room(q.RoomID); err != nil {
		return err
	}
	w, err := s.rooms.World(q.RoomID, q.WorldID)
	if err != nil {
		return err
	}
	if w.RoomID != q.RoomID || w.ID != q.WorldID {
		return ErrDenied
	}
	return nil
}
func (s *Service) limit(p Principal) error {
	if p.ID == "" || len(p.ID) > 128 {
		return ErrDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.buckets {
		if now.Sub(v.at) > time.Minute {
			delete(s.buckets, k)
		}
	}
	b, ok := s.buckets[p.ID]
	if !ok {
		if len(s.buckets) >= 256 {
			return ErrLimited
		}
		b = bucket{tokens: 5, at: now}
	}
	b.tokens = min(5, b.tokens+max(0, now.Sub(b.at).Seconds()))
	b.at = now
	if b.tokens < 1 {
		s.buckets[p.ID] = b
		return ErrLimited
	}
	b.tokens--
	s.buckets[p.ID] = b
	return nil
}
func base(q Request) Result {
	return Result{QueryID: q.QueryID, RoomID: q.RoomID, WorldID: q.WorldID, Source: "runtime", Status: "unavailable", TransportOutcome: "not_sent", ExecutionOutcome: "not_executed", ResultState: "unconfirmed"}
}
func (s *Service) Query(ctx context.Context, p Principal, q Request) (Result, error) {
	if dstruntime.ValidateWorldQuery(q.QueryID, q.Prefab) != nil {
		return Result{}, ErrInvalid
	}
	if err := s.limit(p); err != nil {
		return Result{}, err
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return Result{}, ErrBusy
	}
	if err := s.authorize(p, q); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	identity, err := s.runtime.QueryIdentity(ctx, q.RoomID, q.WorldID)
	if err != nil {
		return unavailable(base(q), err), nil
	}
	key := q.RoomID + "/" + q.WorldID + "/" + identity + "/" + q.QueryID + "/" + q.Prefab
	worldKey := q.RoomID + "/" + q.WorldID
	s.mu.Lock()
	now := s.now()
	for k, v := range s.cache {
		if !now.Before(v.expires) {
			delete(s.cache, k)
			s.cacheBytes -= v.size
		}
	}
	if hit, ok := s.cache[p.ID+"/"+key]; ok {
		s.mu.Unlock()
		r := hit.value
		r.Cached = true
		return r, nil
	}
	if current := s.active[worldKey]; current != nil {
		if current.key != key || current.principal != p.ID {
			s.mu.Unlock()
			return Result{}, ErrBusy
		}
		s.mu.Unlock()
		select {
		case <-current.done:
			return current.result, current.err
		case <-ctx.Done():
			return Result{}, ErrBusy
		}
	}
	if len(s.active) >= 2 {
		s.mu.Unlock()
		return Result{}, ErrBusy
	}
	f := &flight{done: make(chan struct{}), principal: p.ID, key: key}
	s.active[worldKey] = f
	s.mu.Unlock()
	result, err := s.query(ctx, p, q, identity, worldKey)
	s.mu.Lock()
	f.result, f.err = result, err
	if err == nil && (result.Status == "succeeded" || result.Status == "partial") {
		b, _ := json.Marshal(result)
		cacheKey := p.ID + "/" + key
		cost := len(b) + len(cacheKey)
		if len(s.cache) < 128 && s.cacheBytes+cost <= 2<<20 {
			expires := s.now().Add(3 * time.Second)
			if result.Source == "snapshot" && result.ObservedAt != nil && result.ObservedAt.Add(10*time.Second).Before(expires) {
				expires = result.ObservedAt.Add(10 * time.Second)
			}
			s.cache[cacheKey] = cached{result, expires, cost}
			s.cacheBytes += cost
		}
	}
	delete(s.active, worldKey)
	close(f.done)
	s.mu.Unlock()
	return result, err
}
func (s *Service) query(ctx context.Context, p Principal, q Request, identity, worldKey string) (Result, error) {
	// Persisted pending records survive controller restarts. A second caller
	// cannot bypass an uncertain execution by changing prefab or credentials.
	rec, pending, err := s.store.pending(q.RoomID, q.WorldID, s.now())
	if err == nil && rec.Identity == identity {
		if rec.Principal == p.ID {
			return pending, nil
		}
		return Result{}, ErrBusy
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Result{}, err
	}
	if r, ok := s.snapshot(ctx, q); ok {
		current, e := s.runtime.QueryIdentity(ctx, q.RoomID, q.WorldID)
		if e == nil && current == identity {
			return r, nil
		}
		return unavailable(base(q), dstruntime.ErrRuntimeUnavailable), nil
	}
	if q.QueryID == "entity_count" {
		s.mu.Lock()
		now := s.now()
		for k, at := range s.scans {
			if now.Sub(at) >= 5*time.Second {
				delete(s.scans, k)
			}
		}
		if at, ok := s.scans[worldKey]; ok && now.Sub(at) < 5*time.Second {
			s.mu.Unlock()
			return Result{}, ErrLimited
		}
		if len(s.scans) >= 2000 {
			s.mu.Unlock()
			return Result{}, ErrBusy
		}
		s.scans[worldKey] = now
		s.mu.Unlock()
	}
	r := base(q)
	r.QueryRunID = uuid.NewString()
	r.Status = "uncertain"
	r.TransportOutcome = "unknown"
	r.ExecutionOutcome = "unknown"
	r.MayHaveExecuted = true
	r.Error = &QueryError{"QUERY_PENDING", "Query dispatch has not been confirmed; read this original run"}
	rec = record{ID: r.QueryRunID, Principal: p.ID, RoomID: q.RoomID, WorldID: q.WorldID, Identity: identity, Prefab: q.Prefab, CreatedAt: s.now().UTC()}
	// At most two active requests; serialise only the small retention+insert.
	s.mu.Lock()
	err = s.store.prune(s.now())
	if err == nil {
		err = s.store.save(rec, r)
	}
	s.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	request := dstruntime.WorldQueryRequest{RequestID: r.QueryRunID, QueryID: q.QueryID, Prefab: q.Prefab, ExpectedIdentity: identity}
	receipt, err := s.runtime.ExecuteWorldQuery(ctx, q.RoomID, q.WorldID, request)
	if err != nil {
		r = unavailable(r, err)
	} else if !validReceipt(r, rec, receipt, s.now()) {
		r = problem(r, "uncertain", "QUERY_RECEIPT_MISMATCH", "Receipt does not belong to the original query")
	} else {
		current, e := s.runtime.QueryIdentity(ctx, q.RoomID, q.WorldID)
		if e != nil || current != identity {
			r = problem(r, "uncertain", "QUERY_INSTANCE_CHANGED", "Runtime identity changed; result cannot be confirmed")
		} else {
			r = acceptReceipt(r, q, receipt)
		}
	}
	if err := s.store.save(rec, r); err != nil {
		return Result{}, fmt.Errorf("persist query result %s: %w", r.QueryRunID, err)
	}
	return r, nil
}
func unavailable(r Result, err error) Result {
	if errors.Is(err, dstruntime.ErrRuntimeCommandUnconfirmed) {
		r.TransportOutcome = "unknown"
		r.ExecutionOutcome = "unknown"
		r.MayHaveExecuted = true
		return problem(r, "uncertain", "QUERY_RECEIPT_UNCONFIRMED", "Query may have executed; read the original receipt, never automatically replay it")
	}
	r.MayHaveExecuted = false
	r.TransportOutcome = "not_sent"
	r.ExecutionOutcome = "not_executed"
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return problem(r, "timed_out", "QUERY_DEADLINE", "Query timed out before dispatch")
	}
	if errors.Is(err, dstruntime.ErrRuntimeCommandBusy) {
		return problem(r, "unavailable", "QUERY_TARGET_BUSY", "The shard execution channel is busy")
	}
	return problem(r, "unavailable", "QUERY_RUNTIME_UNAVAILABLE", "The world is stopped, offline or its Runtime is not ready")
}
func validReceipt(r Result, rec record, receipt dstruntime.CommandReceipt, now time.Time) bool {
	return receipt.RequestID == r.QueryRunID && receipt.Action == "console.execute" && !receipt.CompletedAt.IsZero() && !receipt.CompletedAt.Before(rec.CreatedAt.Truncate(time.Second)) && !receipt.CompletedAt.After(now.Add(30*time.Second))
}
func (s *Service) Get(ctx context.Context, p Principal, room, world, runID string, refresh bool) (Result, error) {
	q := Request{RoomID: room, WorldID: world}
	if err := s.limit(p); err != nil {
		return Result{}, err
	}
	if _, err := uuid.Parse(runID); err != nil {
		return Result{}, ErrInvalid
	}
	if err := s.authorize(p, q); err != nil {
		return Result{}, err
	}
	rec, r, err := s.store.get(runID)
	if err != nil {
		return Result{}, err
	}
	if rec.Principal != p.ID || rec.RoomID != room || rec.WorldID != world {
		return Result{}, ErrNotFound
	}
	if s.now().Sub(rec.CreatedAt) > 7*24*time.Hour {
		return Result{}, ErrNotFound
	}
	if !refresh || r.Status != "uncertain" {
		return r, nil
	}
	if s.now().Sub(rec.CreatedAt) >= 2*time.Minute {
		r.ReceiptCheck = "expired"
		return r, nil
	}
	s.mu.Lock()
	if s.active[room+"/"+world] != nil {
		s.mu.Unlock()
		r.ReceiptCheck = "pending"
		return r, nil
	}
	if s.reads[runID] || len(s.reads) >= 2 {
		s.mu.Unlock()
		return Result{}, ErrBusy
	}
	s.reads[runID] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.reads, runID); s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	identity, err := s.runtime.QueryIdentity(ctx, room, world)
	if err != nil || identity != rec.Identity {
		r.ReceiptCheck = "unavailable"
		return r, nil
	}
	receipt, err := s.runtime.ReadCommandReceipt(ctx, room, world, dstruntime.CommandRequest{RequestID: runID, Action: "console.execute"}, rec.CreatedAt)
	if err != nil {
		r.ReceiptCheck = "unavailable"
		if errors.Is(err, dstruntime.ErrRuntimeResultAbsent) || errors.Is(err, os.ErrNotExist) {
			r.ReceiptCheck = "not_found"
		}
		return r, nil
	}
	if !validReceipt(r, rec, receipt, s.now()) {
		r.ReceiptCheck = "not_found"
		return r, nil
	}
	current, e := s.runtime.QueryIdentity(ctx, room, world)
	if e != nil || current != identity {
		r.ReceiptCheck = "unavailable"
		return r, nil
	}
	q.QueryID = r.QueryID
	q.Prefab = rec.Prefab
	r = acceptReceipt(r, q, receipt)
	r.ReceiptCheck = "found"
	if err := s.store.save(rec, r); err != nil {
		return Result{}, err
	}
	return r, nil
}

func (s *Service) snapshot(ctx context.Context, q Request) (Result, bool) {
	r := base(q)
	r.Source = "snapshot"
	r.TransportOutcome = "not_required"
	r.ExecutionOutcome = "not_required"
	r.ResultState = "output"
	var data any
	var at time.Time
	switch q.QueryID {
	case "world_info":
		v, err := s.runtime.ReadCurrentWorldState(ctx, q.RoomID, q.WorldID)
		if err != nil || v.Runtime.State != "running" || !v.Snapshot.Complete {
			return r, false
		}
		at = v.Snapshot.CapturedAt
		var rain, snow *bool
		if v.Snapshot.Precipitation != "" {
			raining, snowing := v.Snapshot.Precipitation == "rain", v.Snapshot.Precipitation == "snow"
			rain, snow = &raining, &snowing
		}
		season, phase := v.Snapshot.Season, v.Snapshot.Phase
		if len(season) > 64 || len(phase) > 64 {
			return r, false
		}
		data = worldData{v.Snapshot.Cycles, &season, &phase, v.Snapshot.Temperature, rain, snow}
	case "online_players":
		v, err := s.runtime.ReadPlayers(ctx, q.RoomID, q.WorldID)
		if err != nil || !v.Complete || !v.LocalityKnown || len(v.Players) > 128 {
			return r, false
		}
		at = v.CapturedAt
		items := []player{}
		seen := map[string]bool{}
		for _, p := range v.Players {
			if p.Name == "[Host]" && p.Prefab == "" && p.NetID == "" || p.ID == "" || seen[p.ID] {
				continue
			}
			if p.LocalEntity == nil {
				return r, false
			}
			if !*p.LocalEntity {
				continue
			}
			if len(items) >= 64 || len(p.Name) > 192 || len(p.Prefab) > 80 || len(p.ID) > 80 {
				return r, false
			}
			seen[p.ID] = true
			items = append(items, player{p.ID, p.Name, p.Prefab})
		}
		data = playerData{items, len(items)}
	default:
		return r, false
	}
	age := s.now().Sub(at)
	if at.IsZero() || age < 0 || age > 10*time.Second {
		return r, false
	}
	b, err := json.Marshal(data)
	if err != nil || len(b) > 7000 {
		return r, false
	}
	r.Data = b
	r.ObservedAt = &at
	r.Status = "succeeded"
	r.Complete = true
	return r, true
}
