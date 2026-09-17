package runtimefiles

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReadClusterTokenUsesFixedPathAndValidatesResult(t *testing.T) {
	root := t.TempDir()
	worldRoot := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(worldRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Cluster_1", "cluster_token.txt"), []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadClusterToken(context.Background(), root, "Cluster_1", "Master")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateClusterTokenReveal(result); err != nil {
		t.Fatal(err)
	}
	if !result.Exists || result.Token != "secret-token" {
		t.Fatalf("result = %#v", result)
	}
	result.Token = "tampered"
	if err := ValidateClusterTokenReveal(result); err == nil {
		t.Fatal("tampered token reveal was accepted")
	}
}
