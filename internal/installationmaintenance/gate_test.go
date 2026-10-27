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
