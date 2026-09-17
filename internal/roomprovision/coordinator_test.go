package roomprovision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/internal/topology"
	"dont/shared"

	"github.com/go-ini/ini"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

type provisionStoppedControl struct{}

func (provisionStoppedControl) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
}
func (provisionStoppedControl) Start(context.Context, string, string) error        { return nil }
func (provisionStoppedControl) Stop(context.Context, string, string) error         { return nil }
func (provisionStoppedControl) Send(context.Context, string, string, string) error { return nil }

type provisionTrackingControl struct {
	running  map[string]bool
	starts   map[string]int
	stops    map[string]int
	startErr error
}

func newProvisionTrackingControl(running ...string) *provisionTrackingControl {
	control := &provisionTrackingControl{running: map[string]bool{}, starts: map[string]int{}, stops: map[string]int{}}
	for _, shard := range running {
		control.running[shard] = true
	}
	return control
}

func (c *provisionTrackingControl) Status(_ context.Context, _, shard string) (shards.RuntimeStatus, error) {
	if c.running[shard] {
		return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
	}
	return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
}
func (c *provisionTrackingControl) Start(_ context.Context, _, shard string) error {
	c.starts[shard]++
	if c.startErr != nil {
		return c.startErr
	}
	c.running[shard] = true
	return nil
}
func (c *provisionTrackingControl) Stop(_ context.Context, _, shard string) error {
	c.stops[shard]++
	c.running[shard] = false
	return nil
}
func (*provisionTrackingControl) Send(context.Context, string, string, string) error { return nil }

type provisionTestRooms struct {
	bundle    rooms.ProvisionBundle
	staged    bool
	published bool
}

func (f *provisionTestRooms) Room(string) (rooms.Room, error) { return f.bundle.Room, nil }
func (f *provisionTestRooms) ProvisionBundle(string) (rooms.ProvisionBundle, error) {
	return f.bundle, nil
}
func (f *provisionTestRooms) StageProvisionCluster(_, _ string, _ []byte) (bool, error) {
	f.staged = true
	return true, nil
}
func (f *provisionTestRooms) PublishProvisionCluster(_, _ string) error {
	f.published = true
	return nil
}
func (f *provisionTestRooms) RollbackProvisionCluster(_, _ string) error {
	f.staged, f.published = false, false
	return nil
}
func (f *provisionTestRooms) CompleteProvisionCluster(_, _ string) error {
	f.staged = false
	return nil
}

type provisionTestTopology struct {
	placements []topology.ExecutionPlacement
	links      []topology.ShardLink
	resources  topology.InfrastructureSnapshot
	applyErr   error
	verifyErr  error
	applied    bool
}

func (f *provisionTestTopology) VerifyDesiredShardLinks(context.Context, string) error {
	return f.verifyErr
}

func (f *provisionTestTopology) ResolveDesiredShardLinks(context.Context, string) ([]topology.ShardLink, error) {
	return append([]topology.ShardLink(nil), f.links...), nil
}

func (f *provisionTestTopology) ResolveDesiredRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error) {
	return append([]topology.ExecutionPlacement(nil), f.placements...), nil
}

func (f *provisionTestTopology) ApplyProvision(_ string, revision string, values []topology.PlacementInput) (string, error) {
	if f.applyErr != nil {
		return "", f.applyErr
	}
	if len(f.placements) != len(values) || len(f.placements) == 0 || f.placements[0].Revision != revision {
		return "", topology.ErrRevisionConflict
	}
	requested := make(map[string]string, len(values))
	for _, value := range values {
		requested[value.WorldID] = value.TargetID
	}
	for index := range f.placements {
		placement := &f.placements[index]
		if requested[placement.World.ID] != placement.DesiredTargetID {
			return "", topology.ErrRevisionConflict
		}
		placement.AppliedTargetID = placement.DesiredTargetID
		placement.Revision = "revision-applied"
	}
	f.applied = true
	return "revision-applied", nil
}

func (f *provisionTestTopology) Infrastructure(context.Context) (topology.InfrastructureSnapshot, error) {
	return f.resources, nil
}

type provisionTestRouter struct {
	source  runtimedriver.Driver
	targets map[string]runtimedriver.Driver
}

func (f provisionTestRouter) DriverTarget(_ context.Context, roomID, worldID string) (runtimedriver.Driver, runtimedriver.Target, error) {
	return f.source, runtimedriver.Target{
		TargetID: "local", InstallationID: "default", RoomID: roomID, WorldID: worldID,
		Cluster: "Cluster_1", Shard: shardForWorld(worldID), TopologyRevision: "revision-1",
	}, nil
}

func (f provisionTestRouter) ProvisionTarget(placement topology.ExecutionPlacement) (runtimedriver.Driver, runtimedriver.Target, error) {
	driver, ok := f.targets[placement.DesiredTargetID]
	if !ok {
		return nil, runtimedriver.Target{}, runtimedriver.ErrInvalidTarget
	}
	return driver, runtimedriver.Target{
		TargetID: placement.DesiredTargetID, InstallationID: placement.Target.Config.InstallationID,
		RoomID: placement.Room.ID, WorldID: placement.World.ID, Cluster: placement.Room.DirectoryName,
		Shard: placement.World.DirectoryName, TopologyRevision: placement.Revision,
	}, nil
}

func (f provisionTestRouter) TrustedTarget(target runtimedriver.Target) (runtimedriver.Driver, error) {
	if target.TargetID == "local" {
		return f.source, nil
	}
	driver, ok := f.targets[target.TargetID]
	if !ok {
		return nil, runtimedriver.ErrInvalidTarget
	}
	return driver, nil
}

type provisionCommitFailure struct {
	runtimedriver.Driver
	err           error
	rollbackCalls *int
}

func (d provisionCommitFailure) CommitMigrationImport(context.Context, runtimedriver.Target, runtimedriver.Operation, string) error {
	return d.err
}

func (d provisionCommitFailure) RollbackMigrationTarget(ctx context.Context, target runtimedriver.Target, operation runtimedriver.Operation, migrationID string) error {
	if d.rollbackCalls != nil {
		*d.rollbackCalls++
	}
	return d.Driver.RollbackMigrationTarget(ctx, target, operation, migrationID)
}

type provisionBeginFailure struct {
	runtimedriver.Driver
	err           error
	rollbackCalls *int
}

func (d provisionBeginFailure) BeginMigrationImport(context.Context, runtimedriver.Target, runtimedriver.Operation, runtimedriver.MigrationDescriptor) error {
	return d.err
}

func (d provisionBeginFailure) RollbackMigrationTarget(ctx context.Context, target runtimedriver.Target, operation runtimedriver.Operation, migrationID string) error {
	if d.rollbackCalls != nil {
		*d.rollbackCalls++
	}
	return d.Driver.RollbackMigrationTarget(ctx, target, operation, migrationID)
}

type provisionTestLeases struct {
	next uint64
}

func (f *provisionTestLeases) Acquire(_ context.Context, roomID, key string, ttl time.Duration) (operationlease.Lease, error) {
	f.next++
	return operationlease.Lease{
		RoomID: roomID, LeaseID: "lease-id", OperationKey: key, FencingToken: f.next,
		ExpiresAt: time.Now().UTC().Add(ttl),
	}, nil
}
func (f *provisionTestLeases) Renew(_ context.Context, lease operationlease.Lease, ttl time.Duration) (operationlease.Lease, error) {
	lease.ExpiresAt = time.Now().UTC().Add(ttl)
	return lease, nil
}
func (*provisionTestLeases) Release(operationlease.Lease) error { return nil }

type provisionFixture struct {
	coordinator *Coordinator
	store       *Store
	rooms       *provisionTestRooms
	topology    *provisionTestTopology
	router      provisionTestRouter
	roots       map[string]string
}

func newProvisionFixture(t *testing.T, masterTarget, cavesTarget string) provisionFixture {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "provision_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room-1", DirectoryName: "Cluster_1", Name: "测试房间", Managed: true}
	master := rooms.World{ID: "master", RoomID: room.ID, DirectoryName: "Master", Name: "地面", Role: rooms.WorldRoleMaster, IsMaster: true}
	caves := rooms.World{ID: "caves", RoomID: room.ID, DirectoryName: "Caves", Name: "洞穴", Role: rooms.WorldRoleCaves}
	catalog := &provisionTestRooms{bundle: rooms.ProvisionBundle{
		Room: room,
		Shared: []rooms.ProvisionFile{
			{Name: "cluster.ini", Data: []byte("[NETWORK]\ncluster_name = Test\n[SHARD]\nshard_enabled = true\nbind_ip = 127.0.0.1\nmaster_ip = 127.0.0.1\nmaster_port = 10889\n"), Mode: 0o600},
			{Name: "cluster_token.txt", Data: []byte("token\n"), Mode: 0o600},
		},
		Worlds: []rooms.ProvisionWorld{
			{World: master, Files: []rooms.ProvisionFile{{Name: "server.ini", Data: []byte("[SHARD]\nis_master = true\n"), Mode: 0o600}}},
			{World: caves, Files: []rooms.ProvisionFile{{Name: "server.ini", Data: []byte("[SHARD]\nid = 2\n"), Mode: 0o600}}},
		},
	}}
	targetIDs := map[string]bool{masterTarget: true, cavesTarget: true}
	roots := make(map[string]string, len(targetIDs))
	targetDrivers := make(map[string]runtimedriver.Driver, len(targetIDs))
	for targetID := range targetIDs {
		root := filepath.Join(t.TempDir(), strings.ReplaceAll(targetID, ":", "-"))
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		driver, err := runtimedriver.NewNative(root, provisionStoppedControl{})
		if err != nil {
			t.Fatal(err)
		}
		roots[targetID], targetDrivers[targetID] = root, driver
	}
	sourceRoot := t.TempDir()
	source, err := runtimedriver.NewNative(sourceRoot, provisionStoppedControl{})
	if err != nil {
		t.Fatal(err)
	}
	placement := func(world rooms.World, targetID string) topology.ExecutionPlacement {
		return topology.ExecutionPlacement{
			Room: room, World: world, Revision: "revision-1", DesiredTargetID: targetID, AppliedTargetID: "local",
			Target: agents.RuntimeTarget{
				ID: targetID, Kind: agents.RuntimeKindAgent, Online: true, Configured: true,
				Config: agents.RuntimeConfig{InstallationID: "default"},
			},
			Inventory: agents.RuntimeTargetInventory{Available: true},
		}
	}
	topologyService := &provisionTestTopology{
		placements: []topology.ExecutionPlacement{placement(master, masterTarget), placement(caves, cavesTarget)},
		resources:  provisionResources(targetIDs, masterTarget),
	}
	router := provisionTestRouter{source: source, targets: targetDrivers}
	coordinator, err := NewCoordinator(catalog, topologyService, router, &provisionTestLeases{}, store)
	if err != nil {
		t.Fatal(err)
	}
	return provisionFixture{coordinator: coordinator, store: store, rooms: catalog, topology: topologyService, router: router, roots: roots}
}

func provisionResources(targets map[string]bool, masterTarget string) topology.InfrastructureSnapshot {
	value := topology.InfrastructureSnapshot{}
	for targetID := range targets {
		environmentID := "environment-" + strings.TrimPrefix(targetID, "agent:")
		profileID := "network-" + strings.TrimPrefix(targetID, "agent:")
		value.Environments = append(value.Environments, topology.ExecutionEnvironment{
			ID: environmentID, TargetID: targetID, NetworkProfileID: profileID,
		})
		address := "192.0.2.20"
		if targetID == masterTarget {
			address = "192.0.2.10"
		}
		value.NetworkProfiles = append(value.NetworkProfiles, topology.NetworkProfile{
			ID: profileID, EnvironmentID: environmentID, AdvertiseAddress: address,
		})
	}
	return value
}

func shardForWorld(worldID string) string {
	if worldID == "master" {
		return "Master"
	}
	return "Caves"
}

func TestCoordinatorProvisionsTwoShardsOnOneRemoteTarget(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-a")
	operation, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-1")
	if err != nil || operation.Status != StatusSucceeded || !fixture.topology.applied {
		t.Fatalf("operation=%#v applied=%v err=%v", operation, fixture.topology.applied, err)
	}
	for _, shard := range []string{"Master", "Caves"} {
		data, readErr := os.ReadFile(filepath.Join(fixture.roots["agent:node-a"], "Cluster_1", shard, "server.ini"))
		if readErr != nil || !strings.Contains(string(data), "[SHARD]") {
			t.Fatalf("shard=%s data=%q err=%v", shard, data, readErr)
		}
	}
}

func TestCoordinatorProvisionsRoomAcrossRemoteTargets(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-b")
	operation, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-2")
	if err != nil || operation.Status != StatusSucceeded || operation.AppliedRevision != "revision-applied" {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	for targetID, shard := range map[string]string{"agent:node-a": "Master", "agent:node-b": "Caves"} {
		root := fixture.roots[targetID]
		if _, statErr := os.Stat(filepath.Join(root, "Cluster_1", shard, "server.ini")); statErr != nil {
			t.Fatalf("target=%s shard=%s: %v", targetID, shard, statErr)
		}
		runtimeOutput, statErr := os.Stat(filepath.Join(root, "Cluster_1", shard, "save", "mod_config_data", "dst-admin"))
		if statErr != nil || !runtimeOutput.IsDir() {
			t.Fatalf("target=%s shard=%s runtime output=%#v err=%v", targetID, shard, runtimeOutput, statErr)
		}
		cluster, readErr := os.ReadFile(filepath.Join(root, "Cluster_1", "cluster.ini"))
		config, parseErr := ini.Load(cluster)
		expectedMasterIP := "192.0.2.10"
		if targetID == "agent:node-a" {
			expectedMasterIP = "127.0.0.1"
		}
		if readErr != nil || parseErr != nil || config.Section("SHARD").Key("master_ip").String() != expectedMasterIP || config.Section("SHARD").Key("bind_ip").String() != "0.0.0.0" {
			t.Fatalf("target=%s cluster=%q readErr=%v parseErr=%v", targetID, cluster, readErr, parseErr)
		}
	}
}

func TestCoordinatorRendersSelectedShardLinkOnlyForSecondary(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-b")
	fixture.topology.links = []topology.ShardLink{{
		SourceTargetID: "agent:node-b", MasterTargetID: "agent:node-a",
		Address: "100.64.0.10", Port: 11889, Mode: topology.ShardLinkOverlay,
	}}
	operation, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-link")
	if err != nil || operation.Status != StatusSucceeded {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	secondary, err := ini.Load(filepath.Join(fixture.roots["agent:node-b"], "Cluster_1", "cluster.ini"))
	if err != nil {
		t.Fatal(err)
	}
	section := secondary.Section("SHARD")
	if section.Key("master_ip").String() != "100.64.0.10" || section.Key("master_port").MustInt(0) != 11889 {
		t.Fatalf("secondary shard config=%#v", section.KeysHash())
	}
	master, err := ini.Load(filepath.Join(fixture.roots["agent:node-a"], "Cluster_1", "cluster.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if master.Section("SHARD").Key("master_port").MustInt(0) != 10889 {
		t.Fatalf("master listener port changed: %#v", master.Section("SHARD").KeysHash())
	}
}

func TestCoordinatorRestoresOnlyWorldsThatWereRunning(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-a")
	sourceControl := newProvisionTrackingControl("Master")
	sourceDriver, err := runtimedriver.NewNative(t.TempDir(), sourceControl)
	if err != nil {
		t.Fatal(err)
	}
	targetControl := newProvisionTrackingControl()
	targetDriver, err := runtimedriver.NewNative(fixture.roots["agent:node-a"], targetControl)
	if err != nil {
		t.Fatal(err)
	}
	fixture.router.source = sourceDriver
	fixture.router.targets["agent:node-a"] = targetDriver
	fixture.coordinator.runtimes = fixture.router

	operation, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-running")
	if err != nil || operation.Status != StatusSucceeded || sourceControl.stops["Master"] != 1 || targetControl.starts["Master"] != 1 || targetControl.starts["Caves"] != 0 {
		t.Fatalf("operation=%#v source=%#v target=%#v err=%v", operation, sourceControl, targetControl, err)
	}
	for _, step := range operation.Steps {
		if step.WorldID == "master" && (!step.WasRunning || !step.RuntimeRestored) {
			t.Fatalf("master runtime state was not persisted: %#v", step)
		}
		if step.WorldID == "caves" && (step.WasRunning || step.RuntimeRestored) {
			t.Fatalf("stopped caves should remain stopped: %#v", step)
		}
	}
}

func TestCoordinatorRecoversRuntimeRestoreAfterTopologyCommit(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-a")
	sourceControl := newProvisionTrackingControl("Master")
	sourceDriver, err := runtimedriver.NewNative(t.TempDir(), sourceControl)
	if err != nil {
		t.Fatal(err)
	}
	targetControl := newProvisionTrackingControl()
	targetControl.startErr = errors.New("forced start failure")
	targetDriver, err := runtimedriver.NewNative(fixture.roots["agent:node-a"], targetControl)
	if err != nil {
		t.Fatal(err)
	}
	fixture.router.source = sourceDriver
	fixture.router.targets["agent:node-a"] = targetDriver
	fixture.coordinator.runtimes = fixture.router

	operation, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-runtime-recovery")
	if err == nil || operation.Status != StatusRecoveryRequired || operation.Phase != "runtime_restore_target" || !fixture.topology.applied {
		t.Fatalf("operation=%#v applied=%v err=%v", operation, fixture.topology.applied, err)
	}

	targetControl.startErr = nil
	restarted, err := NewCoordinator(fixture.rooms, fixture.topology, fixture.router, &provisionTestLeases{}, fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.RecoverOperation(context.Background(), operation.ID)
	if err != nil || recovered.Status != StatusSucceeded || !targetControl.running["Master"] || targetControl.starts["Master"] != 2 {
		t.Fatalf("recovered=%#v target=%#v err=%v", recovered, targetControl, err)
	}
}

func TestCoordinatorRollsBackEveryTargetWhenSecondPublishFails(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-b")
	base := fixture.router.targets["agent:node-b"]
	rollbackCalls := 0
	fixture.router.targets["agent:node-b"] = provisionCommitFailure{Driver: base, err: errors.New("forced second publish failure"), rollbackCalls: &rollbackCalls}
	fixture.coordinator.runtimes = fixture.router
	operation, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-3")
	if err == nil || operation.Status != StatusRolledBack || fixture.topology.applied {
		t.Fatalf("operation=%#v applied=%v err=%v", operation, fixture.topology.applied, err)
	}
	for targetID, shard := range map[string]string{"agent:node-a": "Master", "agent:node-b": "Caves"} {
		if _, statErr := os.Stat(filepath.Join(fixture.roots[targetID], "Cluster_1", shard)); !os.IsNotExist(statErr) {
			t.Fatalf("target %s was not rolled back: %v", targetID, statErr)
		}
	}
	if rollbackCalls != 1 {
		t.Fatalf("second target rollback calls=%d", rollbackCalls)
	}
}

func TestCoordinatorDoesNotCallRemoteRollbackWhenBeginWasNotDispatched(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-b")
	rollbackCalls := 0
	for targetID, base := range fixture.router.targets {
		beginErr := error(nil)
		if targetID == "agent:node-a" {
			beginErr = errors.Join(runtimedriver.ErrOperationNotDispatched, agents.ErrRuntimeInstallationNotRegistered)
		}
		fixture.router.targets[targetID] = provisionBeginFailure{Driver: base, err: beginErr, rollbackCalls: &rollbackCalls}
	}
	fixture.coordinator.runtimes = fixture.router

	operation, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-not-dispatched")
	if !errors.Is(err, runtimedriver.ErrOperationNotDispatched) || operation.Status != StatusRolledBack || operation.Phase != "rolled_back" {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	if rollbackCalls != 0 {
		t.Fatalf("remote rollback calls=%d", rollbackCalls)
	}
	for _, step := range operation.Steps {
		if step.Phase != "rolled_back" {
			t.Fatalf("step=%#v", step)
		}
	}
}

func TestCoordinatorRecoversCommitDecisionAfterRestart(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-b")
	fixture.topology.applyErr = errors.New("controller stopped after commit decision")
	operation, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-4")
	if err == nil || operation.Status != StatusRecoveryRequired || operation.Phase != "commit_decided" {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	fixture.topology.applyErr = nil
	restarted, newErr := NewCoordinator(fixture.rooms, fixture.topology, fixture.router, &provisionTestLeases{}, fixture.store)
	if newErr != nil {
		t.Fatal(newErr)
	}
	recovered, recoverErr := restarted.RecoverOperation(context.Background(), operation.ID)
	if recoverErr != nil || recovered.Status != StatusSucceeded || recovered.Phase != "completed" {
		t.Fatalf("recovered=%#v err=%v", recovered, recoverErr)
	}
	for targetID, shard := range map[string]string{"agent:node-a": "Master", "agent:node-b": "Caves"} {
		marker := filepath.Join(fixture.roots[targetID], "Cluster_1", shard, ".dst-admin-migration-id")
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatalf("target marker was not completed: %s: %v", marker, statErr)
		}
	}
}

func TestCoordinatorRecoveryRejectsChangedDesiredTarget(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-b")
	fixture.topology.applyErr = errors.New("pause before topology apply")
	operation, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-5")
	if err == nil || operation.Status != StatusRecoveryRequired {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	fixture.topology.applyErr = nil
	fixture.topology.placements[1].DesiredTargetID = "agent:node-a"
	_, recoverErr := fixture.coordinator.RecoverOperation(context.Background(), operation.ID)
	if !errors.Is(recoverErr, ErrTopologyChanged) || !errors.Is(recoverErr, ErrRecoveryNeeded) {
		t.Fatalf("recover error=%v", recoverErr)
	}
}

func TestCoordinatorRejectsExistingTargetShardAndMissingMasterAddress(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-b")
	fixture.topology.placements[0].Inventory.Inventory = shared.RuntimeInventoryReport{Rooms: []shared.RoomInventoryReport{{
		Directory: "Cluster_1", Shards: []shared.ShardInventoryReport{{Directory: "Master"}},
	}}}
	if _, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-6"); !errors.Is(err, ErrTargetExists) {
		t.Fatalf("existing target error=%v", err)
	}

	fixture = newProvisionFixture(t, "agent:node-a", "agent:node-b")
	fixture.topology.resources.NetworkProfiles = nil
	if _, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-7"); !errors.Is(err, ErrTargetNotReady) {
		t.Fatalf("missing master address error=%v", err)
	}
}

func TestCoordinatorRequiresMigrationForExistingRemoteShard(t *testing.T) {
	fixture := newProvisionFixture(t, "agent:node-a", "agent:node-b")
	fixture.topology.placements[0].AppliedTargetID = "agent:old-node"
	if _, err := fixture.coordinator.Provision(context.Background(), "room-1", "revision-1", "job-8"); !errors.Is(err, ErrMigrationNeeded) {
		t.Fatalf("remote move error=%v", err)
	}
}
