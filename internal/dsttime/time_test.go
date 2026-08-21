package dsttime

import (
	"testing"
	"time"
)

func TestResolveTimestampSupportsRuntimeBeyondOneDay(t *testing.T) {
	startedAt := time.Date(2026, time.August, 21, 1, 0, 0, 0, time.UTC)
	occurredAt, err := ResolveTimestamp(startedAt, "100:15:30")
	if err != nil {
		t.Fatal(err)
	}
	want := startedAt.Add(100*time.Hour + 15*time.Minute + 30*time.Second)
	if !occurredAt.Equal(want) {
		t.Fatalf("occurredAt=%s want=%s", occurredAt, want)
	}
}

func TestResolveTimestampRejectsInvalidRuntime(t *testing.T) {
	startedAt := time.Now()
	for _, value := range []string{"1:02:03", "24:60:00", "24:00:60", "invalid"} {
		if _, err := ResolveTimestamp(startedAt, value); err == nil {
			t.Fatalf("runtime %q was accepted", value)
		}
	}
}
