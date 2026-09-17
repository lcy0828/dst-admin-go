package mods

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/operationprogress"
	"dont/shared"
)

func TestSteamCMDOutputFailed(t *testing.T) {
	tests := []struct {
		name   string
		output string
		failed bool
	}{
		{name: "success", output: "Success. Downloaded item 1392778117", failed: false},
		{name: "download failure", output: "ERROR! Download item 1392778117 failed (I/O Operation Failed).", failed: true},
		{name: "staging failure", output: "Update canceled: Staging folder not writable (Disk write failure)", failed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := steamCMDOutputFailed(test.output); actual != test.failed {
				t.Fatalf("steamCMDOutputFailed() = %v, want %v", actual, test.failed)
			}
		})
	}
}

func TestSteamCMDOutputFailurePreservesIOReason(t *testing.T) {
	output := "ERROR! Download item 1392778117 failed (I/O Operation Failed)."
	if actual := steamCMDOutputFailure(output); actual != "I/O Operation Failed" {
		t.Fatalf("steamCMDOutputFailure() = %q, want exact I/O reason", actual)
	}
}

func TestSteamCMDOutputFailurePreservesDiskWriteReason(t *testing.T) {
	output := "Update canceled: Staging folder not writable (Disk write failure)"
	if actual := steamCMDOutputFailure(output); actual != "Disk write failure" {
		t.Fatalf("steamCMDOutputFailure() = %q, want exact disk reason", actual)
	}
}

func TestSteamCMDDownloadSessionReusesLoginAndAcceptsMultipleItems(t *testing.T) {
	executable, logPath := writeInteractiveSteamCMD(t)
	runner := NewSteamCMDRunner(executable, t.TempDir(), "322330")
	runner.idleTimeout = time.Hour
	t.Cleanup(func() { _ = runner.Close() })

	var output strings.Builder
	var updates []operationprogress.Update
	var progressMu sync.Mutex
	ctx := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) {
		progressMu.Lock()
		defer progressMu.Unlock()
		updates = append(updates, update)
	})
	if err := runner.DownloadSession(ctx, []string{"111", "222"}, false, &output); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	previous := 0
	for _, update := range updates {
		if update.Percent < previous || update.TotalItems != 2 {
			t.Fatalf("bad batch progress: %+v", updates)
		}
		previous = update.Percent
		if update.CurrentItem == 1 && update.WorkshopID == "111" || update.CurrentItem == 2 && update.WorkshopID == "222" {
			seen[update.WorkshopID] = true
		}
		if update.CurrentItem == 2 && update.Message != "" && update.CurrentBytes != 0 {
			t.Fatalf("second mod inherited first mod bytes: %+v", update)
		}
	}
	if len(seen) != 2 || previous != 100 {
		t.Fatalf("batch incomplete: %+v", updates)
	}
	if err := runner.DownloadSession(context.Background(), []string{"333"}, true, &output); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(commands)
	if strings.Count(log, "start\n") != 1 || strings.Count(log, "login anonymous\n") != 1 {
		t.Fatalf("SteamCMD session was not reused:\n%s", log)
	}
	for _, command := range []string{
		"workshop_download_item 322330 111",
		"workshop_download_item 322330 222",
		"workshop_download_item 322330 333 validate",
	} {
		if !strings.Contains(log, command+"\n") {
			t.Fatalf("missing command %q:\n%s", command, log)
		}
	}
	if strings.Count(output.String(), "Success. Downloaded item") != 3 {
		t.Fatalf("unexpected SteamCMD output: %s", output.String())
	}
}

func TestSteamCMDDownloadSessionReportsIOFailureAndStartsCleanSession(t *testing.T) {
	executable, logPath := writeInteractiveSteamCMD(t)
	runner := NewSteamCMDRunner(executable, t.TempDir(), "322330")
	runner.idleTimeout = time.Hour
	t.Cleanup(func() { _ = runner.Close() })

	err := runner.DownloadSession(context.Background(), []string{"999"}, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "I/O Operation Failed") {
		t.Fatalf("expected exact SteamCMD I/O failure, got %v", err)
	}
	if err := runner.DownloadSession(context.Background(), []string{"111"}, false, io.Discard); err != nil {
		t.Fatalf("new session after failure: %v", err)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(commands), "start\n") != 2 || strings.Count(string(commands), "login anonymous\n") != 2 {
		t.Fatalf("failed session was unexpectedly reused:\n%s", commands)
	}
}

func TestSteamCMDBatchReportsCompletedFailedAndUnstartedItemsTogether(t *testing.T) {
	executable, _ := writeInteractiveSteamCMD(t)
	runner := NewSteamCMDRunner(executable, t.TempDir(), "322330")
	t.Cleanup(func() { _ = runner.Close() })
	var latest []shared.ModDownloadProgress
	var first []shared.ModDownloadProgress
	ctx := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) {
		if len(first) == 0 {
			first = update.Items
		}
		latest = update.Items
	})
	err := runner.DownloadSession(ctx, []string{"111", "999", "222"}, false, io.Discard)
	if err == nil || len(latest) != 3 || latest[0].Status != "succeeded" || latest[1].Status != "failed" || latest[2].Status != "queued" || !strings.Contains(latest[1].Message, "I/O Operation Failed") {
		t.Fatalf("missing cumulative results: %+v, %v", latest, err)
	}
	if first[0].Status != "queued" {
		t.Fatal("new progress mutated earlier events")
	}
}

func TestSteamCMDDownloadSessionClosesAfterIdleTimeout(t *testing.T) {
	executable, logPath := writeInteractiveSteamCMD(t)
	runner := NewSteamCMDRunner(executable, t.TempDir(), "322330")
	runner.idleTimeout = 20 * time.Millisecond
	t.Cleanup(func() { _ = runner.Close() })
	if err := runner.DownloadSession(context.Background(), []string{"111"}, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		commands, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(commands), "quit\n") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle SteamCMD session did not close: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeInteractiveSteamCMD(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	logPath := filepath.Join(root, "commands.log")
	executable := filepath.Join(root, "steamcmd")
	quotedLogPath := "'" + strings.ReplaceAll(logPath, "'", "'\\''") + "'"
	script := "#!/bin/sh\n" +
		"log_path=" + quotedLogPath + "\n" +
		"printf 'start\\n' >> \"$log_path\"\n" +
		"while IFS= read -r command; do\n" +
		"  printf '%s\\n' \"$command\" >> \"$log_path\"\n" +
		"  case \"$command\" in\n" +
		"    workshop_download_item*)\n" +
		"      set -- $command\n" +
		"      if [ \"$3\" = \"999\" ]; then\n" +
		"        printf 'ERROR! Download item %s failed (I/O Operation Failed).\\n' \"$3\"\n" +
		"      else\n" +
		"        printf 'Success. Downloaded item %s\\n' \"$3\"\n" +
		"      fi\n" +
		"      ;;\n" +
		"    quit) exit 0 ;;\n" +
		"  esac\n" +
		"done\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return executable, logPath
}

func TestBoundedTailWriter(t *testing.T) {
	writer := &boundedTailWriter{limit: 8}
	for _, value := range []string{"abc", "def", "ghijk"} {
		if count, err := writer.Write([]byte(value)); err != nil || count != len(value) {
			t.Fatalf("Write(%q) = %d, %v", value, count, err)
		}
	}
	if actual := writer.String(); actual != "defghijk" {
		t.Fatalf("unexpected tail %q", actual)
	}
	long := strings.Repeat("x", 12)
	_, _ = writer.Write([]byte(long))
	if actual := writer.String(); actual != strings.Repeat("x", 8) {
		t.Fatalf("unexpected replacement tail %q", actual)
	}
}

func TestSteamDownloadProgressTrackerReportsOnlyActiveAppDownload(t *testing.T) {
	tracker := &steamDownloadProgressTracker{appID: "322330"}
	if _, ok := tracker.consume("AppID 343050 update started : download 0/100"); ok {
		t.Fatal("unrelated app download was reported")
	}
	update, ok := tracker.consume("[time] AppID 322330 update started : download 1048576/4194304, store 0/0")
	if !ok || update.Percent != 24 || update.CurrentBytes != 1048576 || update.TotalBytes != 4194304 || update.BytesPerSecond != 0 {
		t.Fatalf("unexpected download start update: %#v, ok=%t", update, ok)
	}
	update, ok = tracker.consume("Increasing target number of download connections to 9 (rate was 0.000, now 36.003)")
	if !ok || update.BytesPerSecond != 4_500_375 {
		t.Fatalf("unexpected target rate update: %#v, ok=%t", update, ok)
	}
	update, ok = tracker.consume("Current download rate: 1.035 Mbps")
	if !ok || update.BytesPerSecond != 129_375 {
		t.Fatalf("unexpected current rate update: %#v, ok=%t", update, ok)
	}
	update, ok = tracker.consume("Current download rate: 0.000 Mbps")
	if !ok || update.BytesPerSecond != 0 || update.TotalBytes != 4194304 {
		t.Fatalf("zero rate should clear speed without losing byte totals: %#v, ok=%t", update, ok)
	}
	_, _ = tracker.consume("AppID 322330 scheduler finished : removed from schedule")
	if _, ok := tracker.consume("Current download rate: 44.000 Mbps"); ok {
		t.Fatal("rate after the app download finished was reported")
	}
}

func TestSteamDownloadProgressTrackerUsesStagedSizeAndSampledRate(t *testing.T) {
	started := time.Unix(100, 0)
	tracker := &steamDownloadProgressTracker{appID: "322330", sampledAt: started}
	update, ok := tracker.consume("AppID 322330 update started : download 0/472256, store 0/0, stage 0/1396139")
	if !ok || update.CurrentBytes != 0 || update.TotalBytes != 1396139 {
		t.Fatalf("unexpected staged download update: %#v, ok=%t", update, ok)
	}
	update, ok = tracker.sample(started.Add(time.Second), 700000)
	if !ok || update.CurrentBytes != 700000 || update.TotalBytes != 1396139 || update.BytesPerSecond != 700000 {
		t.Fatalf("unexpected sampled download update: %#v, ok=%t", update, ok)
	}
}

func TestSteamWorkshopBytesOnlyCountsSelectedItems(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		filepath.Join(root, "steamapps", "workshop", "downloads", "322330", "111", "part.bin"): "1234",
		filepath.Join(root, "steamapps", "workshop", "content", "322330", "111", "mod.lua"):    "12",
		filepath.Join(root, "steamapps", "workshop", "content", "322330", "222", "mod.lua"):    "ignored",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if actual := steamWorkshopBytes(root, "322330", []string{"111", "111"}); actual != 6 {
		t.Fatalf("selected workshop bytes = %d, want 6", actual)
	}
}

func TestSteamCMDLiveProgress(t *testing.T) {
	executable := strings.TrimSpace(os.Getenv("DST_STEAMCMD_LIVE"))
	workshopID := strings.TrimSpace(os.Getenv("DST_STEAMCMD_LIVE_WORKSHOP_ID"))
	if executable == "" || workshopID == "" {
		t.Skip("set DST_STEAMCMD_LIVE and DST_STEAMCMD_LIVE_WORKSHOP_ID to run the live download check")
	}
	var mu sync.Mutex
	updates := make([]operationprogress.Update, 0, 16)
	ctx := operationprogress.WithReporter(context.Background(), func(update operationprogress.Update) {
		mu.Lock()
		updates = append(updates, update)
		mu.Unlock()
	})
	runner := NewSteamCMDRunner(executable, t.TempDir(), "322330")
	if err := runner.Download(ctx, []string{workshopID}, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var maxBytes, totalBytes, maxRate int64
	for _, update := range updates {
		if update.CurrentBytes > maxBytes {
			maxBytes = update.CurrentBytes
		}
		if update.TotalBytes > totalBytes {
			totalBytes = update.TotalBytes
		}
		if update.BytesPerSecond > maxRate {
			maxRate = update.BytesPerSecond
		}
	}
	if totalBytes == 0 || maxBytes == 0 || maxRate == 0 {
		t.Fatalf("live progress incomplete: updates=%d current=%d total=%d rate=%d", len(updates), maxBytes, totalBytes, maxRate)
	}
	t.Logf("live progress: updates=%d current=%d total=%d peak_rate=%d", len(updates), maxBytes, totalBytes, maxRate)
}

func TestSteamDownloadProgressTrackerClampsCompletedBytes(t *testing.T) {
	tracker := &steamDownloadProgressTracker{appID: "322330"}
	update, ok := tracker.consume("AppID 322330 update started : download 120/100, store 0/0")
	if !ok || update.CurrentBytes != 100 || update.TotalBytes != 100 || update.Percent != 84 {
		t.Fatalf("unexpected clamped update: %#v, ok=%t", update, ok)
	}
}

func TestFindSteamContentLogUsesExistingExecutableLog(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "steamcmd")
	logPath := filepath.Join(root, "logs", "content_log.txt")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("steamcmd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	actual := findSteamContentLog(executable)
	actualInfo, actualErr := os.Stat(actual)
	wantInfo, wantErr := os.Stat(logPath)
	if actualErr != nil || wantErr != nil || !os.SameFile(actualInfo, wantInfo) {
		t.Fatalf("content log = %q, want file %q", actual, logPath)
	}
}

func TestSteamProcessContentLogPathUsesRuntimeExecutableDirectory(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "linux32", "steamcmd")
	logDirectory := filepath.Join(filepath.Dir(executable), "logs")
	if err := os.MkdirAll(logDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(logDirectory, "content_log.txt")
	if actual := steamProcessContentLogPath(executable); actual != want {
		t.Fatalf("content log = %q, want %q", actual, want)
	}
}

func TestFindSteamProcessContentLogUsesRunningLinuxExecutable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux exposes the running executable through /proc")
	}
	source, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	executable := filepath.Join(root, "linux32", "steamcmd")
	if err := os.MkdirAll(filepath.Join(filepath.Dir(executable), "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, binary, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "5")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want := filepath.Join(filepath.Dir(executable), "logs", "content_log.txt")
	if actual := findSteamProcessContentLog(ctx, command.Process.Pid); actual != want {
		t.Fatalf("content log = %q, want %q", actual, want)
	}
}
