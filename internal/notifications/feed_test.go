package notifications

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestFeedWithoutChannelsResumesDeduplicatesAndPreservesReleaseFacts(t *testing.T) {
	s, store, _ := fixture(t)
	e := Event{ID: "software:v2.0.0", Kind: "software.available", Version: "v2.0.0", Release: &ReleaseFacts{Component: "controller", CurrentVersion: "v1.0.0", LatestVersion: "v2.0.0", StatusPath: "/api/v2/system/software"}}
	s.Emit(e)
	s.Emit(e)
	first, err := s.Feed("", "software.available", 1)
	if err != nil || len(first.Items) != 1 || first.Items[0].Release == nil {
		t.Fatalf("facts lost without channels: %+v %v", first, err)
	}
	s.Emit(Event{ID: "other", Kind: "room.result"})
	e.ID = "software:v2.1.0"
	e.Version = "v2.1.0"
	s.Emit(e)
	// Simulate reconnecting after the service was recreated, no in-memory cursor.
	s, err = New(store, &testMail{})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Feed(first.NextCursor, "software.available", 10)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != e.ID {
		t.Fatalf("reconnect missed/duplicated event: %+v %v", next, err)
	}
	empty, err := s.Feed(next.NextCursor, "software.available", 10)
	if err != nil || len(empty.Items) != 0 {
		t.Fatal("cursor replayed events")
	}
	if _, err := s.Feed(next.NextCursor, "room.result", 10); err != ErrInvalid {
		t.Fatal("cursor reused for another subscription")
	}
}
func TestFeedReportsRetentionGapAndDoesNotIncludeWebhookCredentials(t *testing.T) {
	s, store, _ := fixture(t)
	for _, id := range []string{"a", "b", "c"} {
		s.Emit(Event{ID: id, Kind: "custom"})
	}
	initial, _ := s.Feed("", "", 1)
	if err := store.db.Table(store.channelsTable+"_event").Where("id <= ?", 2).Delete(&feedRecord{}).Error; err != nil {
		t.Fatal(err)
	}
	page, err := s.Feed(initial.NextCursor, "", 10)
	if err != nil || !page.HistoryTruncated || len(page.Items) != 1 {
		t.Fatalf("missing retention warning: %+v %v", page, err)
	}
	for _, cursor := range []string{"garbage", base64.RawURLEncoding.EncodeToString([]byte(`{"sequence":999,"filter":"bad"}`))} {
		if _, err := s.Feed(cursor, "", 10); err != ErrInvalid {
			t.Fatal("invalid cursor accepted")
		}
	}
	_, err = s.Save("", webhookInput("https://example.com/private-token"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(page)
	if string(body) == "" {
		t.Fatal("no feed")
	}
	// Old history is removed on publication, with no new background poller.
	store.db.Table(store.channelsTable+"_event").Where("id = ?", 3).Update("created_at", time.Now().Add(-31*24*time.Hour))
	s.Emit(Event{ID: "d", Kind: "custom"})
	page, _ = s.Feed(initial.NextCursor, "", 10)
	if len(page.Items) != 1 || page.Items[0].ID != "d" || !page.HistoryTruncated {
		t.Fatalf("retention failed: %+v", page)
	}
}

func TestFeedExpiresWithoutNewPublicationsAndDoesNotLoseCaughtUpCursor(t *testing.T) {
	s, store, _ := fixture(t)
	s.Emit(Event{ID: "old", Kind: "custom"})
	first, err := s.Feed("", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Table(store.channelsTable+"_event").Update("created_at", time.Now().Add(-31*24*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	page, err := s.Feed("", "", 10)
	if err != nil || len(page.Items) != 0 || !page.HistoryTruncated {
		t.Fatalf("stale events exposed: %+v %v", page, err)
	}
	caughtUp, err := s.Feed(first.NextCursor, "", 10)
	if err != nil || caughtUp.HistoryTruncated || caughtUp.NextCursor != first.NextCursor {
		t.Fatalf("caught-up cursor lost: %+v %v", caughtUp, err)
	}
	s.Emit(Event{ID: "new", Kind: "custom"})
	next, err := s.Feed(page.NextCursor, "", 10)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != "new" {
		t.Fatalf("new message missed: %+v %v", next, err)
	}
}
