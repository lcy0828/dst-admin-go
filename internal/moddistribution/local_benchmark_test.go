package moddistribution

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Each operation includes the directory and literal metadata reads. No Steam,
// full Lua parser, tree hashing or application metadata cache participates.
func BenchmarkModInventory(b *testing.B) {
	for _, count := range []int{8, 50, 200} {
		for _, mode := range []string{"legacy", "workshop", "mixed"} {
			b.Run(fmt.Sprintf("count=%d/%s", count, mode), func(b *testing.B) {
				root, err := filepath.EvalSymlinks(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				installation := TrustedInstallation{ID: "primary", ServerPath: filepath.Join(root, "server"), SavePath: filepath.Join(root, "saves")}
				for _, dir := range []string{installation.ServerPath, installation.SavePath} {
					if err := os.MkdirAll(dir, 0750); err != nil {
						b.Fatal(err)
					}
				}
				for i := 0; i < count; i++ {
					name := fmt.Sprintf("workshop-%d", i+100)
					if mode == "mixed" && i%2 == 0 {
						name = fmt.Sprintf("local_mod_%d", i)
					}
					dir := filepath.Join(installation.ServerPath, "mods", name)
					if err := os.MkdirAll(dir, 0750); err != nil {
						b.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "modinfo.lua"), []byte("name='Fixture'\nversion='1.0'\nauthor='Fixture'\n"), 0640); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var result FilesObservation
					if mode != "legacy" {
						result, err = InventoryInstallationMods(context.Background(), installation)
					} else {
						result, err = InventoryInstallationFiles(context.Background(), installation)
					}
					if err != nil || len(result.Mods) != count {
						b.Fatalf("inventory=%d %v", len(result.Mods), err)
					}
				}
			})
		}
	}
}
