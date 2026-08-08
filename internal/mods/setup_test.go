package mods

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderManagedSetupPreservesManualContent(t *testing.T) {
	source := []byte("-- maintained by operator\nServerModSetup(\"999\")\n")
	rendered, err := renderManagedSetup(source, []string{"123", "456"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	for _, expected := range []string{"maintained by operator", `ServerModSetup("999")`, `ServerModSetup("123")`, `ServerModSetup("456")`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q from rendered setup:\n%s", expected, text)
		}
	}
	withoutManaged, err := renderManagedSetup(rendered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withoutManaged), `ServerModSetup("999")`) || strings.Contains(string(withoutManaged), `ServerModSetup("123")`) {
		t.Fatalf("managed removal touched manual content:\n%s", withoutManaged)
	}
}

func TestSafeRemoveDirectoryCannotEscapeRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ugc")
	outside := filepath.Join(parent, "keep")
	inside := filepath.Join(root, "content", "322330", "123")
	for _, path := range []string{outside, inside} {
		if err := os.MkdirAll(path, 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := safeRemoveDirectory(root, outside); err == nil {
		t.Fatal("outside directory removal was accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside directory was changed: %v", err)
	}
	if err := safeRemoveDirectory(root, inside); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("inside directory still exists: %v", err)
	}
}
