package runtimefiles

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/shared"
)

func healthFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	world := filepath.Join(root, "Room", "Master")
	output := filepath.Join(world, "save/mod_config_data/dst-admin")
	for _, path := range []string{output, filepath.Join(world, "dst-admin")} {
		if err := os.MkdirAll(path, 0750); err != nil {
			t.Fatal(err)
		}
	}
	return root, world, output
}

func writeHealthFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
}

func writeHealthManifest(t *testing.T, world, version string) {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{"version": version, "protocolVersion": 2, "assets": map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	writeHealthFixture(t, filepath.Join(world, "dst-admin/manifest.json"), data)
}

func TestRuntimeHealthSelectsInstalledVersionAndPreservesTransportContract(t *testing.T) {
	root, world, output := healthFixture(t)
	writeHealthManifest(t, world, "2.4.12")
	current := []byte(`{"producerVersion":"2.4.12","running":true}`)
	writeHealthFixture(t, filepath.Join(output, "health-2.4.12.json"), current)
	modified := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(output, "health-2.4.12.json"), modified, modified); err != nil {
		t.Fatal(err)
	}
	// A later write from the retired instance cannot win by mtime.
	legacy := []byte(`{"producerVersion":"2.4.11","running":false}`)
	writeHealthFixture(t, filepath.Join(output, "health.json"), legacy)
	bundle, err := ReadArtifacts(context.Background(), root, "Room", "Master", shared.ArtifactRuntimeHealth)
	if err != nil || len(bundle.Artifacts) != 1 {
		t.Fatalf("health bundle: %+v %v", bundle, err)
	}
	artifact := bundle.Artifacts[0]
	if artifact.Name != "health.json" || string(artifact.Data) != string(current) || !artifact.UpdatedAt.Equal(modified) {
		t.Fatalf("wrong health or transport metadata: %+v", artifact)
	}
	if err := ValidateArtifactBundle(shared.ArtifactRuntimeHealth, bundle); err != nil {
		t.Fatalf("new health is incompatible with existing transport: %v", err)
	}
	// File recovery after an unsuccessful activation must also restore reads.
	writeHealthManifest(t, world, "2.4.11")
	data, _, exists, err := ReadRuntimeHealth(world)
	if err != nil || !exists || string(data) != string(legacy) {
		t.Fatalf("legacy recovery selected candidate health: %s %v", data, err)
	}
	// Later versioned upgrades select the previous version's own file.
	writeHealthManifest(t, world, "2.4.13")
	writeHealthFixture(t, filepath.Join(output, "health-2.4.13.json"), []byte(`{"running":false}`))
	writeHealthManifest(t, world, "2.4.12")
	data, _, exists, err = ReadRuntimeHealth(world)
	if err != nil || !exists || string(data) != string(current) {
		t.Fatalf("versioned recovery selected newer health: %s %v", data, err)
	}
}

func TestRuntimeHealthKeepsLegacyInstallationsReadable(t *testing.T) {
	for _, version := range []string{"", "2.4.11", "2.4.12"} {
		t.Run("manifest="+version, func(t *testing.T) {
			_, world, output := healthFixture(t)
			if version != "" {
				writeHealthManifest(t, world, version)
			}
			data, _, exists, err := ReadRuntimeHealth(world)
			if err != nil || exists || data != nil {
				t.Fatalf("missing health: %s %v %v", data, exists, err)
			}
			writeHealthFixture(t, filepath.Join(output, "health.json"), []byte(`{"running":true}`))
			data, _, exists, err = ReadRuntimeHealth(world)
			if err != nil || !exists || string(data) != `{"running":true}` {
				t.Fatalf("legacy fallback: %s %v", data, err)
			}
		})
	}
}

func TestRuntimeHealthRejectsUnsafeSelectionWithoutLegacyFallback(t *testing.T) {
	for _, scenario := range []string{"version path", "invalid manifest", "oversized manifest", "manifest symlink", "managed symlink", "health symlink", "oversized health", "empty health", "invalid health"} {
		t.Run(scenario, func(t *testing.T) {
			root, world, output := healthFixture(t)
			writeHealthManifest(t, world, "2.4.12")
			writeHealthFixture(t, filepath.Join(output, "health.json"), []byte(`{"ready":true}`))
			manifestPath := filepath.Join(world, "dst-admin/manifest.json")
			healthPath := filepath.Join(output, "health-2.4.12.json")
			switch scenario {
			case "version path":
				writeHealthManifest(t, world, "../../outside")
			case "invalid manifest":
				writeHealthFixture(t, manifestPath, []byte(`{"version":`))
			case "oversized manifest":
				writeHealthFixture(t, manifestPath, []byte(strings.Repeat(" ", int(maximumHealthBytes)+1)))
			case "manifest symlink":
				if err := os.Remove(manifestPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(output, "health.json"), manifestPath); err != nil {
					t.Fatal(err)
				}
			case "managed symlink":
				managed := filepath.Join(world, "dst-admin")
				if err := os.Rename(managed, managed+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(managed+"-original", managed); err != nil {
					t.Fatal(err)
				}
			case "health symlink":
				if err := os.Symlink(filepath.Join(output, "health.json"), healthPath); err != nil {
					t.Fatal(err)
				}
			case "oversized health":
				writeHealthFixture(t, healthPath, []byte(strings.Repeat(" ", int(maximumHealthBytes)+1)))
			case "empty health":
				writeHealthFixture(t, healthPath, nil)
			case "invalid health":
				writeHealthFixture(t, healthPath, []byte(`{"running":`))
			}
			bundle, err := ReadArtifacts(context.Background(), root, "Room", "Master", shared.ArtifactRuntimeHealth)
			if scenario == "invalid health" {
				// The reader returns selected bytes for the strict decoder. It must
				// never silently replace corrupt current output with legacy data.
				if err != nil || len(bundle.Artifacts) != 1 || string(bundle.Artifacts[0].Data) != `{"running":` {
					t.Fatalf("corrupt selected file hidden: %+v %v", bundle, err)
				}
			} else if err == nil {
				t.Fatalf("unsafe selected file fell back to legacy health: %+v", bundle)
			}
		})
	}
}
