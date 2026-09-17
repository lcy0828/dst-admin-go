package runtimedriver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/internal/topology"
	"dont/shared"
)

type worldStatePlacement struct {
	values            map[string]topology.ExecutionPlacement
	applied, resolved int
}

func (p *worldStatePlacement) AppliedPlacement(_, worldID string) (topology.ExecutionPlacement, error) {
	p.applied++
	return p.values[worldID], nil
}

func (p *worldStatePlacement) ResolveExecution(_ context.Context, _, worldID string) (topology.ExecutionPlacement, error) {
	p.resolved++
	return p.values[worldID], nil
}

type worldStateRoutingDriver struct {
	Driver
	targets []Target
	err     error
}

func (*worldStateRoutingDriver) Kind() Kind { return KindNative }
func (*worldStateRoutingDriver) Capabilities() []Capability {
	return []Capability{CapabilityWorldStateRead}
}
func (d *worldStateRoutingDriver) ReadWorldState(_ context.Context, target Target) (shared.RuntimeWorldStateRead, error) {
	d.targets = append(d.targets, target)
	return shared.RuntimeWorldStateRead{Runtime: shared.ShardRuntimeStatus{State: "running"}}, d.err
}

func TestWorldStateRouterResolvesEachAppliedTargetOnceWithoutLease(t *testing.T) {
	placements := &worldStatePlacement{values: map[string]topology.ExecutionPlacement{}}
	for world, node := range map[string]string{"Master": "agent:one", "Caves": "agent:two"} {
		placements.values[world] = topology.ExecutionPlacement{
			AppliedTargetID: node, AppliedInstallationID: "native", Revision: "revision",
			Room: rooms.Room{DirectoryName: "all"}, World: rooms.World{DirectoryName: world},
			Target: agents.RuntimeTarget{ID: node, Kind: agents.RuntimeKindAgent, Capabilities: []string{"runtime.worldstate.read.v1"}},
		}
	}
	driver := &worldStateRoutingDriver{}
	acquires := 0
	router, err := NewRouter(placements, cpuLifecycleLease{acquires: &acquires}, driver, driver)
	if err != nil {
		t.Fatal(err)
	}
	for _, world := range []string{"Master", "Caves"} {
		if _, err := router.ReadWorldState(context.Background(), "room", world); err != nil {
			t.Fatal(err)
		}
	}
	if placements.applied != 2 || placements.resolved != 2 || len(driver.targets) != 2 || acquires != 0 || driver.targets[0].TargetID != "agent:one" || driver.targets[1].TargetID != "agent:two" {
		t.Fatalf("routing=%+v targets=%+v leases=%d", placements, driver.targets, acquires)
	}
	driver.err = errors.New("Agent offline")
	if _, err := router.ReadWorldState(context.Background(), "room", "Master"); !errors.Is(err, driver.err) || len(driver.targets) != 3 {
		t.Fatalf("remote error was hidden or retried locally: %v, %#v", err, driver.targets)
	}
}

func TestNativeWorldStateReadUsesLocalFilesAndOneStatusRead(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "all", "Master")
	if err := os.MkdirAll(filepath.Join(world, "save/mod_config_data/dst-admin"), 0750); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"server.ini":     "[SHARD]\nid=1\n",
		"server_log.txt": "[00:00:00]: Current time: Sun Sep 6 12:00:00 2026\n",
		"save/mod_config_data/dst-admin/worldstate-a.json": `{"sequence":1}`,
	} {
		if err := os.WriteFile(filepath.Join(world, name), []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	control := &nativeLifecycleControl{statuses: []shards.RuntimeStatus{{State: shards.RuntimeRunning, SessionExists: true}}}
	driver, err := NewNative(root, control)
	if err != nil {
		t.Fatal(err)
	}
	value, err := driver.ReadWorldState(context.Background(), Target{TargetID: "local", InstallationID: "native", Cluster: "all", Shard: "Master"})
	if err != nil || value.ReadError != "" || len(value.Artifacts.Artifacts) != 1 || control.index != 1 || control.starts != 0 || control.stops != 0 {
		t.Fatalf("native read=%#v control=%+v error=%v", value, control, err)
	}
}

type worldStateExecutor struct {
	emptyMigrationExecutor
	requests []shared.RuntimeOperationRequest
}

func (e *worldStateExecutor) ExecuteRuntime(_ context.Context, _ string, request shared.RuntimeOperationRequest, _ int) (agents.RuntimeExecutionResult, error) {
	e.requests = append(e.requests, request)
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{WorldState: &shared.RuntimeWorldStateRead{Runtime: shared.ShardRuntimeStatus{State: "stopped"}}}}, nil
}

func TestAgentWorldStateReadUsesOneReadOnlyCommand(t *testing.T) {
	executor := &worldStateExecutor{}
	driver, err := NewAgent(executor)
	if err != nil {
		t.Fatal(err)
	}
	value, err := driver.ReadWorldState(context.Background(), Target{TargetID: "agent:one", InstallationID: "native", Cluster: "all", Shard: "Master"})
	if err != nil || value.Runtime.State != "stopped" || len(executor.requests) != 1 {
		t.Fatalf("read=%#v requests=%#v error=%v", value, executor.requests, err)
	}
	request := executor.requests[0]
	if request.Action != shared.RuntimeActionWorldStateRead || request.Console != nil || shared.RuntimeOperationRequiresLease(request) || request.FencingToken != 0 {
		t.Fatalf("not a read-only request: %#v", request)
	}
}
