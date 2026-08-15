package runtimefiles

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"dont/shared"
)

func TestReadLogsContinuesByFileIdentityAndResetsAfterRotation(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(world, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(world, "server_log.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := ReadLogs(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLogRequest{Cursor: -1, MaxBytes: 1024, MaxLines: 10})
	if err != nil || len(first.Lines) != 2 || first.Cursor != int64(len("one\ntwo\n")) || first.FileID == "" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("three\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	continued, err := ReadLogs(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLogRequest{FileID: first.FileID, Cursor: first.Cursor, MaxBytes: 1024, MaxLines: 10})
	if err != nil || continued.Reset || len(continued.Lines) != 1 || continued.Lines[0].Text != "three" {
		t.Fatalf("continued=%#v err=%v", continued, err)
	}
	if err := os.WriteFile(path, []byte("rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotated, err := ReadLogs(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLogRequest{FileID: first.FileID, Cursor: continued.Cursor, MaxBytes: 1024, MaxLines: 10})
	if err != nil || !rotated.Reset || len(rotated.Lines) != 1 || rotated.Lines[0].Text != "rotated" {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
}

func TestReadArtifactsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	artifactRoot := filepath.Join(root, "Cluster_1", "Master", "save", "mod_config_data", "dst-admin")
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"ready":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(artifactRoot, "health.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ReadArtifacts(context.Background(), root, "Cluster_1", "Master", shared.ArtifactRuntimeHealth); err == nil {
		t.Fatal("expected unsafe symlink error")
	}
}

func TestReadLogsRejectsIntermediateShardSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Cluster_1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "server_log.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Cluster_1", "Master")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ReadLogs(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLogRequest{Cursor: -1, MaxBytes: 1024, MaxLines: 10}); err == nil {
		t.Fatal("expected intermediate symlink escape error")
	}
}
