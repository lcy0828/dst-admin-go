package capabilities

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFindDSTExecutableAndReadinessRoomCount(t *testing.T) {
	root := t.TempDir()
	executablePath := filepath.Join(root, "bin64", "dontstarve_dedicated_server_nullrenderer_x64")
	if err := os.MkdirAll(filepath.Dir(executablePath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executablePath, []byte("test"), 0750); err != nil {
		t.Fatal(err)
	}
	if actual := findDSTExecutable(root); actual != executablePath {
		t.Fatalf("executable = %q, want %q", actual, executablePath)
	}
	readiness := ProbeReadiness(Config{SavePath: root, BackupPath: root, ServerPath: root}, func() (int, error) { return 3, nil })
	foundRooms := false
	for _, check := range readiness.Checks {
		if check.ID == "rooms" {
			foundRooms = check.Status == CheckPass && check.Details["count"] == 3
		}
	}
	if !foundRooms {
		t.Fatalf("room discovery check missing: %#v", readiness.Checks)
	}
}

func TestServerExecutableCheckRecognizesMacApplication(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Don't Starve Together")
	executablePath := filepath.Join(root, "dontstarve_steam.app", "Contents", "MacOS", "dontstarve_dedicated_server_nullrenderer")
	if err := os.MkdirAll(filepath.Dir(executablePath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executablePath, []byte("test"), 0750); err != nil {
		t.Fatal(err)
	}
	check := serverExecutableCheck(root, "64")
	if check.Status != CheckPass || check.Details["executable"] != executablePath || check.Details["layout"] != "macos-app" {
		t.Fatalf("macOS server check = %#v", check)
	}
}

func TestConfiguredMacDeploymentProbe(t *testing.T) {
	serverPath := os.Getenv("DST_ADMIN_TEST_REAL_SERVER_PATH")
	if serverPath == "" {
		t.Skip("set DST_ADMIN_TEST_REAL_SERVER_PATH to run the local deployment probe")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("macOS deployment probe")
	}
	report := Probe(Config{
		ServerPath:   serverPath,
		ServerMode:   "64",
		SteamCMDPath: os.Getenv("DST_ADMIN_TEST_REAL_STEAMCMD_PATH"),
	})
	if !report.Tools["tmux"].Available || !report.Tools["steamcmd"].Available || !report.Features["localShardControl"] {
		t.Fatalf("macOS deployment is incomplete: %#v", report)
	}
	check := serverExecutableCheck(serverPath, "64")
	if check.Status != CheckPass {
		t.Fatalf("macOS server executable is unavailable: %#v", check)
	}
	if check.Details["appId"] != "322330" || check.Details["updateMethod"] != "steam-client" || check.Details["updateSupported"] != false {
		t.Fatalf("macOS Steam update policy is incorrect: %#v", check.Details)
	}
	t.Logf("macOS deployment ready: executable=%s tmux=%s steamcmd=%s", check.Details["executable"], report.Tools["tmux"].Path, report.Tools["steamcmd"].Path)
}

func TestLuaFallbackCheckUsesInterpreterConfigAndDiagnosesMissingModulePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell test adapter is POSIX-only")
	}
	root := t.TempDir()
	luaBinary := filepath.Join(root, "lua-test")
	if err := os.WriteFile(luaBinary, []byte("#!/bin/sh\nprintf 'Lua test'\n"), 0750); err != nil {
		t.Fatal(err)
	}
	missingModules := filepath.Join(root, "missing-modules")
	config := Config{LuaBinary: luaBinary, LuaFallbackPath: missingModules}
	report := Probe(config)
	tool := report.Tools["luaFallback"]
	if !tool.Available || tool.Path != luaBinary || tool.Kind != "lua" {
		t.Fatalf("configured Lua was not reported: %#v", tool)
	}
	check := luaFallbackCheck(config, tool)
	if check.Status != CheckWarning || !strings.Contains(check.Summary, "模块目录不存在") || check.Details["modulePathAvailable"] != false {
		t.Fatalf("missing optional module directory was not diagnosed: %#v", check)
	}
}
