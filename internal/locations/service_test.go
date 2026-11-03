package locations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dont/internal/console"
	"dont/internal/players"
	"dont/internal/rooms"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type testCatalog struct{}

func (testCatalog) Room(id string) (rooms.Room, error)     { return rooms.Room{ID: id, Managed: true}, nil }
func (testCatalog) World(r, w string) (rooms.World, error) { return rooms.World{ID: w}, nil }
func (testCatalog) Resolve(r, q string, o int) (players.Resolution, error) {
	return players.Resolution{Status: "resolved", Items: []players.Player{{ID: "KU_TEST"}}}, nil
}
func (testCatalog) OnlineWorld(ctx context.Context, r, p string) (rooms.World, error) {
	return rooms.World{ID: "world"}, nil
}

type testRuntime struct {
	mu       sync.Mutex
	identity string
	sent     int
	unknown  bool
	runs     map[string]console.Run
	hook     func()
}

func (f *testRuntime) LocationIdentity(context.Context, string, string) (console.LocationIdentity, error) {
	return console.LocationIdentity{SessionID: "session", Instance: f.identity}, nil
}
func (f *testRuntime) ExecuteLocation(ctx context.Context, r, w, id string, a map[string]any) (console.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	phase := a["phase"].(string)
	if phase == "execute" {
		f.sent++
		if f.hook != nil {
			f.hook()
		}
	}
	v := Effect{State: "prepared", OperationID: a["operationId"].(string), PlayerID: a["playerId"].(string), PlayerName: "Example", SessionID: "session", Point: &Point{12, 24}, ExpiresAt: time.Now().Add(60 * time.Second).Unix(), ObservedAt: time.Now().Unix()}
	if phase == "capture" {
		v.State = "captured"
	}
	if phase == "execute" {
		v.State = "confirmed"
		v.Before = &Point{1, 2}
		v.After = &Point{12, 24}
	}
	b, _ := json.Marshal(v)
	run := console.Run{ID: id, RoomID: r, WorldID: w, Mode: "location", Status: console.RunSucceeded, ExecutionOutcome: "confirmed", Output: &console.Output{Text: "[DST-ADMIN-TELEPORT-V1] " + string(b)}}
	if f.unknown && phase == "execute" {
		run.Output = nil
		run.Status = console.RunUncertain
		run.MayHaveExecuted = true
	}
	f.runs[id] = run
	return run, nil
}
func (f *testRuntime) Run(id string) (console.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[id], nil
}
func (f *testRuntime) RefreshRun(ctx context.Context, r, id string) (console.Run, error) {
	return f.Run(id)
}
func fixture(t *testing.T) (*Service, *testRuntime) {
	t.Helper()
	db, e := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "locations.db"))
	if e != nil {
		t.Fatal(e)
	}
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	st := NewStore(db, "test_")
	if e = st.Migrate(); e != nil {
		t.Fatal(e)
	}
	rt := &testRuntime{identity: "instance", runs: map[string]console.Run{}}
	s := New(st, rt, testCatalog{})
	s.Configure(testCatalog{}, func(context.Context, string, string) (string, error) { return "generation", nil })
	return s, rt
}
func pointInput() SaveInput {
	x, z := 12., 24.
	return SaveInput{Name: "基地", WorldID: "world", X: &x, Z: &z}
}
func prepared(t *testing.T, s *Service) (Location, Preview) {
	t.Helper()
	l, e := s.Save(context.Background(), "actor", "room", "", pointInput())
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.Prepare(context.Background(), "actor", "room", PrepareInput{PlayerID: "KU_TEST", LocationID: l.ID})
	if e != nil || p.State != "prepared" {
		t.Fatalf("%+v %v", p, e)
	}
	return l, p
}
func TestTeleportRenameWalkingDuplicateAndOwnership(t *testing.T) {
	s, rt := fixture(t)
	l, p := prepared(t, s)
	_, e := s.Save(context.Background(), "actor", "room", l.ID, SaveInput{Name: "新基地", Revision: l.Revision})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Execute(context.Background(), "other", "room", p.ID); e == nil {
		t.Fatal("cross-owner execute")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Execute(context.Background(), "actor", "room", p.ID)
			if e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	p, e = s.GetPreview(context.Background(), "actor", "room", p.ID, true)
	if e != nil || p.State != "confirmed" || rt.sent != 1 {
		t.Fatalf("%+v %v sent=%d", p, e, rt.sent)
	}
	if p.LocationName != "基地" {
		t.Fatal("preview did not freeze displayed name")
	}
}
func TestFinalEvidenceCannotBeDowngradedAndOrphanPrepareExpires(t *testing.T) {
	s, _ := fixture(t)
	_, p := prepared(t, s)
	p, err := s.Execute(context.Background(), "actor", "room", p.ID)
	if err != nil || p.State != "confirmed" {
		t.Fatalf("%+v %v", p, err)
	}
	p.State = "uncertain"
	p.Effect = nil
	if err = s.store.finish(p); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale refresh overwrote final evidence: %v", err)
	}
	p, err = s.GetPreview(context.Background(), "actor", "room", p.ID, true)
	if err != nil || p.State != "confirmed" || p.Effect == nil {
		t.Fatalf("%+v %v", p, err)
	}
	orphan := Preview{ID: "orphan", Actor: "actor", RoomID: "room", WorldID: "world", PlayerID: "KU_OTHER", State: "preparing", RunID: "precheck-identity", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(-time.Second)}
	if err = s.store.insertPreview(orphan); err != nil {
		t.Fatal(err)
	}
	p, err = s.GetPreview(context.Background(), "actor", "room", orphan.ID, true)
	if err != nil || p.State != "expired" || p.RunID != orphan.RunID {
		t.Fatalf("orphan not recoverable: %+v %v", p, err)
	}
}

func TestTeleportChangesAndCancellationNeverDispatch(t *testing.T) {
	for _, kind := range []string{"coordinate", "delete", "instance", "binding", "expired", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s, rt := fixture(t)
			l, p := prepared(t, s)
			switch kind {
			case "coordinate":
				l.X++
				if _, e := s.store.Save(l, l.Revision); e != nil {
					t.Fatal(e)
				}
			case "delete":
				if e := s.store.Delete("room", l.ID, l.Revision); e != nil {
					t.Fatal(e)
				}
			case "instance":
				rt.identity = "other"
			case "binding":
				s.binding = func(context.Context, string, string) (string, error) { return "restored", nil }
			case "expired":
				s.store.db.Table(s.store.previews).Where("id = ?", p.ID).Update("expires_at", time.Now().Add(-time.Second))
			case "cancel":
				if _, e := s.Cancel(context.Background(), "actor", "room", p.ID); e != nil {
					t.Fatal(e)
				}
			}
			got, e := s.Execute(context.Background(), "actor", "room", p.ID)
			if e != nil || rt.sent != 0 || got.State == "confirmed" {
				t.Fatalf("%+v %v sent=%d", got, e, rt.sent)
			}
		})
	}
}
func TestUncertainAndMissingOutputNeverBecomeSuccessOrReplay(t *testing.T) {
	s, rt := fixture(t)
	_, p := prepared(t, s)
	rt.unknown = true
	p, e := s.Execute(context.Background(), "actor", "room", p.ID)
	if e != nil || p.State != "uncertain" {
		t.Fatalf("%+v %v", p, e)
	}
	for i := 0; i < 3; i++ {
		s.Execute(context.Background(), "actor", "room", p.ID)
		s.GetPreview(context.Background(), "actor", "room", p.ID, true)
	}
	if rt.sent != 1 {
		t.Fatal(rt.sent)
	}
	if _, e = s.Prepare(context.Background(), "other", "room", PrepareInput{PlayerID: "KU_TEST", LocationID: p.LocationID}); e != ErrBusy {
		t.Fatalf("uncertain player should stay protected: %v", e)
	}
}
func TestStoreNamesRevisionsAndConcurrentQuota(t *testing.T) {
	s, _ := fixture(t)
	for _, name := range []string{"", "\x00x", "a\u202eb", string(make([]byte, 193))} {
		p := Location{Name: name, RoomID: "r", WorldID: "w", SessionID: "s"}
		if _, e := s.store.Save(p, 0); e != ErrInvalid {
			t.Errorf("name accepted %q", name)
		}
	}
	l, e := s.store.Save(Location{Name: " Café ", RoomID: "r", WorldID: "w", SessionID: "s"}, 0)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.store.Save(Location{Name: "CAFE\u0301", RoomID: "r", WorldID: "w", SessionID: "s"}, 0); e != ErrConflict {
		t.Fatal(e)
	}
	if _, e = s.store.Save(l, 999); e != ErrConflict {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 120; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := s.store.Save(Location{Name: fmt.Sprint("点", i), RoomID: "quota", WorldID: "w", SessionID: "s"}, 0)
			if e != nil && e != ErrLimit {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	var n int
	s.store.db.Table(s.store.locations).Where("room_id = ?", "quota").Count(&n)
	if n != 100 {
		t.Fatal(n)
	}
}
func TestCancelExecuteRace(t *testing.T) {
	for i := 0; i < 15; i++ {
		s, rt := fixture(t)
		_, p := prepared(t, s)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.Cancel(context.Background(), "actor", "room", p.ID) }()
		go func() { defer wg.Done(); s.Execute(context.Background(), "actor", "room", p.ID) }()
		wg.Wait()
		got, e := s.GetPreview(context.Background(), "actor", "room", p.ID, false)
		if e != nil || got.State == "cancelled" && rt.sent != 0 || got.State == "confirmed" && rt.sent != 1 {
			t.Fatalf("%+v %v sent=%d", got, e, rt.sent)
		}
	}
}

func TestDeletedWorldCannotReviveOldLocations(t *testing.T) {
	s, _ := fixture(t)
	l, p := prepared(t, s)
	if e := s.store.Archive("room", "world"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.store.Get("room", l.ID); e != ErrNotFound {
		t.Fatal(e)
	}
	got, e := s.GetPreview(context.Background(), "actor", "room", p.ID, false)
	if e != nil || got.State != "expired" {
		t.Fatalf("%+v %v", got, e)
	}
	_, e = s.store.Save(Location{RoomID: "room", WorldID: "world", Name: l.Name, SessionID: "session"}, 0)
	if e != nil {
		t.Fatal("archived name should be reusable", e)
	}
}
