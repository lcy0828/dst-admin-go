package dstserver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveLinuxLayoutsAndModePreference(t *testing.T) {
	root := t.TempDir()
	x64 := writeExecutable(t, filepath.Join(root, "bin64", BinaryX64))
	x86 := writeExecutable(t, filepath.Join(root, "bin", Binary))

	layout, ok := Resolve(root, "64")
	if !ok || layout.Executable != x64 || layout.InstallRoot != root || layout.ContentRoot != root {
		t.Fatalf("64-bit layout = %#v, ok=%v", layout, ok)
	}
	layout, ok = Resolve(root, "32")
	if !ok || layout.Executable != x86 {
		t.Fatalf("32-bit layout = %#v, ok=%v", layout, ok)
	}
}

func TestResolveMacSteamApplicationLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Don't Starve Together")
	app := filepath.Join(root, "dontstarve_steam.app")
	executablePath := writeExecutable(t, filepath.Join(app, "Contents", "MacOS", Binary))

	for _, configuredPath := range []string{root, app, filepath.Join(app, "Contents", "MacOS"), executablePath} {
		layout, ok := Resolve(configuredPath, "64")
		if !ok {
			t.Fatalf("failed to resolve %q", configuredPath)
		}
		if layout.Executable != executablePath || layout.WorkingDirectory != filepath.Dir(executablePath) {
			t.Fatalf("runtime layout = %#v", layout)
		}
		if layout.InstallRoot != root || layout.ContentRoot != filepath.Join(app, "Contents") || layout.Kind != LayoutMac {
			t.Fatalf("macOS layout = %#v", layout)
		}
		if layout.AppID != AppIDGame || layout.UpdateMethod != UpdateMethodSteamClient || layout.UpdateSupported {
			t.Fatalf("macOS Steam update policy = %#v", layout)
		}
	}
}

func TestSteamClientLibraryDirectoryUsesConfiguredPath(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "steamclient.dylib"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DST_ADMIN_STEAM_CLIENT_LIBRARY_PATH", directory)
	resolved := SteamClientLibraryDirectory(Layout{Kind: LayoutMac})
	if resolved != directory {
		t.Fatalf("library directory = %q", resolved)
	}
}

func TestResolveLegacyDedicatedServerApplication(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "dontstarve_dedicated_server_nullrenderer.app")
	executablePath := writeExecutable(t, filepath.Join(app, "Contents", "MacOS", Binary))
	layout, ok := Resolve(root, "64")
	if !ok || layout.Executable != executablePath || layout.InstallRoot != root || layout.Kind != LayoutMac {
		t.Fatalf("legacy layout = %#v, ok=%v", layout, ok)
	}
	if layout.AppID != AppIDDedicatedServer || layout.UpdateMethod != UpdateMethodSteamCMD || !layout.UpdateSupported {
		t.Fatalf("legacy update policy = %#v", layout)
	}
}

func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test"), 0750); err != nil {
		t.Fatal(err)
	}
	return path
}
