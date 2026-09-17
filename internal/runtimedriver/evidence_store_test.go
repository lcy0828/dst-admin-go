package runtimedriver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/shared"
)

func TestNativeOperationEvidenceSurvivesDriverRestart(t *testing.T) {
	root := t.TempDir()
	driver, err := NewNative(root, &nativeLifecycleControl{})
	if err != nil {
		t.Fatal(err)
	}
	target := nativeLifecycleTarget()
	target.InstallationID = "default"
	result, err := driver.SendConsole(context.Background(), target, Operation{ID: "operation-persisted"}, shared.RuntimeConsoleRequest{
		Mode: shared.ConsoleModeManaged, Command: `c_announce("persisted")`,
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewNative(root, &nativeLifecycleControl{})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := restarted.ObserveOperation(context.Background(), target, result.OperationID, "")
	if err != nil || evidence.Outcome != shared.RuntimeOutcomeSent || evidence.Action != string(shared.RuntimeActionConsoleSend) {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
	info, err := os.Stat(filepath.Join(root, nativeEvidenceFileName))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestNativeProbeDoesNotPersistOperationEvidence(t *testing.T) {
	root := t.TempDir()
	driver, err := NewNative(root, &nativeLifecycleControl{})
	if err != nil {
		t.Fatal(err)
	}
	target := nativeLifecycleTarget()
	if _, err := driver.SendConsole(context.Background(), target, Operation{ID: "probe-not-persisted"}, shared.RuntimeConsoleRequest{
		Mode: shared.ConsoleModeProbe, CoalesceKey: "players", Command: `return true`,
	}, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, nativeEvidenceFileName)); !os.IsNotExist(err) {
		t.Fatalf("probe created evidence file: %v", err)
	}
	if _, err := driver.ObserveOperation(context.Background(), target, "probe-not-persisted", ""); err == nil {
		t.Fatal("probe unexpectedly remained in operation evidence")
	}
}

func TestNativeEvidenceStoreRetainsNewestBoundedEntries(t *testing.T) {
	root := t.TempDir()
	store := newNativeEvidenceStore(root)
	start := time.Now().UTC().Add(-time.Hour)
	for index := 0; index < nativeEvidenceLimit+8; index++ {
		if err := store.remember(shared.RuntimeOperationEvidence{
			OperationID: fmt.Sprintf("operation-%03d", index), Action: "test", Completed: true,
			Outcome: shared.RuntimeOutcomeConfirmed, ObservedAt: start.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	reloaded := newNativeEvidenceStore(root)
	if len(reloaded.entries) != nativeEvidenceLimit {
		t.Fatalf("entries=%d", len(reloaded.entries))
	}
	if _, exists := reloaded.entries["operation-000"]; exists {
		t.Fatal("oldest evidence was not trimmed")
	}
	if _, exists := reloaded.entries[fmt.Sprintf("operation-%03d", nativeEvidenceLimit+7)]; !exists {
		t.Fatal("newest evidence was trimmed")
	}
}

func TestNativeEvidenceStoreIgnoresDamagedDocument(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, nativeEvidenceFileName)
	if err := os.WriteFile(path, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	store := newNativeEvidenceStore(root)
	if len(store.entries) != 0 {
		t.Fatalf("entries=%#v", store.entries)
	}
	if err := store.remember(shared.RuntimeOperationEvidence{
		OperationID: "recovered", Action: "test", Completed: true,
		Outcome: shared.RuntimeOutcomeConfirmed, ObservedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, exists := newNativeEvidenceStore(root).entries["recovered"]; !exists {
		t.Fatal("store did not recover after damaged document")
	}
}
