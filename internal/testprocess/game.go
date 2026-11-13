// Package testprocess provides native process fixtures for installation tests.
package testprocess

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// StartGame installs a real executable at path and waits until it is ready.
// Do not rename sleep: Ubuntu 26.04 ships a multicall coreutils executable
// which exits when launched under an unrecognized game executable name.
func StartGame(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	data, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0755); err != nil {
		t.Fatal(err)
	}
	// Both commands are shell builtins; no child process is left behind when
	// the fixture is killed. The open pipe keeps read blocked until cleanup.
	cmd := exec.Command(path, "-c", "printf 'ready\\n'; read hold")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	ready := make(chan error, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr == nil && line != "ready\n" {
			readErr = fmt.Errorf("unexpected process output: %q", line)
		}
		ready <- readErr
	}()
	select {
	case err = <-ready:
		if err != nil {
			t.Fatalf("game process fixture did not start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("game process fixture did not become ready")
	}
	return cmd
}
