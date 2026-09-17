package runtimefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"dont/shared"
)

func TestPublishCommandDocumentIsAtomicAndRejectsInvalidInput(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "Cluster_1", "Master", "dst-admin")
	if err := os.MkdirAll(runtimeRoot, 0750); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"requestId":"document-request-1234","action":"console.execute","arguments":{"script":"return true"}}`)
	sum := sha256.Sum256(data)
	document := shared.RuntimeCommandDocument{
		RequestID: "document-request-1234", SHA256: hex.EncodeToString(sum[:]), Data: data,
	}
	if err := PublishCommandDocument(context.Background(), root, "Cluster_1", "Master", document); err != nil {
		t.Fatal(err)
	}
	if err := PublishCommandDocument(context.Background(), root, "Cluster_1", "Master", document); err != nil {
		t.Fatalf("idempotent publish failed: %v", err)
	}
	directory := filepath.Join(runtimeRoot, runtimeCommandDocumentDirectory)
	path := filepath.Join(directory, document.RequestID+".json")
	written, err := os.ReadFile(path)
	if err != nil || string(written) != string(data) {
		t.Fatalf("written=%q err=%v", written, err)
	}
	invalid := document
	invalid.SHA256 = "00"
	if err := PublishCommandDocument(context.Background(), root, "Cluster_1", "Master", invalid); err == nil {
		t.Fatal("invalid digest was accepted")
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(data) {
		t.Fatalf("invalid publish changed document: %q err=%v", unchanged, err)
	}
	conflictData := []byte(`{"requestId":"document-request-1234","action":"console.execute","arguments":{"script":"return false"}}`)
	conflictSum := sha256.Sum256(conflictData)
	conflict := shared.RuntimeCommandDocument{RequestID: document.RequestID, SHA256: hex.EncodeToString(conflictSum[:]), Data: conflictData}
	if err := PublishCommandDocument(context.Background(), root, "Cluster_1", "Master", conflict); err == nil {
		t.Fatal("conflicting content reused an existing request ID")
	}
	unchanged, err = os.ReadFile(path)
	if err != nil || string(unchanged) != string(data) {
		t.Fatalf("conflicting publish changed document: %q err=%v", unchanged, err)
	}
	secondData := []byte(`{"requestId":"document-request-9876","action":"console.execute","arguments":{"script":"return true"}}`)
	secondSum := sha256.Sum256(secondData)
	second := shared.RuntimeCommandDocument{RequestID: "document-request-9876", SHA256: hex.EncodeToString(secondSum[:]), Data: secondData}
	if err := PublishCommandDocument(context.Background(), root, "Cluster_1", "Master", second); err != nil {
		t.Fatal(err)
	}
	if secondWritten, err := os.ReadFile(filepath.Join(directory, second.RequestID+".json")); err != nil || string(secondWritten) != string(secondData) {
		t.Fatalf("second document=%q err=%v", secondWritten, err)
	}
}

func TestPublishCommandDocumentIsIdempotentUnderConcurrency(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "Cluster_1", "Master", "dst-admin")
	if err := os.MkdirAll(runtimeRoot, 0750); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"requestId":"concurrent-document-1234","action":"console.execute","arguments":{"script":"return true"}}`)
	sum := sha256.Sum256(data)
	document := shared.RuntimeCommandDocument{RequestID: "concurrent-document-1234", SHA256: hex.EncodeToString(sum[:]), Data: data}
	var wait sync.WaitGroup
	errorsFound := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsFound <- PublishCommandDocument(context.Background(), root, "Cluster_1", "Master", document)
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent publish failed: %v", err)
		}
	}
	path := filepath.Join(runtimeRoot, runtimeCommandDocumentDirectory, document.RequestID+".json")
	written, err := os.ReadFile(path)
	if err != nil || string(written) != string(data) {
		t.Fatalf("written=%q err=%v", written, err)
	}
}

func TestPublishCommandDocumentRejectsSymlinkedRuntimeDirectory(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "Cluster_1", "Master")
	escaped := filepath.Join(root, "escaped")
	if err := os.MkdirAll(world, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(escaped, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escaped, filepath.Join(world, "dst-admin")); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"requestId":"document-request-5678"}`)
	sum := sha256.Sum256(data)
	document := shared.RuntimeCommandDocument{RequestID: "document-request-5678", SHA256: hex.EncodeToString(sum[:]), Data: data}
	if err := PublishCommandDocument(context.Background(), root, "Cluster_1", "Master", document); err == nil {
		t.Fatal("symlinked runtime directory was accepted")
	}
}

func TestPublishCommandDocumentRejectsSymlinkedRequestDirectory(t *testing.T) {
	root := t.TempDir()
	runtimeRoot := filepath.Join(root, "Cluster_1", "Master", "dst-admin")
	escaped := filepath.Join(root, "escaped")
	if err := os.MkdirAll(runtimeRoot, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(escaped, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escaped, filepath.Join(runtimeRoot, runtimeCommandDocumentDirectory)); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"requestId":"document-request-9012"}`)
	sum := sha256.Sum256(data)
	document := shared.RuntimeCommandDocument{RequestID: "document-request-9012", SHA256: hex.EncodeToString(sum[:]), Data: data}
	if err := PublishCommandDocument(context.Background(), root, "Cluster_1", "Master", document); err == nil {
		t.Fatal("symlinked command request directory was accepted")
	}
}
