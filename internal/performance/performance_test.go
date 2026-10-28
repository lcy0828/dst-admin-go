package performance

import (
	"context"
	"dont/internal/runtimeaudit"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/rooms"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func storeFixture(t testing.TB) *Store {
	t.Helper()
	db, err := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "performance.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s := NewStore(db, "test_")
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	return s
}
func input(at time.Time, sim, wall float64) Input {
	return Input{RoomID: "room", WorldID: "master", WorldName: "Master", TargetID: "local", InstallationID: "default", InstanceID: "boot/session", State: "running", At: at, ObservedAt: at, SimulationSeconds: number(sim), CaptureSeconds: number(wall), TimeScale: number(1), ProcessID: 42, ProcessStartedAt: at.Add(-time.Hour), CPUSampledAt: at, CPUSeconds: number(wall / 2), RSSBytes: 256 << 20}
}
func TestScoreUsesIndependentNativeClocksAndCPUTime(t *testing.T) {
	at := time.Now().UTC()
	old := input(at, 100, 100)
	next := input(at.Add(30*time.Second), 124, 130)
	next.ProcessStartedAt = old.ProcessStartedAt
	p := score(&old, next, 30*time.Second)
	if p.Score == nil || *p.Score != 80 || p.CPUPercent == nil || *p.CPUPercent != 50 || p.ValidSeconds != 30 {
		t.Fatalf("score and single-core CPU: %+v", p)
	}
	next.SimulationSeconds = number(115)
	p = score(&old, next, 30*time.Second)
	if p.Score == nil || *p.Score != 50 || p.LowSeconds != 30 {
		t.Fatalf("lag not reflected: %+v", p)
	}
	next.TimeScale = number(2)
	old.TimeScale = number(2)
	next.SimulationSeconds = number(160)
	p = score(&old, next, 30*time.Second)
	if p.Score == nil || *p.Score != 100 {
		t.Fatalf("explicit simulation speed counted as lag: %+v", p)
	}
}
func TestScoresExcludePauseSleepRestartMigrationAndBadClocks(t *testing.T) {
	at := time.Now().UTC()
	for name, change := range map[string]func(*Input){
		"pause":         func(v *Input) { v.Paused = true },
		"sleep":         func(v *Input) { v.SimulationSeconds = number(100) },
		"restart":       func(v *Input) { v.InstanceID = "new" },
		"migration":     func(v *Input) { v.TargetID = "agent:new" },
		"clock reset":   func(v *Input) { v.CaptureSeconds = number(2) },
		"clock missing": func(v *Input) { v.CaptureSeconds = nil },
		"speed changed": func(v *Input) { v.TimeScale = number(2) },
		"offline":       func(v *Input) { v.State = "unavailable" },
	} {
		t.Run(name, func(t *testing.T) {
			old := input(at, 100, 100)
			next := input(at.Add(30*time.Second), 130, 130)
			change(&next)
			p := score(&old, next, 30*time.Second)
			if p.Score != nil || p.ValidSeconds != 0 {
				t.Fatalf("invalid window scored: %+v", p)
			}
		})
	}
}

func TestCaptureClockJumpsDoNotCreatePerformanceScores(t *testing.T) {
	at := time.Now()
	old := input(at, 100, 100)
	for _, observed := range []time.Duration{0, 5 * time.Second, 60 * time.Second} {
		next := input(at.Add(30*time.Second), 130, 130)
		next.ObservedAt = at.Add(observed)
		p := score(&old, next, 30*time.Second)
		if p.Score != nil || p.Reason != "CLOCK_DISCONTINUITY" {
			t.Fatalf("capture clock advanced 30s but observation advanced %v: %+v", observed, p)
		}
	}
}

func TestResourceCPUContinuesWhileSimulationPaused(t *testing.T) {
	at := time.Now().UTC()
	for _, states := range [][2]bool{{false, true}, {true, true}, {true, false}} {
		old := input(at, 100, 100)
		next := input(at.Add(30*time.Second), 100, 130)
		old.Paused, next.Paused = states[0], states[1]
		next.ProcessStartedAt = old.ProcessStartedAt
		p := score(&old, next, 30*time.Second)
		if p.Score != nil || p.ValidSeconds != 0 || p.CPUPercent == nil || *p.CPUPercent != 50 || p.RSSBytes != next.RSSBytes {
			t.Fatalf("pause transition %v lost resource measurements or invented a score: %+v", states, p)
		}
		next.ProcessStartedAt = next.ProcessStartedAt.Add(time.Second)
		if p := score(&old, next, 30*time.Second); p.CPUPercent != nil {
			t.Fatalf("replaced process reused old CPU time: %+v", p)
		}
	}
}

func TestHistoryWeightedSummaryDedupAndSettingsSurviveRestart(t *testing.T) {
	s := storeFixture(t)
	at := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	for i, value := range []float64{100, 50, 90} {
		p := Point{RoomID: "room", WorldID: "master", WorldName: "Master", TargetID: "local", InstanceID: "a", At: at.Add(time.Duration(i) * 30 * time.Second), State: "running", Score: number(value), WindowSeconds: 30, ValidSeconds: 30, RSSBytes: uint64(i+1) << 20, CPUPercent: number(float64(i) * 30)}
		if value < 80 {
			p.LowSeconds = 30
		}
		if err := s.Append(p); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(p); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Report("room", Query{From: at.Add(-time.Second), To: at.Add(2 * time.Minute)}, 1, time.Now().UTC())
	if err != nil || len(r.Worlds) != 1 || r.Worlds[0].AverageScore == nil || *r.Worlds[0].AverageScore != 80 || r.Worlds[0].LowSeconds != 30 || r.Worlds[0].PeakRSSBytes != 3<<20 {
		t.Fatalf("bad weighted report: %+v %v", r, err)
	}
	v := Defaults()
	v.Enabled = false
	v.IntervalSeconds = 60
	v, err = s.SaveSettings("room", v)
	if err != nil {
		t.Fatal(err)
	}
	reopened := NewStore(s.db, "test_")
	actual, err := reopened.Settings("room")
	if err != nil || actual != v {
		t.Fatalf("settings not persisted: %+v %v", actual, err)
	}
	if _, err := s.SaveSettings("room", Defaults()); err != ErrConflict {
		t.Fatalf("stale edit accepted: %v", err)
	}
}
func TestLongReportUsesAggregatesAndPreservesBootBoundaries(t *testing.T) {
	s := storeFixture(t)
	now := time.Now().UTC().Truncate(5 * time.Minute)
	start := now.Add(-48 * time.Hour)
	for i := 0; i < 576; i++ {
		p := Point{RoomID: "room", WorldID: "master", WorldName: "Master", InstanceID: "old", State: "running", At: start.Add(time.Duration(i) * 5 * time.Minute), Score: number(95), WindowSeconds: 30, ValidSeconds: 30}
		if i > 288 {
			p.InstanceID = "new"
		}
		if err := s.Append(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Table(s.raw).Where("room_id = ?", "room").Delete(&record{}).Error; err != nil {
		t.Fatal(err)
	}
	r, err := s.Report("room", Query{From: start, To: now, MaxPoints: 120}, 1, now)
	if err != nil || len(r.Items) > 120 || r.ResolutionSeconds < 300 || len(r.Worlds) != 1 {
		t.Fatalf("unbounded/empty aggregate report: %+v %v", r, err)
	}
	boots := map[string]bool{}
	for _, p := range r.Items {
		boots[p.InstanceID] = true
		if p.Score == nil || *p.Score != 95 {
			t.Fatalf("aggregation lost evidence: %+v", p)
		}
	}
	if len(boots) != 2 {
		t.Fatal("restart boundary lost")
	}
}

type catalogStub struct{}

func (catalogStub) List() ([]rooms.Room, error) {
	return []rooms.Room{{ID: "room", Managed: true}}, nil
}
func (catalogStub) Room(id string) (rooms.Room, error)   { return rooms.Room{ID: id, Managed: true}, nil }
func (catalogStub) Worlds(string) ([]rooms.World, error) { return []rooms.World{{ID: "master"}}, nil }

type sourceStub struct {
	calls int
	value Input
}

func (s *sourceStub) Samples(context.Context, string) ([]Input, error) {
	s.calls++
	return []Input{s.value}, nil
}
func TestCollectorDisabledAndCadenceDoNotDependOnBrowser(t *testing.T) {
	store := storeFixture(t)
	clock := time.Now().UTC()
	src := &sourceStub{value: input(clock, 100, 100)}
	s := New(store, catalogStub{}, src)
	s.now = func() time.Time { return clock }
	s.collectDue(context.Background())
	s.collectDue(context.Background())
	if src.calls != 1 {
		t.Fatal("duplicate sampling")
	}
	clock = clock.Add(30 * time.Second)
	src.value = input(clock, 130, 130)
	s.collectDue(context.Background())
	if src.calls != 2 {
		t.Fatal("background collection missing")
	}
	c := Defaults()
	c.Enabled = false
	if _, err := s.UpdateSettings("room", c); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	s.collectDue(context.Background())
	if src.calls != 2 {
		t.Fatal("disabled collector accessed runtime")
	}
	r, err := s.Report("room", Query{From: clock.Add(-5 * time.Minute), To: clock})
	if err != nil || len(r.Items) != 2 {
		t.Fatalf("history unavailable after disable: %+v %v", r, err)
	}
}

func TestSummaryCoversUnplottedPointsAndKeepsAlgorithmVersionsSeparate(t *testing.T) {
	s := storeFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)
	for i := 0; i < 60; i++ {
		version := ScoreVersion
		if i < 10 {
			version = "old-score"
		}
		v := 100.0
		if i < 30 {
			v = 40
		}
		if err := s.Append(Point{RoomID: "room", WorldID: "master", WorldName: "Master", InstanceID: time.Duration(i).String(), At: start.Add(time.Duration(i) * 30 * time.Second), State: "running", Score: number(v), ScoreVersion: version, WindowSeconds: 30, ValidSeconds: 30}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Report("room", Query{From: start, To: now, MaxPoints: 30}, 1, now)
	if err != nil || !r.Truncated || len(r.Items) != 30 || len(r.Worlds) != 2 {
		t.Fatalf("bounds/versions: %+v %v", r, err)
	}
	valid := 0.0
	for _, w := range r.Worlds {
		valid += w.ValidSeconds
		if w.ScoreVersion == ScoreVersion && (w.AverageScore == nil || *w.AverageScore != 76) {
			t.Fatalf("summary excludes unplotted evidence: %+v", w)
		}
	}
	if valid != 1800 {
		t.Fatalf("summary was limited by plotted rows: %v", valid)
	}
	for _, p := range r.Items {
		if p.ScoreVersion == "" {
			t.Fatal("score version lost")
		}
	}
}
func TestExpiredHistoryIsNotReadableBeforeCleanupAndRemovedRoomsExpire(t *testing.T) {
	s := storeFixture(t)
	now := time.Now().UTC()
	p := Point{RoomID: "room", WorldID: "master", State: "running", At: now.Add(-91 * 24 * time.Hour), Score: number(99), ValidSeconds: 30, WindowSeconds: 30}
	if err := s.Append(p); err != nil {
		t.Fatal(err)
	}
	r, err := s.Report("room", Query{From: now.Add(-100 * 24 * time.Hour), To: now}, 1, now)
	if err != nil || len(r.Items) != 0 || r.From.Before(now.Add(-90*24*time.Hour)) {
		t.Fatalf("expired history exposed: %+v %v", r, err)
	}
	p.RoomID = "removed"
	p.At = now.Add(-181 * 24 * time.Hour)
	if err := s.Append(p); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneExpired(now); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{s.raw, s.aggregate} {
		var count int
		if err := s.db.Table(table).Where("room_id = ?", "removed").Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("removed room leaked: %d %v", count, err)
		}
	}
}
func TestPausedSamplesAreObservedWithoutFabricatedScores(t *testing.T) {
	now := time.Now().UTC()
	old := input(now, 100, 100)
	old.Paused = true
	next := old
	next.At = now.Add(30 * time.Second)
	p := score(&old, next, 30*time.Second)
	if p.Score != nil || p.ValidSeconds != 0 || p.WindowSeconds != 30 || p.State != "paused" {
		t.Fatalf("pause counted as outage or zero: %+v", p)
	}
}

func BenchmarkScore(b *testing.B) {
	at := time.Now().UTC()
	old := input(at, 100, 100)
	next := input(at.Add(30*time.Second), 129, 130)
	next.ProcessStartedAt = old.ProcessStartedAt
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = score(&old, next, 30*time.Second)
	}
}

type eventSourceStub struct {
	filter  runtimeaudit.ListFilter
	failure bool
}

func (s *eventSourceStub) List(room string, f runtimeaudit.ListFilter) (runtimeaudit.List, error) {
	s.filter = f
	if s.failure {
		return runtimeaudit.List{}, errors.New("unavailable")
	}
	return runtimeaudit.List{Total: 101, Items: []runtimeaudit.Event{{ID: 1, WorldID: "master", Type: runtimeaudit.EventUnexpectedExit, OccurredAt: f.To.Add(-time.Minute)}}}, nil
}
func TestReportEventsUseSameBoundedRangeAndFailureDoesNotErasePerformance(t *testing.T) {
	store := storeFixture(t)
	now := time.Now().UTC()
	if err := store.Append(Point{RoomID: "room", WorldID: "master", State: "running", At: now.Add(-time.Minute), Score: number(100), ValidSeconds: 30, WindowSeconds: 30}); err != nil {
		t.Fatal(err)
	}
	events := &eventSourceStub{}
	s := New(store, catalogStub{}, nil)
	s.now = func() time.Time { return now }
	s.ConfigureEvents(events)
	r, err := s.Report("room", Query{From: now.Add(-time.Hour), To: now})
	if err != nil || len(r.Events) != 1 || !r.EventsTruncated || events.filter.Limit != 100 || !events.filter.From.Equal(r.From) || len(r.Worlds) != 1 {
		t.Fatalf("events/range: %+v %v", r, err)
	}
	found := false
	for _, f := range r.Worlds[0].Findings {
		found = found || f.Code == "UNEXPECTED_EXIT_RECORDED"
	}
	if !found {
		t.Fatal("exit finding absent")
	}
	events.failure = true
	r, err = s.Report("room", Query{From: now.Add(-time.Hour), To: now})
	if err != nil || len(r.Items) != 1 || r.EventsWarning != "EVENTS_UNAVAILABLE" {
		t.Fatalf("audit error erased history: %+v %v", r, err)
	}
}

func TestSettingsChangeDuringReadRejectsLateBatch(t *testing.T) {
	s := storeFixture(t)
	settings, err := s.SaveSettings("room", Defaults())
	if err != nil {
		t.Fatal(err)
	}
	oldRevision := settings.Revision
	settings.Enabled = false
	if _, err = s.SaveSettings("room", settings); err != nil {
		t.Fatal(err)
	}
	err = s.appendCurrent(Point{RoomID: "room", WorldID: "master", At: time.Now().UTC(), State: "running", Score: number(100)}, oldRevision)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("late batch after disabling was accepted: %v", err)
	}
	var n int
	s.db.Table(s.raw).Count(&n)
	if n != 0 {
		t.Fatal("disabled collector persisted late sample")
	}
}
