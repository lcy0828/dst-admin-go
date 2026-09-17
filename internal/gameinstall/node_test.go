package gameinstall

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dont/internal/dstserver"
	"dont/internal/installationlock"
)

func writeGame(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin64"), 0755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	data[16] = 3
	data[18] = 62
	data[20] = 1
	data[52] = 64
	if err := os.WriteFile(filepath.Join(root, "bin64", dstserver.BinaryX64), data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte("747465\n"), 0644); err != nil {
		t.Fatal(err)
	}
}
func nodeOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	save := filepath.Join(root, "saves")
	if err = os.Mkdir(save, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(save, "user-save"), []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	steam, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	return Options{ServerPath: filepath.Join(root, "server"), SavePath: save, ServerMode: "64", Driver: "native", Platform: "linux", Architecture: "amd64", SteamCMDPath: steam}
}
func TestAdoptUsesExistingFilesAndPreservesSaves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory aliases")
	}
	o := nodeOptions(t)
	source := filepath.Join(filepath.Dir(o.ServerPath), "existing")
	writeGame(t, source)
	before, err := o.Probe(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(o.ServerPath); !os.IsNotExist(err) {
		t.Fatal("inspection created destination")
	}
	if err = os.Mkdir(o.ServerPath, 0755); err != nil {
		t.Fatal(err)
	}
	after, err := o.Adopt(context.Background(), source, before.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Installed || after.GameVersion != "747465" || after.ResolvedPath != source {
		t.Fatalf("report=%+v", after)
	}
	if target, err := os.Readlink(o.ServerPath); err != nil || target != source {
		t.Fatalf("alias=%s %v", target, err)
	}
	if b, err := os.ReadFile(filepath.Join(o.SavePath, "user-save")); err != nil || string(b) != "preserve me" {
		t.Fatal("user save changed", err)
	}
	// Every existing consumer can continue using the registered path, including
	// a newly created process after restart, without a mutable config overlay.
	if l, ok := dstserver.Resolve(o.ServerPath, "64"); !ok || l.InstallRoot != o.ServerPath {
		t.Fatal("registered path no longer resolves")
	}
}
func TestInspectDoesNotReportIncompleteGameAsInstalled(t *testing.T) {
	o := nodeOptions(t)
	writeGame(t, o.ServerPath)
	if !o.Inspect().Installed {
		t.Fatal("complete fixture was not detected")
	}
	if err := os.WriteFile(filepath.Join(o.ServerPath, "version.txt"), []byte("broken"), 0644); err != nil {
		t.Fatal(err)
	}
	if o.Inspect().Installed {
		t.Fatal("invalid version was accepted")
	}
	writeGame(t, o.ServerPath)
	if err := os.WriteFile(filepath.Join(o.ServerPath, "bin64", dstserver.BinaryX64), []byte("broken"), 0755); err != nil {
		t.Fatal(err)
	}
	if report := o.Inspect(); report.Installed || !report.CanInstall {
		t.Fatalf("damaged game should require repair: %+v", report)
	}
}
func TestAdoptRejectsChangedSourceOccupiedDestinationAndSaveOverlap(t *testing.T) {
	for _, kind := range []string{"source-changed", "occupied", "save-overlap", "running-lock"} {
		t.Run(kind, func(t *testing.T) {
			o := nodeOptions(t)
			source := filepath.Join(filepath.Dir(o.ServerPath), "existing")
			writeGame(t, source)
			p, err := o.Probe(source)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "source-changed":
				if err = os.WriteFile(filepath.Join(source, "version.txt"), []byte("747466"), 0644); err != nil {
					t.Fatal(err)
				}
			case "occupied":
				if err = os.Mkdir(o.ServerPath, 0755); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(o.ServerPath, "keep.txt"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "save-overlap":
				o.SavePath = filepath.Join(source, "saves")
			case "running-lock":
				unlock, e := installationlock.Acquire(source)
				if e != nil {
					t.Fatal(e)
				}
				defer unlock()
			}
			if _, err = o.Adopt(context.Background(), source, p.Fingerprint); err == nil {
				t.Fatal("unsafe adoption accepted")
			}
			if info, err := os.Lstat(o.ServerPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
				t.Fatal("destination was changed")
			}
			if kind == "occupied" {
				b, e := os.ReadFile(filepath.Join(o.ServerPath, "keep.txt"))
				if e != nil || string(b) != "keep" {
					t.Fatal("existing file changed")
				}
			}
		})
	}
}

type runnerFunc func(context.Context, string, []string, io.Writer) error

func (f runnerFunc) Run(ctx context.Context, p string, a []string, w io.Writer) error {
	return f(ctx, p, a, w)
}
func TestFreshInstallValidatesResultAndNeverTouchesSaves(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{true: "complete", false: "steam-exited-without-game"}[complete], func(t *testing.T) {
			o := nodeOptions(t)
			called := false
			o.Runner = runnerFunc(func(_ context.Context, exe string, args []string, _ io.Writer) error {
				called = true
				if exe != o.SteamCMDPath || strings.Join(args, " ") != "+force_install_dir "+o.ServerPath+" +login anonymous +app_update 343050 validate +quit" {
					t.Fatal("unexpected Steam command", exe, args)
				}
				if complete {
					writeGame(t, o.ServerPath)
				}
				return nil
			})
			r, err := o.Install(context.Background())
			if !called || (err == nil) != complete || r.Installed != complete {
				t.Fatalf("report=%+v err=%v called=%v", r, err, called)
			}
			b, e := os.ReadFile(filepath.Join(o.SavePath, "user-save"))
			if e != nil || string(b) != "preserve me" {
				t.Fatal("save changed")
			}
		})
	}
}
func TestInstallRefusesRunningNativeGame(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Linux process detection")
	}
	o := nodeOptions(t)
	writeGame(t, o.ServerPath)
	data, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(o.ServerPath, "bin64", dstserver.BinaryX64)
	if err = os.WriteFile(binary, data, 0755); err != nil {
		t.Fatal(err)
	}
	process := exec.Command(binary, "30")
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.Process.Kill(); _ = process.Wait() }()
	o.Runner = runnerFunc(func(context.Context, string, []string, io.Writer) error {
		t.Fatal("Steam ran while game was active")
		return nil
	})
	if _, err = o.Install(context.Background()); !errors.Is(err, ErrRunning) {
		t.Fatalf("expected running refusal, got %v", err)
	}
}
