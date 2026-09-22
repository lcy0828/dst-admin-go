package entitycatalog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"dont/internal/dstruntime"
)

var ErrRuntimeCatalogChanged = errors.New("world entity catalog changed during collection")

type snapshotFlight struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	waiters int
	result  RuntimeSearchResult
	err     error
}
type cachedSnapshot struct {
	result  RuntimeSearchResult
	expires time.Time
}

// SnapshotCache shares a bounded, demand-driven read across workbench instances.
// It uses bounded batches over the existing protocol, including on older Agents. Only
// two worlds can be collected/cached; unused caches expire without a poller.
type SnapshotCache struct {
	mu        sync.Mutex
	commander RuntimeCommander
	flights   map[string]*snapshotFlight
	cache     map[string]cachedSnapshot
}

func NewSnapshotCache(commander RuntimeCommander) *SnapshotCache {
	return &SnapshotCache{commander: commander, flights: map[string]*snapshotFlight{}, cache: map[string]cachedSnapshot{}}
}

func (s *SnapshotCache) Read(ctx context.Context, room, world string, refresh bool) (RuntimeSearchResult, error) {
	key := room + "\x00" + world
	// A canceled caller can leave a command unwinding on the target world.
	// Join its completion before replacing the flight; otherwise quick navigation
	// starts an overlapping read which the Runtime correctly rejects as busy.
	for {
		s.mu.Lock()
		for key, value := range s.cache {
			if time.Now().After(value.expires) {
				delete(s.cache, key)
			}
		}
		flight := s.flights[key]
		if flight != nil && flight.ctx.Err() != nil {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return RuntimeSearchResult{}, ctx.Err()
			case <-flight.done:
				continue
			}
		}
		if flight == nil {
			if cached, ok := s.cache[key]; ok && !refresh {
				s.mu.Unlock()
				return cached.result, nil
			}
			if len(s.flights) >= 2 {
				s.mu.Unlock()
				return RuntimeSearchResult{}, ErrRuntimeCatalogLimit
			}
			delete(s.cache, key)
			workCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			flight = &snapshotFlight{ctx: workCtx, cancel: cancel, done: make(chan struct{})}
			s.flights[key] = flight
			go s.collect(key, room, world, refresh, flight)
		}
		flight.waiters++
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			flight.waiters--
			if flight.waiters == 0 {
				flight.cancel()
			}
		}()
		select {
		case <-ctx.Done():
			return RuntimeSearchResult{}, ctx.Err()
		case <-flight.done:
			return flight.result, flight.err
		}
	}
}

func (s *SnapshotCache) collect(key, room, world string, refresh bool, flight *snapshotFlight) {
	result, err := collectSnapshot(flight.ctx, s.commander, room, world, refresh)
	flight.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.flights[key] == flight
	if current {
		delete(s.flights, key)
	}
	if err == nil && current {
		if len(s.cache) >= 2 {
			oldest := ""
			for k, value := range s.cache {
				if oldest == "" || value.expires.Before(s.cache[oldest].expires) {
					oldest = k
				}
			}
			delete(s.cache, oldest)
		}
		s.cache[key] = cachedSnapshot{result: result, expires: time.Now().Add(5 * time.Minute)}
	}
	flight.result, flight.err = result, err
	close(flight.done)
}

func collectSnapshot(ctx context.Context, commander RuntimeCommander, room, world string, refresh bool) (RuntimeSearchResult, error) {
	result := RuntimeSearchResult{Items: []Entity{}, Mods: []RuntimeMod{}}
	seen := map[string]bool{}
	bytes := 0
	retries := 0
	batch := true
	for offset := 0; ; {
		if err := ctx.Err(); err != nil {
			return RuntimeSearchResult{}, err
		}
		limit := 120
		if batch {
			limit = snapshotPageLimit
		}
		page, err := searchRuntimePage(ctx, commander, room, world, RuntimeSearchOptions{Limit: limit, Offset: offset, Refresh: refresh && offset == 0}, batch)
		if offset == 0 && batch && errors.Is(err, errSnapshotBatchUnsupported) {
			// Only explicit rejection negotiates the smaller legacy page size.
			// Network, placement and execution failures never change the target.
			batch = false
			continue
		}
		if err != nil {
			// Only read-only catalog pages are retried. Never replay user commands.
			if retries < 3 && (errors.Is(err, dstruntime.ErrRuntimeCommandBusy) || errors.Is(err, dstruntime.ErrRuntimeResultStale)) {
				delay := time.NewTimer(time.Duration(1<<retries) * 200 * time.Millisecond)
				retries++
				select {
				case <-ctx.Done():
					delay.Stop()
					return RuntimeSearchResult{}, ctx.Err()
				case <-delay.C:
					continue
				}
			}
			return RuntimeSearchResult{}, fmt.Errorf("catalog page offset %d: %w", offset, err)
		}
		if page.Total > 50000 || len(page.Mods) > 500 {
			return RuntimeSearchResult{}, ErrRuntimeCatalogLimit
		}
		if offset == 0 {
			result.Total, result.Mods, result.ObservedAt = page.Total, page.Mods, page.ObservedAt
		}
		if page.Total != result.Total || !page.ObservedAt.Equal(result.ObservedAt) {
			return RuntimeSearchResult{}, ErrRuntimeCatalogChanged
		}
		if page.HasMore && len(page.Items) == 0 {
			return RuntimeSearchResult{}, ErrRuntimeCatalogUnavailable
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				return RuntimeSearchResult{}, ErrRuntimeCatalogUnavailable
			}
			seen[item.ID] = true
			bytes += len(item.ID) + len(item.NameZhCN) + len(item.NameEn) + len(item.ModID) + len(item.ModName) + 128
			if bytes > 8<<20 {
				return RuntimeSearchResult{}, ErrRuntimeCatalogLimit
			}
		}
		result.Items = append(result.Items, page.Items...)
		offset += len(page.Items)
		if !page.HasMore {
			if offset != result.Total {
				return RuntimeSearchResult{}, ErrRuntimeCatalogUnavailable
			}
			return result, nil
		}
		if offset >= result.Total {
			return RuntimeSearchResult{}, ErrRuntimeCatalogUnavailable
		}
	}
}
