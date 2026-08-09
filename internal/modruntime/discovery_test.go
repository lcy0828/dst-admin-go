package modruntime

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDiscoverPrefersConfiguredLua(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell test adapter is POSIX-only")
	}
	binary := filepath.Join(t.TempDir(), "configured-lua")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'Lua test'\n"), 0750); err != nil {
		t.Fatal(err)
	}
	discovery := Discover(binary, "")
	primary, ok := discovery.Primary()
	if !ok || primary.Kind != KindLua || primary.Path != binary || primary.Source != "configured" {
		t.Fatalf("configured Lua was not preferred: %#v", discovery)
	}
}

func TestDiscoverFindsVersionedLuaOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell test adapter is POSIX-only")
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "lua5.5")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'Lua 5.5'\n"), 0750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	discovery := Discover("missing-lua", "missing-python")
	if len(discovery.Runtimes) == 0 || discovery.Runtimes[0].Path != binary || discovery.Runtimes[0].Kind != KindLua {
		t.Fatalf("versioned Lua was not discovered: %#v", discovery)
	}
}

func TestDiscoverAcceptsPythonOnlyWhenLupaImports(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell test adapter is POSIX-only")
	}
	directory := t.TempDir()
	withoutLupa := filepath.Join(directory, "python-without-lupa")
	withLupa := filepath.Join(directory, "python-with-lupa")
	if err := os.WriteFile(withoutLupa, []byte("#!/bin/sh\nprintf 'missing lupa' >&2\nexit 1\n"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(withLupa, []byte("#!/bin/sh\nprintf '2.6'\n"), 0750); err != nil {
		t.Fatal(err)
	}
	missing := Discover("missing-lua", withoutLupa)
	if !missing.PythonAvailable || missing.PythonLupa {
		t.Fatalf("missing Lupa was not diagnosed: %#v", missing)
	}
	available := Discover("missing-lua", withLupa)
	found := false
	for _, item := range available.Runtimes {
		found = found || item.Kind == KindPythonLupa && item.Path == withLupa
	}
	if !found || !available.PythonLupa {
		t.Fatalf("Python/Lupa runtime was not discovered: %#v", available)
	}
}

func TestDiscoverDeduplicatesExecutableAliases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test is POSIX-only")
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "lua-real")
	alias := filepath.Join(directory, "lua")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'Lua test'\n"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binary, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)

	discovery := Discover(binary, "missing-python")
	count := 0
	for _, item := range discovery.Runtimes {
		if item.Kind == KindLua && executableIdentity(item.Path) == executableIdentity(binary) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("executable aliases were not deduplicated: %#v", discovery.Runtimes)
	}
}
