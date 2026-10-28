package runtimeinventory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dont/shared"
)

func TestScanRoomsReportsShardIDRoleAndWorldTypeIndependently(t *testing.T) {
	root := t.TempDir()
	roomPath := filepath.Join(root, "Cluster_Mixed")
	if err := os.MkdirAll(roomPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roomPath, "cluster.ini"), []byte("[NETWORK]\ncluster_name = Mixed\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	writeShard := func(directory string, id int, master bool, override string) {
		t.Helper()
		worldPath := filepath.Join(roomPath, directory)
		if err := os.MkdirAll(worldPath, 0o750); err != nil {
			t.Fatal(err)
		}
		server := []byte("[SHARD]\nid = " + fmt.Sprint(id) + "\nis_master = " + fmt.Sprint(master) + "\n[NETWORK]\nserver_port = 10999\n")
		if err := os.WriteFile(filepath.Join(worldPath, "server.ini"), server, 0o640); err != nil {
			t.Fatal(err)
		}
		if override != "" {
			if err := os.WriteFile(filepath.Join(worldPath, "leveldataoverride.lua"), []byte(override), 0o640); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeShard("CavePrime", 7, true, `return { location = "cave" }`)
	writeShard("ForestTwo", 3, false, `return { worldgen_id = "SURVIVAL_TOGETHER" }`)
	writeShard("Mystery", 9, false, "")

	rooms, warnings, err := scanRooms(root)
	if err != nil || len(warnings) != 0 || len(rooms) != 1 || len(rooms[0].Shards) != 3 {
		t.Fatalf("rooms=%#v warnings=%#v err=%v", rooms, warnings, err)
	}
	shards := rooms[0].Shards
	if shards[0].Directory != "CavePrime" || shards[0].ID != 7 || shards[0].Role != "master" || shards[0].Type != "cave" {
		t.Fatalf("Cave Master inventory=%#v", shards[0])
	}
	if shards[1].Directory != "ForestTwo" || shards[1].ID != 3 || shards[1].Role != "secondary" || shards[1].Type != "forest" {
		t.Fatalf("secondary Forest inventory=%#v", shards[1])
	}
	if shards[2].Directory != "Mystery" || shards[2].ID != 9 || shards[2].Type != "unknown" {
		t.Fatalf("unknown world inventory=%#v", shards[2])
	}
}

func TestScanRoomsIgnoresStagingDirectoriesDuringImportAndPublication(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"MyDediServer", ".dst-admin-import-123", ".dst-admin-rollback-123", ".dst-admin-create-123", ".trash"} {
		room := filepath.Join(root, directory)
		for _, world := range []string{"Master", ".dst-admin-staging-123"} {
			worldPath := filepath.Join(room, world)
			if err := os.MkdirAll(worldPath, 0750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worldPath, "server.ini"), []byte("[SHARD]\nis_master = true\nid = 10001\n"), 0640); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(room, "cluster.ini"), []byte("[NETWORK]\ncluster_name = Imported\n"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	rooms, warnings, err := scanRooms(root)
	if err != nil || len(warnings) != 0 || len(rooms) != 1 || rooms[0].Directory != "MyDediServer" || len(rooms[0].Shards) != 1 || rooms[0].Shards[0].Directory != "Master" {
		t.Fatalf("import staging entered runtime inventory: rooms=%+v warnings=%v err=%v", rooms, warnings, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".dst-admin-import-123", "Master", "server.ini")); err != nil {
		t.Fatalf("inventory scan changed staging files: %v", err)
	}
}

func TestHostResourcesReportsConsistentCPUTopology(t *testing.T) {
	cpu, memory := HostResources()
	if cpu.LogicalProcessors < 1 || cpu.PhysicalCores < 0 || cpu.PhysicalCores > cpu.LogicalProcessors {
		t.Fatalf("invalid CPU inventory: %#v", cpu)
	}
	if cpu.TopologyAvailable && len(cpu.Threads) != cpu.LogicalProcessors {
		t.Fatalf("available CPU topology is incomplete: logical=%d threads=%d", cpu.LogicalProcessors, len(cpu.Threads))
	}
	seen := make(map[int]bool, len(cpu.Threads))
	for _, thread := range cpu.Threads {
		if thread.LogicalID < 0 || thread.LogicalID >= cpu.LogicalProcessors || seen[thread.LogicalID] || thread.PackageID == "" || thread.CoreID == "" {
			t.Fatalf("invalid CPU thread inventory: %#v", thread)
		}
		seen[thread.LogicalID] = true
	}
	t.Logf("logical=%d physical=%d topology=%t smt=%t threads=%d memory=%d", cpu.LogicalProcessors, cpu.PhysicalCores, cpu.TopologyAvailable, cpu.SMTDetected, len(cpu.Threads), memory.TotalBytes)
}

func TestInstallationProcessesUsesPathEvidenceBeforeShardIdentityFallback(t *testing.T) {
	primarySave := filepath.Join(string(filepath.Separator), "srv", "primary", "DoNotStarveTogether")
	primaryServer := filepath.Join(string(filepath.Separator), "opt", "dst-primary")
	values := []shared.ShardProcessReport{
		{PID: 1, Cluster: "Cluster_A", Shard: "Master", StorageRoot: filepath.Join(string(filepath.Separator), "srv", "primary"), ConfigDirectory: "DoNotStarveTogether"},
		{PID: 2, Cluster: "Cluster_A", Shard: "Caves", Executable: filepath.Join(primaryServer, "bin64", "dontstarve_dedicated_server_nullrenderer")},
		{PID: 3, Cluster: "Cluster_A", Shard: "Master", StorageRoot: filepath.Join(string(filepath.Separator), "srv", "testing"), ConfigDirectory: "DoNotStarveTogether"},
		{PID: 4, Cluster: "Cluster_A", Shard: "Master"},
		{PID: 5, Cluster: "Other", Shard: "Master"},
	}
	rooms := []shared.RoomInventoryReport{{
		Directory: "Cluster_A",
		Shards:    []shared.ShardInventoryReport{{Directory: "Master"}, {Directory: "Caves"}},
	}}

	filtered := installationProcesses(values, rooms, primarySave, primaryServer)
	if len(filtered) != 3 || filtered[0].PID != 1 || filtered[1].PID != 2 || filtered[2].PID != 4 {
		t.Fatalf("filtered processes=%#v", filtered)
	}
}

func TestDSTProcessMatchRejectsSupervisorWithEmbeddedLaunchCommand(t *testing.T) {
	launch := []string{
		"tmux", "-S", "/tmp/dst.sock", "new-session", "-d",
		"/opt/dst/bin64/dontstarve_dedicated_server_nullrenderer_x64",
		"-cluster", "all", "-shard", "Master",
	}
	if IsDSTServerProcess("tmux: server", launch) {
		t.Fatal("tmux supervisor must not be counted as a DST shard process")
	}
	if !IsDSTServerProcess("dontstarve_dedicated_server_nullrenderer_x64", launch[5:]) {
		t.Fatal("actual DST process was not recognized")
	}
}

func TestShardProcessReadsRuntimeModeFromActualArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want shared.RuntimePerformanceMode
	}{
		{nil, shared.RuntimePerformanceModeGame},
		{[]string{"-lua_vm_type=game"}, shared.RuntimePerformanceModeGame},
		{[]string{"-lua_vm_type=jit"}, shared.RuntimePerformanceModeLuaJIT},
		{[]string{"-lua_vm_type", "jit_gen"}, shared.RuntimePerformanceModeArenaGC},
		{[]string{"-lua_vm_type=future"}, "unknown"},
	} {
		value := ShardProcessFromArguments(42, "dontstarve_dedicated_server_nullrenderer_x64", tc.args)
		if value.RuntimeMode != tc.want {
			t.Fatalf("args=%v got=%s want=%s", tc.args, value.RuntimeMode, tc.want)
		}
	}
}

func TestInventoryReportsPhysicalInstallationRootWithoutChangingConfiguredPath(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "server-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip(err)
	}
	report, err := Collect(context.Background(), shared.RuntimeInventoryRequest{InstallationID: "one", SavePath: t.TempDir(), ServerPath: alias})
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Installation.ServerPath != alias || report.Installation.ServerRoot != resolvedRoot {
		t.Fatalf("configured path or canonical identity lost: %+v", report.Installation)
	}
}
