package docker

import (
	"os"
	"os/exec"
	"path/filepath"
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
