package runtimefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/shared"
)

func TestValidateRemoteRuntimePayloads(t *testing.T) {
	now := time.Now().UTC()
	data := []byte(`{"ready":true}`)
	sum := sha256.Sum256(data)
	bundle := shared.RuntimeArtifactBundle{Kind: shared.ArtifactRuntimeHealth, Artifacts: []shared.RuntimeArtifact{{
		Name: "health.json", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), UpdatedAt: now, Data: data,
	}}}
	if err := ValidateArtifactBundle(shared.ArtifactRuntimeHealth, bundle); err != nil {
		t.Fatalf("validate artifact bundle: %v", err)
	}
	bundle.Artifacts[0].Size++
	if err := ValidateArtifactBundle(shared.ArtifactRuntimeHealth, bundle); err == nil {
		t.Fatal("expected truncated artifact to be rejected")
	}

	request := shared.RuntimeLogRequest{Cursor: 10, MaxBytes: 16, Raw: true}
	chunk := shared.RuntimeLogChunk{
		FileName: "server_log.txt", FileID: "file-1", Size: 20, Cursor: 13, UpdatedAt: now, Data: []byte("abc"),
	}
	if err := ValidateLogChunk(request, chunk); err != nil {
		t.Fatalf("validate raw log chunk: %v", err)
	}
	chunk.Cursor++
	if err := ValidateLogChunk(request, chunk); err == nil {
		t.Fatal("expected inconsistent cursor to be rejected")
	}
}

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

func TestReadLogsSelectsChatSourceWithoutFallingBackToServerLog(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(world, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "server_log.txt"), []byte("[00:00:00]: Current time: Wed Aug 19 19:56:40 2026\nserver only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chatPath := filepath.Join(world, "server_chat_log.txt")
	if err := os.WriteFile(chatPath, []byte("[00:00:01]: [Say] (KU_ONE) Willow: hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := shared.RuntimeLogRequest{Source: shared.RuntimeLogSourceChat, Cursor: -1, MaxBytes: 1024, MaxLines: 10}
	chunk, err := ReadLogs(context.Background(), root, "Cluster_1", "Master", request)
	if err != nil || chunk.FileName != "server_chat_log.txt" || len(chunk.Lines) != 1 || !strings.Contains(chunk.Lines[0].Text, "Willow") {
		t.Fatalf("chat chunk=%#v err=%v", chunk, err)
	}
	if err := ValidateLogChunk(request, chunk); err != nil {
		t.Fatalf("validate chat chunk: %v", err)
	}
	wantStartedAt := time.Date(2026, time.August, 19, 19, 56, 40, 0, time.Local)
	if !chunk.StartedAt.Equal(wantStartedAt) {
		t.Fatalf("chat startedAt=%s want=%s", chunk.StartedAt, wantStartedAt)
	}
	serverRequest := request
	serverRequest.Source = shared.RuntimeLogSourceServer
	if err := ValidateLogChunk(serverRequest, chunk); err == nil {
		t.Fatal("server log request accepted a chat log chunk")
	}
}

func TestReadChatLogsUsesForestServerLogAsStartupFallback(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(world, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "forest_server_log.txt"), []byte("[00:00:00]: Current time: Wed Aug 19 19:56:40 2026\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "server_chat_log.txt"), []byte("[00:00:01]: [Say] (KU_ONE) Willow: hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := shared.RuntimeLogRequest{Source: shared.RuntimeLogSourceChat, Cursor: -1, MaxBytes: 1024, MaxLines: 10}
	chunk, err := ReadLogs(context.Background(), root, "Cluster_1", "Master", request)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, time.August, 19, 19, 56, 40, 0, time.Local)
	if !chunk.StartedAt.Equal(want) {
		t.Fatalf("chat startedAt=%s want=%s", chunk.StartedAt, want)
	}
}

func TestReadLogsDetectsInPlaceTruncateAfterNewLogOutgrowsOldCursor(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(world, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(world, "server_log.txt")
	if err := os.WriteFile(path, []byte("old generation\nold line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := ReadLogs(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLogRequest{Cursor: -1, MaxBytes: 1024, MaxLines: 10})
	if err != nil {
		t.Fatal(err)
	}
	newLog := "new generation\nnew line one\nnew line two\n"
	if int64(len(newLog)) <= first.Cursor {
		t.Fatal("test fixture must outgrow the old cursor")
	}
	if err := os.WriteFile(path, []byte(newLog), 0o600); err != nil {
		t.Fatal(err)
	}
	reset, err := ReadLogs(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLogRequest{
		FileID: first.FileID, Cursor: first.Cursor, MaxBytes: 1024, MaxLines: 10,
	})
	if err != nil || !reset.Reset || len(reset.Lines) != 3 || reset.Lines[0].Text != "new generation" {
		t.Fatalf("reset=%#v err=%v", reset, err)
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
