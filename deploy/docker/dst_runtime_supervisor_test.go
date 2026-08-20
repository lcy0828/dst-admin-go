package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRuntimeSupervisorReadsPaneExitStatus(t *testing.T) {
	tests := []struct {
		name         string
		emptyReads   int
		retryLimit   int
		wantExitCode int
		wantError    string
	}{
		{name: "status available immediately", retryLimit: 3, wantExitCode: 0},
		{name: "status becomes available after race", emptyReads: 2, retryLimit: 3, wantExitCode: 0},
		{name: "status remains unavailable", emptyReads: 3, retryLimit: 3, wantExitCode: 70, wantError: "unable to read DST runtime exit status from wrapper or tmux after 3 attempts"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			binDir := filepath.Join(tempDir, "bin")
			if err := os.Mkdir(binDir, 0o755); err != nil {
				t.Fatal(err)
			}

			tmuxPath := filepath.Join(binDir, "tmux")
			if err := os.WriteFile(tmuxPath, []byte(fakeTmux), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("/bin/sh", "dst-runtime-supervisor.sh")
			cmd.Dir = "."
			cmd.Env = append(os.Environ(),
				"PATH="+binDir+":"+os.Getenv("PATH"),
				"DST_RUNTIME_STATE_DIR="+filepath.Join(tempDir, "state"),
				"DST_RUNTIME_INSTANCE_ID=test-instance",
				"DST_SUPERVISOR_STATUS_RETRY_LIMIT="+strconv.Itoa(tt.retryLimit),
				"DST_SUPERVISOR_STATUS_RETRY_DELAY=0",
				"FAKE_TMUX_STATE="+filepath.Join(tempDir, "tmux-state"),
				"FAKE_TMUX_EMPTY_STATUS_READS="+strconv.Itoa(tt.emptyReads),
			)

			output, err := cmd.CombinedOutput()
			gotExitCode := 0
			if err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run supervisor: %v", err)
				}
				gotExitCode = exitErr.ExitCode()
			}
			if gotExitCode != tt.wantExitCode {
				t.Fatalf("exit code = %d, want %d; output:\n%s", gotExitCode, tt.wantExitCode, output)
			}
			if tt.wantError != "" && !strings.Contains(string(output), tt.wantError) {
				t.Fatalf("output %q does not contain %q", output, tt.wantError)
			}
		})
	}
}

const fakeTmux = `#!/bin/sh
set -eu

shift 2
command=$1
shift

case "$command" in
  display-message)
    case "$*" in
      *pane_dead_status*)
        count=0
        if [ -f "$FAKE_TMUX_STATE" ]; then
          count="$(cat "$FAKE_TMUX_STATE")"
        fi
        count=$((count + 1))
        printf '%s\n' "$count" > "$FAKE_TMUX_STATE"
        if [ "$count" -gt "$FAKE_TMUX_EMPTY_STATUS_READS" ]; then
          printf '0\n'
        fi
        ;;
      *pane_dead*) printf '1\n' ;;
    esac
    ;;
  capture-pane) printf 'fake DST pane output\n' ;;
esac
`
