package distributedbackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/maintenance"
)

func TestMaintenanceGuardProtectsColdBackupStops(t *testing.T) {
	for _, local := range []bool{true, false} {
		t.Run(map[bool]string{true: "local", false: "split"}[local], func(t *testing.T) {
			f := newBackupPlacementFixture(t, local)
			// Preserve the entire test save tree, independent of its fixture format.
			before := map[string]string{}
			for _, root := range []string{f.masterRoot, f.cavesRoot} {
				if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if entry.IsDir() {
						return nil
					}
					content, err := os.ReadFile(path)
					if err == nil {
						before[path] = string(content)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			ctx := maintenance.WithCheck(context.Background(), func(context.Context) error { return maintenance.ErrPlayersOnline })
			_, err := f.coordinator.Create(ctx, "room", "guarded", "protection", "job")
			if !errors.Is(err, maintenance.ErrPlayersOnline) {
				t.Fatalf("guard ignored: %v", err)
			}
			for path, content := range before {
				actual, err := os.ReadFile(path)
				if err != nil || string(actual) != content {
					t.Fatalf("save changed: %s: %v", path, err)
				}
			}
			if len(f.master.stops) != 0 || len(f.caves.stops) != 0 {
				t.Fatalf("backup stopped runtime: %v %v", f.master.stops, f.caves.stops)
			}
		})
	}
}
