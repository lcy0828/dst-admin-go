package configpublication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInlineApplyPreservesRevisionsModesAndOtherWorlds(t *testing.T) {
	for _, scenario := range []string{"success", "manual edit", "missing", "symlink", "canceled", "wrong scope"} {
		t.Run(scenario, func(t *testing.T) {
			root, state := t.TempDir(), t.TempDir()
			world := filepath.Join(root, "Cluster_1", "Master")
			caves := filepath.Join(root, "Cluster_1", "Caves")
			for _, directory := range []string{world, caves} {
				if err := os.MkdirAll(directory, 0750); err != nil {
					t.Fatal(err)
				}
			}
			old := map[string][]byte{"server.ini": []byte("[CUSTOM]\nmanual=yes\n"), "leveldataoverride.lua": []byte("return { custom = 123 }\n")}
			expected := map[string]string{}
			for name, data := range old {
				for _, directory := range []string{world, caves} {
					if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				sum := sha256.Sum256(data)
				expected[name] = hex.EncodeToString(sum[:])
			}
			next := map[string][]byte{"server.ini": []byte("[CUSTOM]\nmanual=yes\n[NETWORK]\nserver_port=11001\n"), "leveldataoverride.lua": []byte("return { custom = 123, overrides = { season = 'winter' } }\n")}
			payload := publicationArchive(t, next)
			descriptor := testDescriptor("inline-world-0001", "Cluster_1", "Master", ScopeWorld, payload)
			manager, err := New(root, state)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			path := filepath.Join(world, "server.ini")
			switch scenario {
			case "manual edit":
				if err := os.WriteFile(path, []byte("operator edit"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing", "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if scenario == "symlink" {
					if err := os.Symlink(filepath.Join(caves, "server.ini"), path); err != nil {
						t.Fatal(err)
					}
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "wrong scope":
				delete(expected, "server.ini")
				expected["../server.ini"] = expected["leveldataoverride.lua"]
			}
			warnings, err := manager.Apply(ctx, descriptor, payload, expected)
			if scenario == "success" {
				if err != nil || len(warnings) != 0 {
					t.Fatalf("apply=%v, warnings=%v", err, warnings)
				}
				for name, data := range next {
					assertFile(t, filepath.Join(world, name), string(data))
					info, statErr := os.Stat(filepath.Join(world, name))
					if statErr != nil || info.Mode().Perm() != 0600 {
						t.Fatalf("mode changed: %v, %v", info, statErr)
					}
				}
			} else {
				if err == nil {
					t.Fatal("invalid save succeeded")
				}
				if (scenario == "manual edit" || scenario == "missing") && !errors.Is(err, ErrRevisionConflict) {
					t.Fatalf("file revision conflict hidden: %v", err)
				}
				assertFile(t, filepath.Join(world, "leveldataoverride.lua"), string(old["leveldataoverride.lua"]))
				if scenario == "manual edit" {
					assertFile(t, path, "operator edit")
				}
			}
			for name, data := range old {
				assertFile(t, filepath.Join(caves, name), string(data))
			}
			entries, err := os.ReadDir(state)
			if err != nil || len(entries) != 0 {
				t.Fatalf("completed/rejected inline operation left state: %v, %v", entries, err)
			}
		})
	}
}
