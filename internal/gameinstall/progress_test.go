package gameinstall

import (
	"context"
	"dont/internal/operationprogress"
	"strings"
	"testing"
)

func TestInstallStreamsSharedSteamProgressAndKeepsFinalOutput(t *testing.T) {
	var updates []operationprogress.Update
	ctx := operationprogress.WithReporter(context.Background(), func(p operationprogress.Update) { updates = append(updates, p) })
	w := newInstallOutput(ctx)
	_, _ = w.Write([]byte("Connecting to Steam\nUpdate state (0x61) down"))
	_, _ = w.Write([]byte("loading, progress: 10.00 (100 / 1000)\r"))
	_, _ = w.Write([]byte("Update state (0x61) downloading, progress: 100.00 (1000 / 1000)\r"))
	download := updates[len(updates)-1]
	if download.Stage != "game.download" || download.CurrentBytes != 1000 || download.TotalBytes != 1000 {
		t.Fatalf("download=%+v", download)
	}
	// Validation can restart from 0 after a download and must clear its rate.
	_, _ = w.Write([]byte("Update state (0x81) validating, progress: 2.00 (20 / 1000)\n"))
	validation := updates[len(updates)-1]
	if validation.Stage != "game.validate" || validation.BytesPerSecond != 0 || validation.Percent < download.Percent {
		t.Fatalf("validation=%+v", validation)
	}
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
	w := newInstallOutput(operationprogress.WithReporter(context.Background(), func(p operationprogress.Update) { updates = append(updates, p) }))
	_, _ = w.Write([]byte("Update state (0x61) downloading, progress: 200 (200 / 100)\n"))
	if len(updates) != 0 {
		t.Fatal("invalid total was published")
	}
	_, _ = w.Write([]byte(strings.Repeat("x", 32768)))
	if len(w.finish()) > 8192 {
		t.Fatal("output was not bounded")
	}
}
