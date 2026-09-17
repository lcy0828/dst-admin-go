package runtimefiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLatestSimulationPauseUsesOnlyCompleteEngineRecords(t *testing.T) {
	for _, test := range []struct {
		name, log string
		want      string
	}{
		{"paused", "[00:01:00]: Sim paused\n", "true"},
		{"unpaused", "[00:01:00]: Sim paused\n[10:12:30]: Sim unpaused\n", "false"},
		{"wall clock", "[2026-09-09 15:15:30]: Sim paused\r\n", "true"},
		{"long uptime", "[123:01:10]: Sim paused\n", "true"},
		{"empty", "", "unknown"},
		{"echo", "[00:01:00]: remote(console): print('Sim paused')\n", "unknown"},
		{"prefixed", "[00:01:00]: command [00:01:00]: Sim paused\n", "unknown"},
		{"mod output", "[00:01:00]: [mod] Sim paused\n", "unknown"},
		{"invalid time", "[00:99:00]: Sim paused\n", "unknown"},
		{"partial", "[00:01:00]: Sim paused", "unknown"},
		{"restart", "[00:01:00]: Sim paused\n[00:00:00]: Starting Up\n", "unknown"},
		{"restart header", "[00:01:00]: Sim paused\n[00:00:00]: Current time: Wed Sep 9 12:00:00 2026\n", "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _ := latestSimulationPause([]byte(test.log), true)
			assertPause(t, got, test.want)
		})
	}
	if paused, found := latestSimulationPause([]byte("[00:00:01]: Sim paused\n"), false); paused != nil || found {
		t.Fatal("a partial first line was treated as an engine record")
	}
}

func assertPause(t *testing.T, got *bool, want string) {
	t.Helper()
	value := "unknown"
	if got != nil {
		value = "false"
		if *got {
			value = "true"
		}
	}
	if value != want {
		t.Fatalf("pause=%s, want %s", value, want)
	}
}

func TestSimulationPauseLogReadsHistoryOnceAndFollowsAppendGaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_log.txt")
	content := "[00:00:00]: Starting Up\n[00:01:00]: Sim paused\n" + strings.Repeat("mod detail\n", 50000)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var reader SimulationPauseLog
	read := func() *bool {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return reader.Read(path, "process-1", info, []byte(content[max(0, len(content)-4096):]))
	}
	assertPause(t, read(), "true")
	// No history file is opened again when metadata is unchanged.
	info, _ := os.Stat(path)
	assertPause(t, reader.Read(path+".unavailable", "process-1", info, []byte(content[len(content)-4096:])), "true")

	appendLog := func(value string) {
		t.Helper()
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(value); err != nil {
			t.Fatal(err)
		}
		file.Close()
		content += value
	}
	appendLog("[04:01:00]: Sim unpaused\n" + strings.Repeat("new detail\n", 40000))
	assertPause(t, read(), "false")
	appendLog("[04:03:00]: Sim paused")
	assertPause(t, read(), "false")
	appendLog("\n")
	assertPause(t, read(), "true")
	appendLog("[00:00:00]: Starting Up\n" + strings.Repeat("startup detail\n", 40000))
	assertPause(t, read(), "unknown")
}

func TestSimulationPauseLogClearsOnReplacementTruncationAndInstanceChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_log.txt")
	var reader SimulationPauseLog
	write := func(content, instance string) *bool {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(path)
		return reader.Read(path, instance, info, []byte(content))
	}
	assertPause(t, write("[00:01:00]: Sim paused\n", "old"), "true")
	assertPause(t, write("[00:00:00]: Starting Up\n", "new"), "unknown")
	assertPause(t, write("[00:01:00]: Sim paused\n", "new"), "true")
	assertPause(t, write("", "new"), "unknown")
	assertPause(t, write("[00:01:00]: Sim paused\n", "new"), "true")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	assertPause(t, write(strings.Repeat("new detail\n", 100), "new"), "unknown")
	assertPause(t, write("[00:01:00]: Sim paused\n", ""), "unknown")
	assertPause(t, write("[00:01:00]: Sim paused\n", "new"), "true")
	reader.Reset()
	if reader.info != nil || reader.paused != nil || reader.instanceID != "" {
		t.Fatal("stopped process retains pause state")
	}
}

func TestSimulationPauseLogSameSizeRewriteIsNotCached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_log.txt")
	var reader SimulationPauseLog
	for index, content := range []string{"[00:01:00]: Sim paused\n", "[00:01:00]: mod detail\n"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		modified := time.Unix(100+int64(index), 0)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(path)
		want := "true"
		if index == 1 {
			want = "unknown"
		}
		assertPause(t, reader.Read(path, "one", info, []byte(content)), want)
	}
}

func TestSimulationPauseReverseScanHandlesChunkBoundaries(t *testing.T) {
	for padding := 0; padding < 100; padding++ {
		content := "[00:00:00]: Starting Up\n[00:01:00]: Sim paused\n[10:01:00]: Sim unpaused\n" + strings.Repeat("x", simulationScanBytes-padding) + "\n"
		paused, found, err := scanSimulationPause(strings.NewReader(content), 0, int64(len(content)))
		if err != nil || !found {
			t.Fatalf("padding=%d: found=%v err=%v", padding, found, err)
		}
		assertPause(t, paused, "false")
	}
}
