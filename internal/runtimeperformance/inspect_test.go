package runtimeperformance

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"dont/shared"
)

func TestInspectReportsUnmodifiedInstallation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte("747465\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := Inspect(Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64"})
	if report.Status != shared.RuntimePerformanceNotInstalled || report.CanEnable || report.Provider != "game" || report.GameVersion != "747465" || len(report.Issues) != 0 {
		t.Fatalf("report=%#v", report)
	}
}

func TestInspectKeepsLegacyLayoutUnverified(t *testing.T) {
	root := makeLinuxFixture(t, "747465")
	report := Inspect(Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64"})
	if report.Status != shared.RuntimePerformanceDetectedUnverified || report.CanEnable || report.PackageVersion != "2.9.1" || report.GameVersion != "747465" || report.SignatureVersion != "747465" || len(report.BinarySHA256) != 64 || !slices.Contains(report.Issues, "legacy_layout_unverified") {
		t.Fatalf("report=%#v", report)
	}
	if !slices.Equal(report.SupportedModes, []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame}) {
		t.Fatalf("modes=%#v", report.SupportedModes)
	}
}

func TestInspectBlocksMismatchedSignature(t *testing.T) {
	root := makeLinuxFixture(t, "728321")
	report := Inspect(Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64"})
	if report.Status != shared.RuntimePerformanceIncompatible || report.CanEnable || !slices.Contains(report.Issues, "signature_version_mismatch") {
		t.Fatalf("report=%#v", report)
	}
}

func TestInspectTreatsUnknownGameVersionAsUnverified(t *testing.T) {
	root := makeLinuxFixture(t, "747465")
	if err := os.Remove(filepath.Join(root, "version.txt")); err != nil {
		t.Fatal(err)
	}
	report := Inspect(Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64"})
	if report.Status != shared.RuntimePerformanceDetectedUnverified || report.CanEnable || !slices.Contains(report.Issues, "game_version_unknown") || !slices.Contains(report.Issues, "legacy_layout_unverified") {
		t.Fatalf("report=%#v", report)
	}
}

func TestInspectEnablesVerifiedLinuxPluginLayoutBeforeColdSignatureGeneration(t *testing.T) {
	root := makeLinuxPluginFixture(t, false)
	report := Inspect(Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64"})
	if report.Status != shared.RuntimePerformanceReady || !report.CanEnable || !report.AutomaticSignatures || report.Provider != "dontstarve-luajit2" || report.PackageVersion != "3.0.0" || report.GameVersion != "747465" || report.SignatureVersion != "" || len(report.BinarySHA256) != 64 {
		t.Fatalf("report=%#v", report)
	}
	if len(report.Issues) != 0 || !slices.Contains(report.SupportedModes, shared.RuntimePerformanceModeLuaJIT) {
		t.Fatalf("report=%#v", report)
	}
}

func TestInspectKeepsVerifiedPluginLayoutReadyAfterGeneratedSignature(t *testing.T) {
	root := makeLinuxPluginFixture(t, true)
	report := Inspect(Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64"})
	if report.Status != shared.RuntimePerformanceReady || !report.CanEnable || report.SignatureVersion != "747465" || len(report.Issues) != 0 {
		t.Fatalf("report=%#v", report)
	}
}

func TestInspectAcceptsUpstreamWithoutCustomMarker(t *testing.T) {
	root := makeLinuxPluginFixture(t, false)
	if err := os.Remove(filepath.Join(root, "mods", "DontStarveLuaJIT2", "dst_admin_runtime_modes.v1")); err != nil {
		t.Fatal(err)
	}
	report := Inspect(Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64"})
	if report.Status != shared.RuntimePerformanceReady || !report.CanEnable || len(report.Issues) != 0 {
		t.Fatalf("report=%#v", report)
	}
}

func TestInspectIgnoresObsoleteCustomMarker(t *testing.T) {
	root := makeLinuxPluginFixture(t, false)
	marker := filepath.Join(root, "mods", "DontStarveLuaJIT2", "dst_admin_runtime_modes.v1")
	if err := os.WriteFile(marker, []byte("DontStarveLuaJIT2 runtime mode contract v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := Inspect(Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64"})
	if report.Status != shared.RuntimePerformanceReady || !report.CanEnable || !report.AutomaticSignatures || len(report.Issues) != 0 {
		t.Fatalf("report=%#v", report)
	}
}

func TestInspectRejectsInvalidPluginInjectorMarker(t *testing.T) {
	root := makeLinuxPluginFixture(t, false)
	marker := filepath.Join(root, "data", "unsafedata", "ds_luajit_injector.path")
	if err := os.WriteFile(marker, []byte("relative/libInjector.so\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := Inspect(Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64"})
	if report.Status != shared.RuntimePerformanceIncompatible || report.CanEnable || !slices.Contains(report.Issues, "injector_marker_invalid") || !slices.Contains(report.Issues, "installation_incomplete") {
		t.Fatalf("report=%#v", report)
	}
}

func TestInspectRejectsUnsafePackageVersion(t *testing.T) {
	root := makeLinuxPluginFixture(t, false)
	modinfo := filepath.Join(root, "mods", "DontStarveLuaJIT2", "modinfo.lua")
	if err := os.WriteFile(modinfo, []byte("version = \"3.0.0\nforged\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := Inspect(Options{ServerPath: root, ServerMode: "64", Platform: "linux", Architecture: "amd64"})
	if report.PackageVersion != "" || !slices.Contains(report.Issues, "package_version_unknown") {
		t.Fatalf("report=%#v", report)
	}
}

func makeLinuxFixture(t *testing.T, signatureVersion string) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin64")
	lib := filepath.Join(bin, "lib64")
	mod := filepath.Join(root, "mods", "DontStarveLuaJIT2")
	for _, directory := range []string{lib, mod} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "version.txt"), "747465\n", 0o644)
	write(filepath.Join(bin, "dontstarve_dedicated_server_nullrenderer_x64"), "#!/bin/bash\nexport LD_PRELOAD=./lib64/libInjector.so\n./dontstarve_dedicated_server_nullrenderer_x64_1 \"$@\"\n", 0o755)
	write(filepath.Join(bin, "dontstarve_dedicated_server_nullrenderer_x64_1"), "original-binary", 0o755)
	write(filepath.Join(lib, "libInjector.so"), "injector", 0o644)
	write(filepath.Join(lib, "liblua51DS.so"), "jit", 0o644)
	write(filepath.Join(lib, "liblua51DS_gengc.so"), "arena", 0o644)
	write(filepath.Join(bin, "signatures_server.json"), `{"version":`+signatureVersion+`,"funcs":{}}`, 0o644)
	write(filepath.Join(mod, "inject_server_only_mod.lua"), "return true", 0o644)
	write(filepath.Join(mod, "modinfo.lua"), "version = \"2.9.1\"\n", 0o644)
	return root
}

func makeLinuxPluginFixture(t *testing.T, generatedSignature bool) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin64")
	lib := filepath.Join(bin, "lib64")
	mod := filepath.Join(root, "mods", "DontStarveLuaJIT2")
	markerDirectory := filepath.Join(root, "data", "unsafedata")
	for _, directory := range []string{lib, filepath.Join(mod, "deps"), filepath.Join(mod, "plugins", "plugin_core_vm"), markerDirectory} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "version.txt"), "747465\n", 0o644)
	write(filepath.Join(bin, "dontstarve_dedicated_server_nullrenderer_x64"), "#!/bin/bash\nexport LD_PRELOAD=./lib64/libInjector.so\n./dontstarve_dedicated_server_nullrenderer_x64_1 \"$@\"\n", 0o755)
	write(filepath.Join(bin, "dontstarve_dedicated_server_nullrenderer_x64_1"), "original-binary", 0o755)
	write(filepath.Join(lib, "libInjector.so"), "injector-stub", 0o644)
	realInjector := filepath.Join(mod, "libInjector.so")
	write(realInjector, "real-injector", 0o644)
	write(filepath.Join(mod, "deps", "liblua51DS.so"), "jit", 0o644)
	write(filepath.Join(mod, "deps", "liblua51DS_gengc.so"), "arena", 0o644)
	write(filepath.Join(mod, "plugins", "plugin_core_vm", "plugin_core_vm.so"), "core-vm", 0o644)
	write(filepath.Join(mod, "plugins", "plugin_core_vm", "plugin_core_vm.meta.json"), `{"id":"core.vm","version":"0.2.0"}`, 0o644)
	write(filepath.Join(mod, "inject_server_only_mod.lua"), "return true", 0o644)
	write(filepath.Join(mod, "modinfo.lua"), "version = \"3.0.0\"\n", 0o644)
	write(filepath.Join(mod, "dst_admin_runtime_modes.v1"), "DontStarveLuaJIT2 runtime mode contract v1\n-lua_vm_type=game|jit|jit_gen\n-luajit_enabled_jit=true|false\n", 0o644)
	write(filepath.Join(markerDirectory, "ds_luajit_injector.path"), realInjector+"\n", 0o644)
	if generatedSignature {
		write(filepath.Join(mod, "signatures_server.json"), `{"version":747465,"funcs":{}}`, 0o644)
	}
	return root
}
