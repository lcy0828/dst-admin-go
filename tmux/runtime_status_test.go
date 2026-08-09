package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dstinstall "dont/internal/dstserver"
)

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
