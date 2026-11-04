package steamupdate

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"dont/internal/operationprogress"
)

func TestProgressChunksRatePhasesRetryAndFinalLine(t *testing.T) {
	now := time.Unix(1000, 0)
	var updates []operationprogress.Update
	ctx := operationprogress.WithReporter(context.Background(), func(p operationprogress.Update) { updates = append(updates, p) })
	w := NewProgressWriter(ctx, io.Discard)
	w.now = func() time.Time { return now }
	_, _ = w.Write([]byte("Connecting\n\x1b[0mUpdate state (0x61) down"))
	_, _ = w.Write([]byte("loading, progress: 10.00 (100 / 1000)\r"))
	now = now.Add(2 * time.Second)
	_, _ = w.Write([]byte("Update state (0x61) downloading, progress: 50.00 (500 / 1000)\r"))
	p := updates[len(updates)-1]
	if p.Stage != "game.download" || p.CurrentBytes != 500 || p.TotalBytes != 1000 || p.BytesPerSecond != 200 {
		t.Fatal(p)
	}
	now = now.Add(time.Second)
	_, _ = io.WriteString(w, "Steam background status message\n")
	now = now.Add(time.Second)
	_, _ = io.WriteString(w, "Update state (0x61) downloading, progress: 70 (700 / 1000)\r")
	if p = updates[len(updates)-1]; p.BytesPerSecond != 100 {
		t.Fatal("ordinary output distorted throughput", p)
	}
	_, _ = w.Write([]byte("Update state (0x81) validating, progress: 2.00 (20 / 1000)\n"))
	p = updates[len(updates)-1]
	if p.Stage != "game.validate" || p.BytesPerSecond != 0 {
		t.Fatal(p)
	}
	_, _ = w.Write([]byte("[DST Admin] SteamCMD app=343050 validate=true attempt=2/2\n"))
	p = updates[len(updates)-1]
	if p.Attempt != 2 || p.CurrentBytes != 0 || p.TotalBytes != 0 || p.Stage != "game.prepare" {
		t.Fatal(p)
	}
	_, _ = w.Write([]byte("Success! App '343050' fully installed."))
	w.Flush()
	p = updates[len(updates)-1]
	if p.Stage != "game.finalize" || p.Percent == 100 || p.TotalBytes != 0 {
		t.Fatal(p)
	}
}

func TestProgressLimitsInvalidValuesAndCoalesces(t *testing.T) {
	now := time.Unix(1000, 0)
	var updates []operationprogress.Update
	w := NewProgressWriter(operationprogress.WithReporter(context.Background(), func(p operationprogress.Update) { updates = append(updates, p) }), io.Discard)
	w.now = func() time.Time { return now }
	for _, line := range []string{
		"Update state (0x61) downloading, progress: 200 (200 / 100)",
		"Update state (0x61) downloading, progress: 0 (0 / 0)",
		"Update state (0x61) downloading, progress: 1 (1 / 99999999999999999999)",
	} {
		_, _ = io.WriteString(w, line+"\n")
	}
	if len(updates) != 0 {
		t.Fatal(updates)
	}
	for i := 0; i < 100; i++ {
		_, _ = io.WriteString(w, "Update state (0x61) downloading, progress: 50 (500 / 1000)\r")
	}
	if len(updates) != 1 {
		t.Fatal("unbounded reporting", len(updates))
	}
	_, _ = io.WriteString(w, "Update state (0x61) downloading, progress: 100 (1000 / 1000)\r")
	if len(updates) != 2 || updates[1].CurrentBytes != 1000 {
		t.Fatal("lost final phase sample", updates)
	}
	_, _ = io.WriteString(w, strings.Repeat("x", 1<<20))
	if len(w.pending) > 8192 {
		t.Fatal("unbounded pending data")
	}
}
