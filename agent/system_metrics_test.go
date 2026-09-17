package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentDiskProbePathsPrefersRuntimeSavePath(t *testing.T) {
	root := t.TempDir()
	savePath := filepath.Join(root, "clusters")
	if err := os.MkdirAll(savePath, 0o755); err != nil {
		t.Fatal(err)
	}

	paths := agentDiskProbePaths([]RuntimeInstallation{{SavePath: savePath}})
	if len(paths) == 0 || paths[0] != savePath {
		t.Fatalf("expected save path first, got %v", paths)
	}
}

func TestNearestExistingPathUsesParentForPendingRuntimeDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pending", "cluster")
	if got := nearestExistingPath(path); got != root {
		t.Fatalf("expected %q, got %q", root, got)
	}
}
