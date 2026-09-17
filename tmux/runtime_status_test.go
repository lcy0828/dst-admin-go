package tmux

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestStartupMilestonesReuseLogContentWithoutChangingReadiness(t *testing.T) {
	for _, tc := range []struct {
		log, stage string
		state      RuntimeState
	}{
		{"unrecognized log", "", RuntimeStarting},
		{"Starting Up\nLOADING LUA", "initializing", RuntimeStarting},
		{"ModIndex: Beginning normal load sequence for dedicated server.", "loading_mods", RuntimeStarting},
		{"Generating world", "generating_world", RuntimeStarting},
		{"Loading world: session/123\nModIndex:GetModsToLoad", "loading_world", RuntimeStarting},
		{"Loading world: session/123\n[Shard] Sending secondary shard information to master...", "connecting", RuntimeStarting},
		{"Loading world: session/123\n[DST-ADMIN-RUNTIME READY] version=2.4.6", "", RuntimeRunning},
		{"Loading world: session/123\nFailed to bind server port", "", RuntimeFailed},
	} {
		t.Run(tc.log, func(t *testing.T) {
			got := classifyRuntimeStartupLog(tc.log, RuntimeStatus{State: RuntimeStarting, SessionExists: true})
			if got.State != tc.state || got.StartupStage != tc.stage {
				t.Fatalf("got=%+v want state=%s stage=%s", got, tc.state, tc.stage)
			}
		})
	}
}

func TestTmuxSessionAbsentDoesNotHidePermissionFailure(t *testing.T) {
	for _, output := range []string{
		"can't find session: dst",
		"no server running on /tmp/tmux.sock",
		"error connecting to /tmp/tmux.sock (No such file or directory)",
	} {
		if !tmuxSessionAbsent(output) {
			t.Fatalf("absence output was not recognized: %q", output)
		}
	}
	if tmuxSessionAbsent("error connecting to /opt/dst/tmux.sock (Permission denied)") {
		t.Fatal("permission denial was classified as an absent session")
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

func TestClassifyRuntimeLogRejectsWorkshopDownloadTimeout(t *testing.T) {
	status := classifyRuntimeLog(
		"[00:00:02]: Sim paused\n[00:05:03]: DownloadServerMods timed out\n",
		RuntimeStatus{State: RuntimeStarting, SessionExists: true},
	)
	if status.State != RuntimeFailed || status.Code != "WORKSHOP_DOWNLOAD_TIMEOUT" || !strings.Contains(status.Message, "Workshop") {
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

func TestClassifyRuntimeLogKeepsRunningStateWhileSaveIsUnhealthy(t *testing.T) {
	status := ClassifyRuntimeLog("Server registered via geo DNS\nSerializing world: 55\n[CRITICAL] Failed to save file /save/session\n")
	if status.State != RuntimeRunning || status.Code != "SAVE_WRITE_FAILED" || !strings.Contains(status.Message, "可能回档") {
		t.Fatalf("status=%#v", status)
	}
	status = ClassifyRuntimeLog("[CRITICAL] Failed to save file /save/session\nSerializing world: 56\n")
	if status.State != RuntimeRunning || status.Code != "" {
		t.Fatalf("recovered status=%#v", status)
	}
}

func TestRuntimeStartupClassificationDoesNotCacheSaveFailure(t *testing.T) {
	status := classifyRuntimeStartupLog(
		"Server registered via geo DNS\n[CRITICAL] Failed to save file /save/session\n",
		RuntimeStatus{State: RuntimeStarting, SessionExists: true},
	)
	if status.State != RuntimeRunning || status.Code != "" {
		t.Fatalf("startup status=%#v", status)
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

func TestParseRuntimeInstanceIDAcceptsTmuxPaneIdentity(t *testing.T) {
	identity, createdAt, err := parseRuntimeInstance("dstserver_v2_room_master", "1787336791|$1|11010\n")
	if err != nil {
		t.Fatal(err)
	}
	if identity != "dstserver_v2_room_master@1787336791/$1/11010" {
		t.Fatalf("identity = %q", identity)
	}
	if createdAt.Unix() != 1787336791 {
		t.Fatalf("createdAt = %s", createdAt)
	}
}

func TestParseRuntimeInstanceIDRejectsMissingPanePID(t *testing.T) {
	if _, err := parseRuntimeInstanceID("dstserver_v2_room_master", "1787336791|$1|\n"); err == nil {
		t.Fatal("missing tmux pane PID was accepted")
	}
}

func TestParseRuntimeInstanceIDRejectsUnprefixedSessionID(t *testing.T) {
	if _, err := parseRuntimeInstanceID("dstserver_v2_room_master", "1787336791|1|11010\n"); err == nil {
		t.Fatal("unprefixed tmux session ID was accepted")
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
	command := buildStartCommandForOS("darwin", dstinstall.Layout{Kind: dstinstall.LayoutMac}, "/Applications/DST server", []string{"-cluster", "room one"})
	if !strings.Contains(command, "DYLD_LIBRARY_PATH") || !strings.Contains(command, shellArg(libraryPath)) || !strings.Contains(command, "'/Applications/DST server'") || !strings.Contains(command, "'room one'") {
		t.Fatalf("command = %s", command)
	}
	wrapper := "/usr/sbin/taskpolicy -a '/Applications/DST server'"
	if !strings.Contains(command, wrapper) {
		t.Fatalf("macOS command does not apply application scheduling policy: %s", command)
	}
}

func TestBuildStartCommandDoesNotAddMacSchedulingPolicyToOtherLayouts(t *testing.T) {
	command := buildStartCommandForOS("linux", dstinstall.Layout{Kind: dstinstall.LayoutUnix}, "/opt/dst/bin64/dontstarve_dedicated_server_nullrenderer_x64", []string{"-cluster", "room one"})
	if strings.Contains(command, "taskpolicy") {
		t.Fatalf("non-macOS command contains taskpolicy: %s", command)
	}
	if command != "'/opt/dst/bin64/dontstarve_dedicated_server_nullrenderer_x64' '-cluster' 'room one'" {
		t.Fatalf("command = %s", command)
	}
}

func TestBuildStartCommandAddsMacSchedulingPolicyToUnixLayoutOnDarwin(t *testing.T) {
	command := buildStartCommandForOS("darwin", dstinstall.Layout{Kind: dstinstall.LayoutUnix}, "/opt/dst/dontstarve_dedicated_server_nullrenderer", nil)
	want := "/usr/sbin/taskpolicy -a '/opt/dst/dontstarve_dedicated_server_nullrenderer'"
	if command != want {
		t.Fatalf("command = %s, want %s", command, want)
	}
}

func TestPrepareStartPolicySkipsNonMacOSHosts(t *testing.T) {
	if err := prepareStartPolicy("linux", filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("non-macOS policy check returned an error: %v", err)
	}
}

func TestPrepareStartPolicyRejectsMissingOrNonExecutableCommand(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	if err := prepareStartPolicy("darwin", missing); err == nil || !strings.Contains(err.Error(), "macOS 应用级调度策略不可用") {
		t.Fatalf("missing command error = %v", err)
	}

	nonExecutable := filepath.Join(root, "taskpolicy")
	if err := os.WriteFile(nonExecutable, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareStartPolicy("darwin", nonExecutable); err == nil || !strings.Contains(err.Error(), "不是可执行文件") {
		t.Fatalf("non-executable command error = %v", err)
	}
}

func TestPrepareStartPolicyAcceptsExecutableCommand(t *testing.T) {
	command := filepath.Join(t.TempDir(), "taskpolicy")
	if err := os.WriteFile(command, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareStartPolicy("darwin", command); err != nil {
		t.Fatalf("executable command was rejected: %v", err)
	}
}

func TestMacOSApplicationPolicyCommandIsReady(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS system command check")
	}
	if err := prepareStartPolicy(runtime.GOOS, macOSApplicationPolicyCommand); err != nil {
		t.Fatal(err)
	}
}
