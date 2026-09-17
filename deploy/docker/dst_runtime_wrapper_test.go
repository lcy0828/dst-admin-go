package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestRuntimeWrapperPersistsExecutableExitStatus(t *testing.T) {
	for _, wantExitCode := range []int{0, 7} {
		t.Run(strconv.Itoa(wantExitCode), func(t *testing.T) {
			tempDir := t.TempDir()
			executable := filepath.Join(tempDir, "fake-dst")
			contents := "#!/bin/sh\nexit " + strconv.Itoa(wantExitCode) + "\n"
			if err := os.WriteFile(executable, []byte(contents), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("/bin/sh", "dst-runtime-wrapper.sh")
			cmd.Dir = "."
			cmd.Env = append(os.Environ(),
				"DST_CLUSTER=test-cluster",
				"DST_SHARD=Master",
				"DST_CONF_DIR=.",
				"DST_STORAGE_ROOT="+tempDir,
				"DST_SERVER_ROOT="+tempDir,
				"DST_EXECUTABLE="+executable,
				"DST_RUNTIME_STATE_DIR="+tempDir,
			)

			err := cmd.Run()
			gotExitCode := 0
			if err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run wrapper: %v", err)
				}
				gotExitCode = exitErr.ExitCode()
			}
			if gotExitCode != wantExitCode {
				t.Fatalf("exit code = %d, want %d", gotExitCode, wantExitCode)
			}

			status, err := os.ReadFile(filepath.Join(tempDir, "runtime-exit-status"))
			if err != nil {
				t.Fatalf("read persisted status: %v", err)
			}
			if got := strings.TrimSpace(string(status)); got != strconv.Itoa(wantExitCode) {
				t.Fatalf("persisted status = %q, want %d", got, wantExitCode)
			}
		})
	}
}

func TestRuntimeWrapperForwardsSelectedLuaMode(t *testing.T) {
	tests := map[string][]string{
		"game":           {"-lua_vm_type=game"},
		"luajit-jit-off": {"-lua_vm_type=jit", "-luajit_enabled_jit=false"},
		"luajit-jit-on":  {"-lua_vm_type=jit", "-luajit_enabled_jit=true"},
		"arena-gc":       {"-lua_vm_type=jit_gen", "-luajit_enabled_jit=false"},
	}
	for mode, expected := range tests {
		t.Run(mode, func(t *testing.T) {
			tempDir := t.TempDir()
			executable := filepath.Join(tempDir, "fake-dst")
			argumentsFile := filepath.Join(tempDir, "arguments")
			contents := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DST_ARGUMENTS_FILE\"\n"
			if err := os.WriteFile(executable, []byte(contents), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "dst-runtime-wrapper.sh")
			cmd.Dir = "."
			cmd.Env = append(os.Environ(),
				"DST_CLUSTER=test-cluster", "DST_SHARD=Master", "DST_CONF_DIR=DoNotStarveTogether",
				"DST_STORAGE_ROOT="+tempDir, "DST_SERVER_ROOT="+tempDir, "DST_EXECUTABLE="+executable,
				"DST_RUNTIME_STATE_DIR="+tempDir, "DST_RUNTIME_MODE="+mode, "DST_ARGUMENTS_FILE="+argumentsFile,
				"DST_UGC_DIRECTORY="+filepath.Join(tempDir, "shared-workshop"),
			)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("wrapper failed: %v: %s", err, output)
			}
			argumentData, err := os.ReadFile(argumentsFile)
			if err != nil {
				t.Fatal(err)
			}
			arguments := strings.Fields(string(argumentData))
			for _, value := range expected {
				if !slices.Contains(arguments, value) {
					t.Fatalf("arguments=%v missing=%s", arguments, value)
				}
			}
			ugc := filepath.Join(tempDir, "DoNotStarveTogether", ".dst-admin", "runtime", "workshop", "test-cluster", "Master")
			index := slices.Index(arguments, "-ugc_directory")
			if index < 0 || index+1 >= len(arguments) || filepath.Clean(arguments[index+1]) != ugc || !slices.Contains(arguments, "-skip_update_server_mods") {
				t.Fatalf("ordinary launch did not isolate Workshop state and skip downloads: %v", arguments)
			}
			if info, err := os.Stat(ugc); err != nil || !info.IsDir() {
				t.Fatalf("world Steam state directory not ready: %v", err)
			}
		})
	}
}

func TestRuntimeWrapperConsumesOneShotSkipModUpdateOption(t *testing.T) {
	tempDir := t.TempDir()
	executable := filepath.Join(tempDir, "fake-dst")
	argumentsFile := filepath.Join(tempDir, "arguments")
	launchOptionsFile := filepath.Join(tempDir, "launch-options")
	contents := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DST_ARGUMENTS_FILE\"\n"
	if err := os.WriteFile(executable, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchOptionsFile, []byte("skip_update_server_mods=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "dst-runtime-wrapper.sh")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(),
		"DST_CLUSTER=test-cluster", "DST_SHARD=Master", "DST_CONF_DIR=DoNotStarveTogether",
		"DST_STORAGE_ROOT="+tempDir, "DST_SERVER_ROOT="+tempDir, "DST_EXECUTABLE="+executable,
		"DST_RUNTIME_STATE_DIR="+tempDir, "DST_ARGUMENTS_FILE="+argumentsFile,
		"DST_LAUNCH_OPTIONS_FILE="+launchOptionsFile,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper failed: %v: %s", err, output)
	}
	data, err := os.ReadFile(argumentsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Fields(string(data)), "-skip_update_server_mods") {
		t.Fatalf("arguments=%q", data)
	}
	if _, err := os.Stat(launchOptionsFile); !os.IsNotExist(err) {
		t.Fatalf("one-shot launch option was not consumed: %v", err)
	}
}
