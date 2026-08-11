package routers

import (
	"path/filepath"
	"testing"
)

func TestResolveWorkshopPathsFromDownloadRoot(t *testing.T) {
	downloadRoot := filepath.Join(t.TempDir(), "steamcmd-library")
	downloadPath, contentPath, err := resolveWorkshopPaths(downloadRoot, "", "322330")
	if err != nil {
		t.Fatal(err)
	}
	if downloadPath != downloadRoot {
		t.Fatalf("download path = %q, want %q", downloadPath, downloadRoot)
	}
	wantContent := filepath.Join(downloadRoot, "steamapps", "workshop", "content", "322330")
	if contentPath != wantContent {
		t.Fatalf("content path = %q, want %q", contentPath, wantContent)
	}
}

func TestResolveWorkshopPathsFromContentRoot(t *testing.T) {
	downloadRoot := filepath.Join(t.TempDir(), "steamcmd-library")
	contentRoot := filepath.Join(downloadRoot, "steamapps", "workshop", "content", "322330")
	downloadPath, contentPath, err := resolveWorkshopPaths("", contentRoot, "322330")
	if err != nil {
		t.Fatal(err)
	}
	if downloadPath != downloadRoot || contentPath != contentRoot {
		t.Fatalf("resolved paths = (%q, %q), want (%q, %q)", downloadPath, contentPath, downloadRoot, contentRoot)
	}
}

func TestResolveWorkshopPathsRejectsMismatchedExplicitPaths(t *testing.T) {
	downloadRoot := filepath.Join(t.TempDir(), "downloads")
	contentRoot := filepath.Join(t.TempDir(), "content")
	if _, _, err := resolveWorkshopPaths(downloadRoot, contentRoot, "322330"); err == nil {
		t.Fatal("expected mismatched Workshop paths to fail")
	}
}
