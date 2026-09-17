package tmux

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSessionStartBypassesNonLoginShell(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	// macOS has a short Unix socket path limit, so use a short temporary name.
	root := t.TempDir()
	socket := filepath.Join(root, "s")
	command := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command("tmux", append([]string{"-S", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %s: %v", args, out, err)
		}
		return out
	}
	command("new-session", "-d", "-s", "keeper", "/bin/sh", "-c", "sleep 30")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	command("set-option", "-g", "default-shell", "/usr/bin/false")
	server := &DSTServer{SessionName: "dst-managed", SocketPath: socket}
	if out, err := server.createSession(root, "sleep 30"); err != nil {
		t.Fatalf("start: %s: %v", out, err)
	}
	if id, err := server.RuntimeInstanceID(); err != nil || id == "" {
		t.Fatalf("instance=%q err=%v", id, err)
	}
	if string(command("show-options", "-gv", "default-shell")) != "/usr/bin/false\n" {
		t.Fatal("changed default shell")
	}
}
