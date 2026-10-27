package moddiagnostics

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dont/internal/logstream"
	"dont/internal/mods"
	"dont/internal/rooms"
	"dont/internal/runtimeaudit"
	"dont/internal/runtimedriver"
	"dont/shared"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type fakeCatalog struct{}

func (fakeCatalog) Room(id string) (rooms.Room, error) { return rooms.Room{ID: id}, nil }
func (fakeCatalog) World(room, world string) (rooms.World, error) {
	if room != "room" || world != "Master" && world != "Caves" {
		return rooms.World{}, rooms.ErrWorldNotFound
	}
	return rooms.World{ID: world, RoomID: room, Name: world}, nil
}

type fakeMods struct {
	calls atomic.Int32
	reads atomic.Int32
	wait  <-chan struct{}
	items []mods.ModState
	err   error
}

func (m *fakeMods) RoomCompatibilityFacts(ctx context.Context, _ string) (mods.ModList, error) {
	m.calls.Add(1)
	if m.wait != nil {
		select {
		case <-m.wait:
		case <-ctx.Done():
			return mods.ModList{}, ctx.Err()
		}
	}
	return mods.ModList{Items: m.items}, m.err
}
func (m *fakeMods) ConfigurationFile(context.Context, string, string) (mods.ConfigurationFile, error) {
	m.reads.Add(1)
	return mods.ConfigurationFile{Revision: "hash-only", Content: "token=SHOULD_NEVER_LEAVE_SOURCE"}, nil
}

type fakeLogs struct{ calls atomic.Int32 }

func (l *fakeLogs) SnapshotContext(context.Context, string, string, int, string) (logstream.Snapshot, error) {
	l.calls.Add(1)
	return logstream.Snapshot{Lines: []logstream.Line{}}, nil
}

type fakeEvents struct{}

func (fakeEvents) List(string, runtimeaudit.ListFilter) (runtimeaudit.List, error) {
	return runtimeaudit.List{}, nil
}

// Embedding the unused Driver makes accidental writes fail loudly. Only the
// status read is implemented by this diagnostic-only Runtime.
type fakeDriver struct{ runtimedriver.Driver }

func (fakeDriver) Status(context.Context, runtimedriver.Target) (shared.ShardRuntimeStatus, error) {
	return shared.ShardRuntimeStatus{State: "failed"}, nil
}

type fakeRouter struct{ err error }

func (r fakeRouter) EndpointTarget(_ context.Context, room, world string) (runtimedriver.RuntimeEndpoint, runtimedriver.Target, error) {
	return runtimedriver.RuntimeEndpoint{Driver: fakeDriver{}}, runtimedriver.Target{TargetID: "agent:remote", InstallationID: "remote-install", RoomID: room, WorldID: world}, r.err
}

type movedRouter struct{ calls atomic.Int32 }

func (r *movedRouter) EndpointTarget(ctx context.Context, room, world string) (runtimedriver.RuntimeEndpoint, runtimedriver.Target, error) {
	e, t, err := (fakeRouter{}).EndpointTarget(ctx, room, world)
	if r.calls.Add(1) > 1 {
		t.InstallationID = "different-installation"
	}
	return e, t, err
}

func TestPlacementChangeDiscardsMixedFileAndLogEvidence(t *testing.T) {
	s, m, _ := fixtureSource()
	s.Router = &movedRouter{}
	facts, _ := m.RoomCompatibilityFacts(context.Background(), "room")
	in, err := s.Collect(context.Background(), Scope{RoomID: "room", WorldID: "Master"}, &facts, nil, true)
	if err != nil || !slices.Contains(in.Observation.Limitations, "PLACEMENT_CHANGED") || len(in.Observation.Mods) > 0 || in.Logs != nil || in.Observation.ConfigurationRevision != "" {
		t.Fatalf("mixed placement evidence leaked: %+v %v", in, err)
	}
}

type memoryBaseline struct {
	mu sync.Mutex
	b  *Baseline
}

func (m *memoryBaseline) Latest(string, string) (*Baseline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b, nil
}
func (m *memoryBaseline) SaveOnce(b Baseline) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.b == nil {
		m.b = &b
	}
	return nil
}
func fixtureSource() (*RuntimeSource, *fakeMods, *fakeLogs) {
	m := &fakeMods{items: []mods.ModState{{SteamMod: mods.SteamMod{ID: "1", Name: "mod", Version: "LATEST-NOT-INSTALLED"}, EnabledWorlds: []string{"Master", "Caves"}, RuntimeVersions: []mods.RuntimeModVersionTarget{{TargetID: "local", InstallationID: "default", Version: "WRONG"}, {TargetID: "agent:remote", InstallationID: "remote-install", Version: "2", FileStatus: "ready"}}}}}
	l := &fakeLogs{}
	return &RuntimeSource{Catalog: fakeCatalog{}, Mods: m, Logs: l, Events: fakeEvents{}, Router: fakeRouter{}}, m, l
}

func TestSourceUsesExactPlacementAndNeverFallsBackOffline(t *testing.T) {
	s, m, l := fixtureSource()
	facts, _ := m.RoomCompatibilityFacts(context.Background(), "room")
	in, err := s.Collect(context.Background(), Scope{RoomID: "room", WorldID: "Master"}, &facts, nil, true)
	if err != nil || len(in.Observation.Mods) != 1 || in.Observation.Mods[0].Version != "2" || in.Observation.Mods[0].FileStatus != "present" {
		t.Fatalf("input=%+v err=%v", in, err)
	}
	s.Router = fakeRouter{err: errors.New("Agent disconnected")}
	in, err = s.Collect(context.Background(), Scope{RoomID: "room", WorldID: "Master"}, &facts, nil, true)
	if err != nil || !slices.Contains(in.Observation.Limitations, "RUNTIME_UNAVAILABLE") || m.reads.Load() != 1 || l.calls.Load() != 1 {
		t.Fatal("offline target fell back to local files", in, err)
	}
	in, err = s.Collect(context.Background(), Scope{RoomID: "room", WorldID: "unknown"}, &facts, nil, true)
	if !errors.Is(err, rooms.ErrWorldNotFound) {
		t.Fatal(in, err)
	}
}

func TestPreflightReadsRoomFactsOnceAndDoesNotReadLogs(t *testing.T) {
	s, m, l := fixtureSource()
	service := NewService(s, &memoryBaseline{})
	r := service.Preflight(context.Background(), []Scope{{RoomID: "room", WorldID: "Master"}, {RoomID: "room", WorldID: "Caves"}})
	if len(r) != 2 || m.calls.Load() != 1 || l.calls.Load() != 0 || m.reads.Load() != 2 {
		t.Fatal("preflight work expanded", r, m.calls.Load(), l.calls.Load())
	}
	for _, item := range r {
		if item.LogCorrelation != "not_requested" {
			t.Fatal(item)
		}
	}
}

func TestConcurrentAnalysisCoalescesAndInvalidWorldDoesNoIO(t *testing.T) {
	s, m, _ := fixtureSource()
	gate := make(chan struct{})
	m.wait = gate
	service := NewService(s, &memoryBaseline{})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := service.Analyze(ctx, "room", "Master"); first <- err }()
	deadline := time.After(time.Second)
	for m.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("read never began")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	second := make(chan error, 1)
	go func() { _, err := service.Analyze(context.Background(), "room", "Master"); second <- err }()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Allow the second caller to join the in-flight read before releasing it.
	time.Sleep(10 * time.Millisecond)
	close(gate)
	if err := <-second; err != nil || m.calls.Load() != 1 {
		t.Fatal(err, m.calls.Load())
	}
	_, err := service.Analyze(context.Background(), "room", "unknown")
	if !errors.Is(err, rooms.ErrWorldNotFound) || m.calls.Load() != 1 {
		t.Fatal("invalid scope read runtime", err)
	}
}

func TestBaselineIsImmutableBoundedAndPersistsAcrossStoreRestart(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		b := Baseline{ReleaseID: fmt.Sprintf("release-%d", i), CapturedAt: time.Now().UTC(), Observation: Observation{RoomID: "room", WorldID: "Master", ConfigurationRevision: "original"}}
		if err := store.SaveOnce(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveOnce(Baseline{ReleaseID: "release-7", Observation: Observation{RoomID: "room", WorldID: "Master", ConfigurationRevision: "WRONG-AFTER-UPDATE"}}); err != nil {
		t.Fatal(err)
	}
	got, err := NewStore(db, "test_").Latest("room", "Master")
	if err != nil || got.Observation.ConfigurationRevision != "original" || got.ReleaseID != "release-7" {
		t.Fatal(got, err)
	}
	var count int
	store.db.Table(store.table).Count(&count)
	if count != 5 {
		t.Fatal("unbounded baseline history", count)
	}
}

func TestOfflineAnalysisDoesNotInspectOtherRoomPlacements(t *testing.T) {
	s, m, l := fixtureSource()
	s.Router = fakeRouter{err: errors.New("offline")}
	service := NewService(s, &memoryBaseline{})
	report, err := service.Analyze(context.Background(), "room", "Master")
	if err != nil || m.calls.Load() != 0 || l.calls.Load() != 0 || report.Status != "insufficient_evidence" {
		t.Fatal("offline analysis expanded into other Runtime reads", err, m.calls.Load(), l.calls.Load())
	}
}
