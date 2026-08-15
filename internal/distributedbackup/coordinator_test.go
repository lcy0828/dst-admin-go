package distributedbackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type backupTestRooms struct {
	room   rooms.Room
	worlds []rooms.World
}

func (f backupTestRooms) Room(string) (rooms.Room, error) { return f.room, nil }

func (f backupTestRooms) Worlds(string) ([]rooms.World, error) {
	return append([]rooms.World(nil), f.worlds...), nil
}

type backupTestRouter struct {
	values map[string]struct {
		driver runtimedriver.Driver
		target runtimedriver.Target
	}
}

func (f backupTestRouter) DriverTarget(_ context.Context, _, worldID string) (runtimedriver.Driver, runtimedriver.Target, error) {
	value, exists := f.values[worldID]
	if !exists {
		return nil, runtimedriver.Target{}, errors.New("target missing")
	}
	return value.driver, value.target, nil
}

type backupTestControl struct {
	mu     sync.Mutex
	states map[string]shards.RuntimeStatus
	starts map[string]int
	stops  map[string]int
}

func newBackupTestControl(cluster, shard string) *backupTestControl {
	key := cluster + "\x00" + shard
	return &backupTestControl{
		states: map[string]shards.RuntimeStatus{key: {State: shards.RuntimeRunning, SessionExists: true}},
		starts: map[string]int{}, stops: map[string]int{},
	}
}

func (c *backupTestControl) Status(_ context.Context, cluster, shard string) (shards.RuntimeStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[cluster+"\x00"+shard], nil
}

func (c *backupTestControl) Start(_ context.Context, cluster, shard string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cluster + "\x00" + shard
	c.states[key] = shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}
	c.starts[key]++
	return nil
}

func (c *backupTestControl) Stop(_ context.Context, cluster, shard string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cluster + "\x00" + shard
	c.states[key] = shards.RuntimeStatus{State: shards.RuntimeStopped, SessionExists: false}
	c.stops[key]++
	return nil
}

func (c *backupTestControl) Send(context.Context, string, string, string) error { return nil }

type distributedBackupFixture struct {
	coordinator *Coordinator
	store       *Store
	masterRoot  string
	cavesRoot   string
	master      *backupTestControl
	caves       *backupTestControl
}

func newDistributedBackupFixture(t *testing.T) distributedBackupFixture {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "distributed_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	leases := operationlease.NewService(db, "distributed_test_")
	if err := leases.Migrate(); err != nil {
		t.Fatal(err)
	}
	masterRoot := filepath.Join(t.TempDir(), "master-node")
	cavesRoot := filepath.Join(t.TempDir(), "caves-node")
	writeShardFixture(t, masterRoot, "Master", "master-v1")
	writeShardFixture(t, cavesRoot, "Caves", "caves-v1")
	masterControl := newBackupTestControl("Cluster_1", "Master")
	cavesControl := newBackupTestControl("Cluster_1", "Caves")
	masterDriver, err := runtimedriver.NewNative(masterRoot, masterControl)
	if err != nil {
		t.Fatal(err)
	}
	cavesDriver, err := runtimedriver.NewNative(cavesRoot, cavesControl)
	if err != nil {
		t.Fatal(err)
	}
	roomID, masterID, cavesID := "room", "master", "caves"
	catalog := backupTestRooms{
		room: rooms.Room{ID: roomID, Name: "测试房间", DirectoryName: "Cluster_1", Managed: true},
		worlds: []rooms.World{
			{ID: masterID, RoomID: roomID, Name: "地面", DirectoryName: "Master", Role: rooms.WorldRoleMaster},
			{ID: cavesID, RoomID: roomID, Name: "洞穴", DirectoryName: "Caves", Role: rooms.WorldRoleCaves},
		},
	}
	router := backupTestRouter{values: map[string]struct {
		driver runtimedriver.Driver
		target runtimedriver.Target
	}{
		masterID: {driver: masterDriver, target: runtimedriver.Target{TargetID: "agent:master-node", InstallationID: "default", RoomID: roomID, WorldID: masterID, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1"}},
		cavesID:  {driver: cavesDriver, target: runtimedriver.Target{TargetID: "agent:caves-node", InstallationID: "default", RoomID: roomID, WorldID: cavesID, Cluster: "Cluster_1", Shard: "Caves", TopologyRevision: "revision-1"}},
	}}
	coordinator, err := NewCoordinator(filepath.Join(t.TempDir(), "central-backups"), catalog, router, leases, store)
	if err != nil {
		t.Fatal(err)
	}
	return distributedBackupFixture{coordinator: coordinator, store: store, masterRoot: masterRoot, cavesRoot: cavesRoot, master: masterControl, caves: cavesControl}
}

func writeShardFixture(t *testing.T, root, shard, worldData string) {
	t.Helper()
	cluster := filepath.Join(root, "Cluster_1")
	files := map[string]string{
		filepath.Join(cluster, "cluster.ini"):                    "[NETWORK]\ncluster_name = Test\n",
		filepath.Join(cluster, "cluster_token.txt"):              "token-v1\n",
		filepath.Join(cluster, shard, "server.ini"):              "[SHARD]\nname = " + shard + "\n",
		filepath.Join(cluster, shard, "save", "session", "data"): worldData,
	}
	for path, value := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestColdConsistentBackupAndCoordinatedRestoreAcrossTargets(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	created, err := fixture.coordinator.Create(context.Background(), "room", "跨节点备份", "manual", "job-create")
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != StatusVerified || len(created.Parts) != 2 || created.Size == 0 || created.SharedSHA256 == "" {
		t.Fatalf("created=%#v", created)
	}
	assertControlRunning(t, fixture.master, "Master")
	assertControlRunning(t, fixture.caves, "Caves")
	for _, root := range []string{fixture.masterRoot, fixture.cavesRoot} {
		if err := os.WriteFile(filepath.Join(root, "Cluster_1", "cluster_token.txt"), []byte("token-v2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fixture.masterRoot, "Cluster_1", "Master", "save", "session", "data"), []byte("master-mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.cavesRoot, "Cluster_1", "Caves", "save", "session", "data"), []byte("caves-mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.coordinator.Restore(context.Background(), created.ID, "测试房间", "job-restore")
	if err != nil {
		t.Fatal(err)
	}
	if result.ProtectionSetID == "" || len(result.Warnings) != 0 {
		t.Fatalf("restore=%#v", result)
	}
	assertTextFile(t, filepath.Join(fixture.masterRoot, "Cluster_1", "Master", "save", "session", "data"), "master-v1")
	assertTextFile(t, filepath.Join(fixture.cavesRoot, "Cluster_1", "Caves", "save", "session", "data"), "caves-v1")
	assertTextFile(t, filepath.Join(fixture.masterRoot, "Cluster_1", "cluster_token.txt"), "token-v1\n")
	assertTextFile(t, filepath.Join(fixture.cavesRoot, "Cluster_1", "cluster_token.txt"), "token-v1\n")
	assertControlRunning(t, fixture.master, "Master")
	assertControlRunning(t, fixture.caves, "Caves")
	protection, err := fixture.store.GetSet(result.ProtectionSetID)
	if err != nil || protection.Status != StatusVerified || protection.Kind != "protection" {
		t.Fatalf("protection=%#v err=%v", protection, err)
	}
}

func TestRestoreRejectsTamperedPartBeforeStoppingOrPublishing(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	created, err := fixture.coordinator.Create(context.Background(), "room", "备份", "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	partPath, err := fixture.coordinator.partPath(created.Parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.coordinator.Restore(context.Background(), created.ID, "测试房间", ""); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("restore error=%v", err)
	}
	assertControlRunning(t, fixture.master, "Master")
	assertControlRunning(t, fixture.caves, "Caves")
}

func TestBackupSetNeverVerifiesDifferentClusterSharedFiles(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.cavesRoot, "Cluster_1", "cluster_token.txt"), []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := fixture.coordinator.Create(context.Background(), "room", "冲突备份", "manual", "")
	if !errors.Is(err, ErrSharedFilesDiffer) {
		t.Fatalf("create error=%v set=%#v", err, created)
	}
	stored, loadErr := fixture.store.GetSet(created.ID)
	if loadErr != nil || stored.Status == StatusVerified || stored.VerifiedAt != nil {
		t.Fatalf("stored=%#v err=%v", stored, loadErr)
	}
}

func assertControlRunning(t *testing.T, control *backupTestControl, shard string) {
	t.Helper()
	status, err := control.Status(context.Background(), "Cluster_1", shard)
	if err != nil || status.State != shards.RuntimeRunning || !status.SessionExists {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func assertTextFile(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("%s=%q err=%v", path, data, err)
	}
}
