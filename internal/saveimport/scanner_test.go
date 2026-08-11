package saveimport

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type archiveTestEntry struct {
	name    string
	content string
	mode    os.FileMode
}

func TestScannerRecognizesWrappedFinderArchiveAndMods(t *testing.T) {
	root := t.TempDir()
	workshop := filepath.Join(root, "workshop")
	if err := os.MkdirAll(filepath.Join(workshop, "1392778117"), 0750); err != nil {
		t.Fatal(err)
	}
	archive := createZIP(t, []archiveTestEntry{
		{name: "Cluster_1/cluster.ini", content: clusterINI("Wrapped Room")},
		{name: "Cluster_1/cluster_token.txt", content: "token\n"},
		{name: "Cluster_1/Master/server.ini", content: serverINI(true, 1, 10999)},
		{name: "Cluster_1/Master/save/session/session-id/0000000001", content: "save"},
		{name: "Cluster_1/Master/modoverrides.lua", content: `return {["workshop-1392778117"] = { enabled = true }, ["workshop-3781609737"] = { enabled = true }}`},
		{name: "__MACOSX/Cluster_1/._cluster.ini", content: "metadata"},
		{name: "Cluster_1/.DS_Store", content: "metadata"},
	})
	archivePath := filepath.Join(root, "finder.zip")
	if err := os.WriteFile(archivePath, archive, 0640); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "content")
	if err := os.Mkdir(destination, 0750); err != nil {
		t.Fatal(err)
	}
	manifest, err := NewScanner(workshop).Scan(context.Background(), archivePath, "Cluster_1.zip", destination, int64(len(archive)), "sha")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != "zip" || len(manifest.Candidates) != 1 {
		t.Fatalf("manifest = %#v", manifest)
	}
	candidate := manifest.Candidates[0]
	if candidate.Root != "Cluster_1" || candidate.Name != "Wrapped Room" || !candidate.TokenPresent || len(candidate.Worlds) != 1 {
		t.Fatalf("candidate = %#v", candidate)
	}
	if len(candidate.Mods) != 2 || !candidate.Mods[0].Downloaded || candidate.Mods[1].Downloaded {
		t.Fatalf("mods = %#v", candidate.Mods)
	}
	if manifest.IgnoredSystemFiles != 1 || candidate.Compatibility != "needs_attention" {
		t.Fatalf("ignored=%d compatibility=%q diagnostics=%#v", manifest.IgnoredSystemFiles, candidate.Compatibility, candidate.Diagnostics)
	}
}

func TestScannerNormalizesWindowsPathsAndFindsMultipleClusters(t *testing.T) {
	root := t.TempDir()
	archive := createZIP(t, []archiveTestEntry{
		{name: `Klei\DoNotStarveTogether\Cluster_A\cluster.ini`, content: clusterINI("A")},
		{name: `Klei\DoNotStarveTogether\Cluster_A\Master\server.ini`, content: serverINI(true, 1, 10999)},
		{name: "Cluster_B/cluster.ini", content: clusterINI("B")},
		{name: "Cluster_B/Master/server.ini", content: serverINI(true, 1, 11009)},
	})
	archivePath := filepath.Join(root, "windows.zip")
	if err := os.WriteFile(archivePath, archive, 0640); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "content")
	if err := os.Mkdir(destination, 0750); err != nil {
		t.Fatal(err)
	}
	manifest, err := NewScanner("").Scan(context.Background(), archivePath, "windows.zip", destination, int64(len(archive)), "sha")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.NormalizedPaths != 2 || len(manifest.Candidates) != 2 || len(manifest.Diagnostics) != 1 || manifest.Diagnostics[0].Code != "MULTIPLE_CLUSTERS" {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestScannerSupportsTarGzip(t *testing.T) {
	root := t.TempDir()
	archive := createTarGzip(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Tar Room")},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
	})
	archivePath := filepath.Join(root, "cluster.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0640); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "content")
	if err := os.Mkdir(destination, 0750); err != nil {
		t.Fatal(err)
	}
	manifest, err := NewScanner("").Scan(context.Background(), archivePath, "cluster.tar.gz", destination, int64(len(archive)), "sha")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != "tar.gz" || len(manifest.Candidates) != 1 || manifest.Candidates[0].Root != "." {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestScannerIgnoresNestedDSTAdminRecoveryCluster(t *testing.T) {
	root := t.TempDir()
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Current")},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
		{name: ".dst-admin-recovery/worldgen-fix/cluster.ini", content: clusterINI("Old")},
		{name: ".dst-admin-recovery/worldgen-fix/Master/server.ini", content: serverINI(true, 1, 12001)},
	})
	archivePath := filepath.Join(root, "admin-backup.zip")
	if err := os.WriteFile(archivePath, archive, 0640); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "content")
	if err := os.Mkdir(destination, 0750); err != nil {
		t.Fatal(err)
	}
	manifest, err := NewScanner("").Scan(context.Background(), archivePath, "admin-backup.zip", destination, int64(len(archive)), "sha")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Candidates) != 1 || manifest.Candidates[0].Name != "Current" {
		t.Fatalf("candidates = %#v", manifest.Candidates)
	}
}

func TestScannerRejectsTraversalCaseCollisionAndSymlink(t *testing.T) {
	tests := []struct {
		name    string
		entries []archiveTestEntry
	}{
		{name: "traversal", entries: []archiveTestEntry{{name: "../cluster.ini", content: "bad"}}},
		{name: "case-collision", entries: []archiveTestEntry{{name: "cluster.ini", content: "one"}, {name: "Cluster.ini", content: "two"}}},
		{name: "symlink", entries: []archiveTestEntry{{name: "cluster.ini", content: "target", mode: os.ModeSymlink | 0777}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "bad.zip")
			if err := os.WriteFile(archivePath, createZIP(t, test.entries), 0640); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(root, "content")
			if err := os.Mkdir(destination, 0750); err != nil {
				t.Fatal(err)
			}
			_, err := NewScanner("").Scan(context.Background(), archivePath, "bad.zip", destination, 1, "sha")
			if err == nil || !strings.Contains(err.Error(), ErrUnsafeArchive.Error()) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestScannerRejectsCorruptCompressedContent(t *testing.T) {
	root := t.TempDir()
	archive := createZIP(t, []archiveTestEntry{
		{name: "cluster.ini", content: clusterINI("Corrupt")},
		{name: "Master/server.ini", content: serverINI(true, 1, 10999)},
	})
	marker := []byte("cluster_name = Corrupt")
	if offset := bytes.Index(archive, marker); offset >= 0 {
		archive[offset] ^= 0xff
	} else if len(archive) > 40 {
		archive[len(archive)/3] ^= 0xff
	}
	archivePath := filepath.Join(root, "corrupt.zip")
	if err := os.WriteFile(archivePath, archive, 0640); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "content")
	if err := os.Mkdir(destination, 0750); err != nil {
		t.Fatal(err)
	}
	_, err := NewScanner("").Scan(context.Background(), archivePath, "corrupt.zip", destination, int64(len(archive)), "sha")
	if err == nil {
		t.Fatal("corrupt ZIP was accepted")
	}
}

func createZIP(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		destination, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(destination, entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func createTarGzip(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	writer := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: 0640, Size: int64(len(entry.content)), Typeflag: tar.TypeReg}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func clusterINI(name string) string {
	return "[GAMEPLAY]\ngame_mode = survival\nmax_players = 6\npvp = false\n\n[NETWORK]\ncluster_name = " + name + "\n\n[SHARD]\nshard_enabled = true\n"
}

func serverINI(master bool, shardID, port int) string {
	return "[NETWORK]\nserver_port = " + itoa(port) + "\n\n[SHARD]\nis_master = " + boolString(master) + "\nid = " + itoa(shardID) + "\n\n[STEAM]\nauthentication_port = " + itoa(port+1000) + "\nmaster_server_port = " + itoa(port+2000) + "\n"
}

func itoa(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	result := make([]byte, 0, 10)
	for value > 0 {
		result = append([]byte{digits[value%10]}, result...)
		value /= 10
	}
	return string(result)
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
