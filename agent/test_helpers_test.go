package agent

import (
	"os"
	"testing"
)

// Keep native socket fixtures below the Unix path limit, independently of the
// test name that testing.T.TempDir would include in its directory name.
func shortRuntimeRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "dst-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove temporary runtime: %v", err)
		}
	})
	return root
}
