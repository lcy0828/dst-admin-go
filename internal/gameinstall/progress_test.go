package gameinstall

import (
	"context"
	"dont/internal/operationprogress"
	"strings"
	"testing"
	"time"
)

func TestSteamProgressHandlesChunksPhasesAndDownloadRate(t *testing.T) {
	var updates []operationprogress.Update
	now := time.Unix(1000, 0)
	ctx := operationprogress.WithReporter(context.Background(), func(p operationprogress.Update) { updates = append(updates, p) })
	w := &installOutput{ctx: ctx, now: func() time.Time { return now }}
	_, _ = w.Write([]byte("Connecting to Steam\nUpdate state (0x61) down"))
	_, _ = w.Write([]byte("loading, progress: 10.00 (100 / 1000)\r"))
	now = now.Add(2 * time.Second)
	_, _ = w.Write([]byte("Update state (0x61) downloading, progress: 50.00 (500 / 1000)\r"))
	download := updates[len(updates)-1]
	if download.Stage != "game.download" || download.CurrentBytes != 500 || download.TotalBytes != 1000 || download.BytesPerSecond != 200 {
		t.Fatalf("download=%+v", download)
	}
	// Validation can restart from 0 after a download and must clear its rate.
	_, _ = w.Write([]byte("Update state (0x81) validating, progress: 2.00 (20 / 1000)\n"))
	validation := updates[len(updates)-1]
	if validation.Stage != "game.validate" || validation.BytesPerSecond != 0 || validation.Percent < download.Percent {
		t.Fatalf("validation=%+v", validation)
	}
	now = now.Add(2 * time.Second)
	_, _ = w.Write([]byte("Success! App '343050' fully installed."))
	if tail := w.finish(); !strings.Contains(tail, "fully installed") {
		t.Fatal("lost final output")
	}
	if updates[len(updates)-1].Message != "Success! App '343050' fully installed." {
		t.Fatal("lost unterminated line")
	}
}

func TestSteamProgressBoundsOutputAndRejectsInvalidTotals(t *testing.T) {
	var updates []operationprogress.Update
	w := &installOutput{ctx: operationprogress.WithReporter(context.Background(), func(p operationprogress.Update) { updates = append(updates, p) }), now: time.Now}
	_, _ = w.Write([]byte("Update state (0x61) downloading, progress: 200 (200 / 100)\n"))
	if len(updates) != 0 {
		t.Fatal("invalid total was published")
	}
	_, _ = w.Write([]byte(strings.Repeat("x", 32768)))
	if len(w.pending) > 8192 || len(w.finish()) > 8192 {
		t.Fatal("output was not bounded")
	}
}
