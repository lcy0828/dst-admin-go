package configpublication

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestManagerPublishesAndRollsBackSharedConfiguration(t *testing.T) {
	saveRoot := t.TempDir()
	roomRoot := filepath.Join(saveRoot, "Cluster_1")
	if err := os.MkdirAll(filepath.Join(roomRoot, "Master"), 0o750); err != nil {
		t.Fatal(err)
	}
	clusterPath := filepath.Join(roomRoot, "cluster.ini")
	if err := os.WriteFile(clusterPath, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "operations")
	manager, err := New(saveRoot, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	payload := publicationArchive(t, map[string][]byte{
		"cluster.ini":       []byte("new\n"),
		"cluster_token.txt": []byte("token\n"),
	})
	descriptor := testDescriptor("publication-shared-0001", "Cluster_1", "Master", ScopeShared, payload)
	if offset, err := manager.Begin(descriptor); err != nil || offset != 0 {
		t.Fatalf("Begin offset=%d error=%v", offset, err)
	}
	if next, err := manager.Write(descriptor.PublicationID, 0, payload); err != nil || next != int64(len(payload)) {
		t.Fatalf("Write next=%d error=%v", next, err)
	}
	if err := manager.Prepare(context.Background(), descriptor.PublicationID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Publish(descriptor.PublicationID); err != nil {
		t.Fatal(err)
	}
	assertFile(t, clusterPath, "new\n")
	assertFile(t, filepath.Join(roomRoot, "cluster_token.txt"), "token\n")
	if err := manager.Rollback(descriptor.PublicationID); err != nil {
		t.Fatal(err)
	}
	assertFile(t, clusterPath, "old\n")
	if _, err := os.Stat(filepath.Join(roomRoot, "cluster_token.txt")); !os.IsNotExist(err) {
		t.Fatalf("new file still exists after rollback: %v", err)
	}
}

func TestManagerCompletesWorldConfigurationPublication(t *testing.T) {
	saveRoot := t.TempDir()
	worldRoot := filepath.Join(saveRoot, "Cluster_1", "Master")
	if err := os.MkdirAll(worldRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worldRoot, "server.ini"), []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "operations")
	manager, err := New(saveRoot, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	payload := publicationArchive(t, map[string][]byte{"server.ini": []byte("new\n")})
	descriptor := testDescriptor("publication-world-0001", "Cluster_1", "Master", ScopeWorld, payload)
	if _, err := manager.Begin(descriptor); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Write(descriptor.PublicationID, 0, payload); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare(context.Background(), descriptor.PublicationID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Publish(descriptor.PublicationID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Complete(descriptor.PublicationID); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(worldRoot, "server.ini"), "new\n")
	if _, err := os.Stat(filepath.Join(stateRoot, descriptor.PublicationID)); !os.IsNotExist(err) {
		t.Fatalf("publication state remains after completion: %v", err)
	}
}

func TestManagerRejectsUnsafeOrOutOfScopeArchiveEntries(t *testing.T) {
	saveRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(saveRoot, "Cluster_1", "Master"), 0o750); err != nil {
		t.Fatal(err)
	}
	manager, err := New(saveRoot, filepath.Join(t.TempDir(), "operations"))
	if err != nil {
		t.Fatal(err)
	}
	payload := publicationArchive(t, map[string][]byte{"../cluster.ini": []byte("unsafe")})
	descriptor := testDescriptor("publication-unsafe-0001", "Cluster_1", "Master", ScopeShared, payload)
	if _, err := manager.Begin(descriptor); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Write(descriptor.PublicationID, 0, payload); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare(context.Background(), descriptor.PublicationID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Prepare error=%v", err)
	}
}

func publicationArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, data := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func testDescriptor(id, cluster, shard string, scope Scope, payload []byte) Descriptor {
	sum := sha256.Sum256(payload)
	return Descriptor{PublicationID: id, Cluster: cluster, Shard: shard, Scope: scope, Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}
}

func assertFile(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("%s=%q, want %q", path, data, expected)
	}
}
