package luajit

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dont/internal/agents"
	"dont/internal/dstserver"
	"dont/shared"
)

// Minimal ELF metadata is sufficient for archive/architecture tests. These
// fixtures are never executed and do not stand in for a real DST smoke test.
func fixtureELF() []byte {
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 3)
	binary.LittleEndian.PutUint16(data[18:], 62)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint16(data[52:], 64)
	return data
}
func fixtureArchive(t *testing.T, modify func(map[string][]byte)) []byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range requiredFiles {
		files[name] = []byte("return {}\n")
	}
	files["modinfo.lua"] = []byte("version = \"3.0.0\"\n")
	files["dst_admin_runtime_modes.v1"] = []byte("DontStarveLuaJIT2 runtime mode contract v1\n-lua_vm_type=game|jit|jit_gen\n-luajit_enabled_jit=true|false\n")
	for _, name := range []string{"libInjector.so", "bin64/linux/lib64/libInjector.so", "plugins/plugin_core_vm/plugin_core_vm.so", "deps/liblua51DS.so"} {
		files[name] = fixtureELF()
	}
	if modify != nil {
		modify(files)
	}
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for name, value := range files {
		f, err := w.Create("Mod/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func fixtureStore(t *testing.T) (*Store, shared.LuaJITRelease, string) {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Save(context.Background(), bytes.NewReader(fixtureArchive(t, nil)), "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Path(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, r, p
}
func fixtureInstallation(t *testing.T, version bool) (Options, string) {
	t.Helper()
	root := t.TempDir()
	binaryPath := filepath.Join(root, "bin64", dstserver.BinaryX64)
	if err := writeFile(binaryPath, fixtureELF(), 0755); err != nil {
		t.Fatal(err)
	}
	if version {
		if err := writeFile(filepath.Join(root, "version.txt"), []byte("747465"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64", DependencyCheck: func(context.Context, string, string) error { return nil }}, binaryPath
}
func TestInstallReinstallAndRollback(t *testing.T) {
	_, release, archive := fixtureStore(t)
	o, game := fixtureInstallation(t, true)
	for attempt := 0; attempt < 2; attempt++ {
		report, err := Install(context.Background(), o, release, archive)
		if err != nil || !report.CanEnable || report.PackageVersion != "3.0.0" {
			t.Fatalf("install %d: %+v, %v", attempt, report, err)
		}
		original, err := os.ReadFile(game + "_1")
		if err != nil || !bytes.Equal(original, fixtureELF()) {
			t.Fatal("original game lost", err)
		}
		if _, err := os.Stat(filepath.Join(o.ServerPath, ".dst-admin-luajit-transaction")); !os.IsNotExist(err) {
			t.Fatal("transaction not cleaned", err)
		}
	}
	before, _ := os.ReadFile(game)
	if err := os.Remove(filepath.Join(o.ServerPath, "version.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), o, release, archive); err == nil {
		t.Fatal("unknown game version must fail post-install inspection")
	}
	after, _ := os.ReadFile(game)
	if !bytes.Equal(before, after) {
		t.Fatal("failed install changed launcher")
	}
	if _, err := os.Stat(game + "_1"); err != nil {
		t.Fatal("rollback lost original", err)
	}
}
func TestInstallRejectsBusyRunningAndSymlink(t *testing.T) {
	_, release, archive := fixtureStore(t)
	o, game := fixtureInstallation(t, true)
	unlock, err := AcquireInstallation(o.ServerPath, o.ServerMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), o, release, archive); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	unlock()
	o.Stopped = func(context.Context) error { return ErrRunning }
	if _, err := Install(context.Background(), o, release, archive); !errors.Is(err, ErrRunning) {
		t.Fatalf("running: %v", err)
	}
	o.Stopped = nil
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(o.ServerPath, "mods")); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), o, release, archive); err == nil {
		t.Fatal("symlink escape accepted")
	}
	data, _ := os.ReadFile(game)
	if !bytes.Equal(data, fixtureELF()) {
		t.Fatal("rejected install changed game")
	}
	items, _ := os.ReadDir(outside)
	if len(items) != 0 {
		t.Fatal("wrote outside installation")
	}
}

func TestRunningGameProcessBlocksInstallation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("installer process detection is supported on Linux")
	}
	o, game := fixtureInstallation(t, true)
	data, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Skip(err)
	}
	if err = writeFile(game, data, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(game, "30")
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err = o.CheckStopped(context.Background()); !errors.Is(err, ErrRunning) {
		t.Fatalf("live process not detected: %v", err)
	}
}
func TestInterruptedInstallRestoresBeforeRetry(t *testing.T) {
	o, game := fixtureInstallation(t, true)
	paths := []string{filepath.Join(o.ServerPath, "mods", "DontStarveLuaJIT2"), filepath.Join(o.ServerPath, "data", "unsafedata", "ds_luajit_injector.path"), filepath.Join(o.ServerPath, "bin64", "lib64", "libInjector.so"), game, game + "_1"}
	if _, err := beginTransaction(o.ServerPath, paths); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(game, []byte("interrupted"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := recoverTransaction(o.ServerPath, paths); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(game)
	if !bytes.Equal(data, fixtureELF()) {
		t.Fatal("crash recovery lost game")
	}
}
func TestArchiveRejectsTraversalMissingRuntimeWrongArchitecture(t *testing.T) {
	for _, name := range []string{"traversal", "runtime", "architecture", "version", "empty"} {
		t.Run(name, func(t *testing.T) {
			s, _ := NewStore(t.TempDir())
			archive := fixtureArchive(t, func(files map[string][]byte) {
				switch name {
				case "traversal":
					files["../../escaped"] = []byte("bad")
				case "runtime":
					delete(files, "plugins/init.lua")
				case "architecture":
					data := fixtureELF()
					binary.LittleEndian.PutUint16(data[18:], 183)
					files["libInjector.so"] = data
				case "version":
					files["modinfo.lua"] = []byte("version = 'unknown'")
				case "empty":
					files["plugins/plugin_core_vm/plugin_core_vm.so"] = nil
				}
			})
			if _, err := s.Save(context.Background(), bytes.NewReader(archive), ""); err == nil {
				t.Fatal("invalid archive accepted")
			}
			items, err := s.List()
			if err != nil || len(items) != 0 {
				t.Fatal("invalid package published", err)
			}
		})
	}
}
func TestDownloadRequiresBoundGrantAndVerifiesContent(t *testing.T) {
	s, release, archive := fixtureStore(t)
	token, err := s.Grant(release.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(release.ID, "wrong"); !errors.Is(err, os.ErrPermission) {
		t.Fatal("invalid grant accepted")
	}
	if _, err := s.Open(strings.Repeat("a", 64), token); !errors.Is(err, os.ErrPermission) {
		t.Fatal("cross-package grant accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		http.ServeFile(w, req, archive)
	}))
	defer server.Close()
	if _, err := Download(context.Background(), server.URL, token, t.TempDir(), release); err != nil {
		t.Fatal(err)
	}
	release.SHA256 = strings.Repeat("b", 64)
	if _, err := Download(context.Background(), server.URL, token, t.TempDir(), release); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	s.Revoke(token)
	if _, err := s.Open(release.ID, token); !errors.Is(err, os.ErrPermission) {
		t.Fatal("revoked grant accepted")
	}
}

type fixtureTargets struct {
	values       []agents.RuntimeTarget
	called       string
	installation string
}

func (f *fixtureTargets) RuntimeTargets() ([]agents.RuntimeTarget, error) { return f.values, nil }
func (f *fixtureTargets) ExecuteRuntime(_ context.Context, target string, r shared.RuntimeOperationRequest, _ int) (agents.RuntimeExecutionResult, error) {
	f.called, f.installation = target, r.InstallationID
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{LuaJIT: &shared.RuntimePerformanceReport{PackageVersion: "3.0.0"}}}, nil
}
func TestInspectionUsesExactRemoteInstallationAndNeverLocalFallback(t *testing.T) {
	store, _, _ := fixtureStore(t)
	targets := &fixtureTargets{values: []agents.RuntimeTarget{{ID: "agent:node", OS: "linux", Arch: "amd64", Online: true, Capabilities: []string{"runtime.luajit.v2"}, Installations: []agents.RuntimeInstallation{{ID: "first", ServerMode: "64"}, {ID: "second", ServerMode: "64"}}}}}
	s := NewService(store, store, targets, nil, nil)
	value, err := s.Inspect(context.Background(), "agent:node", "second")
	if err != nil || value.InstallationID != "second" || targets.called != "agent:node" || targets.installation != "second" {
		t.Fatalf("wrong target %+v, %v", value, err)
	}
	targets.called = ""
	if _, err := s.Inspect(context.Background(), "agent:missing", "second"); !errors.Is(err, agents.ErrRuntimeInstallationNotRegistered) {
		t.Fatal(err)
	}
	if targets.called != "" {
		t.Fatal("unknown target fell through")
	}
}

func TestBundledReleaseIsInstallable(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SeedBundled(context.Background()); err != nil {
		t.Fatal(err)
	}
	releases, err := s.List()
	if err != nil || len(releases) == 0 {
		t.Fatalf("build must include a usable package: %v", err)
	}
	o, _ := fixtureInstallation(t, true)
	archive, err := s.Path(releases[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Install(context.Background(), o, releases[0], archive)
	if err != nil || !report.CanEnable || report.PackageVersion != releases[0].Version {
		t.Fatalf("bundled package rejected: %+v, %v", report, err)
	}
}
