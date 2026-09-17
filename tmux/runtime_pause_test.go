package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRuntimePauseBelongsToTheCurrentProcessOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux is a Unix runtime")
	}
	root := t.TempDir()
	server := &DSTServer{StorageRoot: root, ConfDir: "saves", ArchiveName: "room", WorldName: "Master", SessionName: "test"}
	if err := os.MkdirAll(filepath.Dir(server.runtimeLogPath()), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	started := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local)
	setSession := func(created time.Time, pid int) {
		t.Helper()
		script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\nhas-session) exit 0 ;;\ndisplay-message) printf '%%s\\n' '%d|$1|%d' ;;\nesac\n", created.Unix(), pid)
		if err := os.WriteFile(filepath.Join(root, "tmux"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeLog := func(instant time.Time, data string) {
		t.Helper()
		header := "[00:00:00]: Starting Up\n[00:00:00]: Current time: " + instant.Format("Mon Jan 2 15:04:05 2006") + "\n"
		if err := os.WriteFile(server.runtimeLogPath(), []byte(header+data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(server.runtimeLogPath(), instant.Add(time.Minute), instant.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	setSession(started.Add(-time.Second), 101)
	writeLog(started, "[00:00:01]: Sim paused\n"+strings.Repeat("runtime detail\n", 40000))
	for range 2 {
		status, err := server.RuntimeStatus()
		if err != nil || status.State != RuntimeRunning || status.Paused == nil || !*status.Paused {
			t.Fatalf("current process status=%+v error=%v", status, err)
		}
	}
	setSession(started.Add(2*time.Hour), 202)
	status, err := server.RuntimeStatus()
	if err != nil || status.State != RuntimeStarting || status.Paused != nil {
		t.Fatalf("new process reused old log: %+v error=%v", status, err)
	}
	writeLog(started.Add(2*time.Hour+time.Second), "Server registered via geo DNS\n")
	status, err = server.RuntimeStatus()
	if err != nil || status.State != RuntimeRunning || status.Paused != nil {
		t.Fatalf("absence of pause record defaulted to a bool: %+v error=%v", status, err)
	}
	if err := os.WriteFile(filepath.Join(root, "tmux"), []byte("#!/bin/sh\nprintf '%s\\n' \"can't find session\" >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	status, err = server.RuntimeStatus()
	if err != nil || status.State != RuntimeStopped || status.Paused != nil {
		t.Fatalf("stopped process retains pause: %+v error=%v", status, err)
	}
}
