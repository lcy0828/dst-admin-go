//go:build linux || darwin

package softwareupdate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A real subprocess, not an in-memory health mock: each boot must have the
// supervisor's random identity, version and a distinct operating-system PID.
func TestSupervisorHelperProcess(t *testing.T) {
	if os.Getenv("DST_UPDATE_TEST_HELPER") != "1" {
		return
	}
	version := os.Getenv("DST_UPDATE_TEST_VERSION")
	if version == "crash" {
		if marker := os.Getenv("DST_UPDATE_TEST_CRASH_MARKER"); marker != "" {
			file, _ := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if file != nil {
				_, _ = file.WriteString("attempt\n")
				_ = file.Close()
			}
		}
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-DST-Admin-Boot-ID", os.Getenv("DST_ADMIN_BOOT_ID"))
		w.Header().Set("X-DST-Admin-Version", version)
		w.Header().Set("X-Test-Config", os.Getenv("DST_ADMIN_CONFIG"))
		fmt.Fprint(w, `{"data":{"authenticated":false}}`)
	})
	server := &http.Server{Addr: os.Getenv("DST_UPDATE_TEST_ADDRESS"), Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() { <-ctx.Done(); _ = server.Close() }()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		os.Exit(3)
	}
	os.Exit(0)
}

func testSupervisor(t *testing.T) *supervisor {
	t.Helper()
	root := t.TempDir()
	if err := ensureRoot(root); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{root: root, base: binary, baseVersion: "v1.0.0", platform: "linux-amd64", args: []string{"-test.run=^TestSupervisorHelperProcess$"}, environment: environmentWith(os.Environ(), map[string]string{"DST_UPDATE_TEST_HELPER": "1", "DST_UPDATE_TEST_VERSION": "v1.0.0", "DST_UPDATE_TEST_ADDRESS": address}), healthURL: "http://" + address + "/api/v2/auth/session", bootTimeout: 3 * time.Second, validate: func(context.Context, string, string, string) error { return nil }}
	state := diskState{Protocol: Protocol, Current: &installedRelease{Version: s.baseVersion}}
	if err := writeState(root, state); err != nil {
		t.Fatal(err)
	}
	s.committed = state.Current
	return s
}

func installTestRelease(t *testing.T, s *supervisor, version, behavior string) *installedRelease {
	t.Helper()
	// The wrapper delegates to the test executable, injecting candidate behavior.
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	data := []byte("#!/bin/sh\nexport DST_UPDATE_TEST_VERSION=" + quote(behavior) + "\nexec " + quote(s.base) + " \"$@\"\n")
	files := map[string][]byte{"dst-admin": data, "dst-map-renderer": []byte("#!/bin/sh\nexit 0\n"), "mod-local-setup": []byte("#!/bin/sh\nexit 0\n")}
	archive := filepath.Join(s.root, "fixture.tar.gz")
	if err := os.WriteFile(archive, testBundle(t, version, s.platform, files), 0600); err != nil {
		t.Fatal(err)
	}
	release := &installedRelease{ID: newID(), Version: version}
	directory := filepath.Join(s.root, "releases", release.ID)
	_ = os.Mkdir(directory, 0700)
	if err := extractBundle(archive, directory); err != nil {
		t.Fatal(err)
	}
	return release
}

func stageSwitch(t *testing.T, s *supervisor, target *installedRelease) {
	t.Helper()
	state, err := readState(s.root)
	if err != nil {
		t.Fatal(err)
	}
	state.Pending = target
	state.Operation = &Operation{ID: newID(), Version: target.Version, Phase: "restarting"}
	if err := writeState(s.root, state); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorUpdatesRecoveryAndLauncherRecreationPreserveRoom(t *testing.T) {
	s := testSupervisor(t)
	room := exec.Command("sleep", "120")
	if err := room.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = room.Process.Kill(); _ = room.Wait() }()
	save := filepath.Join(s.root, "existing-save")
	_ = os.WriteFile(save, []byte("room data"), 0600)
	child, bootID, err := s.start(s.committed)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.stop() }()
	if err := s.healthy(context.Background(), child, bootID, s.baseVersion); err != nil {
		t.Fatal(err)
	}
	oldPID := child.command.Process.Pid
	target := installTestRelease(t, s, "v1.1.0", "v1.1.0")
	stageSwitch(t, s, target)
	child, err = s.apply(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := readState(s.root)
	if child.command.Process.Pid == oldPID || state.Current.ID != target.ID || state.Previous.Version != s.baseVersion || state.Operation.Phase != "succeeded" {
		t.Fatal("new process was not committed")
	}
	// Stop and recreate the launcher; it must boot the persisted updated code.
	_ = child.stop()
	signals := make(chan os.Signal, 1)
	s.signals = signals
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("launcher did not stop")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(s.healthURL)
		ready := err == nil && response.Header.Get("X-DST-Admin-Version") == "v1.1.0"
		if response != nil {
			response.Body.Close()
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("updated version did not survive launcher recreation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A later failed update restores the verified program, not a manual downgrade.
	time.Sleep(600 * time.Millisecond)
	state, _ = readState(s.root)
	stageSwitch(t, s, installTestRelease(t, s, "v1.2.0", "crash"))
	signals <- syscall.SIGUSR1
	deadline = time.Now().Add(5 * time.Second)
	for {
		state, _ = readState(s.root)
		if state.Current.ID == target.ID && state.Operation.Phase == "rolled_back" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("automatic recovery timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := room.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("room process stopped")
	}
	if data, _ := os.ReadFile(save); string(data) != "room data" {
		t.Fatal("save changed")
	}
}

func TestSupervisorRestoresCommittedProcessOnCrashAndRejectedPreflight(t *testing.T) {
	for _, failure := range []string{"crash", "preflight"} {
		t.Run(failure, func(t *testing.T) {
			s := testSupervisor(t)
			child, bootID, err := s.start(s.committed)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.stop() }()
			if err := s.healthy(context.Background(), child, bootID, s.baseVersion); err != nil {
				t.Fatal(err)
			}
			target := installTestRelease(t, s, "v1.1.0", "crash")
			stageSwitch(t, s, target)
			if failure == "preflight" {
				s.validate = func(context.Context, string, string, string) error { return ErrInvalid }
			}
			oldPID := child.command.Process.Pid
			child, err = s.apply(context.Background(), child)
			if err != nil {
				t.Fatal(err)
			}
			state, _ := readState(s.root)
			if child.command.Process.Pid == oldPID || state.Current.Version != s.baseVersion || state.Pending != nil || state.Operation.Phase != "rolled_back" {
				t.Fatal("old service not restored")
			}
		})
	}
}

func TestSupervisorLockRejectsConcurrentLaunchers(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockSupervisor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if next, err := lockSupervisor(root); err == nil {
		next()
		t.Fatal("duplicate launcher accepted")
	}
}

func TestManagedParentSupportsContainerPIDOneAndRejectsStaleParent(t *testing.T) {
	if !managedParent(1, 1, "1") || !managedParent(42, 42, "1") || managedParent(42, 1, "1") || managedParent(0, 0, "1") || managedParent(1, 1, "") {
		t.Fatal("invalid launcher parent validation")
	}
}

func TestRecoveryRestoresCommittedPointersAfterPartialCommit(t *testing.T) {
	s := testSupervisor(t)
	committed := s.committed
	target := installTestRelease(t, s, "v1.1.0", "v1.1.0")
	// Simulate rename succeeding followed by fsync failure. Disk points at the
	// trial, while the launcher's committed version must remain the old code.
	state := diskState{Protocol: Protocol, Current: target, Previous: committed, Operation: &Operation{ID: newID(), Version: target.Version, Phase: "succeeded"}}
	if err := writeState(s.root, state); err != nil {
		t.Fatal(err)
	}
	child, err := s.recover(context.Background(), nil, committed, errors.New("directory sync failed"))
	if err != nil {
		t.Fatal(err)
	}
	defer child.stop()
	state, err = readState(s.root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Current.Version != s.baseVersion || state.Current.ID != "" || state.Previous != nil || state.Operation.Phase != "rolled_back" {
		t.Fatalf("disk and running version disagree: %+v", state)
	}
}

func TestSupervisorPinsConfigurationAcrossProgramDirectories(t *testing.T) {
	s := testSupervisor(t)
	s.configPath = filepath.Join(s.root, "operator.conf")
	s.environment = environmentWith(s.environment, map[string]string{"DST_ADMIN_CONFIG": "wrong.conf"})
	for _, release := range []*installedRelease{s.committed, installTestRelease(t, s, "v1.1.0", "v1.1.0")} {
		child, bootID, err := s.start(release)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.healthy(context.Background(), child, bootID, release.Version); err != nil {
			child.stop()
			t.Fatal(err)
		}
		response, err := http.Get(s.healthURL)
		if err != nil {
			child.stop()
			t.Fatal(err)
		}
		config := response.Header.Get("X-Test-Config")
		response.Body.Close()
		child.stop()
		if config != s.configPath {
			t.Fatal("operator configuration was rediscovered", config)
		}
	}
}

func TestFailedNewBaseIsNotRetriedOnSameImageRestart(t *testing.T) {
	s := testSupervisor(t)
	old := installTestRelease(t, s, "v1.1.0", "v1.1.0")
	state := diskState{Protocol: Protocol, BaseVersion: "v1.0.0", Current: old}
	if err := writeState(s.root, state); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(s.root, "failed-base-attempts")
	s.baseVersion = "v1.2.0"
	s.environment = environmentWith(s.environment, map[string]string{"DST_UPDATE_TEST_VERSION": "crash", "DST_UPDATE_TEST_CRASH_MARKER": marker})
	s.signals = make(chan os.Signal)
	for boot := 0; boot < 2; boot++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.run(ctx) }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			state, _ = readState(s.root)
			_, readyErr := os.Stat(filepath.Join(s.root, "ready.json"))
			if readyErr == nil && state.Current != nil && state.Current.ID == old.ID && state.BaseVersion == s.baseVersion {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				<-done
				t.Fatal("verified previous program did not recover")
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if state.BaseVersion != s.baseVersion {
			t.Fatal("failed base identity was not retained")
		}
	}
	attempts, _ := os.ReadFile(marker)
	if string(attempts) != "attempt\n" {
		t.Fatalf("failed base was retried on restart: %q", attempts)
	}
}
