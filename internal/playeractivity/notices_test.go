package playeractivity

import (
	"errors"
	"testing"
	"time"
)

func TestNoticeOutboxCommitsWithEventsAndRetriesOnlyProjection(t *testing.T) {
	s, _ := testService(t)
	calls := 0
	failing := true
	observer := func(events []Event) error {
		calls++
		if len(events) != 1 || events[0].Kind != "player.death" {
			t.Fatal(events)
		}
		if failing {
			return errors.New("synthetic failure")
		}
		return nil
	}
	s.ConfigureNotices(func(e Event) bool { return e.Kind == "player.death" }, observer)
	row := cursorRecord{ID: "cursor", RoomID: "room", WorldID: "world"}
	e := Event{ID: "example", RoomID: "room", WorldID: "world", Kind: "player.death", OccurredAt: time.Now(), Fields: EventFields{}}
	if err := s.store.ingest(row, []Event{e}); err != nil {
		t.Fatal(err)
	}
	s.publishNotices("room", "world")
	var saved eventRecord
	s.store.db.Table(s.store.events).First(&saved)
	if !saved.NoticePending || calls != 1 {
		t.Fatal("failed publication lost durable pending state")
	}
	// Reconstruct service, including its callback, using the same persistent store.
	s = New(s.store, s.rooms, s.source, s.commander)
	s.ConfigureNotices(func(e Event) bool { return e.Kind == "player.death" }, observer)
	failing = false
	s.publishNotices("room", "world")
	s.publishNotices("room", "world")
	if calls != 2 {
		t.Fatal("retry duplicated or lost notice", calls)
	}
	if err := s.store.ingest(row, []Event{e}); err != nil {
		t.Fatal(err)
	}
	s.publishNotices("room", "world")
	if calls != 2 {
		t.Fatal("source replay reactivated notice")
	}
}
func TestNoticeOutboxNeverBackfillsOldOrOtherRoomRecords(t *testing.T) {
	s, _ := testService(t)
	e := Event{ID: "legacy", RoomID: "room", WorldID: "world", Kind: "player.death", OccurredAt: time.Now(), Fields: EventFields{}}
	if err := s.store.ingest(cursorRecord{ID: "one"}, []Event{e}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.ConfigureNotices(func(e Event) bool { return true }, func([]Event) error { calls++; return nil })
	s.publishNotices("room", "world")
	if calls != 0 {
		t.Fatal("old events implicitly opted into notifications")
	}
	e.ID = "expired"
	e.OccurredAt = time.Now().Add(-25 * time.Hour)
	if err := s.store.ingest(cursorRecord{ID: "two"}, []Event{e}); err != nil {
		t.Fatal(err)
	}
	s.publishNotices("room", "world")
	if calls != 0 {
		t.Fatal("expired gameplay was replayed as new")
	}
}

func TestSettledNoticeOutboxWakesOnNewEventsAndNeverLeaksRolledBackEvents(t *testing.T) {
	s, _ := testService(t)
	calls := 0
	s.ConfigureNotices(func(e Event) bool { return e.Kind == "player.death" }, func(events []Event) error { calls += len(events); return nil })
	s.publishNotices("room", "world") // Initial restart recovery scan, no events.
	e := Event{ID: "new-death", RoomID: "room", WorldID: "world", Kind: "player.death", OccurredAt: time.Now(), Fields: EventFields{}}
	row := cursorRecord{ID: "example-cursor", RoomID: "room", WorldID: "world"}
	if err := s.store.db.Exec("CREATE TRIGGER reject_activity_cursor BEFORE INSERT ON test_player_activity_cursor BEGIN SELECT RAISE(FAIL, 'synthetic failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.store.ingest(row, []Event{e}); err == nil {
		t.Fatal("failed cursor transaction unexpectedly succeeded")
	}
	s.publishNotices("room", "world")
	if calls != 0 {
		t.Fatal("rolled back activity leaked to notifications")
	}
	s.store.db.Exec("DROP TRIGGER reject_activity_cursor")
	if err := s.store.ingest(row, []Event{e}); err != nil {
		t.Fatal(err)
	}
	s.publishNotices("room", "world")
	s.publishNotices("room", "world")
	if calls != 1 {
		t.Fatal("settled collector did not wake exactly once for a new event", calls)
	}
}
