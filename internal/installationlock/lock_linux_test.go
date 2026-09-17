//go:build linux

package installationlock

import (
	"bufio"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTwoWorldsShareLockAndExcludePackageWrites(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 2; i++ {
		cmd := exec.Command("flock", "-s", "-n", filepath.Join(root, Filename), "sh", "-c", "echo ready; read finish")
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { input.Close(); _ = cmd.Wait() })
		scanner := bufio.NewScanner(output)
		if !scanner.Scan() || scanner.Text() != "ready" {
			t.Fatal("world failed to obtain shared installation lock")
		}
	}
	if unlock, err := Acquire(root); !errors.Is(err, ErrBusy) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("installer wrote through running worlds: %v", err)
	}
}
