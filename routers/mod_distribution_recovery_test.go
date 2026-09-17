package routers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/moddistribution"
)

func TestOpenLocalModManagerKeepsControllerAvailableForBlockedRecovery(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	serverPath := filepath.Join(root, "server")
	savePath := filepath.Join(root, "saves")
	statePath := filepath.Join(root, "state")
	journalPath := filepath.Join(statePath, "journals", "blocked-journal-01.json")
	for _, directory := range []string{serverPath, savePath, filepath.Dir(journalPath)} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(journalPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := openLocalModManager(moddistribution.Config{
		CacheRoot: filepath.Join(root, "cache"),
		StateRoot: statePath,
		NodeID:    "local",
		Installations: []moddistribution.TrustedInstallation{{
			ID: "default", NodeID: "local", ServerPath: serverPath, SavePath: savePath,
		}},
	})
	if err != nil || manager == nil {
		t.Fatalf("journal recovery blocked Controller initialization: manager=%v err=%v", manager, err)
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("opening the manager unexpectedly processed the journal: %v", err)
	}

	recoveryErr := recoverLocalModManager(context.Background(), manager)
	if !errors.Is(recoveryErr, moddistribution.ErrIntegrity) {
		t.Fatalf("background recovery did not inspect the blocked journal: %v", recoveryErr)
	}
}

func TestOpenLocalModManagerStillRejectsInvalidBaseConfiguration(t *testing.T) {
	manager, err := openLocalModManager(moddistribution.Config{})
	if err == nil || manager != nil {
		t.Fatalf("invalid base configuration was accepted: manager=%v err=%v", manager, err)
	}
}
