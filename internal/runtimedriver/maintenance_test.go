package runtimedriver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/installationmaintenance"
	"dont/internal/shards"
	"dont/shared"
)

type parallelStartControl struct {
	mu      sync.Mutex
	running map[string]bool
	entered chan string
	resume  <-chan struct{}
	err     error
}

func (c *parallelStartControl) Status(_ context.Context, _, shard string) (shards.RuntimeStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[shard] {
		return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
	}
	return shards.RuntimeStatus{State: shards.RuntimeStopped}, nil
}

func (c *parallelStartControl) Start(ctx context.Context, _, shard string) error {
	c.entered <- shard
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.resume:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running[shard] = c.err == nil
	return c.err
}

func (c *parallelStartControl) Stop(_ context.Context, _, shard string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.running, shard)
	return nil
}

func (*parallelStartControl) Send(context.Context, string, string, string) error { return nil }

type parallelStartExecutor struct {
	emptyMigrationExecutor
	control *parallelStartControl
}

func (e parallelStartExecutor) ExecuteShard(ctx context.Context, _ string, request shared.ShardOperationRequest, _ int) (agents.ShardExecutionResult, error) {
	err := e.control.Start(ctx, request.Cluster, request.Shard)
	return agents.ShardExecutionResult{Result: shared.ShardOperationResult{Status: shared.ShardRuntimeStatus{State: "running", SessionExists: true}}}, err
}

type maintenanceLifecycleDriver interface {
	ExecuteShard(context.Context, Target, Operation, shared.ShardAction, time.Duration) (shared.ShardOperationResult, error)
	ConfigureMaintenance(*installationmaintenance.Gate, func(Target) (installationmaintenance.Resource, error))
}

func newMaintenanceLifecycleDriver(t *testing.T, remote bool, gate *installationmaintenance.Gate, control *parallelStartControl) (maintenanceLifecycleDriver, Target, installationmaintenance.Resource) {
	t.Helper()
	var driver maintenanceLifecycleDriver
	target := Target{TargetID: "local", InstallationID: "default", Cluster: "room"}
	if remote {
		target.TargetID = "agent:node"
		value, err := NewAgent(parallelStartExecutor{control: control})
		if err != nil {
			t.Fatal(err)
		}
		driver = value
	} else {
		driver = newNativeLifecycleDriver(t, control)
	}
	resource := installationmaintenance.Resource{TargetID: target.TargetID, InstallationID: target.InstallationID, Directory: t.TempDir()}
	driver.ConfigureMaintenance(gate, func(Target) (installationmaintenance.Resource, error) { return resource, nil })
	return driver, target, resource
}

// This exercises the real Native and Agent lifecycle boundaries with maintenance
// configured, reproducing the all-start race that unconfigured driver tests miss.
func TestConcurrentShardLifecycleSharesInstallationWithoutBypassingMaintenance(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, action := range []shared.ShardAction{shared.ShardActionStart, shared.ShardActionRestart} {
			name := "local/"
			if remote {
				name = "agent/"
			}
			t.Run(name+string(action), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				var workers sync.WaitGroup
				defer func() {
					cancel()
					workers.Wait()
				}()
				resume := make(chan struct{})
				control := &parallelStartControl{running: map[string]bool{}, entered: make(chan string, 2), resume: resume}
				gate := installationmaintenance.New(nil)
				driver, target, resource := newMaintenanceLifecycleDriver(t, remote, gate, control)
				done := make(chan error, 2)
				for _, world := range []string{"Master", "Caves"} {
					selected := target
					selected.Shard = world
					workers.Add(1)
					go func() {
						defer workers.Done()
						_, err := driver.ExecuteShard(ctx, selected, Operation{ID: selected.Shard}, action, 5*time.Second)
						done <- err
					}()
				}
				entered := map[string]bool{}
				for len(entered) != 2 {
					select {
					case shard := <-control.entered:
						entered[shard] = true
					case err := <-done:
						t.Fatalf("shard completed before both processes could start: %v", err)
					case <-ctx.Done():
						t.Fatal("shards did not start concurrently")
					}
				}
				if _, _, err := gate.Acquire(ctx, []installationmaintenance.Resource{resource}, "game-update"); !errors.Is(err, installationmaintenance.ErrBusy) {
					t.Fatalf("update entered during startup: %v", err)
				}
				close(resume)
				for range 2 {
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
				_, release, err := gate.Acquire(ctx, []installationmaintenance.Resource{resource}, "game-update")
				if err != nil {
					t.Fatalf("completed startup retained installation reservation: %v", err)
				}
				defer release()
				target.Shard = "Master"
				if _, err := driver.ExecuteShard(ctx, target, Operation{ID: "blocked"}, action, time.Second); !errors.Is(err, installationmaintenance.ErrBusy) {
					t.Fatalf("start entered during real maintenance: %v", err)
				}
			})
		}
	}
}

func TestFailedShardLifecycleReleasesSharedInstallation(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "agent"}[remote], func(t *testing.T) {
			resume := make(chan struct{})
			close(resume)
			failure := errors.New("launch failed")
			control := &parallelStartControl{running: map[string]bool{}, entered: make(chan string, 1), resume: resume, err: failure}
			gate := installationmaintenance.New(nil)
			driver, target, resource := newMaintenanceLifecycleDriver(t, remote, gate, control)
			target.Shard = "Master"
			if _, err := driver.ExecuteShard(context.Background(), target, Operation{ID: "failed"}, shared.ShardActionStart, time.Second); !errors.Is(err, failure) {
				t.Fatalf("lost launch error: %v", err)
			}
			_, release, err := gate.Acquire(context.Background(), []installationmaintenance.Resource{resource}, "update")
			if err != nil {
				t.Fatalf("failed start leaked installation reservation: %v", err)
			}
			release()
		})
	}
}
