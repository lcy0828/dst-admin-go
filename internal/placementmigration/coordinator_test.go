package placementmigration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/modpublication"
	"dont/internal/operationlease"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/internal/topology"
	"dont/shared"
)

type stoppedControl struct{}

func (stoppedControl) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
}
func (stoppedControl) Start(context.Context, string, string) error        { return nil }
func (stoppedControl) Stop(context.Context, string, string) error         { return nil }
func (stoppedControl) Send(context.Context, string, string, string) error { return nil }

type trackingControl struct {
	running  bool
	starts   int
	stops    int
	startErr error
}

func (c *trackingControl) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	if c.running {
		return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
	}
	return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
}
func (c *trackingControl) Start(context.Context, string, string) error {
	c.starts++
	if c.startErr != nil {
		return c.startErr
	}
	c.running = true
	return nil
}
func (c *trackingControl) Stop(context.Context, string, string) error {
	c.stops++
	c.running = false
	return nil
}
func (*trackingControl) Send(context.Context, string, string, string) error { return nil }

type fakeTopology struct {
	plan       topology.MigrationPlacement
	links      []topology.ShardLink
	verifyErr  error
	verified   bool
	applyErr   error
	applied    bool
	rolledBack bool
}

func (f *fakeTopology) RollbackMigration(
	_, _, revision, sourceTarget, sourceInstallation, targetTarget, targetInstallation string,
	appliedShardLinks []topology.ShardLink,
) (topology.ExecutionPlacement, error) {
	if !f.applied || revision != "revision-2" || sourceTarget != f.plan.SourceTargetID || sourceInstallation != f.plan.SourceInstallationID ||
		targetTarget != f.plan.TargetTargetID || targetInstallation != f.plan.TargetInstallationID ||
		!reflect.DeepEqual(appliedShardLinks, f.plan.AppliedShardLinks) {
		return topology.ExecutionPlacement{}, errors.New("unexpected rollback arguments")
	}
	f.applied = false
	f.rolledBack = true
	return topology.ExecutionPlacement{
		Revision: "revision-3", AppliedTargetID: sourceTarget, DesiredTargetID: targetTarget,
		AppliedInstallationID: sourceInstallation, DesiredInstallationID: targetInstallation,
	}, nil
}

func (f *fakeTopology) ResolveDesiredShardLinks(context.Context, string) ([]topology.ShardLink, error) {
	return append([]topology.ShardLink(nil), f.links...), nil
}

func (f *fakeTopology) VerifyDesiredShardLinks(context.Context, string) error {
	f.verified = true
	return f.verifyErr
}

func (f *fakeTopology) PrepareMigration(context.Context, string, string) (topology.MigrationPlacement, error) {
	return f.plan, nil
}
func (f *fakeTopology) ApplyMigration(_, _, revision, target, installation string) (topology.ExecutionPlacement, error) {
	if f.applyErr != nil {
		return topology.ExecutionPlacement{}, f.applyErr
	}
	if revision != f.plan.Revision || target != f.plan.TargetTargetID || installation != f.plan.TargetInstallationID {
		return topology.ExecutionPlacement{}, errors.New("unexpected apply arguments")
	}
	f.applied = true
	return topology.ExecutionPlacement{
		Revision: "revision-2", AppliedTargetID: target, DesiredTargetID: target,
		AppliedInstallationID: installation, DesiredInstallationID: installation,
	}, nil
}

type fakeRuntimeRouter struct {
	sourceDriver runtimedriver.Driver
	source       runtimedriver.Target
	targetDriver runtimedriver.Driver
	target       runtimedriver.Target
}

type peerSourceDriver struct {
	runtimedriver.Driver
	grants   int
	grantErr error
}

func (d *peerSourceDriver) Capabilities() []runtimedriver.Capability {
	return append(d.Driver.Capabilities(), runtimedriver.CapabilityMigrationPeer)
}

func (d *peerSourceDriver) GrantMigrationExport(_ context.Context, _ runtimedriver.Target, descriptor runtimedriver.MigrationDescriptor, _ string) (shared.RuntimeMigrationFetchLocation, error) {
	d.grants++
	if d.grantErr != nil {
		return shared.RuntimeMigrationFetchLocation{}, d.grantErr
	}
	return shared.RuntimeMigrationFetchLocation{
		DownloadURL:   "http://peer.example.test/migration-peer/source/" + descriptor.MigrationID,
		DownloadPath:  "/migration-peer/source/" + descriptor.MigrationID,
		DownloadToken: strings.Repeat("p", 40), Size: descriptor.Size, SHA256: descriptor.SHA256,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}, nil
}

type peerTargetDriver struct {
	runtimedriver.Driver
	source        runtimedriver.Driver
	sourceTarget  runtimedriver.Target
	fetches       int
	confirmedFail bool
	unknownErr    error
}

func (d *peerTargetDriver) Capabilities() []runtimedriver.Capability {
	return append(d.Driver.Capabilities(), runtimedriver.CapabilityMigrationPeer)
}

func (d *peerTargetDriver) FetchMigrationImport(ctx context.Context, target runtimedriver.Target, operation runtimedriver.Operation, descriptor runtimedriver.MigrationDescriptor, _ []shared.RuntimeMigrationFetchLocation) (int64, error) {
	d.fetches++
	if d.unknownErr != nil {
		return 0, d.unknownErr
	}
	for offset := int64(0); offset < descriptor.Size; {
		chunk, err := d.source.ReadMigrationExport(ctx, d.sourceTarget, descriptor.MigrationID, offset)
		if err != nil {
			return offset, err
		}
		data := chunk.Data
		if d.confirmedFail {
			data = data[:max(1, len(data)/2)]
		}
		next, err := d.Driver.WriteMigrationImport(ctx, target, operation, descriptor, offset, data)
		if err != nil {
			return offset, err
		}
		if d.confirmedFail {
			return next, errors.Join(runtimedriver.ErrMigrationPeerFallback, errors.New("forced confirmed Peer failure"))
		}
		offset = next
	}
	return descriptor.Size, nil
}

type cleanupFailDriver struct {
	runtimedriver.Driver
	targetRollbackErr error
}

type exportFailDriver struct {
	runtimedriver.Driver
	err error
}

func (d exportFailDriver) PrepareMigrationExport(context.Context, runtimedriver.Target, runtimedriver.Operation, string) (runtimedriver.MigrationDescriptor, error) {
	return runtimedriver.MigrationDescriptor{}, d.err
}

func (d cleanupFailDriver) RollbackMigrationTarget(context.Context, runtimedriver.Target, runtimedriver.Operation, string) error {
	return d.targetRollbackErr
}

func (f fakeRuntimeRouter) MigrationTargets(topology.MigrationPlacement) (runtimedriver.Driver, runtimedriver.Target, runtimedriver.Driver, runtimedriver.Target, error) {
	return f.sourceDriver, f.source, f.targetDriver, f.target, nil
}

type fakeLeases struct{ lease operationlease.Lease }

type migrationMutationObserver struct{ targets []string }

func (o *migrationMutationObserver) RuntimeTargetChanged(targetID string) {
	o.targets = append(o.targets, targetID)
}

type migrationModReconciler struct {
	calls          int
	lease          operationlease.Lease
	prepareObserve func()
	observe        func()
	err            error
	rollbacks      int
}

func (r *migrationModReconciler) PreparePlacementMigration(_ context.Context, _ topology.MigrationPlacement, lease operationlease.Lease) (modpublication.PreparedTransaction, error) {
	r.calls++
	r.lease = lease
	if r.prepareObserve != nil {
		r.prepareObserve()
	}
	return &migrationModTransaction{reconciler: r, publication: modpublication.Publication{Status: modpublication.StatusPrepared}}, nil
}

type migrationModTransaction struct {
	reconciler  *migrationModReconciler
	publication modpublication.Publication
}

func (t *migrationModTransaction) Publication() modpublication.Publication { return t.publication }
func (*migrationModTransaction) Renew(context.Context) error               { return nil }
func (t *migrationModTransaction) Publish(context.Context) (modpublication.Publication, error) {
	if t.reconciler.observe != nil {
		t.reconciler.observe()
	}
	if t.reconciler.err != nil {
		t.publication.Status = modpublication.StatusRolledBack
		return t.publication, t.reconciler.err
	}
	t.publication.Status = modpublication.StatusPublishing
	return t.publication, nil
}
func (t *migrationModTransaction) Commit(context.Context) (modpublication.Publication, error) {
	t.publication.Status = modpublication.StatusSucceeded
	t.publication.CommitDecision = true
	return t.publication, nil
}
func (t *migrationModTransaction) Rollback(_ context.Context, _ string, _ error) (modpublication.Publication, error) {
	t.reconciler.rollbacks++
	t.publication.Status = modpublication.StatusRolledBack
	return t.publication, nil
}
func (*migrationModTransaction) Close() {}

func TestCoordinatorPreparesModsBeforeStoppingSource(t *testing.T) {
	coordinator, _, sourceRoot, targetRoot := migrationFixture(t, nil)
	sourceControl, targetControl := &trackingControl{running: true}, &trackingControl{}
	sourceDriver, err := runtimedriver.NewNative(sourceRoot, sourceControl)
	if err != nil {
		t.Fatal(err)
	}
	targetDriver, err := runtimedriver.NewNative(targetRoot, targetControl)
	if err != nil {
		t.Fatal(err)
	}
	router := coordinator.runtimes.(fakeRuntimeRouter)
	router.sourceDriver, router.targetDriver = sourceDriver, targetDriver
	coordinator.runtimes = router
	reconciler := &migrationModReconciler{prepareObserve: func() {
		if sourceControl.stops != 0 || !sourceControl.running {
			t.Fatal("source stopped before target Mod preparation completed")
		}
	}}
	if err := coordinator.ConfigureModReconciler(reconciler); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1"); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeLeases) Acquire(_ context.Context, roomID, key string, ttl time.Duration) (operationlease.Lease, error) {
	f.lease = operationlease.Lease{RoomID: roomID, LeaseID: "lease-1", OperationKey: key, FencingToken: 1, ExpiresAt: time.Now().UTC().Add(ttl)}
	return f.lease, nil
}
func (f *fakeLeases) Renew(_ context.Context, lease operationlease.Lease, ttl time.Duration) (operationlease.Lease, error) {
	lease.ExpiresAt = time.Now().UTC().Add(ttl)
	f.lease = lease
	return lease, nil
}
func (*fakeLeases) Release(operationlease.Lease) error { return nil }

func migrationFixture(t *testing.T, applyErr error) (*Coordinator, *fakeTopology, string, string) {
	t.Helper()
	sourceRoot, targetRoot := filepath.Join(t.TempDir(), "source"), filepath.Join(t.TempDir(), "target")
	for _, root := range []string{sourceRoot, targetRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	worldPath := filepath.Join(sourceRoot, "Cluster_1", "Master", "save", "session")
	if err := os.MkdirAll(worldPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(sourceRoot, "Cluster_1", "cluster.ini"):          "[NETWORK]\ncluster_name=Test\n",
		filepath.Join(sourceRoot, "Cluster_1", "Master", "server.ini"): "[SHARD]\nis_master=true\n",
		filepath.Join(worldPath, "data"):                               "save-data",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sourceDriver, err := runtimedriver.NewNative(sourceRoot, stoppedControl{})
	if err != nil {
		t.Fatal(err)
	}
	targetDriver, err := runtimedriver.NewNative(targetRoot, stoppedControl{})
	if err != nil {
		t.Fatal(err)
	}
	room := rooms.Room{ID: "room-1", DirectoryName: "Cluster_1", Name: "Test", Managed: true}
	world := rooms.World{ID: "world-1", RoomID: room.ID, DirectoryName: "Master", Name: "Master", IsMaster: true}
	plan := topology.MigrationPlacement{
		Room: room, World: world, Revision: "revision-1", SourceTargetID: "local", TargetTargetID: "agent:node",
		SourceInstallationID: "local", TargetInstallationID: "remote",
		Source: agents.RuntimeTarget{ID: "local", Config: agents.RuntimeConfig{InstallationID: "local"}},
		Target: agents.RuntimeTarget{ID: "agent:node", Config: agents.RuntimeConfig{InstallationID: "remote"}},
	}
	topologyService := &fakeTopology{plan: plan, applyErr: applyErr}
	router := fakeRuntimeRouter{
		sourceDriver: sourceDriver, source: runtimedriver.Target{TargetID: "local", InstallationID: "local", RoomID: room.ID, WorldID: world.ID, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1"},
		targetDriver: targetDriver, target: runtimedriver.Target{TargetID: "agent:node", InstallationID: "remote", RoomID: room.ID, WorldID: world.ID, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1"},
	}
	coordinator, err := New(topologyService, router, &fakeLeases{})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, topologyService, sourceRoot, targetRoot
}

func TestCoordinatorMigratesAndAppliesPlacement(t *testing.T) {
	coordinator, topologyService, sourceRoot, targetRoot := migrationFixture(t, nil)
	mutations := &migrationMutationObserver{}
	if err := coordinator.ConfigureMutationObserver(mutations); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if err != nil || !topologyService.applied || result.AppliedRevision != "revision-2" || result.BytesTransferred == 0 || result.RecoveryRef == "" {
		t.Fatalf("result=%#v applied=%v err=%v", result, topologyService.applied, err)
	}
	data, err := os.ReadFile(filepath.Join(targetRoot, "Cluster_1", "Master", "save", "session", "data"))
	if err != nil || string(data) != "save-data" {
		t.Fatalf("target data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "Cluster_1", "Master")); !os.IsNotExist(err) {
		t.Fatalf("source shard still active: %v", err)
	}
	if len(mutations.targets) != 2 || mutations.targets[0] != "local" || mutations.targets[1] != "agent:node" {
		t.Fatalf("mutation targets=%v", mutations.targets)
	}
}

func TestCoordinatorUsesPeerTransferWhenBothRuntimeTargetsSupportIt(t *testing.T) {
	coordinator, _, _, targetRoot := migrationFixture(t, nil)
	router := coordinator.runtimes.(fakeRuntimeRouter)
	source := &peerSourceDriver{Driver: router.sourceDriver}
	target := &peerTargetDriver{Driver: router.targetDriver, source: router.sourceDriver, sourceTarget: router.source}
	router.sourceDriver, router.targetDriver = source, target
	coordinator.runtimes = router

	result, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if err != nil || result.TransferSource != TransferSourcePeer || result.BytesTransferred == 0 || source.grants != 1 || target.fetches != 1 {
		t.Fatalf("result=%#v grants=%d fetches=%d err=%v", result, source.grants, target.fetches, err)
	}
	data, err := os.ReadFile(filepath.Join(targetRoot, "Cluster_1", "Master", "save", "session", "data"))
	if err != nil || string(data) != "save-data" {
		t.Fatalf("target data=%q err=%v", data, err)
	}
}

func TestCoordinatorResetsPartialPeerImportBeforeControllerFallback(t *testing.T) {
	coordinator, _, _, targetRoot := migrationFixture(t, nil)
	router := coordinator.runtimes.(fakeRuntimeRouter)
	source := &peerSourceDriver{Driver: router.sourceDriver}
	target := &peerTargetDriver{
		Driver: router.targetDriver, source: router.sourceDriver, sourceTarget: router.source, confirmedFail: true,
	}
	router.sourceDriver, router.targetDriver = source, target
	coordinator.runtimes = router

	result, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if err != nil || result.TransferSource != TransferSourceControllerRelay || result.BytesTransferred == 0 || source.grants != 1 || target.fetches != 1 {
		t.Fatalf("result=%#v grants=%d fetches=%d err=%v", result, source.grants, target.fetches, err)
	}
	data, err := os.ReadFile(filepath.Join(targetRoot, "Cluster_1", "Master", "save", "session", "data"))
	if err != nil || string(data) != "save-data" {
		t.Fatalf("fallback target data=%q err=%v", data, err)
	}
}

func TestCoordinatorRejectsLegacyAgentBeforeApplyingShardRouteOverride(t *testing.T) {
	coordinator, topologyService, sourceRoot, targetRoot := migrationFixture(t, nil)
	topologyService.links = []topology.ShardLink{{
		SourceTargetID: "agent:node", SourceInstallationID: "remote",
		MasterTargetID: "local", MasterInstallationID: "local",
		Address: "192.168.2.20", Port: 10889, Mode: topology.ShardLinkLAN,
	}}
	router := coordinator.runtimes.(fakeRuntimeRouter)
	router.target.CapabilitiesKnown = true
	router.target.Capabilities = []runtimedriver.Capability{}
	coordinator.runtimes = router

	_, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if !errors.Is(err, runtimedriver.ErrCapabilityMissing) {
		t.Fatalf("legacy Agent route capability error=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(sourceRoot, "Cluster_1", "Master")); statErr != nil {
		t.Fatalf("source changed before capability rejection: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(targetRoot, "Cluster_1", "Master")); !os.IsNotExist(statErr) {
		t.Fatalf("target changed before capability rejection: %v", statErr)
	}
}

func TestCoordinatorVerifiesShardRouteBeforeStoppingOrTransferring(t *testing.T) {
	coordinator, topologyService, sourceRoot, targetRoot := migrationFixture(t, nil)
	topologyService.links = []topology.ShardLink{{
		SourceTargetID: "agent:node", SourceInstallationID: "remote",
		MasterTargetID: "local", MasterInstallationID: "local",
		Address: "192.168.2.20", Port: 10889, Mode: topology.ShardLinkLAN,
	}}
	topologyService.verifyErr = errors.New("forced route probe failure")

	_, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if err == nil || !strings.Contains(err.Error(), "forced route probe failure") || !topologyService.verified {
		t.Fatalf("route verification result verified=%v err=%v", topologyService.verified, err)
	}
	if _, statErr := os.Stat(filepath.Join(sourceRoot, "Cluster_1", "Master")); statErr != nil {
		t.Fatalf("source changed before route verification: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(targetRoot, "Cluster_1", "Master")); !os.IsNotExist(statErr) {
		t.Fatalf("target changed before route verification: %v", statErr)
	}
}

func TestCoordinatorRollsBackBothSidesWhenTopologyApplyFails(t *testing.T) {
	coordinator, _, sourceRoot, targetRoot := migrationFixture(t, errors.New("revision changed"))
	if _, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1"); err == nil {
		t.Fatal("expected apply failure")
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "Cluster_1", "Master", "server.ini")); err != nil {
		t.Fatalf("source shard was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "Cluster_1", "Master")); !os.IsNotExist(err) {
		t.Fatalf("target shard was not rolled back: %v", err)
	}
}

func TestCoordinatorReportsRollbackFailures(t *testing.T) {
	coordinator, _, _, _ := migrationFixture(t, errors.New("revision changed"))
	router := coordinator.runtimes.(fakeRuntimeRouter)
	router.targetDriver = cleanupFailDriver{Driver: router.targetDriver, targetRollbackErr: errors.New("forced target cleanup failure")}
	coordinator.runtimes = router
	_, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if err == nil || !strings.Contains(err.Error(), "revision changed") || !strings.Contains(err.Error(), "forced target cleanup failure") {
		t.Fatalf("joined migration error=%v", err)
	}
}

func TestCoordinatorPreservesRunningStateAcrossMigration(t *testing.T) {
	coordinator, _, sourceRoot, targetRoot := migrationFixture(t, nil)
	sourceControl, targetControl := &trackingControl{running: true}, &trackingControl{}
	sourceDriver, err := runtimedriver.NewNative(sourceRoot, sourceControl)
	if err != nil {
		t.Fatal(err)
	}
	targetDriver, err := runtimedriver.NewNative(targetRoot, targetControl)
	if err != nil {
		t.Fatal(err)
	}
	router := coordinator.runtimes.(fakeRuntimeRouter)
	router.sourceDriver, router.targetDriver = sourceDriver, targetDriver
	coordinator.runtimes = router

	result, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if err != nil || !result.WasRunning || !result.RuntimeRestored || sourceControl.stops != 1 || targetControl.starts != 1 || !targetControl.running {
		t.Fatalf("result=%#v source=%#v target=%#v err=%v", result, sourceControl, targetControl, err)
	}
}

func TestCoordinatorSynchronizesModsBeforeRestoringTargetRuntime(t *testing.T) {
	coordinator, _, sourceRoot, targetRoot := migrationFixture(t, nil)
	sourceControl, targetControl := &trackingControl{running: true}, &trackingControl{}
	sourceDriver, err := runtimedriver.NewNative(sourceRoot, sourceControl)
	if err != nil {
		t.Fatal(err)
	}
	targetDriver, err := runtimedriver.NewNative(targetRoot, targetControl)
	if err != nil {
		t.Fatal(err)
	}
	router := coordinator.runtimes.(fakeRuntimeRouter)
	router.sourceDriver, router.targetDriver = sourceDriver, targetDriver
	coordinator.runtimes = router
	reconciler := &migrationModReconciler{observe: func() {
		if targetControl.starts != 0 || targetControl.running {
			t.Fatal("target runtime started before Mod synchronization")
		}
	}}
	if err := coordinator.ConfigureModReconciler(reconciler); err != nil {
		t.Fatal(err)
	}

	result, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if err != nil || reconciler.calls != 1 || reconciler.lease.RoomID != "room-1" || !result.RuntimeRestored || targetControl.starts != 1 {
		t.Fatalf("result=%#v reconciler=%#v target=%#v err=%v", result, reconciler, targetControl, err)
	}
}

func TestCoordinatorDoesNotStartTargetWhenModSynchronizationFails(t *testing.T) {
	coordinator, _, sourceRoot, targetRoot := migrationFixture(t, nil)
	sourceControl, targetControl := &trackingControl{running: true}, &trackingControl{}
	sourceDriver, err := runtimedriver.NewNative(sourceRoot, sourceControl)
	if err != nil {
		t.Fatal(err)
	}
	targetDriver, err := runtimedriver.NewNative(targetRoot, targetControl)
	if err != nil {
		t.Fatal(err)
	}
	router := coordinator.runtimes.(fakeRuntimeRouter)
	router.sourceDriver, router.targetDriver = sourceDriver, targetDriver
	coordinator.runtimes = router
	reconciler := &migrationModReconciler{err: errors.New("forced Mod synchronization failure")}
	if err := coordinator.ConfigureModReconciler(reconciler); err != nil {
		t.Fatal(err)
	}

	result, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if !errors.Is(err, ErrRuntimeRestore) || reconciler.calls != 1 || !result.RuntimeRestored ||
		sourceControl.starts != 1 || !sourceControl.running || targetControl.starts != 0 || targetControl.running {
		t.Fatalf("result=%#v reconciler=%#v source=%#v target=%#v err=%v", result, reconciler, sourceControl, targetControl, err)
	}
}

func TestCoordinatorRestartsSourceWhenMigrationRollsBack(t *testing.T) {
	coordinator, _, sourceRoot, targetRoot := migrationFixture(t, errors.New("revision changed"))
	sourceControl, targetControl := &trackingControl{running: true}, &trackingControl{}
	sourceDriver, err := runtimedriver.NewNative(sourceRoot, sourceControl)
	if err != nil {
		t.Fatal(err)
	}
	targetDriver, err := runtimedriver.NewNative(targetRoot, targetControl)
	if err != nil {
		t.Fatal(err)
	}
	router := coordinator.runtimes.(fakeRuntimeRouter)
	router.sourceDriver, router.targetDriver = sourceDriver, targetDriver
	coordinator.runtimes = router

	result, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if err == nil || !result.WasRunning || !result.RuntimeRestored || sourceControl.stops != 1 || sourceControl.starts != 1 || !sourceControl.running || targetControl.starts != 0 {
		t.Fatalf("result=%#v source=%#v target=%#v err=%v", result, sourceControl, targetControl, err)
	}
}

func TestCoordinatorRollsBackTopologyWhenTargetStartFails(t *testing.T) {
	coordinator, topologyService, sourceRoot, targetRoot := migrationFixture(t, nil)
	sourceControl := &trackingControl{running: true}
	targetControl := &trackingControl{startErr: errors.New("forced target start failure")}
	sourceDriver, err := runtimedriver.NewNative(sourceRoot, sourceControl)
	if err != nil {
		t.Fatal(err)
	}
	targetDriver, err := runtimedriver.NewNative(targetRoot, targetControl)
	if err != nil {
		t.Fatal(err)
	}
	router := coordinator.runtimes.(fakeRuntimeRouter)
	router.sourceDriver, router.targetDriver = sourceDriver, targetDriver
	coordinator.runtimes = router
	reconciler := &migrationModReconciler{}
	if err := coordinator.ConfigureModReconciler(reconciler); err != nil {
		t.Fatal(err)
	}

	result, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if !errors.Is(err, ErrRuntimeRestore) || !topologyService.rolledBack || topologyService.applied ||
		!result.RuntimeRestored || !sourceControl.running || sourceControl.starts != 1 || targetControl.starts != 1 || reconciler.rollbacks != 1 {
		t.Fatalf("result=%#v topology=%#v source=%#v target=%#v reconciler=%#v err=%v", result, topologyService, sourceControl, targetControl, reconciler, err)
	}
}

func TestCoordinatorRestartsSourceWhenExportPreparationFails(t *testing.T) {
	coordinator, _, sourceRoot, _ := migrationFixture(t, nil)
	sourceControl := &trackingControl{running: true}
	sourceDriver, err := runtimedriver.NewNative(sourceRoot, sourceControl)
	if err != nil {
		t.Fatal(err)
	}
	router := coordinator.runtimes.(fakeRuntimeRouter)
	router.sourceDriver = exportFailDriver{Driver: sourceDriver, err: errors.New("forced export failure")}
	coordinator.runtimes = router

	result, err := coordinator.Migrate(context.Background(), "room-1", "world-1", "revision-1")
	if err == nil || !result.WasRunning || !result.RuntimeRestored || sourceControl.stops != 1 || sourceControl.starts != 1 || !sourceControl.running {
		t.Fatalf("result=%#v source=%#v err=%v", result, sourceControl, err)
	}
}
