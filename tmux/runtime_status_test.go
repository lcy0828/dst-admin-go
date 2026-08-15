package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dstinstall "dont/internal/dstserver"
)

func TestClassifyConsoleCommandError(t *testing.T) {
	tests := []struct {
		output  string
		private bool
		want    string
	}{
		{"permission denied", true, "permission_denied"},
		{"can't find session: dst", false, "not_found"},
		{"connection refused", true, "socket_unavailable"},
		{"", true, "socket_unavailable"},
	}
	for _, test := range tests {
		if got := classifyConsoleCommandError(test.output, test.private); got != test.want {
			t.Fatalf("classify(%q)=%q want=%q", test.output, got, test.want)
		}
	}
}

func TestConsoleTransportHealthHonorsDisabledAndPrivateSocketState(t *testing.T) {
	root := t.TempDir()
	cluster := filepath.Join(root, "DoNotStarveTogether", "Cluster_1")
	if err := os.MkdirAll(cluster, 0o700); err != nil {
		t.Fatal(err)
	}
	server := &DSTServer{StorageRoot: root, ConfDir: "DoNotStarveTogether", ArchiveName: "Cluster_1", SessionName: "dst", SocketPath: filepath.Join(root, "missing.sock")}
	if err := os.WriteFile(filepath.Join(cluster, "cluster.ini"), []byte("[MISC]\nconsole_enabled=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, _, err := server.ConsoleTransportHealth(); err != nil || status != "disabled" {
		t.Fatalf("disabled status=%q err=%v", status, err)
	}
	if err := os.WriteFile(filepath.Join(cluster, "cluster.ini"), []byte("[MISC]\nconsole_enabled=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, _, err := server.ConsoleTransportHealth(); err != nil || status != "socket_unavailable" {
		t.Fatalf("socket status=%q err=%v", status, err)
	}
}

func TestClassifyRuntimeLogRejectsExpiredToken(t *testing.T) {
	status := classifyRuntimeLog(`[00:00:03]: [200] Account Failed (6): "E_EXPIRED_TOKEN"
[00:00:03]: !!!! Your Server Will Not Start !!!!`, RuntimeStatus{State: RuntimeStarting, SessionExists: true})
	if status.State != RuntimeFailed || status.Code != "TOKEN_EXPIRED" || !status.SessionExists {
		t.Fatalf("status = %#v", status)
	}
}

func TestClassifyRuntimeLogAcceptsLatestReadySignal(t *testing.T) {
	status := classifyRuntimeLog("No auth token could be found\nServer registered via geo DNS in ap-east\n", RuntimeStatus{State: RuntimeStarting})
	if status.State != RuntimeRunning {
		t.Fatalf("status = %#v", status)
	}
}

func TestClassifyRuntimeLogRejectsFailureAfterAReadySignal(t *testing.T) {
	status := classifyRuntimeLog("E_EXPIRED_TOKEN\nServer registered via geo DNS\nFailed to bind server port\n", RuntimeStatus{State: RuntimeStarting})
	if status.State != RuntimeFailed || status.Code != "PORT_BIND_FAILED" {
		t.Fatalf("status = %#v", status)
	}
}

func TestClassifyRuntimeLogReportsMissingWorldgenTaskSet(t *testing.T) {
	status := classifyRuntimeLog("Error loading worldgen_main.lua\nMust specify the task set for a level!\nWorldgen had an error\n", RuntimeStatus{State: RuntimeStarting})
	if status.State != RuntimeFailed || status.Code != "WORLDGEN_TASK_SET_MISSING" || !strings.Contains(status.Message, "地图任务集") {
		t.Fatalf("status = %#v", status)
	}
}

func TestClassifyRuntimeLogAcceptsSecondaryShardReadySignal(t *testing.T) {
	status := classifyRuntimeLog("[00:00:21]: [Shard] secondary shard is now ready!\n[00:00:21]: World 1(Master) is now connected\n", RuntimeStatus{State: RuntimeStarting})
	if status.State != RuntimeRunning {
		t.Fatalf("status = %#v", status)
	}
}

func TestRuntimeLogMustBeNewerThanSession(t *testing.T) {
	createdAt := time.Unix(100, 0)
	if runtimeLogModifiedAfterSession(time.Unix(99, 999999999), createdAt) {
		t.Fatal("an older log was accepted")
	}
	if runtimeLogModifiedAfterSession(createdAt, createdAt) {
		t.Fatal("a log with second-level timestamp ambiguity was accepted")
	}
	if !runtimeLogModifiedAfterSession(time.Unix(100, 1), createdAt) {
		t.Fatal("a log written after session creation must be accepted")
	}
}

func TestRuntimeLogStartedAtParsesDSTTimestamp(t *testing.T) {
	startedAt, ok := runtimeLogStartedAt("[00:00:00]: Current time: Sun Aug 9 22:14:38 2026\n")
	if !ok || startedAt.Year() != 2026 || startedAt.Month() != time.August || startedAt.Day() != 9 || startedAt.Hour() != 22 || startedAt.Minute() != 14 {
		t.Fatalf("startedAt = %s, ok = %t", startedAt, ok)
	}
	if _, ok := runtimeLogStartedAt("Current time: unknown\n"); ok {
		t.Fatal("invalid timestamp was accepted")
	}
}

func TestClassifyRuntimeLogFilePreservesReadySignalAfterLongRuntimeLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_log.txt")
	content := "[00:00:00]: Current time: Wed Aug 12 09:19:26 2026\n" +
		strings.Repeat("startup detail\n", 400000) +
		"[00:00:27]: Server registered via geo DNS in us-east-1\n" +
		strings.Repeat("runtime detail\n", 400000) +
		"[16:51:26]: routine mod output\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := classifyRuntimeLogFile(path, RuntimeStatus{State: RuntimeStarting, SessionExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != RuntimeRunning {
		t.Fatalf("long-running status = %#v", status)
	}
}

func TestClassifyRuntimeLogFileLetsRecentFailureOverrideStartupReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_log.txt")
	content := "[00:00:20]: Server registered via geo DNS\n" +
		strings.Repeat("runtime detail\n", 400000) +
		"[16:51:26]: Failed to bind server port\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := classifyRuntimeLogFile(path, RuntimeStatus{State: RuntimeStarting, SessionExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != RuntimeFailed || status.Code != "PORT_BIND_FAILED" {
		t.Fatalf("long-running failure status = %#v", status)
	}
}

func TestBuildStartCommandAddsMacSteamEnvironmentWithoutLeakingArguments(t *testing.T) {
	libraryPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(libraryPath, "steamclient.dylib"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DST_ADMIN_STEAM_CLIENT_LIBRARY_PATH", libraryPath)
	command := buildStartCommand(dstinstall.Layout{Kind: dstinstall.LayoutMac}, "/Applications/DST server", []string{"-cluster", "room one"})
	if !strings.Contains(command, "DYLD_LIBRARY_PATH") || !strings.Contains(command, shellArg(libraryPath)) || !strings.Contains(command, "'/Applications/DST server'") || !strings.Contains(command, "'room one'") {
		t.Fatalf("command = %s", command)
	}
}
