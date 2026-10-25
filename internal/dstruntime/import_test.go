package dstruntime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestImportedCollectorPreservesOptOutAndSourceUserFiles(t *testing.T) {
	manager, catalog, current := newRuntimeTestManager(t)
	user := []byte("print('current user code')\n")
	if err := os.WriteFile(filepath.Join(current, customCommandsName), user, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UninstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(current, customCommandsName))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	incoming := filepath.Join(root, "Master")
	if err := os.MkdirAll(incoming, 0700); err != nil {
		t.Fatal(err)
	}
	sourceUser := []byte("print('imported user code')\n")
	if err := os.WriteFile(filepath.Join(incoming, customCommandsName), sourceUser, 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.InitializeWorld(root, "Master"); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(incoming, managedDirectory, "user-notes.txt")
	if err := os.WriteFile(unknown, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareImportedWorld(context.Background(), incoming, current); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(incoming, customCommandsName))
	if err != nil || !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(sourceUser)) {
		t.Fatalf("user code changed: %q %v", got, err)
	}
	if _, found, err := locateManagedBlock(got); err != nil || found {
		t.Fatalf("opt-out ignored: %v", err)
	}
	if got, err := os.ReadFile(unknown); err != nil || string(got) != "keep" {
		t.Fatalf("unknown file changed: %q %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(current, customCommandsName))
	if err != nil || !bytes.Equal(got, before) {
		t.Fatalf("preparation modified target: %q %v", got, err)
	}
}
