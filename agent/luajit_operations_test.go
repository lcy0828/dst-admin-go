package agent

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"dont/internal/luajit"
	"dont/shared"
)

func TestLuaJITPayloadSeparatesNodeDownloadInstallAndRelay(t *testing.T) {
	id := strings.Repeat("a", 64)
	cases := []struct {
		name    string
		action  shared.RuntimeAction
		payload *shared.RuntimeLuaJITRequest
		valid   bool
	}{
		{"observe", shared.RuntimeActionLuaJITObserve, nil, true},
		{"refresh-upstream", shared.RuntimeActionLuaJITObserve, &shared.RuntimeLuaJITRequest{RefreshCatalog: true}, true},
		{"refresh-install-rejected", shared.RuntimeActionLuaJITInstall, &shared.RuntimeLuaJITRequest{RefreshCatalog: true, ReleaseID: id}, false},
		{"mixed-observe-rejected", shared.RuntimeActionLuaJITObserve, &shared.RuntimeLuaJITRequest{RefreshCatalog: true, ReleaseID: id}, false},
		{"cached-install", shared.RuntimeActionLuaJITInstall, &shared.RuntimeLuaJITRequest{ReleaseID: id}, true},
		{"direct-download", shared.RuntimeActionLuaJITDownload, &shared.RuntimeLuaJITRequest{SourceURL: "https://example.invalid/runtime.zip", SHA256: id}, true},
		{"http-rejected", shared.RuntimeActionLuaJITDownload, &shared.RuntimeLuaJITRequest{SourceURL: "http://example.invalid/runtime.zip", SHA256: id}, false},
		{"mixed-download", shared.RuntimeActionLuaJITDownload, &shared.RuntimeLuaJITRequest{SourceURL: "https://example.invalid/runtime.zip", SHA256: id, ReleaseID: id}, false},
		{"implicit-relay", shared.RuntimeActionLuaJITInstall, &shared.RuntimeLuaJITRequest{ReleaseID: id, DownloadToken: id}, false},
		{"install-url", shared.RuntimeActionLuaJITInstall, &shared.RuntimeLuaJITRequest{ReleaseID: id, SourceURL: "https://example.invalid/runtime.zip"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runtimeOperationRequest(tc.action)
			r.LuaJIT = tc.payload
			err := validateRuntimeOperationRequest(string(r.Action), r, 30, agentNow())
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	r := runtimeOperationRequest(shared.RuntimeActionLuaJITDownload)
	r.LuaJIT = &shared.RuntimeLuaJITRequest{SourceURL: "https://example.invalid/runtime.zip", SHA256: id}
	r.LeaseID = ""
	if err := validateRuntimeOperationRequest(string(r.Action), r, 540, agentNow()); err == nil {
		t.Fatal("download without lease accepted")
	}
}
func TestLuaJITCatalogReadOnly(t *testing.T) {
	t.Setenv("DST_ADMIN_LUAJIT_RELEASE_DIR", "")
	a, _ := newShardOperationAgent(t, &fakeShardRuntime{})
	r := runtimeOperationRequest(shared.RuntimeActionLuaJITObserve)
	r.Cluster, r.Shard = "MissingCluster", "MissingShard"
	result, err := a.executeRuntimeOperation(string(r.Action), &r, 30)
	if err != nil || len(result.LuaJITReleases) == 0 {
		t.Fatalf("catalog: %+v %v", result, err)
	}
	root := filepath.Join(filepath.Dir(a.Config.OperationStateFile), "luajit-releases")
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 0 {
		t.Fatalf("catalog materialized package: %v %v", files, err)
	}
}
func luaJITSourcePackage(t *testing.T) (shared.LuaJITRelease, string) {
	t.Helper()
	store, err := luajit.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SeedBundled(context.Background()); err != nil {
		t.Fatal(err)
	}
	releases, err := store.List()
	if err != nil || len(releases) == 0 {
		t.Fatal(err)
	}
	r, err := store.EnsureAvailable(context.Background(), releases[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r, path
}
func TestLuaJITOptionalRelayPersistsPackageOnAgent(t *testing.T) {
	a, _ := newShardOperationAgent(t, &fakeShardRuntime{})
	t.Setenv("DST_ADMIN_LUAJIT_RELEASE_DIR", t.TempDir())
	release, path := luaJITSourcePackage(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/luajit-packages/"+release.ID || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("b", 64) {
			http.Error(w, "invalid request", 403)
			return
		}
		http.ServeFile(w, r, path)
	}))
	defer server.Close()
	a.Config.ServerURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/agent"
	store, err := a.luaJITPackages()
	if err != nil {
		t.Fatal(err)
	}
	request := &shared.RuntimeLuaJITRequest{Release: &release, DownloadPath: "/luajit-packages/" + release.ID, DownloadToken: strings.Repeat("b", 64)}
	received, err := a.receiveLuaJITPackage(context.Background(), store, request)
	if err != nil || received != release {
		t.Fatalf("relay: %+v %v", received, err)
	}
	server.Close()
	got, err := store.Get(release.ID)
	if err != nil || got != release {
		t.Fatalf("relay did not persist: %+v %v", got, err)
	}
}
func TestLuaJITDirectCacheInstall(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("installer supports Linux amd64")
	}
	t.Setenv("DST_ADMIN_LUAJIT_RELEASE_DIR", t.TempDir())
	a, installation := newShardOperationAgent(t, &fakeShardRuntime{})
	release, path := luaJITSourcePackage(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("direct download leaked controller credentials")
		}
		http.ServeFile(w, r, path)
	}))
	defer server.Close()
	original := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = original })
	a.Config.ServerURL = "ws://127.0.0.1:1/unreachable-controller"
	r := runtimeOperationRequest(shared.RuntimeActionLuaJITDownload)
	r.Cluster, r.Shard = "MissingCluster", "MissingShard"
	r.LuaJIT = &shared.RuntimeLuaJITRequest{SourceURL: server.URL + "/runtime.zip", SHA256: release.SHA256}
	result, err := a.executeRuntimeOperation(string(r.Action), &r, 540)
	if err != nil || result.Outcome != shared.RuntimeOutcomeConfirmed || result.LuaJITRelease == nil || *result.LuaJITRelease != release {
		t.Fatalf("node download: %+v %v", result, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("source requests=%d", calls.Load())
	}
	store, err := a.luaJITPackages()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(release.ID); err != nil || got != release {
		t.Fatalf("uncached package: %+v %v", got, err)
	}
	// A bad digest must not publish a package.
	r.OperationID, r.OperationKey = "bad-download", "bad-download-key"
	r.LuaJIT.SHA256 = strings.Repeat("f", 64)
	if _, err = a.executeRuntimeOperation(string(r.Action), &r, 540); err == nil {
		t.Fatal("bad digest accepted")
	}
	if _, err = store.Get(r.LuaJIT.SHA256); !os.IsNotExist(err) {
		t.Fatal("bad package cached")
	}
	server.Close()
	// Fixture game is never executed; this checks real-package installation only.
	elf := make([]byte, 64)
	copy(elf, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(elf[16:], 3)
	binary.LittleEndian.PutUint16(elf[18:], 62)
	binary.LittleEndian.PutUint32(elf[20:], 1)
	binary.LittleEndian.PutUint16(elf[52:], 64)
	if err = os.MkdirAll(filepath.Join(installation.ServerPath, "bin64"), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(installation.ServerPath, "bin64", "dontstarve_dedicated_server_nullrenderer_x64"), elf, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(installation.ServerPath, "version.txt"), []byte("747465"), 0644); err != nil {
		t.Fatal(err)
	}
	// The real package links to the game's Steam library. This fixture never
	// executes the game, but needs a loadable library for dependency preflight.
	libdir := filepath.Join(installation.ServerPath, "bin64", "lib64")
	if err = os.MkdirAll(libdir, 0755); err != nil {
		t.Fatal(err)
	}
	if source := os.Getenv("DST_ADMIN_TEST_STEAM_API"); source != "" {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(libdir, "libsteam_api.so"), data, 0755); err != nil {
			t.Fatal(err)
		}
	} else {
		cc, err := exec.LookPath("cc")
		if err != nil {
			t.Skip("set DST_ADMIN_TEST_STEAM_API or install a C compiler for the fixture Steam shared library")
		}
		command := exec.Command(cc, "-shared", "-fPIC", "-x", "c", "-", "-o", filepath.Join(libdir, "libsteam_api.so"))
		command.Stdin = strings.NewReader("int fixture_steam_api(void) { return 0; }\n")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("Steam fixture: %s %v", output, err)
		}
	}
	r = runtimeOperationRequest(shared.RuntimeActionLuaJITInstall)
	r.OperationID, r.OperationKey = "cached-install", "cached-install-key"
	r.LuaJIT = &shared.RuntimeLuaJITRequest{ReleaseID: release.ID}
	result, err = a.executeRuntimeOperation(string(r.Action), &r, 540)
	if err != nil || result.LuaJIT == nil || !result.LuaJIT.CanEnable {
		t.Fatalf("cached install: %+v %v", result, err)
	}
	replay, err := a.executeRuntimeOperation(string(r.Action), &r, 540)
	if err != nil || !replay.Idempotent {
		t.Fatalf("install replay: %+v %v", replay, err)
	}
}
