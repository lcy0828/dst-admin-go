package worldquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type testRooms struct{}

func (testRooms) Room(id string) (rooms.Room, error) { return rooms.Room{ID: id}, nil }
func (testRooms) World(room, id string) (rooms.World, error) {
	return rooms.World{ID: id, RoomID: room}, nil
}

type testRuntime struct {
	mu           sync.Mutex
	identity     string
	calls, reads int
	fail         error
	output       string
	onExecute    func()
	last         dstruntime.WorldQueryRequest
	now          time.Time
	snapshot     dstruntime.CurrentWorldState
}

func (f *testRuntime) QueryIdentity(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identity, nil
}
func (f *testRuntime) ExecuteWorldQuery(_ context.Context, _, _ string, q dstruntime.WorldQueryRequest) (dstruntime.CommandReceipt, error) {
	f.mu.Lock()
	f.calls++
	f.last = q
	hook := f.onExecute
	err := f.fail
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return f.receipt(q.RequestID), err
}
func (f *testRuntime) receipt(id string) dstruntime.CommandReceipt {
	f.mu.Lock()
	defer f.mu.Unlock()
	output := f.output
	if output == "" {
		output = fmt.Sprintf(`{"protocolVersion":1,"runId":%q,"queryId":"entity_count","complete":true,"data":{"prefab":"spiderden","count":0},"scannedEntities":12}`, id)
	}
	return dstruntime.CommandReceipt{RequestID: id, Action: "console.execute", OK: true, CompletedAt: f.now, Details: map[string]interface{}{"output": map[string]interface{}{"text": output, "truncated": false}}}
}
func (f *testRuntime) ReadCommandReceipt(_ context.Context, _, _ string, q dstruntime.CommandRequest, _ time.Time) (dstruntime.CommandReceipt, error) {
	f.mu.Lock()
	f.reads++
	f.mu.Unlock()
	return f.receipt(q.RequestID), nil
}
func (f *testRuntime) ReadCurrentWorldState(context.Context, string, string) (dstruntime.CurrentWorldState, error) {
	if f.snapshot.Runtime.State != "" {
		return f.snapshot, nil
	}
	return dstruntime.CurrentWorldState{}, errors.New("missing")
}
func (f *testRuntime) ReadPlayers(context.Context, string, string) (dstruntime.Snapshot, error) {
	return dstruntime.Snapshot{}, errors.New("missing")
}
func fixture(t *testing.T) (*Service, *testRuntime, Principal, Request) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	db.LogMode(false)
	t.Cleanup(func() { db.Close() })
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runtime := &testRuntime{identity: "instance-a", now: now}
	s := New(testRooms{}, runtime, store)
	s.now = func() time.Time { return now }
	return s, runtime, Principal{ID: "test-token"}, Request{RoomID: rooms.EncodeID("Example"), WorldID: rooms.EncodeID("Master"), QueryID: "entity_count", Prefab: "spiderden"}
}
func TestQueriesValidateBeforeDispatch(t *testing.T) {
	for _, change := range []func(*Request){
		func(q *Request) { q.QueryID = "save_world" }, func(q *Request) { q.QueryID = "custom" }, func(q *Request) { q.Prefab = "" },
		func(q *Request) { q.Prefab = `spiderden\");SpawnPrefab("beefalo")` }, func(q *Request) { q.Prefab = "spiderden\n" },
		func(q *Request) { q.Prefab = string(make([]byte, 81)) }, func(q *Request) { q.QueryID = "world_info" }, func(q *Request) { q.RoomID = "../../private" },
	} {
		s, f, p, q := fixture(t)
		change(&q)
		if _, err := s.Query(context.Background(), p, q); !errors.Is(err, ErrInvalid) {
			t.Fatal(q, err)
		}
		if f.calls != 0 {
			t.Fatal("invalid query dispatched")
		}
	}
}
func TestCompleteZeroCacheAndPerShardScanLimit(t *testing.T) {
	s, f, p, q := fixture(t)
	r, err := s.Query(context.Background(), p, q)
	if err != nil || r.Status != "succeeded" || !r.Complete || r.ExecutionOutcome != "confirmed" {
		t.Fatalf("%+v %v", r, err)
	}
	var data entityData
	if json.Unmarshal(r.Data, &data) != nil || data.Count == nil || *data.Count != 0 {
		t.Fatal(string(r.Data))
	}
	second, err := s.Query(context.Background(), p, q)
	if err != nil || !second.Cached || f.calls != 1 || !second.ObservedAt.Equal(*r.ObservedAt) {
		t.Fatal(second, err, f.calls)
	}
	q.Prefab = "ModPrefab_2"
	if _, err := s.Query(context.Background(), p, q); !errors.Is(err, ErrLimited) {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatal("changed prefab bypassed scan limit")
	}
	if len(s.cache) != 1 {
		t.Fatal("invalid query populated cache")
	}
}
func TestUncertainNeverReplaysEvenAcrossRestartAndOtherPrefab(t *testing.T) {
	s, f, p, q := fixture(t)
	f.fail = dstruntime.ErrRuntimeCommandUnconfirmed
	r, err := s.Query(context.Background(), p, q)
	if err != nil || r.Status != "uncertain" || !r.MayHaveExecuted || len(r.Data) != 0 {
		t.Fatal(r, err)
	}
	restarted := New(testRooms{}, f, s.store)
	restarted.now = s.now
	changed := q
	changed.Prefab = "beefalo"
	got, err := restarted.Query(context.Background(), p, changed)
	if err != nil || got.QueryRunID != r.QueryRunID || f.calls != 1 {
		t.Fatal(got, err, f.calls)
	}
	got, err = restarted.Get(context.Background(), p, q.RoomID, q.WorldID, r.QueryRunID, true)
	if err != nil || got.Status != "succeeded" || got.ReceiptCheck != "found" || f.calls != 1 || f.reads != 1 {
		t.Fatal(got, err, f.calls, f.reads)
	}
	if _, err := restarted.Get(context.Background(), Principal{ID: "other-token"}, q.RoomID, q.WorldID, r.QueryRunID, true); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign query disclosed", err)
	}
}
func TestScopeAndMissingAdministratorRun(t *testing.T) {
	s, f, p, q := fixture(t)
	p.RoomID = rooms.EncodeID("Other")
	if _, err := s.Query(context.Background(), p, q); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	p.RoomID = ""
	if _, err := s.Get(context.Background(), p, q.RoomID, q.WorldID, "00000000-0000-4000-8000-000000000001", true); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if f.calls != 0 || f.reads != 0 {
		t.Fatal("history lookup dispatched/read administrator command")
	}
}
func TestFreshSnapshotDoesNotExecuteAndKeepsObservationTime(t *testing.T) {
	s, f, p, q := fixture(t)
	q.QueryID = "world_info"
	q.Prefab = ""
	at := f.now.Add(-3 * time.Second)
	f.snapshot = dstruntime.CurrentWorldState{Snapshot: dstruntime.WorldStateSnapshot{Complete: true, CapturedAt: at, Season: "autumn", Phase: "day"}}
	f.snapshot.Runtime.State = "running"
	r, err := s.Query(context.Background(), p, q)
	if err != nil || r.Source != "snapshot" || r.ExecutionOutcome != "not_required" || !r.ObservedAt.Equal(at) || f.calls != 0 {
		t.Fatal(r, err)
	}
}
func TestRequestAndCacheCapacityAreBounded(t *testing.T) {
	s, _, p, q := fixture(t)
	for i := 0; i < 5; i++ {
		if _, err := s.Query(context.Background(), p, q); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := s.Query(context.Background(), p, q); !errors.Is(err, ErrLimited) {
		t.Fatal("cache bypassed rate limit", err)
	}
	if len(s.cache) > 128 || s.cacheBytes > 2<<20 {
		t.Fatal("cache unbounded")
	}
}
func TestSingleflightDoesNotCreateSecondDispatch(t *testing.T) {
	s, f, p, q := fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.onExecute = func() { close(entered); <-release }
	type answer struct {
		r   Result
		err error
	}
	out := make(chan answer, 2)
	go func() { r, e := s.Query(context.Background(), p, q); out <- answer{r, e} }()
	<-entered
	started := make(chan struct{})
	go func() { close(started); r, e := s.Query(context.Background(), p, q); out <- answer{r, e} }()
	<-started
	close(release)
	a, b := <-out, <-out
	if a.err != nil || b.err != nil || a.r.QueryRunID != b.r.QueryRunID || f.calls != 1 {
		t.Fatal(a, b, f.calls)
	}
}
func TestReceiptInterpretation(t *testing.T) {
	s, f, _, q := fixture(t)
	_ = s
	r := base(q)
	r.QueryRunID = "query-example"
	for _, tc := range []struct {
		name, text, status string
		truncated          bool
	}{
		{"empty", "\n", "unavailable", false},
		{"zero", `{"protocolVersion":1,"runId":"query-example","queryId":"entity_count","complete":true,"data":{"prefab":"spiderden","count":0},"scannedEntities":0}`, "succeeded", false},
		{"partial", `{"protocolVersion":1,"runId":"query-example","queryId":"entity_count","complete":false,"reason":"CPU_BUDGET","data":{"prefab":"spiderden","lowerBound":4},"scannedEntities":10}`, "partial", false},
		{"missing count", `{"protocolVersion":1,"runId":"query-example","queryId":"entity_count","complete":true,"data":{"prefab":"spiderden"},"scannedEntities":10}`, "unavailable", false},
		{"wrong request", `{"protocolVersion":1,"runId":"other","queryId":"entity_count","complete":true,"data":{"prefab":"spiderden","count":0},"scannedEntities":0}`, "unavailable", false},
		{"truncated", "0", "unavailable", true},
		{"unknown fields", `{"protocolVersion":1,"runId":"query-example","queryId":"entity_count","complete":true,"readOnly":true,"data":{"prefab":"spiderden","count":0},"scannedEntities":0}`, "unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt := f.receipt(r.QueryRunID)
			receipt.Details["output"] = map[string]interface{}{"text": tc.text, "truncated": tc.truncated}
			got := acceptReceipt(r, q, receipt)
			if got.Status != tc.status {
				t.Fatal(got)
			}
			if got.Status == "unavailable" && len(got.Data) != 0 {
				t.Fatal("unusable output acquired count", got)
			}
		})
	}
	failed := f.receipt(r.QueryRunID)
	failed.OK = false
	if got := acceptReceipt(r, q, failed); got.Status != "failed" || got.ExecutionOutcome != "rejected" {
		t.Fatal(got)
	}
}

func TestNewInstanceInvalidatesCachedResultAndLateReceipt(t *testing.T) {
	s, f, p, q := fixture(t)
	r, err := s.Query(context.Background(), p, q)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.identity = "instance-b"
	f.mu.Unlock()
	// An identity change cannot turn an old cached observation into live data.
	if _, err := s.Query(context.Background(), p, q); !errors.Is(err, ErrLimited) {
		t.Fatal("stale instance cache reused", err)
	}
	if f.calls != 1 {
		t.Fatal(f.calls)
	}
	// A separate uncertain record cannot attach a receipt from another boot.
	r.Status = "uncertain"
	r.Complete = false
	r.Data = nil
	rec, _, err := s.store.get(r.QueryRunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.save(rec, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), p, q.RoomID, q.WorldID, r.QueryRunID, true)
	if err != nil || got.Status != "uncertain" || got.ReceiptCheck != "unavailable" || f.reads != 0 {
		t.Fatal(got, err, f.reads)
	}
}

func TestExpiredUncertainReceiptIsNotReadOrReplayed(t *testing.T) {
	s, f, p, q := fixture(t)
	f.fail = dstruntime.ErrRuntimeCommandUnconfirmed
	r, err := s.Query(context.Background(), p, q)
	if err != nil {
		t.Fatal(err)
	}
	now := f.now.Add(3 * time.Minute)
	s.now = func() time.Time { return now }
	got, err := s.Get(context.Background(), p, q.RoomID, q.WorldID, r.QueryRunID, true)
	if err != nil || got.Status != "uncertain" || got.ReceiptCheck != "expired" || f.reads != 0 || f.calls != 1 {
		t.Fatal(got, err)
	}
}

func TestSnapshotCacheNeverExtendsFreshnessLimit(t *testing.T) {
	s, f, p, q := fixture(t)
	q.QueryID = "world_info"
	q.Prefab = ""
	at := f.now.Add(-9 * time.Second)
	f.snapshot = dstruntime.CurrentWorldState{Snapshot: dstruntime.WorldStateSnapshot{Complete: true, CapturedAt: at, Season: "autumn", Phase: "day"}}
	f.snapshot.Runtime.State = "running"
	if r, err := s.Query(context.Background(), p, q); err != nil || r.Source != "snapshot" {
		t.Fatal(r, err)
	}
	now := f.now.Add(2 * time.Second)
	s.now = func() time.Time { return now }
	r, err := s.Query(context.Background(), p, q)
	if err != nil || r.Cached || r.Source == "snapshot" || f.calls != 1 {
		t.Fatal("old snapshot remained live through cache", r, err, f.calls)
	}
}

func TestGlobalConcurrencyRejectsInsteadOfQueueing(t *testing.T) {
	s, f, p, q := fixture(t)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	f.onExecute = func() { entered <- struct{}{}; <-release }
	answers := make(chan error, 2)
	for _, world := range []string{"Master", "Caves"} {
		q := q
		q.WorldID = rooms.EncodeID(world)
		go func() { _, err := s.Query(context.Background(), p, q); answers <- err }()
	}
	<-entered
	<-entered
	third := q
	third.WorldID = rooms.EncodeID("Extra")
	if _, err := s.Query(context.Background(), p, third); !errors.Is(err, ErrBusy) {
		t.Fatal("third request queued", err)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-answers; err != nil {
			t.Fatal(err)
		}
	}
	if f.calls != 2 || len(s.slots) != 0 {
		t.Fatal("global slots leaked", f.calls, len(s.slots))
	}
}
