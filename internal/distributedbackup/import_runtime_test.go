package distributedbackup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
)

type importCollectorCatalog struct{}

func (importCollectorCatalog) Room(string) (rooms.Room, error) {
	return rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "测试房间", Managed: true}, nil
}
func (importCollectorCatalog) World(_ string, id string) (rooms.World, error) {
	name := "Master"
	if id == "caves" {
		name = "Caves"
	}
	return rooms.World{ID: id, RoomID: "room", DirectoryName: name, Name: name}, nil
}
func (r importCollectorCatalog) Worlds(id string) ([]rooms.World, error) {
	a, _ := r.World(id, "master")
	b, _ := r.World(id, "caves")
	return []rooms.World{a, b}, nil
}

func TestCoordinatedImportKeepsEnabledCollectors(t *testing.T) {
	for _, local := range []bool{true, false} {
		t.Run(map[bool]string{true: "local", false: "split-targets"}[local], func(t *testing.T) {
			f := newBackupPlacementFixture(t, local)
			for _, item := range []struct{ root, id string }{{f.masterRoot, "master"}, {f.cavesRoot, "caves"}} {
				manager, err := dstruntime.NewManager(item.root, importCollectorCatalog{})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.InstallWorld(context.Background(), "room", item.id); err != nil {
					t.Fatal(err)
				}
			}
			source := t.TempDir()
			for path, data := range map[string]string{
				"cluster.ini":                        "[NETWORK]\ncluster_name=Imported\n",
				"Master/server.ini":                  "[SHARD]\nis_master=true\nid=1\n",
				"Master/save/saveindex":              `return {slots={{session_id="NEW"}}}`,
				"Master/save/session/NEW/0000000042": "new-master-world",
				"Caves/server.ini":                   "[SHARD]\nis_master=false\nid=2\n",
				"Caves/save/shardindex":              `return {session_id="NEW"}`,
				"Caves/save/session/NEW/0000000042":  "new-caves-world",
			} {
				target := filepath.Join(source, path)
				if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte(data), 0640); err != nil {
					t.Fatal(err)
				}
			}
			worlds := []DirectoryImportWorld{{WorldID: "master", DirectoryName: "Master"}, {WorldID: "caves", DirectoryName: "Caves"}}
			if !local {
				for index := range worlds {
					world := &worlds[index]
					world.SourceRoot = filepath.Join(source, "nodes", world.DirectoryName)
					if err := os.MkdirAll(world.SourceRoot, 0700); err != nil {
						t.Fatal(err)
					}
					cluster := "[NETWORK]\ncluster_name=Imported\n[SHARD]\nbind_ip=0.0.0.0\n"
					if world.WorldID == "caves" {
						cluster += "master_ip=192.0.2.24\n"
					}
					if err := os.WriteFile(filepath.Join(world.SourceRoot, "cluster.ini"), []byte(cluster), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(filepath.Join(source, world.DirectoryName), filepath.Join(world.SourceRoot, world.DirectoryName)); err != nil {
						t.Fatal(err)
					}
				}
			}
			imported, err := f.coordinator.ImportDirectory(context.Background(), DirectoryImportRequest{RoomID: "room", SourceRoot: source, Worlds: worlds})
			if err != nil {
				t.Fatal(err)
			}
			restored, err := f.coordinator.Restore(context.Background(), imported.ID, "测试房间", "review")
			if err != nil {
				t.Fatal(err)
			}
			if !local {
				data, err := os.ReadFile(filepath.Join(f.cavesRoot, "Cluster_1/cluster.ini"))
				if err != nil || !bytes.Contains(data, []byte("192.0.2.24")) {
					t.Fatalf("split node configuration lost: %q %v", data, err)
				}
			}
			if restored.ProtectionSetID == "" {
				t.Fatal("restore has no protection backup")
			}

			for _, item := range []struct{ root, id string }{{f.masterRoot, "master"}, {f.cavesRoot, "caves"}} {
				manager, _ := dstruntime.NewManager(item.root, importCollectorCatalog{})
				statuses, err := manager.StatusRoom("room")
				if err != nil {
					t.Fatal(err)
				}
				for _, status := range statuses {
					if status.WorldID == item.id && status.State != dstruntime.InstallStateInstalled {
						t.Errorf("previously installed collector lost on %s: %+v", item.id, status)
					}
				}
			}
		})
	}
}

func TestImportRuntimePreparationFailureKeepsOriginalSaveAndCollector(t *testing.T) {
	f := newBackupPlacementFixture(t, false)
	for _, item := range []struct{ root, id string }{{f.masterRoot, "master"}, {f.cavesRoot, "caves"}} {
		manager, err := dstruntime.NewManager(item.root, importCollectorCatalog{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.InstallWorld(context.Background(), "room", item.id); err != nil {
			t.Fatal(err)
		}
	}
	source := t.TempDir()
	for name, data := range map[string]string{
		"cluster.ini":                        "[NETWORK]\ncluster_name=Imported\n",
		"Master/server.ini":                  "[SHARD]\nis_master=true\nid=1\n",
		"Caves/server.ini":                   "[SHARD]\nis_master=false\nid=2\n",
		"Master/save/shardindex":             `return {session_id="NEW"}`,
		"Caves/save/shardindex":              `return {session_id="NEW"}`,
		"Master/save/session/NEW/0000000001": "new-master",
		"Caves/save/session/NEW/0000000001":  "new-caves",
		"Master/customcommands.lua":          "invalid lua syntax !",
	} {
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	imported, err := f.coordinator.ImportDirectory(context.Background(), DirectoryImportRequest{RoomID: "room", SourceRoot: source, Worlds: []DirectoryImportWorld{{WorldID: "master", DirectoryName: "Master"}, {WorldID: "caves", DirectoryName: "Caves"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.coordinator.Restore(context.Background(), imported.ID, "测试房间", "test"); err == nil {
		t.Fatal("invalid collector published")
	}
	for _, item := range []struct{ root, id, world, data string }{{f.masterRoot, "master", "Master", "master-v1"}, {f.cavesRoot, "caves", "Caves", "caves-v1"}} {
		assertTextFile(t, filepath.Join(item.root, "Cluster_1", item.world, "save/session/SESSION/0000000001"), item.data)
		manager, err := dstruntime.NewManager(item.root, importCollectorCatalog{})
		if err != nil {
			t.Fatal(err)
		}
		statuses, err := manager.StatusRoom("room")
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range statuses {
			if status.WorldID == item.id && status.State != dstruntime.InstallStateInstalled {
				t.Fatalf("collector changed after failure: %+v", status)
			}
		}
	}
	assertControlRunning(t, f.master, "Master")
	assertControlRunning(t, f.caves, "Caves")
}

func TestOldAgentRestoreIsRejectedBeforeStoppingWorlds(t *testing.T) {
	f := newBackupPlacementFixture(t, false)
	created, err := f.coordinator.Create(context.Background(), "room", "Existing save", "manual", "test")
	if err != nil {
		t.Fatal(err)
	}
	before := make(map[*backupTestControl]map[string]int)
	for _, control := range []*backupTestControl{f.master, f.caves} {
		control.mu.Lock()
		counts := make(map[string]int)
		for shard, count := range control.stops {
			counts[shard] = count
		}
		before[control] = counts
		control.mu.Unlock()
	}
	router := f.coordinator.runtimes.(backupTestRouter)
	for id, value := range router.values {
		value.target.CapabilitiesKnown = true
		value.target.Capabilities = []runtimedriver.Capability{runtimedriver.CapabilityBackupStage, runtimedriver.CapabilityBackupRestore}
		router.values[id] = value
	}
	if _, err := f.coordinator.Restore(context.Background(), created.ID, "测试房间", "test"); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("old Agent accepted restore: %v", err)
	}
	if _, err := f.coordinator.ImportDirectory(context.Background(), DirectoryImportRequest{RoomID: "room"}); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("old Agent accepted import: %v", err)
	}
	for _, control := range []*backupTestControl{f.master, f.caves} {
		control.mu.Lock()
		for shard, count := range control.stops {
			if count != before[control][shard] {
				t.Errorf("stopped %s before capability check", shard)
			}
		}
		control.mu.Unlock()
	}
}
