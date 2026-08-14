package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"dont/shared"
)

func TestRuntimeInventoryDiscoversRoomsAndShards(t *testing.T) {
	root := t.TempDir()
	serverRoot := t.TempDir()
	roomRoot := filepath.Join(root, "Cluster_1")
	for _, directory := range []string{roomRoot, filepath.Join(roomRoot, "Master"), filepath.Join(roomRoot, "Caves")} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeInventoryFixture(t, filepath.Join(roomRoot, "cluster.ini"), "[NETWORK]\ncluster_name = 朋友世界\n[SHARD]\nmaster_port = 10889\ncluster_key = secret-present\n")
	writeInventoryFixture(t, filepath.Join(roomRoot, "Master", "server.ini"), "[NETWORK]\nserver_port = 10999\n[SHARD]\nis_master = true\nname = 地表\nid = 1\n[STEAM]\nmaster_server_port = 27018\nauthentication_port = 8768\n")
	writeInventoryFixture(t, filepath.Join(roomRoot, "Caves", "server.ini"), "[NETWORK]\nserver_port = 10998\n[SHARD]\nis_master = false\nname = 洞穴\nid = 2\n")

	value, err := (&Agent{}).collectRuntimeInventory(context.Background(), shared.RuntimeInventoryRequest{
		InstallationID: "default", DisplayName: "测试节点", SavePath: root, ServerPath: serverRoot, ServerMode: "64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.ProtocolVersion != shared.RuntimeInventoryProtocolVersion || len(value.Rooms) != 1 || len(value.Rooms[0].Shards) != 2 {
		t.Fatalf("inventory=%#v", value)
	}
	room := value.Rooms[0]
	if room.Name != "朋友世界" || room.MasterPort != 10889 || !room.ClusterKeySet {
		t.Fatalf("room=%#v", room)
	}
	if room.Shards[0].Directory != "Caves" || room.Shards[0].Role != "secondary" || room.Shards[1].Role != "master" {
		t.Fatalf("shards=%#v", room.Shards)
	}
	if value.CPU.LogicalProcessors < 1 || value.CPU.PhysicalCores < 1 || value.CPU.PhysicalCores > value.CPU.LogicalProcessors {
		t.Fatalf("cpu=%#v", value.CPU)
	}
}

func TestRuntimeInventoryRejectsRelativePaths(t *testing.T) {
	_, err := (&Agent{}).collectRuntimeInventory(context.Background(), shared.RuntimeInventoryRequest{
		InstallationID: "default", SavePath: "relative/save", ServerPath: "/srv/dst",
	})
	if err == nil {
		t.Fatal("expected relative path rejection")
	}
}

func TestShardProcessArgumentParsing(t *testing.T) {
	arguments := []string{
		"/opt/dst/bin64/dontstarve_dedicated_server_nullrenderer_x64",
		"-persistent_storage_root", "/srv/klei", "-conf_dir=DoNotStarveTogether",
		"-cluster", "Cluster_1", "-shard=Caves",
	}
	if !isDSTServerProcess("dontstarve_dedicated_server_nullrenderer_x64", arguments) {
		t.Fatal("expected DST process match")
	}
	value := shardProcessFromArguments(42, "dst", arguments)
	if value.Cluster != "Cluster_1" || value.Shard != "Caves" || value.StorageRoot != "/srv/klei" || value.ConfigDirectory != "DoNotStarveTogether" {
		t.Fatalf("process=%#v", value)
	}
	if isDSTServerProcess("postgres", []string{"postgres", "-D", "/var/lib/postgres"}) {
		t.Fatal("non-DST process matched")
	}
}

func writeInventoryFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
