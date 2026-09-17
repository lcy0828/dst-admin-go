package runtimefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func modRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestPublishModOverridesPreservesOtherFilesAndRejectsExternalChanges(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "Cluster", "Master")
	if err := os.MkdirAll(world, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(world, "modoverrides.lua")
	before := []byte("return {}\n")
	after := []byte("return { enabled = true }\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "server.ini"), []byte("untouched"), 0o640); err != nil {
		t.Fatal(err)
	}
	revision, err := PublishModOverrides(context.Background(), root, "Cluster", "Master", modRevision(before), after)
	if err != nil || revision != modRevision(after) {
		t.Fatalf("revision=%s err=%v", revision, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode was not preserved: info=%v err=%v", info, err)
	}
	if _, err := PublishModOverrides(context.Background(), root, "Cluster", "Master", revision, after); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := os.Stat(path)
	if !os.SameFile(info, unchanged) {
		t.Fatal("unchanged content was rewritten")
	}
	manual := []byte("return { manual = true }\n")
	if err := os.WriteFile(path, manual, 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := PublishModOverrides(context.Background(), root, "Cluster", "Master", revision, before)
	var conflict *ConfigurationConflictError
	if !errors.As(err, &conflict) || current != modRevision(manual) {
		t.Fatalf("conflict=%v revision=%s", err, current)
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(manual) {
		t.Fatal("manual configuration was overwritten")
	}
	data, _ = os.ReadFile(filepath.Join(world, "server.ini"))
	if string(data) != "untouched" {
		t.Fatal("unrelated configuration changed")
	}
	entries, _ := os.ReadDir(world)
	if len(entries) != 2 {
		t.Fatalf("unexpected staging files: %v", entries)
	}
}

func TestPublishModOverridesMissingFileCancellationAndSymlink(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "Cluster", "Master")
	if err := os.MkdirAll(world, 0o750); err != nil {
		t.Fatal(err)
	}
	content := []byte("return {}\n")
	if _, err := PublishModOverrides(context.Background(), root, "Cluster", "Master", modRevision(nil), content); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PublishModOverrides(ctx, root, "Cluster", "Master", modRevision(content), []byte("return { changed = true }")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.lua")
	if err := os.WriteFile(outside, content, 0o640); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(world, "modoverrides.lua")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishModOverrides(context.Background(), root, "Cluster", "Master", modRevision(content), content); err == nil {
		t.Fatal("symlink write was accepted")
	}
	if _, err := PublishModOverrides(context.Background(), root, "../Cluster", "Master", modRevision(content), content); err == nil {
		t.Fatal("path traversal was accepted")
	}
}
