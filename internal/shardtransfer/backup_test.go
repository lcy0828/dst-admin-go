package shardtransfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func copyBackupForRestore(t *testing.T, id string, source, target *Manager) BackupDescriptor {
	t.Helper()
	descriptor, err := source.PrepareBackup(context.Background(), id, "Cluster_1", "Master")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.BeginRestore(descriptor); err != nil {
		t.Fatal(err)
	}
	for offset := int64(0); offset < descriptor.Size; {
		chunk, err := source.ReadBackup(context.Background(), id, offset)
		if err != nil {
			t.Fatal(err)
		}
		next, err := target.WriteRestore(id, offset, chunk.Data)
		if err != nil {
			t.Fatal(err)
		}
		if next != chunk.NextOffset {
			t.Fatalf("restore next offset=%d, want %d", next, chunk.NextOffset)
		}
		offset = next
	}
	if _, err := target.PrepareRestore(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func populateRestoreTarget(t *testing.T, root string) {
	t.Helper()
	cluster := filepath.Join(root, "Cluster_1")
	files := map[string]string{
		filepath.Join(cluster, "cluster.ini"):          "old-cluster\n",
		filepath.Join(cluster, "adminlist.txt"):        "old-admin\n",
		filepath.Join(cluster, "Master", "server.ini"): "old-server\n",
		filepath.Join(cluster, "Master", "old.txt"):    "old-world\n",
	}
	for path, value := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackupRestoreRollbackRestoresShardAndSharedFiles(t *testing.T) {
	_, targetRoot, source, target := prepareTransferRoots(t)
	populateRestoreTarget(t, targetRoot)
	id := "backup-restore-rollback-0001"
	copyBackupForRestore(t, id, source, target)
	recovery, err := target.PublishRestore(id, true)
	if err != nil || recovery == "" {
		t.Fatalf("publish recovery=%q err=%v", recovery, err)
	}
	assertFileContent(t, filepath.Join(targetRoot, "Cluster_1", "cluster.ini"), "[NETWORK]\ncluster_name = Test\n")
	assertFileContent(t, filepath.Join(targetRoot, "Cluster_1", "Master", "save", "session", "0001", "1"), "world-data")
	if _, err := os.Stat(filepath.Join(targetRoot, "Cluster_1", "adminlist.txt")); !os.IsNotExist(err) {
		t.Fatalf("shared file absent from backup was not removed: %v", err)
	}
	if err := target.RollbackRestore(id); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(targetRoot, "Cluster_1", "cluster.ini"), "old-cluster\n")
	assertFileContent(t, filepath.Join(targetRoot, "Cluster_1", "adminlist.txt"), "old-admin\n")
	assertFileContent(t, filepath.Join(targetRoot, "Cluster_1", "Master", "old.txt"), "old-world\n")
}

func TestBackupRestoreCompleteKeepsPublishedData(t *testing.T) {
	_, targetRoot, source, target := prepareTransferRoots(t)
	populateRestoreTarget(t, targetRoot)
	id := "backup-restore-complete-0001"
	copyBackupForRestore(t, id, source, target)
	recovery, err := target.PublishRestore(id, true)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := target.CompleteRestore(id)
	if err != nil || completed != recovery {
		t.Fatalf("complete=%q recovery=%q err=%v", completed, recovery, err)
	}
	assertFileContent(t, filepath.Join(targetRoot, "Cluster_1", "Master", "save", "session", "0001", "1"), "world-data")
	if _, err := os.Stat(filepath.Join(targetRoot, filepath.FromSlash(recovery))); !os.IsNotExist(err) {
		t.Fatalf("recovery directory remains: %v", err)
	}
}

func TestBackupRestoreRejectsCorruptArchive(t *testing.T) {
	_, _, source, target := prepareTransferRoots(t)
	id := "backup-restore-corrupt-0001"
	descriptor, err := source.PrepareBackup(context.Background(), id, "Cluster_1", "Master")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.BeginRestore(descriptor); err != nil {
		t.Fatal(err)
	}
	if _, err := target.WriteRestore(id, 0, []byte("not-a-zip")); err != nil {
		t.Fatal(err)
	}
	if _, err := target.PrepareRestore(context.Background(), id); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt restore error=%v", err)
	}
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("file %s=%q err=%v", path, data, err)
	}
}
