package logparser

import (
	"testing"
	"time"
)

func TestParseDSTStartTimeUsesLocalWallClock(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("CST", 8*60*60)
	t.Cleanup(func() { time.Local = previous })

	got, err := ParseDSTStartTime("Wed Aug 19 19:56:40 2026")
	if err != nil {
		t.Fatalf("ParseDSTStartTime() error = %v", err)
	}
	want := time.Date(2026, time.August, 19, 19, 56, 40, 0, time.Local)
	if !got.Equal(want) || got.Location() != time.Local {
		t.Fatalf("ParseDSTStartTime() = %v (%v), want %v (%v)", got, got.Location(), want, want.Location())
	}
}

func TestResolveDSTTimestampConvertsRuntimeClock(t *testing.T) {
	start := time.Date(2026, time.August, 19, 19, 56, 40, 0, time.FixedZone("CST", 8*60*60))
	got, err := ResolveDSTTimestamp(start, "05:21:33")
	if err != nil {
		t.Fatalf("ResolveDSTTimestamp() error = %v", err)
	}
	want := time.Date(2026, time.August, 20, 1, 18, 13, 0, start.Location())
	if !got.Equal(want) {
		t.Fatalf("ResolveDSTTimestamp() = %v, want %v", got, want)
	}
}

func TestResolveDSTTimestampRejectsInvalidInput(t *testing.T) {
	start := time.Date(2026, time.August, 19, 19, 56, 40, 0, time.Local)
	for _, relative := range []string{"", "5:21:33", "05:61:33", "05:21:33x"} {
		if _, err := ResolveDSTTimestamp(start, relative); err == nil {
			t.Errorf("ResolveDSTTimestamp(%q) accepted invalid input", relative)
		}
	}
	if _, err := ResolveDSTTimestamp(time.Time{}, "05:21:33"); err == nil {
		t.Error("ResolveDSTTimestamp accepted a zero startup time")
	}
}
