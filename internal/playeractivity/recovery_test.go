package playeractivity

import (
	"context"
	"dont/internal/rooms"
	"dont/shared"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestBadMiddleSegmentPreservesPrefixLaterDataAndConfig(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 3)
	good := append([]byte{}, src.files["activity-02.json"]...)
	var v Segment
	_ = json.Unmarshal(good, &v)
	v.Generation = 999
	src.files["activity-02.json"], _ = json.Marshal(v)
	c, err := s.UpdateSettings("room", SettingsPatch{Revision: "default"})
	if err != nil {
		t.Fatal(err)
	}
	s.collect(context.Background(), "room", "world", c)
	row, _ := s.store.cursor("room", "world")
	if row.Generation != 3 || s.commander.(*commandStub).calls != 1 {
		t.Fatalf("blocked good data/config: %+v", row)
	}
	page, _ := s.Events("room", Query{Kind: "position"})
	if len(page.Items) != 2 {
		t.Fatalf("valid records discarded: %+v", page)
	}
	pending, _ := s.store.pendingRecovery("room", "world")
	if len(pending) != 1 || pending[0].Code != "ACTIVITY_SEGMENT_METADATA" {
		t.Fatalf("missing diagnostic: %+v", pending)
	}
	// Restart the controller coordinator, keep only durable state.
	s = New(s.store, s.rooms, src, s.commander)
	clock := time.Now().Add(31 * time.Second)
	s.store.now = func() time.Time { return clock }
	src.files["activity-02.json"] = good
	s.collect(context.Background(), "room", "world", c)
	page, _ = s.Events("room", Query{Kind: "position"})
	coverage, _ := s.store.coverage("room", "world")
	if len(page.Items) != 3 || !coverage.Complete || coverage.RecoveredSegments != 1 {
		t.Fatalf("not recovered: %+v %+v", page, coverage)
	}
	s.collect(context.Background(), "room", "world", c)
	page, _ = s.Events("room", Query{Kind: "position"})
	if len(page.Items) != 3 {
		t.Fatal("duplicated recovered records")
	}
}
func TestUnconfirmedJSONNeverBecomesProvenCorruption(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 3)
	src.files["activity-02.json"] = []byte(`{"schemaVersion":1,"events":[`)
	clock := time.Now()
	s.store.now = func() time.Time { return clock }
	for i := 0; i < 4; i++ {
		s.collect(context.Background(), "room", "world", Defaults())
		clock = clock.Add(25 * time.Second)
	}
	pending, _ := s.store.pendingRecovery("room", "world")
	if len(pending) != 1 || pending[0].State != "pending" || pending[0].Code != "ACTIVITY_SEGMENT_JSON" {
		t.Fatalf("stable bytes falsely proved commit: %+v", pending)
	}
	status, _ := s.Status("room")
	if status.Worlds[0].State != "recording" || status.Worlds[0].Coverage.Complete {
		t.Fatalf("current output/history confused: %+v", status)
	}
}
func TestTrustedEventsSalvagedAndExactMissingRecordRecovered(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 3)
	var v Segment
	_ = json.Unmarshal(src.files["activity-02.json"], &v)
	v.FirstSequence = 20
	v.LastSequence = 22
	first := v.Events[0]
	first.Sequence = 20
	bad := first
	bad.Sequence = 21
	bad.Name = string(make([]byte, 129))
	last := first
	last.Sequence = 22
	v.Events = []Event{first, bad, last}
	src.files["activity-02.json"], _ = json.Marshal(v)
	var m Manifest
	_ = json.Unmarshal(src.files["activity-manifest.json"], &m)
	m.Slots[1].FirstSequence = 20
	m.Slots[1].LastSequence = 22
	src.files["activity-manifest.json"], _ = json.Marshal(m)
	clock := time.Now()
	s.store.now = func() time.Time { return clock }
	s.collect(context.Background(), "room", "world", Defaults())
	clock = clock.Add(31 * time.Second)
	s.collect(context.Background(), "room", "world", Defaults())
	p, _ := s.store.pendingRecovery("room", "world")
	if len(p) != 1 || p[0].State != "isolated" {
		t.Fatalf("not isolated after independent observations: %+v", p)
	}
	page, _ := s.Events("room", Query{Kind: "position"})
	if len(page.Items) != 4 {
		t.Fatalf("valid events lost: %+v", page)
	}
	v.Events[1].Name = "fixed"
	src.files["activity-02.json"], _ = json.Marshal(v)
	clock = clock.Add(31 * time.Second)
	s.collect(context.Background(), "room", "world", Defaults())
	page, _ = s.Events("room", Query{Kind: "position"})
	coverage, _ := s.store.coverage("room", "world")
	if len(page.Items) != 5 || !coverage.Complete {
		t.Fatalf("partial recovery failed: %d %+v", len(page.Items), coverage)
	}
}
func TestUnsupportedProtocolPausesStream(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 3)
	var v Segment
	_ = json.Unmarshal(src.files["activity-02.json"], &v)
	v.SchemaVersion = 2
	src.files["activity-02.json"], _ = json.Marshal(v)
	for i := 0; i < 3; i++ {
		s.collect(context.Background(), "room", "world", Defaults())
	}
	row, _ := s.store.cursor("room", "world")
	if row.Generation != 1 || row.Error != "ACTIVITY_SCHEMA_UNSUPPORTED" {
		t.Fatalf("incompatible stream skipped: %+v", row)
	}
}
func TestSlotReuseCannotResolveMissingDescriptor(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 2)
	src.files["activity-01.json"] = []byte(`{`)
	s.collect(context.Background(), "room", "world", Defaults())
	// A placement change retains the old gap as outside the source window.
	src.scope = "agent/other"
	fixture(t, src, 2)
	s.collect(context.Background(), "room", "world", Defaults())
	c, _ := s.store.coverage("room", "world")
	if c.RecoveredSegments != 0 || c.UnavailableRanges != 1 {
		t.Fatalf("reused slot healed unrelated history: %+v", c)
	}
}
func TestCompactTrajectoryFiltersAndPreservesDurableBoundaries(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 3)
	src.files["activity-02.json"] = []byte(`{`)
	s.collect(context.Background(), "room", "world", Defaults())
	first, err := s.CompactTrajectory("room", Query{UserID: "KU_TEST", Limit: 1})
	if err != nil || len(first.Items) != 1 || !first.HasMore {
		t.Fatalf("%+v %v", first, err)
	}
	next, err := s.CompactTrajectory("room", Query{UserID: "KU_TEST", Limit: 1, Before: first.NextCursor})
	if err != nil || len(next.Items) != 1 {
		t.Fatal(next, err)
	}
	if first.Items[0].ContinuousRangeID == next.Items[0].ContinuousRangeID || first.Coverage[0].Complete {
		t.Fatal("connected across missing segment/pagination")
	}
	if _, err = s.Events("room", Query{UserID: "KU_TEST", Before: first.NextCursor}); err == nil {
		t.Fatal("projection cursor accepted for different query")
	}
	legacy, _ := s.Trajectory("room", Query{UserID: "KU_TEST"})
	if len(legacy.Items) <= 2 {
		t.Fatal("legacy response lost boundary records")
	}
}
func TestCapacityPruningPreventsDeferredReadResurrection(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 3)
	good := src.files["activity-02.json"]
	src.files["activity-02.json"] = []byte(`{`)
	s.collect(context.Background(), "room", "world", Defaults())
	floor := time.Now().Add(time.Second).UTC()
	s.store.db.Table(s.store.cursors).Update("discard_before", floor)
	src.files["activity-02.json"] = good
	clock := time.Now().Add(time.Minute)
	s.store.now = func() time.Time { return clock }
	s.collect(context.Background(), "room", "world", Defaults())
	page, _ := s.Events("room", Query{Kind: "position"})
	if len(page.Items) != 2 {
		t.Fatalf("old deferred event resurrected: %d", len(page.Items))
	}
}

func TestTypedEventFailureDoesNotDiscardTrustedSiblingRecords(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 1)
	var v map[string]any
	_ = json.Unmarshal(src.files["activity-01.json"], &v)
	first := v["events"].([]any)[0].(map[string]any)
	bad := map[string]any{}
	for k, x := range first {
		bad[k] = x
	}
	bad["sequence"] = float64(2)
	bad["position"] = map[string]any{"x": "wrong type", "y": 0, "z": 0}
	v["lastSequence"] = float64(2)
	v["events"] = []any{first, bad}
	src.files["activity-01.json"], _ = json.Marshal(v)
	var m Manifest
	_ = json.Unmarshal(src.files["activity-manifest.json"], &m)
	m.Slots[0].LastSequence = 2
	src.files["activity-manifest.json"], _ = json.Marshal(m)
	s.collect(context.Background(), "room", "world", Defaults())
	page, _ := s.Events("room", Query{Kind: "position"})
	if len(page.Items) != 1 {
		t.Fatalf("typed field error discarded sibling: %+v", page)
	}
	pending, _ := s.store.pendingRecovery("room", "world")
	if len(pending) != 1 || pending[0].MissingSequences != "[2]" {
		t.Fatalf("not exact missing record: %+v", pending)
	}
}
func TestRecoveryEvidenceAndCursorRollbackTogetherOnDatabaseFailure(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 2)
	src.files["activity-01.json"] = []byte(`{`)
	s.store.db.Exec("CREATE TRIGGER reject_recovery BEFORE INSERT ON " + s.store.recoveries + " BEGIN SELECT RAISE(FAIL, 'injected'); END")
	s.collect(context.Background(), "room", "world", Defaults())
	row, _ := s.store.cursor("room", "world")
	page, _ := s.Events("room", Query{})
	if row.Generation != 0 || len(page.Items) != 0 {
		t.Fatal("cursor/events escaped failed recovery transaction")
	}
	s.store.db.Exec("DROP TRIGGER reject_recovery")
	s.collect(context.Background(), "room", "world", Defaults())
	row, _ = s.store.cursor("room", "world")
	if row.Generation != 2 {
		t.Fatal("transaction did not recover")
	}
}
func TestResyncIsCoalescedAndDoesNotResetPersistentHistory(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 2)
	s.collect(context.Background(), "room", "world", Defaults())
	before, _ := s.store.cursor("room", "world")
	reads := len(src.reads)
	for i := 0; i < 20; i++ {
		r, err := s.Resynchronize("room")
		if err != nil || !r.Accepted || r.Status != "pending" {
			t.Fatal(r, err)
		}
	}
	after, _ := s.store.cursor("room", "world")
	if after.Generation != before.Generation || len(s.wake) != 1 || len(src.reads) != reads {
		t.Fatal("request executed synchronously or reset history")
	}
}

type multiWorldCatalog struct{ catalogStub }

func (multiWorldCatalog) Worlds(string) ([]rooms.World, error) {
	return []rooms.World{{ID: "slow", Name: "Slow"}, {ID: "fast", Name: "Master"}, {ID: "next", Name: "Caves"}}, nil
}

type fairActivitySource struct {
	mu          sync.Mutex
	active, max int
	src         *sourceStub
	fastRead    bool
}

func (s *fairActivitySource) ActivityScope(_ context.Context, _ string, world string) (string, error) {
	return "node/" + world, nil
}
func (s *fairActivitySource) ReadActivityArtifacts(ctx context.Context, _, world string, names []string) (shared.RuntimeArtifactBundle, error) {
	s.mu.Lock()
	s.active++
	if s.active > s.max {
		s.max = s.active
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
	if world == "slow" {
		<-ctx.Done()
		return shared.RuntimeArtifactBundle{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fastRead = true
	return s.src.ReadActivityArtifacts(ctx, "room", world, names)
}
func TestSlowNodeDoesNotStarveOtherWorldsAndReadsStayBounded(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 1)
	source := &fairActivitySource{src: src}
	s.source = source
	s.rooms = multiWorldCatalog{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.collectDue(ctx)
	if time.Since(start) > time.Second || !source.fastRead || source.max > 2 {
		t.Fatal("slow node starved a parallel world or exceeded concurrency")
	}
	fast, _ := s.store.cursor("room", "fast")
	if fast.Generation != 1 {
		t.Fatal("healthy peer did not commit")
	}
	s.collectDue(context.Background())
	next, _ := s.store.cursor("room", "next")
	if next.Generation != 1 {
		t.Fatal("next due target starved")
	}
	for _, names := range src.reads {
		if len(names) > 8 {
			t.Fatal("segment read budget exceeded")
		}
	}
}
func TestRingRotationAndOldProducerSlotsRecoverWithoutMixing(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 64)
	var m Manifest
	_ = json.Unmarshal(src.files["activity-manifest.json"], &m)
	// Generation 65 wraps slot 1 while the remaining slots legitimately belong
	// to the previous producer/session after a hot handover.
	m.Generation = 65
	m.ProducerInstanceID = "instance-new"
	m.SessionID = "new-session"
	slot := Slot{Slot: 1, Generation: 65, ProducerInstanceID: m.ProducerInstanceID, SessionID: m.SessionID, ShardID: "1", FirstSequence: 1, LastSequence: 1}
	m.Slots[0] = slot
	v := Segment{SchemaVersion: 1, ProducerInstanceID: slot.ProducerInstanceID, SessionID: slot.SessionID, ShardID: "1", Generation: 65, FirstSequence: 1, LastSequence: 1, Events: []Event{{Sequence: 1, OccurredAtUnix: time.Now().Unix(), Kind: "position", UserID: "KU_TEST", Position: &Position{X: 65}}}}
	src.files["activity-01.json"], _ = json.Marshal(v)
	src.files["activity-manifest.json"], _ = json.Marshal(m)
	for i := 0; i < 9; i++ {
		s.collect(context.Background(), "room", "world", Defaults())
	}
	row, _ := s.store.cursor("room", "world")
	coverage, _ := s.store.coverage("room", "world")
	if row.Generation != 65 || row.Error != "" || coverage.UnavailableRanges != 1 {
		t.Fatalf("ring/old producer rejected: %+v %+v", row, coverage)
	}
	page, _ := s.Events("room", Query{Kind: "position", Limit: 100})
	if len(page.Items) != 64 {
		t.Fatalf("retained window not imported: %d", len(page.Items))
	}
	for _, names := range src.reads {
		if len(names) > 8 {
			t.Fatal("unbounded catch-up")
		}
	}
}

func TestNormalIdleOfflineAndBacklogRequestBudgets(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 16)
	delay := s.collect(context.Background(), "room", "world", Defaults())
	if delay != 5*time.Second || len(src.reads) != 2 || len(src.reads[1]) != 8 {
		t.Fatalf("catch-up budget %v %+v", delay, src.reads)
	}
	src.reads = nil
	s.collect(context.Background(), "room", "world", Defaults())
	if len(src.reads) != 2 || len(src.reads[1]) != 8 {
		t.Fatal("catch-up batch changed")
	}
	src.reads = nil
	s.collect(context.Background(), "room", "world", Defaults())
	if len(src.reads) != 1 {
		t.Fatal("caught-up world reread old segments")
	}
	var m Manifest
	_ = json.Unmarshal(src.files["activity-manifest.json"], &m)
	m.PlayerCount = 0
	src.files["activity-manifest.json"], _ = json.Marshal(m)
	src.reads = nil
	delay = s.collect(context.Background(), "room", "world", Defaults())
	if delay != 30*time.Second || len(src.reads) != 1 {
		t.Fatal("idle polling expanded")
	}
	src.fail = "activity-manifest.json"
	src.reads = nil
	delay = s.collect(context.Background(), "room", "world", Defaults())
	if delay != time.Minute || len(src.reads) != 1 {
		t.Fatal("offline world retried excessively")
	}
}

func TestRecoveryUsesLatestRetentionPolicyWithoutClaimingFilteredEvents(t *testing.T) {
	s, _ := testService(t)
	row, _ := s.store.cursor("room", "world")
	days := 1
	_, err := s.UpdateSettings("room", SettingsPatch{Revision: "default", RetentionDays: &days})
	if err != nil {
		t.Fatal(err)
	}
	event := Event{ID: identity("late-policy"), RoomID: "room", WorldID: "world", ProducerInstanceID: "instance", SessionID: "session", Sequence: 1, OccurredAt: time.Now().Add(-48 * time.Hour), Kind: "position"}
	r := recoveryRecord{ID: identity("pending-policy"), RoomID: "room", WorldID: "world", Producer: "instance", Session: "session", FirstSequence: 1, LastSequence: 1, State: "resolved", FirstSeenAt: time.Now(), LastSeenAt: time.Now()}
	if err = s.store.ingestRecovery(row, []Event{event}, []recoveryRecord{r}); err != nil {
		t.Fatal(err)
	}
	var count int
	s.store.db.Table(s.store.events).Count(&count)
	coverage, _ := s.store.coverage("room", "world")
	if count != 0 || coverage.RecoveredSegments != 0 || coverage.UnavailableRanges != 1 {
		t.Fatalf("policy bypass/false recovery: %d %+v", count, coverage)
	}
}

func TestNodeDropsAndLegacyGapsRemainVisibleOutsideCurrentPage(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 1)
	var m Manifest
	_ = json.Unmarshal(src.files["activity-manifest.json"], &m)
	m.Dropped = 9
	src.files["activity-manifest.json"], _ = json.Marshal(m)
	s.collect(context.Background(), "room", "world", Defaults())
	s.collect(context.Background(), "room", "world", Defaults())
	c, _ := s.store.coverage("room", "world")
	if c.Complete || c.UnavailableRanges != 1 {
		t.Fatalf("drops lost or double-counted: %+v", c)
	}
	row, _ := s.store.cursor("room", "world")
	row.StreamID = ""
	row.GapCount = 3
	if err := s.store.saveCursor(s.store.db, row); err != nil {
		t.Fatal(err)
	}
	s.collect(context.Background(), "room", "world", Defaults())
	c, _ = s.store.coverage("room", "world")
	if !c.LegacyUnknown || c.Complete {
		t.Fatal("prior gaps silently declared complete after upgrade")
	}
}

func TestTerminalDiagnosticCompactionPreservesCoverageCounts(t *testing.T) {
	s, _ := testService(t)
	now := time.Now().UTC()
	for i := 0; i < 1030; i++ {
		r := recoveryRecord{ID: identity("terminal", fmt.Sprint(i)), RoomID: "room", WorldID: "world", State: "expired", Code: "SPOOL_WINDOW_EXCEEDED", FirstSeenAt: now, LastSeenAt: now}
		if err := s.store.db.Table(s.store.recoveries).Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.compactRecovery("room"); err != nil {
		t.Fatal(err)
	}
	var count int
	s.store.db.Table(s.store.recoveries).Count(&count)
	c, _ := s.store.coverage("room", "world")
	if count >= 1030 || c.UnavailableRanges != 1030 || c.Complete {
		t.Fatalf("compaction lost evidence/counts: %d %+v", count, c)
	}
}
