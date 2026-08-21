package structuredlogs

import (
	"context"
	"testing"
	"time"

	"dont/internal/logstream"
	"dont/internal/rooms"
)

func TestClassifySnapshotResolvesDSTRelativeTimestamps(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("CST", 8*60*60)
	t.Cleanup(func() { time.Local = previous })

	world := rooms.World{ID: "master", RoomID: "room", Name: "Master"}
	observedAt := time.Date(2026, time.August, 20, 1, 20, 0, 0, time.UTC)
	entries, err := classifySnapshot(context.Background(), []logstream.Line{
		{Cursor: 1, Text: "[00:00:00]: Current time: Wed Aug 19 19:56:40 2026"},
		{Cursor: 2, Text: "[05:21:33]: Available disk space for save files: 5515865 MB"},
	}, world, nil, observedAt, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].OccurredAt == nil {
		t.Fatalf("entries = %#v", entries)
	}
	want := time.Date(2026, time.August, 19, 17, 18, 13, 0, time.UTC)
	if !entries[1].OccurredAt.Equal(want) {
		t.Fatalf("OccurredAt = %s, want %s", entries[1].OccurredAt, want)
	}
	if entries[1].SourceTimestamp != "05:21:33" {
		t.Fatalf("SourceTimestamp = %q, want runtime clock preserved", entries[1].SourceTimestamp)
	}
}
