package playeractivity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/shared"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type catalogStub struct{}

func (catalogStub) List() ([]rooms.Room, error) {
	return []rooms.Room{{ID: "room", Managed: true}}, nil
}
func (catalogStub) Room(id string) (rooms.Room, error) {
	if id != "room" {
		return rooms.Room{}, ErrNotFound
	}
	return rooms.Room{ID: id, Managed: true}, nil
}
func (catalogStub) Worlds(string) ([]rooms.World, error) {
	return []rooms.World{{ID: "world", Name: "Master"}}, nil
}

type sourceStub struct {
	files     map[string][]byte
	reads     [][]string
	scope     string
	fail      string
	afterRead func()
}

func (s *sourceStub) ActivityScope(context.Context, string, string) (string, error) {
	return s.scope, nil
}
func (s *sourceStub) ReadActivityArtifacts(_ context.Context, _, _ string, names []string) (shared.RuntimeArtifactBundle, error) {
	if len(names) == 0 {
		names = []string{"activity-manifest.json"}
	}
	s.reads = append(s.reads, append([]string{}, names...))
	b := shared.RuntimeArtifactBundle{Kind: shared.ArtifactPlayerActivity}
	for _, n := range names {
		if s.fail == n {
			return b, errors.New("offline")
		}
		raw := s.files[n]
		if len(raw) == 0 {
			continue
		}
		sum := sha256.Sum256(raw)
		b.Artifacts = append(b.Artifacts, shared.RuntimeArtifact{Name: n, Data: raw, Size: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]), UpdatedAt: time.Now()})
	}
	if s.afterRead != nil {
		s.afterRead()
	}
	return b, nil
}

type commandStub struct {
	calls   int
	request dstruntime.CommandRequest
}

func (c *commandStub) ExecuteCommand(_ context.Context, _, _ string, r dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	c.calls++
	c.request = r
	return dstruntime.CommandReceipt{OK: true}, nil
}
func testService(t *testing.T) (*Service, *sourceStub) {
	t.Helper()
	db, e := gorm.Open("sqlite3", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	store := NewStore(db, "test_")
	if e := store.Migrate(); e != nil {
		t.Fatal(e)
	}
	source := &sourceStub{scope: "local/master", files: map[string][]byte{}}
	return New(store, catalogStub{}, source, &commandStub{}), source
}
func fixture(t testing.TB, src *sourceStub, generations int) {
	t.Helper()
	m := Manifest{SchemaVersion: 1, ProducerVersion: "2.4.17", ProducerInstanceID: "instance-1", SessionID: "session", ShardID: "1", Generation: int64(generations), Config: Defaults(), UpdatedAtUnix: time.Now().Unix(), PlayerCount: 1, Slots: Slots{}}
	for i := 1; i <= generations; i++ {
		slot := Slot{Slot: i, Generation: int64(i), ProducerInstanceID: m.ProducerInstanceID, SessionID: m.SessionID, ShardID: m.ShardID, FirstSequence: int64(i), LastSequence: int64(i)}
		m.Slots = append(m.Slots, slot)
		segment := Segment{SchemaVersion: 1, ProducerInstanceID: m.ProducerInstanceID, SessionID: m.SessionID, ShardID: m.ShardID, Generation: slot.Generation, FirstSequence: slot.FirstSequence, LastSequence: slot.LastSequence, Events: []Event{{Sequence: int64(i), OccurredAtUnix: time.Now().Unix(), Kind: "position", UserID: "KU_TEST", Name: "测试", Position: &Position{X: float64(i), Z: 2}, Fields: map[string]any{}}}}
		src.files[fmt.Sprintf("activity-%02d.json", i)], _ = json.Marshal(segment)
	}
	src.files["activity-manifest.json"], _ = json.Marshal(m)
}
func TestIncrementalSyncDedupFailureAndPlacementRace(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 2)
	src.fail = "activity-02.json"
	s.collect(context.Background(), "room", "world", Defaults())
	cursor, _ := s.store.cursor("room", "world")
	if cursor.Generation != 0 {
		t.Fatal("failed batch advanced cursor")
	}
	src.fail = ""
	s.collect(context.Background(), "room", "world", Defaults())
	page, e := s.Events("room", Query{})
	if e != nil || len(page.Items) != 3 {
		t.Fatalf("events %d %v", len(page.Items), e)
	}
	src.reads = nil
	s.collect(context.Background(), "room", "world", Defaults())
	if len(src.reads) != 1 || len(src.reads[0]) != 1 {
		t.Fatalf("reread old segments: %v", src.reads)
	}
	page, _ = s.Events("room", Query{})
	if len(page.Items) != 3 {
		t.Fatal("duplicate events")
	}
	fixture(t, src, 3)
	src.afterRead = func() { src.scope = "agent/new" }
	s.collect(context.Background(), "room", "world", Defaults())
	cursor, _ = s.store.cursor("room", "world")
	if cursor.Generation != 2 {
		t.Fatal("mixed placement evidence committed")
	}
	src.afterRead = nil
	s.collect(context.Background(), "room", "world", Defaults())
	page, _ = s.Events("room", Query{})
	if len(page.Items) != 7 {
		t.Fatalf("new scope should preserve identity boundary, got %d", len(page.Items))
	}
}
func TestConfigConflictAndAppliedRevisionEvidence(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 0)
	off := false
	interval := 30
	c, e := s.UpdateSettings("room", SettingsPatch{Revision: "default", Enabled: &off, PositionIntervalSeconds: &interval})
	if e != nil || c.Enabled || c.PositionIntervalSeconds != 30 {
		t.Fatalf("settings %+v %v", c, e)
	}
	if _, e = s.UpdateSettings("room", SettingsPatch{Revision: "default"}); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	s.collect(context.Background(), "room", "world", c)
	status, _ := s.Status("room")
	if !status.Worlds[0].Pending {
		t.Fatal("command ACK must not imply applied")
	}
	command := s.commander.(*commandStub)
	if command.calls != 1 || command.request.Action != "activity.configure" {
		t.Fatal(command)
	}
	var m Manifest
	json.Unmarshal(src.files["activity-manifest.json"], &m)
	m.Config = c
	src.files["activity-manifest.json"], _ = json.Marshal(m)
	s.collect(context.Background(), "room", "world", c)
	status, _ = s.Status("room")
	if status.Worlds[0].Pending || status.Worlds[0].State != "disabled" {
		t.Fatalf("%+v", status)
	}
	src.fail = "activity-manifest.json"
	s.collect(context.Background(), "room", "world", c)
	status, _ = s.Status("room")
	if status.Worlds[0].State != "unavailable" {
		t.Fatal("error hidden by desired config")
	}
}
func TestPaginationRetentionAndCombatUnknown(t *testing.T) {
	s, _ := testService(t)
	now := time.Now().UTC().Truncate(time.Second)
	cursor, _ := s.store.cursor("room", "world")
	events := []Event{}
	for i := 0; i < 6; i++ {
		events = append(events, Event{ID: identity(fmt.Sprint(i)), RoomID: "room", WorldID: "world", ProducerInstanceID: "instance", UserID: "KU_TEST", OccurredAt: now.Add(-time.Duration(i) * time.Minute), Kind: "combat.attack", Fields: map[string]any{"targetGuid": "1", "targetPrefab": "dragonfly", "epic": true, "hits": 1, "unknownDamageHits": 1}})
	}
	events = append(events, Event{ID: identity("old"), RoomID: "room", UserID: "KU_TEST", OccurredAt: now.Add(-8 * 24 * time.Hour), Kind: "position", Fields: map[string]any{}})
	if e := s.store.ingest(cursor, events); e != nil {
		t.Fatal(e)
	}
	if e := s.store.ingest(cursor, events); e != nil {
		t.Fatal(e)
	}
	page, e := s.Events("room", Query{UserID: "KU_TEST", Limit: 2})
	if e != nil || len(page.Items) != 2 || !page.HasMore {
		t.Fatalf("%+v %v", page, e)
	}
	next, e := s.Events("room", Query{UserID: "KU_TEST", Limit: 2, Before: page.NextCursor})
	if e != nil || next.Items[0].ID == page.Items[0].ID {
		t.Fatal("cursor repeats")
	}
	if _, e = s.Events("room", Query{UserID: "KU_OTHER", Before: page.NextCursor}); !errors.Is(e, ErrInvalid) {
		t.Fatal("cursor not bound to filter")
	}
	result, e := s.Combat("room", Query{UserID: "KU_TEST"})
	if e != nil || len(result.Items) != 1 || result.Items[0].Damage != nil || result.Items[0].UnknownDamageHits != 6 || result.Items[0].Kills != 0 {
		t.Fatalf("unknown damage or boss participation fabricated: %+v %v", result, e)
	}
	s.store.db.Exec("CREATE TABLE protected_saves (value TEXT)")
	s.store.db.Exec("INSERT INTO protected_saves VALUES ('keep')")
	if e = s.store.prune("room", Defaults()); e != nil {
		t.Fatal(e)
	}
	var count int
	s.store.db.Table("protected_saves").Count(&count)
	if count != 1 {
		t.Fatal("save touched")
	}
	s.store.db.Table(s.store.events).Count(&count)
	if count != 6 {
		t.Fatal(count)
	}
}
func TestManifestEmptyLuaTableAndInvalidInput(t *testing.T) {
	var m Manifest
	if decode([]byte(`{"schemaVersion":1,"slots":{},"pending":0}`), &m) != nil || len(m.Slots) != 0 {
		t.Fatal("empty Lua table rejected")
	}
	if decode([]byte(`{"slots":{"path":"escape"}}`), &m) == nil {
		t.Fatal("arbitrary object accepted")
	}
	s, _ := testService(t)
	for _, q := range []Query{{UserID: "../x"}, {Kind: "%"}, {Limit: 501}, {WorldID: "other"}} {
		if _, e := s.Events("room", q); e == nil {
			t.Fatal(q)
		}
	}
}
func BenchmarkActivityIngest128(b *testing.B) {
	db, e := gorm.Open("sqlite3", ":memory:")
	if e != nil {
		b.Fatal(e)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(1)
	s := NewStore(db, "bench_")
	if e = s.Migrate(); e != nil {
		b.Fatal(e)
	}
	cursor, _ := s.cursor("room", "world")
	events := make([]Event, 128)
	for i := range events {
		events[i] = Event{ID: identity(fmt.Sprint(i)), RoomID: "room", WorldID: "world", UserID: "KU_TEST", Kind: "position", OccurredAt: time.Now(), Position: &Position{X: 2, Z: 3}}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range events {
			events[j].ID = identity(fmt.Sprint(i), fmt.Sprint(j))
		}
		if e := s.ingest(cursor, events); e != nil {
			b.Fatal(e)
		}
	}
}

type countedCatalog struct {
	catalogStub
	lists, worldLists int
	empty             bool
}

func (c *countedCatalog) List() ([]rooms.Room, error) {
	c.lists++
	if c.empty {
		return []rooms.Room{}, nil
	}
	return c.catalogStub.List()
}
func (c *countedCatalog) Worlds(room string) ([]rooms.World, error) {
	c.worldLists++
	return c.catalogStub.Worlds(room)
}
func TestIdleTicksCacheCatalogAndRetentionSurvivesRoomRemoval(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 0)
	catalog := &countedCatalog{}
	s.rooms = catalog
	s.collectDue(context.Background())
	s.collectDue(context.Background())
	if catalog.lists != 1 || catalog.worldLists != 1 || len(src.reads) != 1 {
		t.Fatalf("idle discovery or reads repeated: %+v %v", catalog, src.reads)
	}
	s.Wake()
	s.collectDue(context.Background())
	if catalog.lists != 2 || len(src.reads) != 2 {
		t.Fatal("lifecycle wake did not invalidate cache")
	}
	cursor, _ := s.store.cursor("room", "world")
	if e := s.store.ingest(cursor, []Event{{ID: identity("expired"), RoomID: "room", WorldID: "world", Kind: "position", OccurredAt: time.Now().Add(-8 * 24 * time.Hour), Fields: map[string]any{}}}); e != nil {
		t.Fatal(e)
	}
	catalog.empty = true
	s.store.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	s.Wake()
	s.collectDue(context.Background())
	var count int
	if e := s.store.db.Table(s.store.events).Where("room_id = ?", "room").Count(&count).Error; e != nil {
		t.Fatal(e)
	}
	if count != 1 {
		t.Fatalf("expected instance boundary only, got %d", count)
	}
}
func TestLogicalBudgetPrunesOldestActivityOnly(t *testing.T) {
	s, _ := testService(t)
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		r := eventRecord{ID: identity(fmt.Sprint(i)), RoomID: "room", OccurredAt: now.Add(-time.Duration(i) * time.Minute), SizeBytes: 8 << 20, Value: "{}"}
		if e := s.store.db.Table(s.store.events).Create(&r).Error; e != nil {
			t.Fatal(e)
		}
	}
	c := Defaults()
	c.MaximumMiB = 16
	if e := s.store.prune("room", c); e != nil {
		t.Fatal(e)
	}
	var rows []eventRecord
	s.store.db.Table(s.store.events).Order("occurred_at DESC").Find(&rows)
	if len(rows) != 2 || rows[0].ID != identity("0") || rows[1].ID != identity("1") {
		t.Fatalf("quota removed wrong records: %+v", rows)
	}
}

func TestActivityPersistentStorageHeaderIsAccepted(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 1)
	for name, raw := range src.files {
		src.files[name] = append([]byte("KLEI 1\n"), raw...)
	}
	s.collect(context.Background(), "room", "world", Defaults())
	row, e := s.store.cursor("room", "world")
	if e != nil || row.Generation != 1 || row.Error != "" {
		t.Fatalf("header prevented ingestion: %+v %v", row, e)
	}
}
