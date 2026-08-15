package placementmigration

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
)

type stoppedControl struct{}

func (stoppedControl) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
}
func (stoppedControl) Start(context.Context, string, string) error        { return nil }
func (stoppedControl) Stop(context.Context, string, string) error         { return nil }
func (stoppedControl) Send(context.Context, string, string, string) error { return nil }

type fakeTopology struct {
	plan     topology.MigrationPlacement
	applyErr error
	applied  bool
}

func (f *fakeTopology) PrepareMigration(context.Context, string, string) (topology.MigrationPlacement, error) {
	return f.plan, nil
}
func (f *fakeTopology) ApplyMigration(_, _, revision, target string) (topology.ExecutionPlacement, error) {
	if f.applyErr != nil {
		return topology.ExecutionPlacement{}, f.applyErr
	}
	if revision != f.plan.Revision || target != f.plan.TargetTargetID {
		return topology.ExecutionPlacement{}, errors.New("unexpected apply arguments")
	}
	f.applied = true
	return topology.ExecutionPlacement{Revision: "revision-2", AppliedTargetID: target, DesiredTargetID: target}, nil
}

type fakeRuntimeRouter struct {
	sourceDriver runtimedriver.Driver
	source       runtimedriver.Target
	targetDriver runtimedriver.Driver
	target       runtimedriver.Target
}

type cleanupFailDriver struct {
	runtimedriver.Driver
	targetRollbackErr error
}

func (d cleanupFailDriver) RollbackMigrationTarget(context.Context, runtimedriver.Target, runtimedriver.Operation, string) error {
	return d.targetRollbackErr
}

func (f fakeRuntimeRouter) MigrationTargets(topology.MigrationPlacement) (runtimedriver.Driver, runtimedriver.Target, runtimedriver.Driver, runtimedriver.Target, error) {
	return f.sourceDriver, f.source, f.targetDriver, f.target, nil
}

type fakeLeases struct{ lease operationlease.Lease }

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
