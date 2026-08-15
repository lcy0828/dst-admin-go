package shardtransfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func prepareTransferRoots(t *testing.T) (string, string, *Manager, *Manager) {
	t.Helper()
	sourceRoot, targetRoot := filepath.Join(t.TempDir(), "source"), filepath.Join(t.TempDir(), "target")
	for _, root := range []string{sourceRoot, targetRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cluster := filepath.Join(sourceRoot, "Cluster_1")
	shard := filepath.Join(cluster, "Master", "save", "session", "0001")
	if err := os.MkdirAll(shard, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(cluster, "cluster.ini"):                            "[NETWORK]\ncluster_name = Test\n",
		filepath.Join(cluster, "cluster_token.txt"):                      "token-value\n",
		filepath.Join(cluster, "Master", "server.ini"):                   "[SHARD]\nis_master = true\n",
		filepath.Join(cluster, "Master", "save", "session", "0001", "1"): "world-data",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source, err := New(sourceRoot, filepath.Join(t.TempDir(), "source-state"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := New(targetRoot, filepath.Join(t.TempDir(), "target-state"))
	if err != nil {
		t.Fatal(err)
	}
	return sourceRoot, targetRoot, source, target
}

func copyMigration(t *testing.T, id string, source, target *Manager) Descriptor {
	t.Helper()
	descriptor, err := source.PrepareExport(context.Background(), id, "Cluster_1", "Master")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.BeginImport(id, descriptor.Size, descriptor.SHA256); err != nil {
		t.Fatal(err)
	}
	for offset := int64(0); offset < descriptor.Size; {
		chunk, err := source.ReadExport(context.Background(), id, offset)
		if err != nil {
			t.Fatal(err)
		}
		next, err := target.WriteImport(id, offset, chunk.Data)
		if err != nil {
			t.Fatal(err)
		}
		offset = next
	}
	return descriptor
}

func TestTransferPublishesTargetAndKeepsSourceRecovery(t *testing.T) {
	sourceRoot, targetRoot, source, target := prepareTransferRoots(t)
	id := "migration-success-0001"
	descriptor := copyMigration(t, id, source, target)
	committed, err := target.CommitImport(context.Background(), id, "Cluster_1", "Master")
	if err != nil || committed.SHA256 != descriptor.SHA256 {
		t.Fatalf("committed=%#v err=%v", committed, err)
	}
	recovery, err := source.FinalizeSource(id, "Cluster_1", "Master")
	if err != nil || recovery == "" {
		t.Fatalf("recovery=%q err=%v", recovery, err)
	}
	if err := target.CompleteTarget(id); err != nil {
		t.Fatal(err)
	}
	if completedRecovery, err := source.CompleteSource(id); err != nil || completedRecovery != recovery {
		t.Fatalf("completed=%q err=%v", completedRecovery, err)
	}
	data, err := os.ReadFile(filepath.Join(targetRoot, "Cluster_1", "Master", "save", "session", "0001", "1"))
	if err != nil || string(data) != "world-data" {
		t.Fatalf("target data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "Cluster_1", "Master", ".dst-admin-migration-id")); !os.IsNotExist(err) {
		t.Fatalf("target marker still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, filepath.FromSlash(recovery), "server.ini")); err != nil {
		t.Fatalf("source recovery missing: %v", err)
	}
}

func TestTransferRollbackRestoresSourceAndRemovesTarget(t *testing.T) {
	sourceRoot, targetRoot, source, target := prepareTransferRoots(t)
	id := "migration-rollback-0001"
	copyMigration(t, id, source, target)
	if _, err := target.CommitImport(context.Background(), id, "Cluster_1", "Master"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.FinalizeSource(id, "Cluster_1", "Master"); err != nil {
		t.Fatal(err)
	}
	if err := source.RollbackSource(id); err != nil {
		t.Fatal(err)
	}
	if err := target.RollbackTarget(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "Cluster_1", "Master", "server.ini")); err != nil {
		t.Fatalf("source was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "Cluster_1", "Master")); !os.IsNotExist(err) {
		t.Fatalf("target was not removed: %v", err)
	}
}

func TestTransferRejectsDifferentSharedClusterConfiguration(t *testing.T) {
	_, targetRoot, source, target := prepareTransferRoots(t)
	if err := os.MkdirAll(filepath.Join(targetRoot, "Cluster_1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetRoot, "Cluster_1", "cluster.ini"), []byte("different"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := "migration-conflict-0001"
	copyMigration(t, id, source, target)
	if _, err := target.CommitImport(context.Background(), id, "Cluster_1", "Master"); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict error=%v", err)
	}
}

func TestFinalizeSourceIsRetrySafeBeforeAndAfterReceiptRecovery(t *testing.T) {
	sourceRoot, _, source, _ := prepareTransferRoots(t)
	id := "migration-finalize-retry-0001"
	first, err := source.FinalizeSource(id, "Cluster_1", "Master")
	if err != nil || first == "" {
		t.Fatalf("first finalize=%q err=%v", first, err)
	}
	second, err := source.FinalizeSource(id, "Cluster_1", "Master")
	if err != nil || second != first {
		t.Fatalf("idempotent finalize=%q err=%v", second, err)
	}
	if err := os.Remove(source.sourceReceiptPath(id)); err != nil {
		t.Fatal(err)
	}
	recovered, err := source.FinalizeSource(id, "Cluster_1", "Master")
	if err != nil || recovered != first {
		t.Fatalf("recovered finalize=%q err=%v", recovered, err)
	}
	if err := source.RollbackSource(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "Cluster_1", "Master", "server.ini")); err != nil {
		t.Fatalf("recovered receipt could not restore source: %v", err)
	}
}
