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
		result = append(result, topology.ExecutionPlacement{Room: f.room, World: world, Revision: f.revision, AppliedTargetID: "local"})
	}
	return result, nil
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
