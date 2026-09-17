package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	dstinstall "dont/internal/dstserver"
)

func TestConsoleTransportThroughLaunchWrappers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux launch uses flock")
	}
	for _, command := range []string{"tmux", "flock", "pgrep"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skip(command + " unavailable")
		}
	}
	root, err := os.MkdirTemp("/tmp", "dst-console-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// A copy of sh supplies a real executable identity and a stdin consumer,
	// without needing DST or starting a game in this integration test.
	game := filepath.Join(root, "dontstarve_dedicated_server_nullrenderer_x64_1")
	data, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(game, data, 0700); err != nil {
		t.Fatal(err)
	}
	server, err := NewDSTServerWithSocketAndSessionName("room", "Master", "managed", filepath.Join(root, "tmux.sock"), "", root, "saves", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", server.SocketPath, "kill-server").Run() })
	received := filepath.Join(root, "received")
	script := "while IFS= read -r line; do printf '%s\\n' \"$line\" > " + shellArg(received) + "; done"
	command := buildStartCommandForOS("linux", dstinstall.Layout{InstallRoot: root}, game, []string{"-c", script, "--", "-cluster", "room", "-shard", "Master"})
	if output, err := server.createSession(root, command); err != nil {
		t.Fatalf("start: %s %v", output, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, _, err := server.ConsoleTransportHealth()
		if err == nil && status == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("console=%s err=%v", status, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	output, err := exec.Command("tmux", "-S", server.SocketPath, "display-message", "-p", "-t", "=managed:0.0", "#{pane_pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !server.consoleGameDescendant(int32(pid)) {
		t.Fatal("wrapped game not found")
	}
	wrong := &DSTServer{ArchiveName: server.ArchiveName, WorldName: "Caves"}
	if wrong.consoleGameDescendant(int32(pid)) {
		t.Fatal("accepted another shard's game")
	}
	worldPath := filepath.Join(root, "saves", "room", "Master")
	if err := os.MkdirAll(worldPath, 0700); err != nil {
		t.Fatal(err)
	}
	log := "[00:00:00]: Current time: " + time.Now().UTC().Format("Mon Jan 2 15:04:05 2006") + "\n[00:00:01]: Server registered via geo DNS\n"
	if err := os.WriteFile(filepath.Join(worldPath, "server_log.txt"), []byte(log), 0600); err != nil {
		t.Fatal(err)
	}
	if err := server.SendCommand("console-probe"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		data, _ := os.ReadFile(received)
		if string(data) == "console-probe\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("game did not receive input: %q", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A shell with no DST child must continue to reject console input.
	if output, err := exec.Command("tmux", "-S", server.SocketPath, "new-session", "-d", "-s", "unrelated", "/bin/sh", "-c", "sleep 30; :").CombinedOutput(); err != nil {
		t.Fatalf("unrelated: %s %v", output, err)
	}
	server.SessionName = "unrelated"
	if status, _, err := server.ConsoleTransportHealth(); err != nil || status != "process_mismatch" {
		t.Fatalf("unrelated console=%s err=%v", status, err)
	}
}
