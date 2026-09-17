package moddistribution

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func localModFixture(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, content := filepath.Join(root, "server"), filepath.Join(root, "workshop", "content", "322330")
	for _, path := range []string{server, filepath.Join(content, "123")} {
		if err := os.MkdirAll(path, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(content, "123", "modinfo.lua"), []byte(`name="Test";version="1"`), 0o640); err != nil {
		t.Fatal(err)
	}
	return server, content
}

func TestLocalModLinksReuseContentWithoutWorkshopRegistration(t *testing.T) {
	server, content := localModFixture(t)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := LinkWorkshopMods(context.Background(), server, content, []string{"123"}); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	entry := filepath.Join(server, "mods", "workshop-123")
	if target, err := os.Readlink(entry); err != nil || target != filepath.Join(content, "123") {
		t.Fatalf("target=%q err=%v", target, err)
	}
	original, _ := os.Stat(filepath.Join(content, "123", "modinfo.lua"))
	linked, err := os.Stat(filepath.Join(entry, "modinfo.lua"))
	if err != nil || !os.SameFile(original, linked) {
		t.Fatalf("content was not reused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(server, "mods", "dedicated_server_mods_setup.lua")); !os.IsNotExist(err) {
		t.Fatalf("linking must not write a download setup: %v", err)
	}
	observation, err := ObserveInstallationFiles(context.Background(), TrustedInstallation{ID: "native", ServerPath: server, SavePath: server, WorkshopContentPath: content}, []string{"123"}, nil)
	if err != nil || observation.Mods["123"].Status != FileReady || observation.Mods["123"].Reason != "" {
		t.Fatalf("local Mod should load without an ACF: %#v err=%v", observation, err)
	}
}

func TestLocalModLinksPreserveManualDirectory(t *testing.T) {
	server, content := localModFixture(t)
	entry := filepath.Join(server, "mods", "workshop-123")
	if err := os.MkdirAll(entry, 0o750); err != nil {
		t.Fatal(err)
	}
	manual := []byte("name=\"Manual\"\nversion=\"edited\"\n")
	if err := os.WriteFile(filepath.Join(entry, "modinfo.lua"), manual, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := LinkWorkshopMods(context.Background(), server, content, []string{"123"}); err != nil {
		t.Fatal(err)
	}
	if actual, err := os.ReadFile(filepath.Join(entry, "modinfo.lua")); err != nil || string(actual) != string(manual) {
		t.Fatalf("manual file changed: %q err=%v", actual, err)
	}
	observation, err := ObserveInstallationFiles(context.Background(), TrustedInstallation{ID: "native", ServerPath: server, SavePath: server, WorkshopContentPath: content}, []string{"123"}, nil)
	if err != nil || observation.Mods["123"].Version != "edited" {
		t.Fatalf("must observe the actual local version: %#v err=%v", observation, err)
	}
}

func TestLocalModLinksRejectMissingContentAndForeignEntries(t *testing.T) {
	for _, mode := range []string{"missing", "foreign_link", "file", "traversal"} {
		t.Run(mode, func(t *testing.T) {
			server, content := localModFixture(t)
			entry := filepath.Join(server, "mods", "workshop-123")
			if err := os.MkdirAll(filepath.Dir(entry), 0o750); err != nil {
				t.Fatal(err)
			}
			ids := []string{"123"}
			switch mode {
			case "missing":
				ids = append(ids, "456")
			case "traversal":
				ids = append(ids, "../456")
			case "foreign_link":
				if err := os.Symlink(t.TempDir(), entry); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(entry, []byte("operator file"), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			if err := LinkWorkshopMods(context.Background(), server, content, ids); err == nil {
				t.Fatal("invalid request succeeded")
			}
			if mode == "missing" || mode == "traversal" {
				if _, err := os.Lstat(entry); !os.IsNotExist(err) {
					t.Fatalf("failed batch created an entry: %v", err)
				}
			}
		})
	}
}
