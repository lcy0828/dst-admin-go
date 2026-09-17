package shards

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dont/shared"
	dsttmux "dont/tmux"
)

func newTmuxControlForCacheTest(t *testing.T) *TmuxControl {
	t.Helper()
	root := t.TempDir()
	control, err := NewTmuxControl(TmuxConfig{
		SaveRoot: root, ServerMode: "64",
	})
	if err != nil {
		t.Fatal(err)
	}
	return control
}

func shortLegacySocketForTest(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "dst-legacy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "tmux.sock")
}

func TestNativeConsoleSocketPathUsesSaveRootIdentity(t *testing.T) {
	root := t.TempDir()
	first, err := NativeConsoleSocketPath(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NativeConsoleSocketPath(root)
	if err != nil || first != second {
		t.Fatalf("first=%q second=%q error=%v", first, second, err)
	}
	other, err := NativeConsoleSocketPath(t.TempDir())
	if err != nil || other == first {
		t.Fatalf("other=%q first=%q error=%v", other, first, err)
	}
}

func TestTmuxControlClaimsOneOwnerPerSaveRoot(t *testing.T) {
	root := t.TempDir()
	first, err := NewTmuxControl(TmuxConfig{SaveRoot: root, ServerMode: "64", OwnerLabel: "first"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := NewTmuxControl(TmuxConfig{SaveRoot: root, ServerMode: "64", OwnerLabel: "second"}); err == nil || !strings.Contains(err.Error(), "RUNTIME_OWNER_CONFLICT") {
		t.Fatalf("second owner error=%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := NewTmuxControl(TmuxConfig{SaveRoot: root, ServerMode: "64", OwnerLabel: "replacement"})
	if err != nil {
		t.Fatalf("replacement owner=%v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitTmuxSocketDoesNotRequirePrivateParent(t *testing.T) {
	root := t.TempDir()
	directory, err := os.MkdirTemp("/tmp", "dst-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	control, err := NewTmuxControl(TmuxConfig{
		SaveRoot: root, ServerMode: "64", ConsoleSocket: filepath.Join(directory, "tmux.sock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("explicit socket directory mode=%v error=%v", info.Mode().Perm(), err)
	}
}

func TestTmuxControlDetectsUnmanagedShardProcess(t *testing.T) {
	root := t.TempDir()
	control, err := NewTmuxControl(TmuxConfig{
		SaveRoot: root, ServerMode: "64",
		ProcessProbe: func(context.Context) ([]shared.ShardProcessReport, error) {
			return []shared.ShardProcessReport{{PID: 42, Cluster: "room", Shard: "Master", StorageRoot: filepath.Dir(root), ConfigDirectory: filepath.Base(root)}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	control.legacySessionProbe = func(*dsttmux.DSTServer) (bool, error) { return false, nil }
	server, err := control.server("room", "Master")
	if err != nil {
		t.Fatal(err)
	}
	status, err := control.inspectRuntimeOwnership(context.Background(), server, "room", "Master", RuntimeStatus{State: RuntimeStopped})
	if err != nil || status.Code != "UNMANAGED_DST_PROCESS_CONFLICT" || status.State != RuntimeFailed || !status.SessionExists {
		t.Fatalf("status=%#v error=%v", status, err)
	}
}

func TestTmuxControlStopsSingleOwnedLegacySession(t *testing.T) {
	root := t.TempDir()
	legacySocket := shortLegacySocketForTest(t)
	control, err := NewTmuxControl(TmuxConfig{
		SaveRoot: root, ServerMode: "64", LegacyConsoleSockets: []string{legacySocket},
		ProcessProbe: func(context.Context) ([]shared.ShardProcessReport, error) {
			return []shared.ShardProcessReport{{
				PID: 42, Cluster: "room", Shard: "Caves",
				StorageRoot: filepath.Dir(root), ConfigDirectory: filepath.Base(root),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	control.legacySessionProbe = func(server *dsttmux.DSTServer) (bool, error) {
		return server.SocketPath == legacySocket, nil
	}
	stoppedSocket := ""
	control.legacyStop = func(server *dsttmux.DSTServer) error {
		stoppedSocket = server.SocketPath
		return nil
	}
	if err := control.Stop(context.Background(), "room", "Caves"); err != nil {
		t.Fatal(err)
	}
	if stoppedSocket != legacySocket {
		t.Fatalf("stopped socket=%q want=%q", stoppedSocket, legacySocket)
	}
}

func TestTmuxControlStopsDefaultSocketFromPreviousMountNamespace(t *testing.T) {
	root := t.TempDir()
	legacySocket := "/proc/77/root/tmp/tmux-1000/default"
	control, err := NewTmuxControl(TmuxConfig{
		SaveRoot: root, ServerMode: "64",
		ProcessProbe: func(context.Context) ([]shared.ShardProcessReport, error) {
			return []shared.ShardProcessReport{{
				PID: 42, Cluster: "room", Shard: "Caves",
				StorageRoot: filepath.Dir(root), ConfigDirectory: filepath.Base(root),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	control.legacyProcessSockets = func(processIDs []int32) []string {
		if len(processIDs) != 1 || processIDs[0] != 42 {
			t.Fatalf("process IDs=%v", processIDs)
		}
		return []string{legacySocket}
	}
	control.legacySessionProbe = func(server *dsttmux.DSTServer) (bool, error) {
		return server.SocketPath == legacySocket, nil
	}
	stoppedSocket := ""
	control.legacyStop = func(server *dsttmux.DSTServer) error {
		stoppedSocket = server.SocketPath
		return nil
	}
	if err := control.Stop(context.Background(), "room", "Caves"); err != nil {
		t.Fatal(err)
	}
	if stoppedSocket != legacySocket {
		t.Fatalf("stopped socket=%q want=%q", stoppedSocket, legacySocket)
	}
}

func TestParseLinuxProcessIdentity(t *testing.T) {
	parentID, uid, ok := parseLinuxProcessIdentity("Name:\tdontstarve\nUid:\t10000\t10000\t10000\t10000\nPPid:\t1433\n")
	if !ok || parentID != 1433 || uid != 10000 {
		t.Fatalf("parent=%d uid=%d ok=%v", parentID, uid, ok)
	}
	if _, _, ok := parseLinuxProcessIdentity("Name:\tdontstarve\nPPid:\tnot-a-number\n"); ok {
		t.Fatal("invalid process status unexpectedly accepted")
	}
}

func TestTmuxControlRefusesAmbiguousLegacyStop(t *testing.T) {
	root := t.TempDir()
	legacySocket := shortLegacySocketForTest(t)
	control, err := NewTmuxControl(TmuxConfig{
		SaveRoot: root, ServerMode: "64", LegacyConsoleSockets: []string{legacySocket},
		ProcessProbe: func(context.Context) ([]shared.ShardProcessReport, error) {
			return []shared.ShardProcessReport{
				{PID: 42, Cluster: "room", Shard: "Caves", StorageRoot: root},
				{PID: 43, Cluster: "room", Shard: "Caves", StorageRoot: root},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	control.legacySessionProbe = func(server *dsttmux.DSTServer) (bool, error) {
		return server.SocketPath == legacySocket, nil
	}
	stopCalled := false
	control.legacyStop = func(*dsttmux.DSTServer) error {
		stopCalled = true
		return nil
	}
	if err := control.Stop(context.Background(), "room", "Caves"); err == nil || !strings.Contains(err.Error(), "无法唯一对应") {
		t.Fatalf("stop error=%v", err)
	}
	if stopCalled {
		t.Fatal("ambiguous legacy session was stopped")
	}
}

func TestTmuxControlDetectsDuplicateShardProcesses(t *testing.T) {
	root := t.TempDir()
	control, err := NewTmuxControl(TmuxConfig{
		SaveRoot: root, ServerMode: "64",
		ProcessProbe: func(context.Context) ([]shared.ShardProcessReport, error) {
			return []shared.ShardProcessReport{
				{PID: 42, Cluster: "room", Shard: "Master", StorageRoot: root},
				{PID: 43, Cluster: "room", Shard: "Master", StorageRoot: root},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	control.legacySessionProbe = func(*dsttmux.DSTServer) (bool, error) { return false, nil }
	server, err := control.server("room", "Master")
	if err != nil {
		t.Fatal(err)
	}
	status, err := control.inspectRuntimeOwnership(context.Background(), server, "room", "Master", RuntimeStatus{State: RuntimeRunning, SessionExists: true})
	if err != nil || status.Code != "DUPLICATE_DST_PROCESS_CONFLICT" || status.State != RuntimeFailed {
		t.Fatalf("status=%#v error=%v", status, err)
	}
}

func TestTmuxControlFailsClosedWhenOwnershipCannotBeInspected(t *testing.T) {
	control, err := NewTmuxControl(TmuxConfig{
		SaveRoot: t.TempDir(), ServerMode: "64",
		ProcessProbe: func(context.Context) ([]shared.ShardProcessReport, error) {
			return nil, fmt.Errorf("process list unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	control.legacySessionProbe = func(*dsttmux.DSTServer) (bool, error) { return false, nil }
	server, err := control.server("room", "Master")
	if err != nil {
		t.Fatal(err)
	}
	status, err := control.inspectRuntimeOwnership(context.Background(), server, "room", "Master", RuntimeStatus{State: RuntimeStopped})
	if err == nil || status.Code != "RUNTIME_OWNERSHIP_INSPECTION_FAILED" || status.State != RuntimeUnknown {
		t.Fatalf("status=%#v error=%v", status, err)
	}
}

func TestNewTmuxControlRejectsRuntimeNameAsServerArchitecture(t *testing.T) {
	_, err := NewTmuxControl(TmuxConfig{SaveRoot: t.TempDir(), ServerMode: "luajit"})
	if err == nil {
		t.Fatal("expected invalid server architecture to be rejected")
	}
}

func TestTmuxControlRechecksRuntimeModeBeforeStart(t *testing.T) {
	control := newTmuxControlForCacheTest(t)
	if err := control.requireRuntimeMode(shared.RuntimePerformanceModeGame); err != nil {
		t.Fatalf("Game Lua should always be available: %v", err)
	}
	if err := control.requireRuntimeMode(shared.RuntimePerformanceModeLuaJIT); !errors.Is(err, ErrRuntimeModeUnavailable) {
		t.Fatalf("missing LuaJIT package error = %v", err)
	}
	if err := control.requireRuntimeMode("future-mode"); !errors.Is(err, ErrInvalidRuntimeMode) {
		t.Fatalf("invalid runtime mode error = %v", err)
	}
}

func TestTmuxControlReusesShardServer(t *testing.T) {
	control := newTmuxControlForCacheTest(t)
	first, err := control.server("room", "Master")
	if err != nil {
		t.Fatal(err)
	}
	second, err := control.server("room", "Master")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same shard created more than one DST server handle")
	}
}

func TestTmuxControlCachesAndExpiresRuntimeStatus(t *testing.T) {
	control := newTmuxControlForCacheTest(t)
	now := time.Date(2026, time.August, 23, 13, 0, 0, 0, time.UTC)
	control.now = func() time.Time { return now }
	var calls atomic.Int32
	load := func() (RuntimeStatus, error) {
		calls.Add(1)
		return RuntimeStatus{State: RuntimeRunning, SessionExists: true}, nil
	}

	for range 2 {
		status, err := control.cachedRuntimeStatus(context.Background(), "room\x00Master", load)
		if err != nil || status.State != RuntimeRunning {
			t.Fatalf("cached status = %#v, error = %v", status, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("status loader called %d times, want 1", calls.Load())
	}

	now = now.Add(tmuxRuntimeStatusCacheTTL + time.Millisecond)
	if _, err := control.cachedRuntimeStatus(context.Background(), "room\x00Master", load); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expired status loader called %d times, want 2", calls.Load())
	}
}

func TestTmuxControlSharesConcurrentRuntimeStatus(t *testing.T) {
	control := newTmuxControlForCacheTest(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	load := func() (RuntimeStatus, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return RuntimeStatus{State: RuntimeRunning, SessionExists: true}, nil
	}

	const consumers = 8
	results := make(chan RuntimeStatus, consumers)
	for range consumers {
		go func() {
			status, _ := control.cachedRuntimeStatus(context.Background(), "room\x00Master", load)
			results <- status
		}()
	}
	<-started
	close(release)
	for range consumers {
		if status := <-results; status.State != RuntimeRunning {
			t.Fatalf("unexpected concurrent status: %#v", status)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent status loader called %d times, want 1", calls.Load())
	}
}

func TestTmuxControlInvalidationRejectsInFlightCache(t *testing.T) {
	control := newTmuxControlForCacheTest(t)
	key := control.shardKey("room", "Master")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		_, _ = control.cachedRuntimeStatus(context.Background(), key, func() (RuntimeStatus, error) {
			started <- struct{}{}
			<-release
			return RuntimeStatus{State: RuntimeStopped}, nil
		})
		close(firstDone)
	}()
	<-started
	control.invalidateRuntimeStatus("room", "Master")
	close(release)
	<-firstDone

	var calls atomic.Int32
	status, err := control.cachedRuntimeStatus(context.Background(), key, func() (RuntimeStatus, error) {
		calls.Add(1)
		return RuntimeStatus{State: RuntimeRunning, SessionExists: true}, nil
	})
	if err != nil || status.State != RuntimeRunning || calls.Load() != 1 {
		t.Fatalf("invalidated in-flight result remained cached: status=%#v calls=%d error=%v", status, calls.Load(), err)
	}
}
