package distributedbackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dont/internal/operationlease"
	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/internal/topology"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type backupTestRooms struct {
	room   rooms.Room
	worlds []rooms.World
}

func (f backupTestRooms) Room(string) (rooms.Room, error) { return f.room, nil }

type backupTestPlacements struct {
	room     rooms.Room
	worlds   []rooms.World
	revision string
}

func (f backupTestPlacements) ResolveRoomExecutions(_ context.Context, roomID string) ([]topology.ExecutionPlacement, error) {
	result := make([]topology.ExecutionPlacement, 0, len(f.worlds))
	for _, world := range f.worlds {
		targetID := "agent:caves-node"
		if world.Role == rooms.WorldRoleMaster {
			targetID = "agent:master-node"
		}
		result = append(result, topology.ExecutionPlacement{Room: f.room, World: world, Revision: f.revision, AppliedTargetID: targetID})
	}
	return result, nil
}

type hotBarrierHarness struct {
	mu        sync.Mutex
	drivers   []*hotBarrierDriver
	snapshots []int64
}

type hotBarrierDriver struct {
	runtimedriver.Driver
	harness        *hotBarrierHarness
	receipt        runtimedriver.SnapshotBarrierReceipt
	released       bool
	stageErr       error
	backupReleases int
}

func (d *hotBarrierDriver) StageBackup(ctx context.Context, target runtimedriver.Target, operation runtimedriver.Operation, backupID string) (runtimedriver.BackupDescriptor, error) {
	if d.stageErr != nil {
		return runtimedriver.BackupDescriptor{}, d.stageErr
	}
	return d.Driver.StageBackup(ctx, target, operation, backupID)
}

func (d *hotBarrierDriver) ReleaseBackup(ctx context.Context, target runtimedriver.Target, operation runtimedriver.Operation, backupID string) error {
	d.harness.mu.Lock()
	d.backupReleases++
	d.harness.mu.Unlock()
	return d.Driver.ReleaseBackup(ctx, target, operation, backupID)
}

func (d *hotBarrierDriver) PrepareSnapshotBarrier(_ context.Context, target runtimedriver.Target, _ runtimedriver.Operation, barrierID string) (runtimedriver.SnapshotBarrierReceipt, error) {
	d.harness.mu.Lock()
	defer d.harness.mu.Unlock()
	d.receipt = runtimedriver.SnapshotBarrierReceipt{
		SchemaVersion: 1, ProducerVersion: "2.4.0", ProducerInstanceID: "instance-" + target.Shard,
		BarrierID: barrierID, State: "prepared", SessionID: "session-" + target.Shard, ShardID: target.Shard,
		SnapshotBefore: 40, PreparedAtUnix: time.Now().Unix(),
	}
	return d.receipt, nil
}

func (d *hotBarrierDriver) CommitSnapshotBarrier(_ context.Context, _ runtimedriver.Target, _ runtimedriver.Operation, barrierID string) error {
	d.harness.mu.Lock()
	defer d.harness.mu.Unlock()
	for index, current := range d.harness.drivers {
		if current.receipt.BarrierID != barrierID {
			return errors.New("barrier was not prepared on every shard")
		}
		snapshot := int64(41)
		if index < len(d.harness.snapshots) {
			snapshot = d.harness.snapshots[index]
		}
		current.receipt.State = "completed"
		current.receipt.SnapshotAfter = snapshot
		current.receipt.CompletedAtUnix = current.receipt.PreparedAtUnix + 1
		current.receipt.Proof = "save_current_callback"
	}
	return nil
}

func (d *hotBarrierDriver) SnapshotBarrier(_ context.Context, _ runtimedriver.Target, barrierID string) (runtimedriver.SnapshotBarrierReceipt, error) {
	d.harness.mu.Lock()
	defer d.harness.mu.Unlock()
	if d.receipt.BarrierID != barrierID {
		return runtimedriver.SnapshotBarrierReceipt{}, errors.New("barrier receipt missing")
	}
	return d.receipt, nil
}

func (d *hotBarrierDriver) ReleaseSnapshotBarrier(_ context.Context, _ runtimedriver.Target, _ runtimedriver.Operation, barrierID string) error {
	d.harness.mu.Lock()
	defer d.harness.mu.Unlock()
	if d.receipt.BarrierID != barrierID || d.receipt.State != "completed" {
		return errors.New("barrier is not complete")
	}
	d.released = true
	d.receipt.State = "released"
	return nil
}

func (d *hotBarrierDriver) CancelSnapshotBarrier(_ context.Context, _ runtimedriver.Target, _ runtimedriver.Operation, barrierID string) error {
	d.harness.mu.Lock()
	defer d.harness.mu.Unlock()
	if d.receipt.BarrierID == barrierID {
		d.released = true
	}
	return nil
}

func installHotBarrierDrivers(t *testing.T, fixture distributedBackupFixture, snapshots []int64) []*hotBarrierDriver {
	t.Helper()
	router := fixture.coordinator.runtimes.(backupTestRouter)
	harness := &hotBarrierHarness{snapshots: snapshots}
	drivers := make([]*hotBarrierDriver, 0, len(router.values))
	for _, worldID := range []string{"master", "caves"} {
		value := router.values[worldID]
		driver := &hotBarrierDriver{Driver: value.driver, harness: harness}
		value.driver = driver
		router.values[worldID] = value
		drivers = append(drivers, driver)
	}
	harness.drivers = drivers
	return drivers
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
	leases      *backupTestLeases
	masterRoot  string
	cavesRoot   string
	master      *backupTestControl
	caves       *backupTestControl
}

type backupMutationObserver struct {
	mu      sync.Mutex
	targets []string
}

func (o *backupMutationObserver) RuntimeTargetChanged(targetID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.targets = append(o.targets, targetID)
}

type backupTestLeases struct {
	service  *operationlease.Service
	mu       sync.Mutex
	acquires int
	renews   int
	releases int
}

func (l *backupTestLeases) Acquire(ctx context.Context, roomID, operationKey string, ttl time.Duration) (operationlease.Lease, error) {
	l.mu.Lock()
	l.acquires++
	l.mu.Unlock()
	return l.service.Acquire(ctx, roomID, operationKey, ttl)
}

func (l *backupTestLeases) Renew(ctx context.Context, lease operationlease.Lease, ttl time.Duration) (operationlease.Lease, error) {
	l.mu.Lock()
	l.renews++
	l.mu.Unlock()
	return l.service.Renew(ctx, lease, ttl)
}

func (l *backupTestLeases) Release(lease operationlease.Lease) error {
	l.mu.Lock()
	l.releases++
	l.mu.Unlock()
	return l.service.Release(lease)
}

func (l *backupTestLeases) counts() (int, int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquires, l.renews, l.releases
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
	leaseService := operationlease.NewService(db, "distributed_test_")
	if err := leaseService.Migrate(); err != nil {
		t.Fatal(err)
	}
	leases := &backupTestLeases{service: leaseService}
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
	placements := backupTestPlacements{room: catalog.room, worlds: catalog.worlds, revision: "revision-1"}
	coordinator, err := NewCoordinator(filepath.Join(t.TempDir(), "central-backups"), catalog, placements, router, leases, store)
	if err != nil {
		t.Fatal(err)
	}
	return distributedBackupFixture{coordinator: coordinator, store: store, leases: leases, masterRoot: masterRoot, cavesRoot: cavesRoot, master: masterControl, caves: cavesControl}
}

func writeShardFixture(t *testing.T, root, shard, worldData string) {
	t.Helper()
	cluster := filepath.Join(root, "Cluster_1")
	files := map[string]string{
		filepath.Join(cluster, "cluster.ini"):                                     "[NETWORK]\ncluster_name = Test\n",
		filepath.Join(cluster, "cluster_token.txt"):                               "token-v1\n",
		filepath.Join(cluster, shard, "server.ini"):                               "[SHARD]\nname = " + shard + "\n",
		filepath.Join(cluster, shard, "save", "shardindex"):                       "shard-index-" + shard,
		filepath.Join(cluster, shard, "save", "session", "SESSION", "0000000001"): worldData,
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
	mutations := &backupMutationObserver{}
	if err := fixture.coordinator.ConfigureMutationObserver(mutations); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(filepath.Join(fixture.masterRoot, "Cluster_1", "Master", "save", "session", "SESSION", "0000000001"), []byte("master-mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.cavesRoot, "Cluster_1", "Caves", "save", "session", "SESSION", "0000000001"), []byte("caves-mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.coordinator.Restore(context.Background(), created.ID, "测试房间", "job-restore")
	if err != nil {
		t.Fatal(err)
	}
	if result.ProtectionSetID == "" || len(result.Warnings) != 0 {
		t.Fatalf("restore=%#v", result)
	}
	operations, err := fixture.coordinator.Operations("room")
	if err != nil || len(operations) < 2 || operations[0].ID != result.OperationID || operations[0].Status != OperationSucceeded {
		t.Fatalf("operations=%#v err=%v", operations, err)
	}
	assertTextFile(t, filepath.Join(fixture.masterRoot, "Cluster_1", "Master", "save", "session", "SESSION", "0000000001"), "master-v1")
	assertTextFile(t, filepath.Join(fixture.cavesRoot, "Cluster_1", "Caves", "save", "session", "SESSION", "0000000001"), "caves-v1")
	assertTextFile(t, filepath.Join(fixture.masterRoot, "Cluster_1", "cluster_token.txt"), "token-v1\n")
	assertTextFile(t, filepath.Join(fixture.cavesRoot, "Cluster_1", "cluster_token.txt"), "token-v1\n")
	assertControlRunning(t, fixture.master, "Master")
	assertControlRunning(t, fixture.caves, "Caves")
	protection, err := fixture.store.GetSet(result.ProtectionSetID)
	if err != nil || protection.Status != StatusVerified || protection.Kind != "protection" {
		t.Fatalf("protection=%#v err=%v", protection, err)
	}
	mutations.mu.Lock()
	defer mutations.mu.Unlock()
	if len(mutations.targets) != 2 || mutations.targets[0] != "agent:master-node" || mutations.targets[1] != "agent:caves-node" {
		t.Fatalf("mutation targets=%v", mutations.targets)
	}
}

func TestSplitRoomBackupAllowsOnlyTopologySpecificClusterFields(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	masterConfig := "[NETWORK]\ncluster_name = Test\n[SHARD]\nshard_enabled = true\nbind_ip = 0.0.0.0\nmaster_port = 10888\ncluster_key = shared-key\n"
	cavesConfig := "[NETWORK]\ncluster_name = Test\n[SHARD]\nshard_enabled = true\nbind_ip = 0.0.0.0\nmaster_ip = 192.168.2.24\nmaster_port = 10888\ncluster_key = shared-key\n"
	if err := os.WriteFile(filepath.Join(fixture.masterRoot, "Cluster_1", "cluster.ini"), []byte(masterConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.cavesRoot, "Cluster_1", "cluster.ini"), []byte(cavesConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := fixture.coordinator.Create(context.Background(), "room", "跨机路由备份", "manual", "job-route-fields")
	if err != nil || created.Status != StatusVerified || created.ManifestVersion != manifestVersion {
		t.Fatalf("created=%#v error=%v", created, err)
	}
	if len(created.Parts) != 2 || created.Parts[0].SharedSHA256 == created.Parts[1].SharedSHA256 {
		t.Fatalf("per-target exact shared hashes were not preserved: %#v", created.Parts)
	}
}

func TestSplitRoomBackupStillRejectsNonTopologySharedDrift(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.cavesRoot, "Cluster_1", "cluster_token.txt"), []byte("different-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.coordinator.Create(context.Background(), "room", "非法配置漂移", "manual", "job-shared-drift"); !errors.Is(err, ErrSharedFilesDiffer) {
		t.Fatalf("shared drift error=%v", err)
	}
}

func TestImportedDirectoryRestoresAcrossCurrentPlacements(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	sourceRoot := filepath.Join(t.TempDir(), "ImportedCluster")
	for path, value := range map[string]string{
		filepath.Join(sourceRoot, "cluster.ini"):                                       "[NETWORK]\ncluster_name = Imported\n",
		filepath.Join(sourceRoot, "cluster_token.txt"):                                 "import-token\n",
		filepath.Join(sourceRoot, "Forest", "server.ini"):                              "[SHARD]\nis_master = true\nid = 1\n",
		filepath.Join(sourceRoot, "Forest", "save", "shardindex"):                      "forest-index",
		filepath.Join(sourceRoot, "Forest", "save", "session", "NEW", "0000000042"):    "imported-master",
		filepath.Join(sourceRoot, "CaveWorld", "server.ini"):                           "[SHARD]\nis_master = false\nid = 2\n",
		filepath.Join(sourceRoot, "CaveWorld", "save", "shardindex"):                   "caves-index",
		filepath.Join(sourceRoot, "CaveWorld", "save", "session", "NEW", "0000000042"): "imported-caves",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	imported, err := fixture.coordinator.ImportDirectory(context.Background(), DirectoryImportRequest{
		RoomID: "room", SourceRoot: sourceRoot, Name: "上传的房间存档", SourceJobID: "job-import",
		Worlds: []DirectoryImportWorld{{WorldID: "master", DirectoryName: "Forest"}, {WorldID: "caves", DirectoryName: "CaveWorld"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Status != StatusVerified || !imported.Restorable || imported.Kind != "import" || len(imported.Parts) != 2 {
		t.Fatalf("imported=%#v", imported)
	}
	fixture.master.mu.Lock()
	masterStops := fixture.master.stops["Cluster_1\x00Master"]
	fixture.master.mu.Unlock()
	fixture.caves.mu.Lock()
	cavesStops := fixture.caves.stops["Cluster_1\x00Caves"]
	fixture.caves.mu.Unlock()
	if masterStops != 0 || cavesStops != 0 {
		t.Fatalf("directory import stopped runtimes: master=%d caves=%d", masterStops, cavesStops)
	}

	result, err := fixture.coordinator.Restore(context.Background(), imported.ID, "测试房间", "job-import-restore")
	if err != nil {
		t.Fatal(err)
	}
	if result.ProtectionSetID == "" {
		t.Fatalf("restore=%#v", result)
	}
	assertTextFile(t, filepath.Join(fixture.masterRoot, "Cluster_1", "Master", "save", "session", "NEW", "0000000042"), "imported-master")
	assertTextFile(t, filepath.Join(fixture.cavesRoot, "Cluster_1", "Caves", "save", "session", "NEW", "0000000042"), "imported-caves")
	assertTextFile(t, filepath.Join(fixture.masterRoot, "Cluster_1", "cluster_token.txt"), "import-token\n")
	assertTextFile(t, filepath.Join(fixture.cavesRoot, "Cluster_1", "cluster_token.txt"), "import-token\n")
}

func TestConfigurationOnlyBackupCannotRestoreOrStopRoom(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	if err := os.RemoveAll(filepath.Join(fixture.masterRoot, "Cluster_1", "Master", "save")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(fixture.cavesRoot, "Cluster_1", "Caves", "save")); err != nil {
		t.Fatal(err)
	}
	created, err := fixture.coordinator.Create(context.Background(), "room", "仅配置备份", "manual", "job-config-only")
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != StatusVerified || created.Restorable || created.ContentKind != "configuration-only" || created.ValidationError == "" {
		t.Fatalf("configuration-only backup=%#v", created)
	}
	fixture.master.mu.Lock()
	masterStops := fixture.master.stops["Cluster_1\x00Master"]
	fixture.master.mu.Unlock()
	fixture.caves.mu.Lock()
	cavesStops := fixture.caves.stops["Cluster_1\x00Caves"]
	fixture.caves.mu.Unlock()
	if _, err := fixture.coordinator.Restore(context.Background(), created.ID, "测试房间", "job-restore-config-only"); !errors.Is(err, ErrNotRestorable) {
		t.Fatalf("configuration-only restore error=%v", err)
	}
	fixture.master.mu.Lock()
	defer fixture.master.mu.Unlock()
	fixture.caves.mu.Lock()
	defer fixture.caves.mu.Unlock()
	if fixture.master.stops["Cluster_1\x00Master"] != masterStops || fixture.caves.stops["Cluster_1\x00Caves"] != cavesStops {
		t.Fatal("non-restorable backup stopped a running shard")
	}
}

func TestHotConsistentBackupStagesAUnifiedSnapshotWithoutStoppingShards(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	drivers := installHotBarrierDrivers(t, fixture, []int64{41, 41})
	created, err := fixture.coordinator.CreateWithMode(context.Background(), "room", "在线一致备份", "manual", "job-hot", ModeHot)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != StatusVerified || created.Mode != ModeHot || created.BarrierID == "" || created.Snapshot != 41 || len(created.Parts) != 2 {
		t.Fatalf("created=%#v", created)
	}
	for _, part := range created.Parts {
		if part.SnapshotAfter != 41 || part.SnapshotBefore != 40 || part.BarrierSessionID == "" || part.BarrierInstance == "" || part.BarrierCompleted == nil {
			t.Fatalf("part lacks barrier proof: %#v", part)
		}
	}
	for _, driver := range drivers {
		if !driver.released {
			t.Fatal("snapshot hold was not released after immutable staging")
		}
	}
	fixture.master.mu.Lock()
	masterStops := fixture.master.stops["Cluster_1\x00Master"]
	fixture.master.mu.Unlock()
	fixture.caves.mu.Lock()
	cavesStops := fixture.caves.stops["Cluster_1\x00Caves"]
	fixture.caves.mu.Unlock()
	if masterStops != 0 || cavesStops != 0 {
		t.Fatalf("hot backup stopped shards: master=%d caves=%d", masterStops, cavesStops)
	}
}

func TestHotConsistentBackupRejectsConflictingShardSnapshots(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	installHotBarrierDrivers(t, fixture, []int64{41, 42})
	created, err := fixture.coordinator.CreateWithMode(context.Background(), "room", "冲突热备份", "manual", "", ModeHot)
	if !errors.Is(err, ErrBarrierFailed) {
		t.Fatalf("create error=%v set=%#v", err, created)
	}
	stored, loadErr := fixture.store.GetSet(created.ID)
	if loadErr != nil || stored.Status == StatusVerified || stored.Snapshot != 0 {
		t.Fatalf("stored=%#v err=%v", stored, loadErr)
	}
}

func TestHotConsistentBackupReleasesEveryStagedPartWhenLaterStageFails(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	drivers := installHotBarrierDrivers(t, fixture, []int64{41, 41})
	drivers[1].stageErr = errors.New("forced caves staging failure")
	created, err := fixture.coordinator.CreateWithMode(context.Background(), "room", "失败热备份", "manual", "", ModeHot)
	if err == nil || created.Status == StatusVerified {
		t.Fatalf("create error=%v set=%#v", err, created)
	}
	drivers[0].harness.mu.Lock()
	masterReleases := drivers[0].backupReleases
	cavesReleases := drivers[1].backupReleases
	drivers[0].harness.mu.Unlock()
	if masterReleases == 0 || cavesReleases == 0 {
		t.Fatalf("staged backup releases master=%d caves=%d", masterReleases, cavesReleases)
	}
	for _, driver := range drivers {
		if !driver.released {
			t.Fatal("snapshot barrier was not released after staging failure")
		}
	}
}

func TestRestoreRecoveryIsIdempotentAfterRuntimeCleanupCompleted(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	created, err := fixture.coordinator.Create(context.Background(), "room", "恢复幂等备份", "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.coordinator.Restore(context.Background(), created.ID, "测试房间", "")
	if err != nil {
		t.Fatal(err)
	}
	operation, err := fixture.store.Operation(result.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	operation.Phase, operation.Status, operation.Failure = "published", OperationRunning, ""
	if _, err := fixture.store.SaveOperation(operation); err != nil {
		t.Fatal(err)
	}
	recovered, err := fixture.coordinator.RecoverOperation(context.Background(), result.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Phase != "completed" || recovered.Status != OperationSucceeded || recovered.Failure != "" {
		t.Fatalf("recovered operation=%#v", recovered)
	}
	assertControlRunning(t, fixture.master, "Master")
	assertControlRunning(t, fixture.caves, "Caves")
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

func TestCoordinatedStopAndStartOrdersMasterAtTheSafeBoundary(t *testing.T) {
	parts := []runtimePart{
		{part: Part{WorldID: "master", WorldRole: string(rooms.WorldRoleMaster), Shard: "Master"}},
		{part: Part{WorldID: "forest", WorldRole: string(rooms.WorldRoleCustom), Shard: "Forest"}},
		{part: Part{WorldID: "caves", WorldRole: string(rooms.WorldRoleCaves), Shard: "Caves"}},
	}
	stopping := orderedRuntimeParts(parts, false)
	starting := orderedRuntimeParts(parts, true)
	if got := []string{stopping[0].part.Shard, stopping[1].part.Shard, stopping[2].part.Shard}; got[2] != "Master" {
		t.Fatalf("stop order=%v", got)
	}
	if got := []string{starting[0].part.Shard, starting[1].part.Shard, starting[2].part.Shard}; got[0] != "Master" {
		t.Fatalf("start order=%v", got)
	}
}

func TestCreateProtectedBorrowsAndRenewsCallerLeaseWithoutReleasing(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	ctx, releaseRoom, err := roomops.Acquire(context.Background(), "room")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRoom()
	lease, err := fixture.leases.Acquire(ctx, "room", "mod.publish:publication-one", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fixture.leases.Release(lease) }()
	acquiresBefore, renewsBefore, releasesBefore := fixture.leases.counts()
	expiresBefore := lease.ExpiresAt
	created, err := fixture.coordinator.CreateProtected(ctx, "room", "Mod 发布前保护备份", "job-protected", &lease)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != StatusVerified || created.Kind != "protection" || created.SourceJobID != "job-protected" {
		t.Fatalf("created=%#v", created)
	}
	acquiresAfter, renewsAfter, releasesAfter := fixture.leases.counts()
	if acquiresAfter != acquiresBefore || renewsAfter <= renewsBefore || releasesAfter != releasesBefore {
		t.Fatalf("lease ownership changed: before=%d/%d/%d after=%d/%d/%d", acquiresBefore, renewsBefore, releasesBefore, acquiresAfter, renewsAfter, releasesAfter)
	}
	if !lease.ExpiresAt.After(expiresBefore) {
		t.Fatalf("renewed lease expiry was not returned to owner: before=%s after=%s", expiresBefore, lease.ExpiresAt)
	}
	if _, err := fixture.leases.Acquire(ctx, "room", "another-operation", 30*time.Second); !errors.Is(err, operationlease.ErrBusy) {
		t.Fatalf("borrowed lease was released by backup: %v", err)
	}
	if err := fixture.leases.Release(lease); err != nil {
		t.Fatal(err)
	}
	next, err := fixture.leases.Acquire(ctx, "room", "another-operation", 30*time.Second)
	if err != nil {
		t.Fatalf("owner could not release renewed lease: %v", err)
	}
	if next.FencingToken <= lease.FencingToken {
		t.Fatalf("fencing token did not advance: old=%d new=%d", lease.FencingToken, next.FencingToken)
	}
	lease = next
}

func TestCreateProtectedFailureStillLeavesCallerLeaseOwned(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.cavesRoot, "Cluster_1", "cluster_token.txt"), []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, releaseRoom, err := roomops.Acquire(context.Background(), "room")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRoom()
	lease, err := fixture.leases.Acquire(ctx, "room", "mod.publish:publication-failure", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fixture.leases.Release(lease) }()
	_, _, releasesBefore := fixture.leases.counts()
	created, err := fixture.coordinator.CreateProtected(ctx, "room", "失败保护备份", "", &lease)
	if !errors.Is(err, ErrSharedFilesDiffer) || created.Status == StatusVerified {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	_, renewsAfter, releasesAfter := fixture.leases.counts()
	if renewsAfter == 0 || releasesAfter != releasesBefore {
		t.Fatalf("failed backup changed lease ownership: renews=%d releases=%d/%d", renewsAfter, releasesBefore, releasesAfter)
	}
	if _, err := fixture.leases.Acquire(ctx, "room", "another-operation", 30*time.Second); !errors.Is(err, operationlease.ErrBusy) {
		t.Fatalf("failed backup released caller lease: %v", err)
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
