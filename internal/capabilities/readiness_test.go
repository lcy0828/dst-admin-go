package capabilities

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindDSTExecutableAndReadinessRoomCount(t *testing.T) {
	root := t.TempDir()
	executablePath := filepath.Join(root, "bin64", "dontstarve_dedicated_server_nullrenderer_x64")
	if err := os.MkdirAll(filepath.Dir(executablePath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executablePath, []byte("test"), 0750); err != nil {
		t.Fatal(err)
	}
	if actual := findDSTExecutable(root); actual != executablePath {
		t.Fatalf("executable = %q, want %q", actual, executablePath)
	}
	readiness := ProbeReadiness(Config{SavePath: root, BackupPath: root, ServerPath: root}, func() (int, error) { return 3, nil })
	foundRooms := false
	for _, check := range readiness.Checks {
		if check.ID == "rooms" {
			foundRooms = check.Status == CheckPass && check.Details["count"] == 3
		}
	}
	if !foundRooms {
		t.Fatalf("room discovery check missing: %#v", readiness.Checks)
	}
}
