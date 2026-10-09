package softwareupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAgentLauncherHelperProcess(t *testing.T) {
	if os.Getenv("DST_AGENT_UPDATE_TEST_HELPER") != "1" {
		return
	}
	version := "v1.2.2"
	if value := os.Getenv("DST_AGENT_UPDATE_TEST_BASE_VERSION"); value != "" {
		version = value
	}
	binary, _ := os.Executable()
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(binary), "manifest.json")); err == nil {
		var manifest Manifest
		if json.Unmarshal(data, &manifest) == nil {
			version = manifest.Version
		}
	}
	if version == "v9.9.9" {
		if marker := os.Getenv("DST_AGENT_UPDATE_TEST_CRASH_MARKER"); marker != "" {
			file, _ := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if file != nil {
				_, _ = file.WriteString("attempt\n")
				_ = file.Close()
			}
		}
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	closeHealth, err := ServeAgentHealth(version, func() bool { return version != "v8.8.8" }, cancel)
	if err != nil {
		os.Exit(3)
	}
	<-ctx.Done()
	closeHealth()
	os.Exit(0)
}

func testAgentSupervisor(t *testing.T) *supervisor {
	t.Helper()
	root := t.TempDir()
	if err := ensureRoot(root); err != nil {
		t.Fatal(err)
	}
	ipc, signals, address, closeIPC, err := newAgentLauncherIPC()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeIPC)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{root: root, base: binary, baseVersion: "v1.2.2", kind: "agent", platform: runtime.GOOS + "-" + runtime.GOARCH, agent: ipc, signals: signals, args: []string{"-test.run=^TestAgentLauncherHelperProcess$"}, environment: environmentWith(os.Environ(), map[string]string{"DST_AGENT_UPDATE_TEST_HELPER": "1", "DST_ADMIN_AGENT_LAUNCHER_URL": address, "DST_ADMIN_AGENT_LAUNCHER_TOKEN": ipc.token}), bootTimeout: 2 * time.Second, stableDuration: time.Millisecond, validate: func(context.Context, string, string, string) error { return nil }}
	state := diskState{Protocol: Protocol, Current: &installedRelease{Version: s.baseVersion}}
	if err := writeState(root, state); err != nil {
		t.Fatal(err)
	}
	s.committed = state.Current
	return s
}

func installAgentFixture(t *testing.T, s *supervisor, version string) *installedRelease {
	t.Helper()
	data, err := os.ReadFile(s.base)
	if err != nil {
		t.Fatal(err)
	}
	archive := agentFixture(t, s.root, s.platform, version, data)
	release := &installedRelease{ID: newID(), Version: version}
	directory := filepath.Join(s.root, "releases", release.ID)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := extractBundleFor(archive, directory, "agent", s.platform); err != nil {
		t.Fatal(err)
	}
	return release
}

func stageAgentSwitch(t *testing.T, s *supervisor, target *installedRelease) {
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

func TestAgentPortableLauncherUpdateRecoveryAndPersistence(t *testing.T) {
	s := testAgentSupervisor(t)
	save := filepath.Join(s.root, "save-sentinel")
	if err := os.WriteFile(save, []byte("preserve all save bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	child, boot, err := s.start(s.committed)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child != nil {
			_ = child.stop()
		}
	}()
	if err := s.healthy(context.Background(), child, boot, s.baseVersion); err != nil {
		t.Fatal(err)
	}
	target := installAgentFixture(t, s, "v1.2.3")
	stageAgentSwitch(t, s, target)
	oldPID := child.command.Process.Pid
	child, err = s.apply(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	state, err := readState(s.root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Current.ID != target.ID || state.Previous.Version != s.baseVersion || state.Operation.Phase != "succeeded" || child.command.Process.Pid == oldPID {
		t.Fatalf("Agent update did not commit: %+v", state)
	}
	_ = child.stop()
	// A recreated stable launcher selects the persisted executable.
	child, boot, err = s.start(state.Current)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.healthy(context.Background(), child, boot, target.Version); err != nil {
		t.Fatal(err)
	}
	stageAgentSwitch(t, s, installAgentFixture(t, s, "v9.9.9"))
	child, err = s.apply(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	state, _ = readState(s.root)
	if state.Current.ID != target.ID || state.Operation.Phase != "rolled_back" {
		t.Fatal("Agent automatic recovery failed")
	}
	data, _ := os.ReadFile(save)
	if string(data) != "preserve all save bytes" {
		t.Fatal("save data changed")
	}
}

func TestAgentRecoveredProgramSurvivesSameBaseRestart(t *testing.T) {
	s := testAgentSupervisor(t)
	old := installAgentFixture(t, s, "v1.3.0")
	if err := writeState(s.root, diskState{Protocol: Protocol, BaseVersion: s.baseVersion, Current: old}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(s.root, "failed-base-attempts")
	s.baseVersion = "v9.9.9"
	s.environment = environmentWith(s.environment, map[string]string{"DST_AGENT_UPDATE_TEST_BASE_VERSION": s.baseVersion, "DST_AGENT_UPDATE_TEST_CRASH_MARKER": marker})
	for boot := 0; boot < 2; boot++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.run(ctx) }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			state, _ := readState(s.root)
			_, readyErr := os.Stat(filepath.Join(s.root, "ready.json"))
			if readyErr == nil && state.Current.ID == old.ID && state.BaseVersion == s.baseVersion {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				<-done
				t.Fatal("Agent did not restore its verified program")
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	attempts, _ := os.ReadFile(marker)
	if string(attempts) != "attempt\n" {
		t.Fatalf("failed Agent base was retried: %q", attempts)
	}
}

func TestAgentPortableLauncherRestoresCrashAndFailedReconnect(t *testing.T) {
	for _, version := range []string{"v9.9.9", "v8.8.8"} {
		t.Run(version, func(t *testing.T) {
			s := testAgentSupervisor(t)
			child, boot, err := s.start(s.committed)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if child != nil {
					_ = child.stop()
				}
			}()
			if err := s.healthy(context.Background(), child, boot, s.baseVersion); err != nil {
				t.Fatal(err)
			}
			stageAgentSwitch(t, s, installAgentFixture(t, s, version))
			child, err = s.apply(context.Background(), child)
			if err != nil {
				t.Fatal(err)
			}
			state, _ := readState(s.root)
			if state.Current.Version != s.baseVersion || state.Pending != nil || state.Operation.Phase != "rolled_back" {
				t.Fatal("old Agent was not restored")
			}
		})
	}
}

func TestAgentLauncherIPCRejectsUnauthenticatedRequestsAndExternalEndpoints(t *testing.T) {
	ipc, signals, address, closeIPC, err := newAgentLauncherIPC()
	if err != nil {
		t.Fatal(err)
	}
	defer closeIPC()
	for _, endpoint := range []string{"/apply", "/register"} {
		response, err := http.Post(address+endpoint, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatal("unauthenticated launcher command accepted")
		}
	}
	request, _ := http.NewRequest(http.MethodPost, address+"/register", bytes.NewBufferString(`{"bootId":"1234567890abcdef1234567890abcdef","url":"http://example.com/health"}`))
	request.Header.Set("Authorization", "Bearer "+ipc.token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("external health endpoint accepted")
	}
	select {
	case <-signals:
		t.Fatal("unauthenticated restart signaled")
	default:
	}
}
