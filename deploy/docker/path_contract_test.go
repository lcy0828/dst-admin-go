package docker

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerArtifactsUseOptDSTDataRoot(t *testing.T) {
	legacyServiceRoot := "/" + "srv"
	legacyDataRoot := "/" + "data"
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), legacyServiceRoot) {
			t.Errorf("Docker artifact %s still references legacy root %s", path, legacyServiceRoot)
		}
		for lineNumber, line := range strings.Split(string(content), "\n") {
			if !strings.Contains(line, legacyDataRoot) {
				continue
			}
			legacyMigration := path == "all-in-one-entrypoint.sh" &&
				(strings.Contains(line, "grep -Eq") || strings.Contains(line, "sed -E"))
			if !legacyMigration {
				t.Errorf("Docker artifact %s:%d still uses legacy data root %s", path, lineNumber+1, legacyDataRoot)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
