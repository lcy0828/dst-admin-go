package roomops

import (
	"context"
	"sort"
	"strings"
	"sync"
)

type leaseContextKey struct{}

type leaseMarker map[string]struct{}

type lockEntry struct {
	token chan struct{}
	refs  int
}

type coordinator struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

var shared = coordinator{locks: make(map[string]*lockEntry)}

type operationLocker struct {
	roomID  string
	mu      sync.Mutex
	release func()
}

func NewLocker(roomID string) sync.Locker {
	return &operationLocker{roomID: strings.TrimSpace(roomID)}
}

func (l *operationLocker) Lock() {
	l.mu.Lock()
	_, release, err := Acquire(context.Background(), l.roomID)
	if err != nil {
		l.mu.Unlock()
		panic(err)
	}
	l.release = release
}

func (l *operationLocker) Unlock() {
	l.release()
	l.release = nil
	l.mu.Unlock()
}

// Acquire serializes filesystem and runtime mutations for one local room.
// The returned context makes acquisition reentrant for nested operations such
// as an import creating its protection backup while holding the room lease.
func Acquire(ctx context.Context, roomID string) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	roomID = strings.TrimSpace(roomID)
	marker, _ := ctx.Value(leaseContextKey{}).(leaseMarker)
	if _, ok := marker[roomID]; ok {
		return ctx, func() {}, nil
	}

	shared.mu.Lock()
	entry := shared.locks[roomID]
	if entry == nil {
		entry = &lockEntry{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		shared.locks[roomID] = entry
	}
	entry.refs++
	shared.mu.Unlock()

	select {
	case <-ctx.Done():
		shared.releaseReference(roomID, entry)
		return ctx, nil, ctx.Err()
	case <-entry.token:
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			entry.token <- struct{}{}
			shared.releaseReference(roomID, entry)
		})
	}
	nextMarker := make(leaseMarker, len(marker)+1)
	for leasedRoomID := range marker {
		nextMarker[leasedRoomID] = struct{}{}
	}
	nextMarker[roomID] = struct{}{}
	return context.WithValue(ctx, leaseContextKey{}, nextMarker), release, nil
}

// AcquireMany takes room leases in stable order so multi-room operations do
// not deadlock with another multi-room operation requesting the same rooms.
func AcquireMany(ctx context.Context, roomIDs []string) (context.Context, func(), error) {
	unique := make(map[string]struct{}, len(roomIDs))
	ordered := make([]string, 0, len(roomIDs))
	for _, roomID := range roomIDs {
		roomID = strings.TrimSpace(roomID)
		if _, exists := unique[roomID]; exists {
			continue
		}
		unique[roomID] = struct{}{}
		ordered = append(ordered, roomID)
	}
	sort.Strings(ordered)
	releases := make([]func(), 0, len(ordered))
	for _, roomID := range ordered {
		var release func()
		var err error
		ctx, release, err = Acquire(ctx, roomID)
		if err != nil {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
			return ctx, nil, err
		}
		releases = append(releases, release)
	}
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
		})
	}, nil
}

func (c *coordinator) releaseReference(roomID string, entry *lockEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.refs--
	if entry.refs == 0 && c.locks[roomID] == entry {
		delete(c.locks, roomID)
	}
}
