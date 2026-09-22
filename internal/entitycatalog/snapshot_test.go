package entitycatalog

import (
	"context"
	"dont/internal/dstruntime"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type snapshotCommander struct {
	mu                  sync.Mutex
	calls               int
	total               int
	pageSize            int
	mismatch, duplicate bool
	gate                <-chan struct{}
	entered             chan struct{}
	err                 error
}

func (c *snapshotCommander) ExecuteCommand(ctx context.Context, room, world string, request dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if c.entered != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
	}
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return dstruntime.CommandReceipt{}, ctx.Err()
		}
	}
	if c.err != nil {
		return dstruntime.CommandReceipt{}, c.err
	}
	offset := request.Arguments["offset"].(int)
	limit := request.Arguments["limit"].(int)
	pageSize := c.pageSize
	if pageSize == 0 {
		pageSize = 120 // Simulate a page shortened by the encoded-byte budget.
	}
	if limit > pageSize {
		limit = pageSize
	}
	items := []Entity{}
	for i := offset; i < c.total && i < offset+limit; i++ {
		id := i
		if c.duplicate && offset > 0 {
			id = 0
		}
		items = append(items, Entity{ID: fmt.Sprintf("prefab_%d", id), NameEn: world})
	}
	observed := "2026-09-22T00:00:00Z"
	if c.mismatch && call > 1 {
		observed = "2026-09-22T00:00:01Z"
	}
	return dstruntime.CommandReceipt{OK: true, Details: map[string]interface{}{"items": items, "mods": []RuntimeMod{}, "total": c.total, "offset": offset, "hasMore": offset+len(items) < c.total, "observedAt": observed}}, nil
}
func TestCompleteSnapshotCachesAllPagesAndRefreshes(t *testing.T) {
	cmd := &snapshotCommander{total: 241}
	cache := NewSnapshotCache(cmd)
	first, err := cache.Read(context.Background(), "remote", "caves", false)
	if err != nil || len(first.Items) != 241 || first.HasMore || cmd.calls != 3 {
		t.Fatalf("%d %v %d", len(first.Items), err, cmd.calls)
	}
	second, err := cache.Read(context.Background(), "remote", "caves", false)
	if err != nil || len(second.Items) != 241 || cmd.calls != 3 {
		t.Fatal("cache missed")
	}
	if _, err = cache.Read(context.Background(), "remote", "caves", true); err != nil || cmd.calls != 6 {
		t.Fatal("refresh missed", err)
	}
	if _, err = cache.Read(context.Background(), "remote", "master", false); err != nil || cmd.calls != 9 {
		t.Fatal("world scope lost")
	}
	if _, err = cache.Read(context.Background(), "other", "master", false); err != nil || len(cache.cache) != 2 {
		t.Fatal("unbounded cache")
	}
}
func TestSnapshotRejectsPartialOrChangedCollectionsAndNeverFallsBack(t *testing.T) {
	for _, cmd := range []*snapshotCommander{{total: 241, mismatch: true}, {total: 241, duplicate: true}, {total: 50001}, {err: errors.New("Agent offline")}} {
		cache := NewSnapshotCache(cmd)
		if _, err := cache.Read(context.Background(), "remote", "world", false); err == nil {
			t.Fatal("bad snapshot accepted")
		}
		if len(cache.cache) != 0 {
			t.Fatal("partial data cached")
		}
	}
}
func TestSnapshotCoalescesWaitersAndCancelsWhenUnused(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 2)
	cmd := &snapshotCommander{total: 1, gate: gate, entered: entered}
	cache := NewSnapshotCache(cmd)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, err := cache.Read(ctx, "r", "w", false); first <- err }()
	<-entered
	go func() { _, err := cache.Read(context.Background(), "r", "w", false); second <- err }()
	deadline := time.After(time.Second)
	for {
		cache.mu.Lock()
		waiters := cache.flights["r\x00w"].waiters
		cache.mu.Unlock()
		if waiters == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second waiter not attached")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if !errors.Is(<-first, context.Canceled) {
		t.Fatal("caller cancellation lost")
	}
	close(gate)
	if err := <-second; err != nil {
		t.Fatal("one canceled caller aborted another", err)
	}
	cmd.mu.Lock()
	defer cmd.mu.Unlock()
	if cmd.calls != 1 {
		t.Fatal("requests not coalesced", cmd.calls)
	}
}
func TestEmbeddedDirectoryHasRealCategoriesAndNoDuplicates(t *testing.T) {
	d := Directory()
	if len(d.Items) != 6121 || d.GameVersion != "747465" {
		t.Fatal(len(d.Items), d.GameVersion)
	}
	found := map[string]Entity{}
	for _, item := range d.Items {
		if !prefabPattern.MatchString(item.ID) || found[item.ID].ID != "" {
			t.Fatal(item.ID)
		}
		found[item.ID] = item
	}
	for id, category := range map[string]string{"log": "item", "spear": "equipment", "armorwood": "equipment", "firepit": "structure", "beefalo": "creature", "deerclops": "boss", "berrybush": "nature", "meatballs": "food"} {
		if found[id].Category != category {
			t.Fatal(id, found[id].Category)
		}
	}
}

type snapshotCommanderFunc func(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error)

func (f snapshotCommanderFunc) ExecuteCommand(ctx context.Context, r, w string, q dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	return f(ctx, r, w, q)
}

func TestCanceledSnapshotFinishesUnwindingBeforeReplacement(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	calls := make(chan int, 4)
	count := 0
	cmd := snapshotCommanderFunc(func(ctx context.Context, r, w string, q dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
		count++
		calls <- count
		if count == 1 {
			close(entered)
			<-release
			return dstruntime.CommandReceipt{}, ctx.Err()
		}
		fixture := snapshotCommander{total: 1}
		return fixture.ExecuteCommand(ctx, r, w, q)
	})
	cache := NewSnapshotCache(cmd)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, err := cache.Read(ctx, "r", "w", false); first <- err }()
	<-entered
	<-calls
	cancel()
	<-first
	go func() { _, err := cache.Read(context.Background(), "r", "w", false); second <- err }()
	select {
	case n := <-calls:
		t.Fatalf("overlapping command before canceled read unwound: %d", n)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if n := <-calls; n != 2 {
		t.Fatal(n)
	}
}

func TestBusyCatalogPageRetriesInPlaceWithoutDiscardingReadProgress(t *testing.T) {
	offsets := []int{}
	busy := true
	fixture := &snapshotCommander{total: 241}
	cmd := snapshotCommanderFunc(func(ctx context.Context, r, w string, q dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
		offset := q.Arguments["offset"].(int)
		offsets = append(offsets, offset)
		if offset == 120 && busy {
			busy = false
			return dstruntime.CommandReceipt{}, dstruntime.ErrRuntimeCommandBusy
		}
		return fixture.ExecuteCommand(ctx, r, w, q)
	})
	result, err := NewSnapshotCache(cmd).Read(context.Background(), "r", "w", false)
	if err != nil || len(result.Items) != 241 || fmt.Sprint(offsets) != "[0 120 120 240]" {
		t.Fatal(err, len(result.Items), offsets)
	}
}

func TestSnapshotBatchesLargeDirectoriesAndNegotiatesLegacyPages(t *testing.T) {
	cmd := &snapshotCommander{total: 6121, pageSize: snapshotPageLimit}
	result, err := NewSnapshotCache(cmd).Read(context.Background(), "remote", "world", false)
	if err != nil || len(result.Items) != 6121 || cmd.calls != 3 {
		t.Fatalf("batch result=%d calls=%d err=%v", len(result.Items), cmd.calls, err)
	}
	legacy := &snapshotCommander{total: 241}
	probes := 0
	wrapped := snapshotCommanderFunc(func(ctx context.Context, r, w string, q dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
		if q.Arguments["snapshot"] == true {
			probes++
			return dstruntime.CommandReceipt{Code: "INVALID_SEARCH"}, nil
		}
		return legacy.ExecuteCommand(ctx, r, w, q)
	})
	result, err = NewSnapshotCache(wrapped).Read(context.Background(), "remote", "world", false)
	if err != nil || len(result.Items) != 241 || probes != 1 || legacy.calls != 3 {
		t.Fatalf("legacy result=%d probes=%d calls=%d err=%v", len(result.Items), probes, legacy.calls, err)
	}
}
