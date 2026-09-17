package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDryRunDoesNotChangeFilesAndApplyIsRepeatable(t *testing.T) {
	root := t.TempDir()
	server := filepath.Join(root, "server")
	content := filepath.Join(root, "workshop", "steamapps", "workshop", "content", "322330")
	for _, directory := range []string{server, filepath.Join(content, "123")} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(content, "123", "modinfo.lua"), []byte("name='fixture'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := run(server, content, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(server, "mods")); !os.IsNotExist(err) {
		t.Fatalf("dry run created files: %v", err)
	}
	for range 2 {
		if err := run(server, content, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(server, "mods", "workshop-123", "modinfo.lua")); err != nil {
		t.Fatal(err)
	}
}
