package maptransfer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"dont/internal/maprenderer"
	"dont/internal/worldmap"
)

type testRenderer struct{}

func (testRenderer) Available() (bool, string) { return true, "test" }

func (testRenderer) Render(_ context.Context, _, output string, _ []worldmap.Layer, log io.Writer) error {
	_, _ = io.WriteString(log, "rendered from /private/save\n")
	for _, item := range []struct {
		name string
		data []byte
	}{
		{maprenderer.TerrainFileName, []byte("terrain")},
		{maprenderer.IconsFileName, []byte("icons")},
		{maprenderer.ManifestFileName, []byte(`{"protocolVersion":"1"}`)},
		{maprenderer.FeaturesFileName, []byte(`{"protocolVersion":"1","features":[]}`)},
	} {
		if err := os.WriteFile(filepath.Join(output, item.name), item.data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func TestManagerListsAndTransfersSessionSnapshot(t *testing.T) {
	saveRoot, stateRoot, source := mapFixture(t)
	manager, err := New(saveRoot, stateRoot, testRenderer{})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.Sessions("room1", "Master")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "ABC123" || sessions[0].FileName != "0000000001" || sessions[0].PlayerCount != 1 || sessions[0].Size != int64(len(source)) {
		t.Fatalf("unexpected sessions: %#v", sessions)
	}
	descriptor, err := manager.PrepareSnapshot(context.Background(), "transfer-snapshot-0001", "room1", "Master", "ABC123", "0000000001")
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Size != int64(len(source)) || descriptor.SHA256 != digest(source) || descriptor.SourceSHA256 != digest(source) {
		t.Fatalf("unexpected descriptor: %#v", descriptor)
	}
	var downloaded bytes.Buffer
	for offset := int64(0); offset < descriptor.Size; {
		chunk, err := manager.Read(context.Background(), descriptor.TransferID, offset)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Offset != offset || chunk.NextOffset <= offset || chunk.Log != "" {
			t.Fatalf("unexpected chunk: %#v", chunk)
		}
		downloaded.Write(chunk.Data)
		offset = chunk.NextOffset
	}
	if !bytes.Equal(downloaded.Bytes(), source) {
		t.Fatal("downloaded snapshot differs from source")
	}
	if err := manager.Release(descriptor.TransferID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Read(context.Background(), descriptor.TransferID, 0); err != ErrTransferMissing {
		t.Fatalf("expected missing transfer, got %v", err)
	}
}

func TestManagerRendersBoundedArtifactArchive(t *testing.T) {
	saveRoot, stateRoot, _ := mapFixture(t)
	manager, err := New(saveRoot, stateRoot, testRenderer{})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := manager.Render(context.Background(), "transfer-render-0001", "room1", "Master", "ABC123", "0000000001", []worldmap.Layer{worldmap.LayerTerrain})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Kind != "artifacts" || descriptor.Log == "" {
		t.Fatalf("unexpected render descriptor: %#v", descriptor)
	}
	if status := manager.RendererStatus(); !status.Available {
		t.Fatalf("renderer status=%#v", status)
	}
	root := filepath.Join(stateRoot, descriptor.TransferID)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("render staging retained unnecessary files: %#v", entries)
	}
	archive, err := zip.OpenReader(filepath.Join(root, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	names := make([]string, 0, len(archive.File))
	for _, entry := range archive.File {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	want := []string{maprenderer.FeaturesFileName, maprenderer.IconsFileName, maprenderer.ManifestFileName, maprenderer.TerrainFileName}
	sort.Strings(want)
	if !equalStrings(names, want) {
		t.Fatalf("archive names=%v want=%v", names, want)
	}
}

func TestManagerRejectsPathEscapeAndTransferReuse(t *testing.T) {
	saveRoot, stateRoot, _ := mapFixture(t)
	manager, err := New(saveRoot, stateRoot, testRenderer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PrepareSnapshot(context.Background(), "transfer-invalid-0001", "../room1", "Master", "ABC123", "0000000001"); err == nil {
		t.Fatal("expected cluster path rejection")
	}
	if _, err := manager.PrepareSnapshot(context.Background(), "transfer-invalid-0002", "room1", "Master", "ABC123", "../0000000001"); err == nil {
		t.Fatal("expected file path rejection")
	}
	if _, err := manager.PrepareSnapshot(context.Background(), "transfer-reuse-0001", "room1", "Master", "ABC123", "0000000001"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saveRoot, "room1", "Master", "save", "session", "ABC123", "0000000002"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PrepareSnapshot(context.Background(), "transfer-reuse-0001", "room1", "Master", "ABC123", "0000000002"); err != ErrInvalidRequest {
		t.Fatalf("expected transfer reuse rejection, got %v", err)
	}
}

func mapFixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	root := t.TempDir()
	saveRoot := filepath.Join(root, "saves")
	stateRoot := filepath.Join(root, "state")
	sessionRoot := filepath.Join(saveRoot, "room1", "Master", "save", "session", "ABC123")
	if err := os.MkdirAll(filepath.Join(sessionRoot, "KU_PLAYER"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := bytes.Repeat([]byte("dst-session-data"), 20000)
	if err := os.WriteFile(filepath.Join(sessionRoot, "0000000001"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	return saveRoot, stateRoot, source
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
