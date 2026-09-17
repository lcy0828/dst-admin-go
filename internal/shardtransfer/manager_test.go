package shardtransfer

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPeerImportResumesAndVerifiesImmutableExport(t *testing.T) {
	_, _, source, target := prepareTransferRoots(t)
	id := "migration-peer-resume-0001"
	descriptor, err := source.PrepareExport(context.Background(), id, "Cluster_1", "Master")
	if err != nil {
		t.Fatal(err)
	}
	opened, file, err := source.OpenExport(id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || opened != descriptor || int64(len(data)) != descriptor.Size {
		t.Fatalf("opened=%#v descriptor=%#v bytes=%d err=%v", opened, descriptor, len(data), err)
	}
	if _, err := target.BeginImport(id, descriptor.Size, descriptor.SHA256); err != nil {
		t.Fatal(err)
	}
	half := len(data) / 2
	next, err := target.ReceiveImport(context.Background(), id, 0, bytes.NewReader(data[:half]))
	if !errors.Is(err, io.ErrUnexpectedEOF) || next != int64(half) {
		t.Fatalf("partial next=%d err=%v", next, err)
	}
	progress, offset, err := target.ImportProgress(id)
	if err != nil || progress != descriptor || offset != int64(half) {
		t.Fatalf("progress=%#v offset=%d err=%v", progress, offset, err)
	}
	next, err = target.ReceiveImport(context.Background(), id, offset, bytes.NewReader(data[half:]))
	if err != nil || next != descriptor.Size {
		t.Fatalf("resume next=%d err=%v", next, err)
	}
	verified, err := target.VerifyImport(context.Background(), id)
	if err != nil || verified != descriptor {
		t.Fatalf("verified=%#v err=%v", verified, err)
	}
}

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
		filepath.Join(cluster, "Master", "save", "shardindex"):           "shard-index",
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

func TestTransferAppliesTargetShardEndpointBeforePublish(t *testing.T) {
	_, targetRoot, source, target := prepareTransferRoots(t)
	id := "migration-endpoint-0001"
	descriptor, err := source.PrepareExport(context.Background(), id, "Cluster_1", "Master")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.BeginImportWithShardEndpoint(id, descriptor.Size, descriptor.SHA256, true, "100.64.0.10", 11889); err != nil {
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
	if _, err := target.CommitImport(context.Background(), id, "Cluster_1", "Master"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(targetRoot, "Cluster_1", "cluster.ini"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{"bind_ip", "0.0.0.0", "master_ip", "100.64.0.10", "master_port", "11889"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("cluster.ini missing %q:\n%s", expected, text)
		}
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

func TestTransferStagesInsideTargetSaveFilesystem(t *testing.T) {
	_, targetRoot, _, target := prepareTransferRoots(t)
	stage, err := target.targetStagePath("Cluster_1", "migration", "migration-stage-path-0001")
	if err != nil {
		t.Fatal(err)
	}
	resolvedTargetRoot, err := filepath.EvalSymlinks(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	resolvedStateRoot, err := filepath.EvalSymlinks(target.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !contained(resolvedTargetRoot, stage) || contained(resolvedStateRoot, stage) {
		t.Fatalf("stage=%q saveRoot=%q stateRoot=%q", stage, targetRoot, target.stateRoot)
	}
}

func TestTransferRejectsSymlinkedTargetStageRoot(t *testing.T) {
	_, targetRoot, _, target := prepareTransferRoots(t)
	cluster := filepath.Join(targetRoot, "Cluster_1")
	if err := os.MkdirAll(cluster, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(cluster, ".dst-admin-staging")); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}
	if _, err := target.targetStagePath("Cluster_1", "migration", "migration-stage-link-0001"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("symlinked stage error=%v", err)
	}
}

func TestExtractArchiveCreatesRuntimeOutputDirectory(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "provision.zip")
	writeTestArchive(t, archivePath, []testArchiveEntry{
		{name: "shared/cluster.ini", mode: 0o600, data: "[NETWORK]\n"},
		{name: "shard/server.ini", mode: 0o600, data: "[SHARD]\n"},
		{name: "shard/save/mod_config_data/dst-admin/", mode: os.ModeDir | 0o700},
	})
	staging := t.TempDir()
	if err := extractArchive(context.Background(), archivePath, staging); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(staging, "shard", "save", "mod_config_data", "dst-admin"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime output=%#v err=%v", info, err)
	}
}

func TestExtractArchiveRejectsUnsafeDirectoryEntries(t *testing.T) {
	tests := []struct {
		name  string
		entry testArchiveEntry
	}{
		{name: "path traversal", entry: testArchiveEntry{name: "shard/../../outside/", mode: os.ModeDir | 0o700}},
		{name: "symlink", entry: testArchiveEntry{name: "shard/runtime-link", mode: os.ModeSymlink | 0o777, data: "outside"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), "unsafe.zip")
			writeTestArchive(t, archivePath, []testArchiveEntry{
				{name: "shared/cluster.ini", mode: 0o600, data: "[NETWORK]\n"},
				{name: "shard/server.ini", mode: 0o600, data: "[SHARD]\n"},
				test.entry,
			})
			if err := extractArchive(context.Background(), archivePath, t.TempDir()); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("unsafe entry error=%v", err)
			}
		})
	}
}

type testArchiveEntry struct {
	name string
	mode os.FileMode
	data string
}

func writeTestArchive(t *testing.T, path string, entries []testArchiveEntry) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		header.SetMode(entry.mode)
		writer, createErr := archive.CreateHeader(header)
		if createErr == nil && entry.data != "" {
			_, createErr = writer.Write([]byte(entry.data))
		}
		if createErr != nil {
			_ = archive.Close()
			_ = file.Close()
			t.Fatal(createErr)
		}
	}
	if err := errors.Join(archive.Close(), file.Close()); err != nil {
		t.Fatal(err)
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
