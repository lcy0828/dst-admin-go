package installationmaintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestGateExcludesAliasesAndAllowsOwnedNestedOperations(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip(err)
	}
	gate := New(nil)
	ctx, release, err := gate.Acquire(context.Background(), []Resource{{"local", "one", root}}, "release")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = gate.Acquire(context.Background(), []Resource{{"local", "two", alias}}, "")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("alias bypassed maintenance: %v", err)
	}
	_, nested, err := gate.Acquire(ctx, []Resource{{"local", "one", root}}, "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	_, _, err = gate.Acquire(context.Background(), []Resource{{"local", "one", root}}, "")
	if !errors.Is(err, ErrBusy) {
		t.Fatal("nested operation lost ownership")
	}
	nested()
	_, free, err := gate.Acquire(context.Background(), []Resource{{"local", "two", alias}}, "")
	if err != nil {
		t.Fatal(err)
	}
	free()
	if len(gate.owners) != 0 {
		t.Fatal("idle gate retained resources")
	}
}
func TestGateConcurrentMaintenanceAndLifecycleCannotBothEnter(t *testing.T) {
	gate := New(nil)
	resources := []Resource{{"agent:node", "default", "/opt/dst/server"}}
	_, release, err := gate.Acquire(context.Background(), resources, "release")
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for i := 0; i < 10; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _, err := gate.Acquire(context.Background(), resources, "")
			if !errors.Is(err, ErrBusy) {
				t.Errorf("concurrent writer entered: %v", err)
			}
		}()
	}
	wait.Wait()
	release()
	if len(gate.owners) != 0 {
		t.Fatal("leaked resources")
	}
}

func TestWindowsExecutableAliasesResolveToSameMaintenanceRoot(t *testing.T) {
	a := Resource{TargetID: "agent:windows", InstallationID: "one", Directory: `C:\DST\Server`}
	b := Resource{TargetID: "agent:windows", InstallationID: "two", Directory: `c:\dst\server\bin64\dontstarve_dedicated_server_nullrenderer_x64.exe`}
	if !Overlaps(a, b) {
		t.Fatal("Windows executable alias bypassed maintenance")
	}
}

func TestSharedLifecyclesAllowParallelStartsAndExcludeMaintenanceAliases(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip(err)
	}
	gate := New(nil)
	first := []Resource{{"local", "one", root}}
	second := []Resource{{"local", "two", alias}}
	_, master, err := gate.AcquireShared(context.Background(), first, "master")
	if err != nil {
		t.Fatal(err)
	}
	defer master()
	_, caves, err := gate.AcquireShared(context.Background(), second, "caves")
	if err != nil {
		t.Fatalf("second shard could not start: %v", err)
	}
	defer caves()
	if _, _, err := gate.Acquire(context.Background(), second, "update"); !errors.Is(err, ErrBusy) {
		t.Fatalf("maintenance entered during startup: %v", err)
	}
	// Resources on another node do not overlap, even with identical paths/IDs.
	_, otherNode, err := gate.Acquire(context.Background(), []Resource{{"agent:node", "one", root}}, "update")
	if err != nil {
		t.Fatal(err)
	}
	otherNode()
	master()
	master() // Releasing a completed operation twice must not drop another user.
	if _, _, err := gate.Acquire(context.Background(), first, "update"); !errors.Is(err, ErrBusy) {
		t.Fatalf("one completed shard released the other: %v", err)
	}
	caves()
	_, update, err := gate.Acquire(context.Background(), first, "update")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := gate.AcquireShared(context.Background(), second, "start"); !errors.Is(err, ErrBusy) {
		t.Fatalf("startup entered during maintenance: %v", err)
	}
	update()
	if len(gate.owners) != 0 || len(gate.readers) != 0 {
		t.Fatal("idle gate retained resources")
	}
}

func TestOwningMaintenanceCanRestoreParallelShardsWithoutDroppingExclusion(t *testing.T) {
	gate := New(nil)
	resources := []Resource{{"local", "default", t.TempDir()}}
	ctx, update, err := gate.Acquire(context.Background(), resources, "release")
	if err != nil {
		t.Fatal(err)
	}
	defer update()
	ctx, master, err := gate.AcquireShared(ctx, resources, "")
	if err != nil {
		t.Fatal(err)
	}
	defer master()
	_, caves, err := gate.AcquireShared(ctx, resources, "")
	if err != nil {
		t.Fatal(err)
	}
	defer caves()
	if Owner(ctx) != "release" {
		t.Fatal("lost inherited maintenance owner")
	}
	update()
	master()
	if _, _, err := gate.Acquire(context.Background(), resources, "other"); !errors.Is(err, ErrBusy) {
		t.Fatalf("restoration lost exclusion before last shard completed: %v", err)
	}
	if _, _, err := gate.AcquireShared(ctx, resources, "other"); !errors.Is(err, ErrBusy) {
		t.Fatalf("shared caller replaced inherited owner: %v", err)
	}
	caves()
	if len(gate.owners) != 0 || len(gate.readers) != 0 {
		t.Fatal("restoration leaked resources")
	}
}

func TestSharedLifecycleChecksPersistentMaintenanceAndCancellation(t *testing.T) {
	resources := []Resource{{"local", "default", t.TempDir()}}
	gate := New(func(_ context.Context, _ []Resource, owner string) error {
		if owner != "interrupted-release" {
			return &BusyError{Owner: "interrupted-release"}
		}
		return nil
	})
	if _, _, err := gate.AcquireShared(context.Background(), resources, "start"); !errors.Is(err, ErrBusy) {
		t.Fatalf("persistent interrupted update did not block startup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := gate.AcquireShared(ctx, resources, "interrupted-release"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled startup entered: %v", err)
	}
	_, release, err := gate.AcquireShared(context.Background(), resources, "interrupted-release")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(gate.owners) != 0 || len(gate.readers) != 0 {
		t.Fatal("failed/canceled shared acquisition retained resources")
	}
}
