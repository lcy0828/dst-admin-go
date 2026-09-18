package tempfiles

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A separate process proves the lock protects live requests and is released by
// the OS on abrupt exit, without relying on process-local bookkeeping.
func TestArtifactProcess(t *testing.T) {
	root := os.Getenv("DST_TEST_ARTIFACT_ROOT")
	if root == "" {
		return
	}
	area := Uploads
	if os.Getenv("DST_TEST_ARTIFACT_AREA") == "exports" {
		area = Exports
	}
	file, err := Create(context.Background(), root, area)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("temporary test artifact"); err != nil {
		t.Fatal(err)
	}
	fmt.Println(file.Name())
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	// Deliberately bypass Close to simulate a crash after receiving bytes.
	os.Exit(0)
}

func TestCleanupProtectsLiveRequestsAndRecoversCrashedProcesses(t *testing.T) {
	for _, area := range []Area{Uploads, Exports} {
		t.Run(fmt.Sprint(area), func(t *testing.T) {
			root := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestArtifactProcess$")
			kind := "uploads"
			if area == Exports {
				kind = "exports"
			}
			cmd.Env = append(os.Environ(), "DST_TEST_ARTIFACT_ROOT="+root, "DST_TEST_ARTIFACT_AREA="+kind)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			path := strings.TrimSpace(line)
			if err := Cleanup(root, area); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("live artifact removed: %v", err)
			}
			other, err := Create(context.Background(), root, area)
			if err != nil {
				t.Fatal(err)
			}
			if err := other.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("concurrent artifact removed: %v", err)
			}
			if _, err := fmt.Fprintln(stdin, "exit"); err != nil {
				t.Fatal(err)
			}
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("crash fixture missing: %v", err)
			}
			if err := Cleanup(root, area); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("abandoned artifact retained: %v", err)
			}
		})
	}
}

func TestCleanupPreservesUnknownFilesDirectoriesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".uploads")
	if err := os.MkdirAll(filepath.Join(dir, "upload-directory"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep.zip", "upload-orphan"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "save-fixture")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(target, filepath.Join(dir, "upload-link")) == nil
	if err := Cleanup(root, Uploads); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep.zip", "upload-directory"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if linked {
		if _, err := os.Lstat(filepath.Join(dir, "upload-link")); err != nil {
			t.Fatal(err)
		}
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "preserve" {
		t.Fatalf("save fixture changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "upload-orphan")); !os.IsNotExist(err) {
		t.Fatal("orphan retained")
	}
}

func TestCreateHonorsCancellationWhileCleanerOwnsDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".uploads")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lease, err := openLease(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := tryLock(lease, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := Create(ctx, root, Uploads); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("create: %v", err)
	}
}
