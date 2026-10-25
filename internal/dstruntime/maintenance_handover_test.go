package dstruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/roomops"
	"dont/internal/shards"
	"dont/shared"
)

func forceRuntimeMaintenance(t *testing.T, worldPath string) []byte {
	t.Helper()
	value := []byte("-- DST-ADMIN MANAGED BLOCK BEGIN protocol=2 version=2.4.10\n-- old loader\n-- DST-ADMIN MANAGED BLOCK END\n-- preserve user code\n")
	if err := os.WriteFile(filepath.Join(worldPath, customCommandsName), value, 0600); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestMaintenanceDrainsHealthWritesBeforeHandover(t *testing.T) {
	for _, legacyStatus := range []bool{false, true} {
		t.Run(map[bool]string{false: "live health writer", true: "late retired callback"}[legacyStatus], func(t *testing.T) {
			manager, catalog, root := newRuntimeTestManager(t)
			path := filepath.Join(root, "Cluster_1", "Master")
			control := newMaintenanceLuaControl(t, path)
			request := shared.RuntimeLuaRequest{Version: RuntimeVersion, InstallIfMissing: true}
			if _, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control); err != nil {
				t.Fatal(err)
			}
			previous, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := control.state.DoString(`
local original=TheSim.SetPersistentString
local hold=true
function TheSim:SetPersistentString(name, value, encode, callback)
    if hold and name=="mod_config_data/dst-admin/health-"..DSTAdmin.version..".json" then
        return original(self,name,value,encode,function(written)
            heldHealthCallback=function() if callback then callback(written) end end
        end)
    end
    return original(self,name,value,encode,callback)
end
assert(DSTAdmin.Refresh())
assert(heldHealthCallback~=nil)
hold=false
`); err != nil {
				t.Fatal(err)
			}
			before := forceRuntimeMaintenance(t, path)
			if legacyStatus {
				// Exercise late callbacks even when an older module interface does
				// not report health writes or understand the handover argument.
				if err := control.state.DoString(`
local status=DSTAdmin.Telemetry.Status
DSTAdmin.Telemetry.Status=function() local value=status();value.writing=false;return value end
local stop=DSTAdmin.Stop
DSTAdmin.Stop=function() return stop() end
`); err != nil {
					t.Fatal(err)
				}
			}
			value, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control)
			if !legacyStatus {
				if !errors.Is(err, ErrRuntimeRefreshDeferred) || !value.Deferred {
					t.Fatalf("in-flight health write did not defer replacement: %+v %v", value, err)
				}
				if data, readErr := os.ReadFile(filepath.Join(path, customCommandsName)); readErr != nil || string(data) != string(before) {
					t.Fatalf("deferred update changed user scripts: %q %v", data, readErr)
				}
			} else if err != nil || !value.Active {
				t.Fatalf("replacement failed: %+v %v", value, err)
			}
			if err := control.state.DoString(`heldHealthCallback()`); err != nil {
				t.Fatal(err)
			}
			if !legacyStatus {
				value, err = EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control)
				if err != nil || !value.Active {
					t.Fatalf("maintenance did not resume after health drained: %+v %v", value, err)
				}
			}
			health, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID)
			if err != nil || !health.Running || health.ProducerInstanceID == previous.ProducerInstanceID {
				t.Fatalf("old health callback replaced the active instance: %+v %v", health, err)
			}
		})
	}
}

func TestMaintenanceBlocksProcessConflictsWithoutWritingFiles(t *testing.T) {
	for _, status := range []shards.RuntimeStatus{
		{State: shards.RuntimeFailed, Code: "UNMANAGED_DST_PROCESS_CONFLICT", SessionExists: true},
		{State: shards.RuntimeFailed, Code: "DUPLICATE_DST_PROCESS_CONFLICT", SessionExists: true},
		{State: shards.RuntimeFailed, Code: "LEGACY_TMUX_SOCKET_CONFLICT", SessionExists: true},
		{State: shards.RuntimeFailed, Code: "UNMANAGED_DST_PROCESS_CONFLICT"},
		{State: shards.RuntimeStopped, SessionExists: true},
	} {
		t.Run(string(status.State)+"/"+status.Code, func(t *testing.T) {
			manager, catalog, root := newRuntimeTestManager(t)
			if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "Cluster_1", "Master")
			before := forceRuntimeMaintenance(t, path)
			control := newMaintenanceLuaControl(t, path)
			control.status = status
			value, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
			if !errors.Is(err, ErrRuntimeProcessConflict) || value.Changed || value.Deferred || len(control.sent) != 0 {
				t.Fatalf("process conflict allowed maintenance: %+v %v", value, err)
			}
			if after, err := os.ReadFile(filepath.Join(path, customCommandsName)); err != nil || string(before) != string(after) {
				t.Fatalf("process conflict changed scripts: %q %v", after, err)
			}
		})
	}
}

func TestMaintenanceUpdatesFilesAfterFailedProcessExited(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Cluster_1", "Master")
	forceRuntimeMaintenance(t, path)
	control := newMaintenanceLuaControl(t, path)
	control.status = shards.RuntimeStatus{State: shards.RuntimeFailed, Code: "GAME_PROCESS_EXITED"}
	value, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
	if err != nil || !value.Changed || value.Running || value.Active || len(control.sent) != 0 {
		t.Fatalf("exited failed process blocked file maintenance: %+v %v", value, err)
	}
}

type gatedMaintenanceControl struct {
	MaintenanceControl
	entered chan struct{}
	proceed chan struct{}
}

func (c *gatedMaintenanceControl) Status(ctx context.Context, cluster, shard string) (shards.RuntimeStatus, error) {
	close(c.entered)
	select {
	case <-c.proceed:
		return c.MaintenanceControl.Status(ctx, cluster, shard)
	case <-ctx.Done():
		return shards.RuntimeStatus{}, ctx.Err()
	}
}

func TestConcurrentUninstallRemainsDisabledAfterMaintenance(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Cluster_1", "Master")
	forceRuntimeMaintenance(t, path)
	control := newMaintenanceLuaControl(t, path)
	control.status = shards.RuntimeStatus{State: shards.RuntimeStopped}
	gate := &gatedMaintenanceControl{MaintenanceControl: control, entered: make(chan struct{}), proceed: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	updated, uninstalled := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, gate)
		updated <- err
	}()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		_, err := manager.UninstallWorld(ctx, catalog.room.ID, catalog.worlds[0].ID)
		uninstalled <- err
	}()
	select {
	case err := <-uninstalled:
		t.Fatalf("uninstall bypassed maintenance transaction: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(gate.proceed)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-uninstalled; err != nil {
		t.Fatal(err)
	}
	if state := manager.inspect(catalog.room, catalog.worlds[0]); state.State != InstallStateMissing {
		t.Fatalf("maintenance re-enabled an uninstalled Runtime: %+v", state)
	}
}

func TestMaintenanceDefersBusyRoomAndRechecksOptInOnRetry(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locked, release, err := roomops.Acquire(ctx, catalog.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	type outcome struct {
		value shared.RuntimeLuaResult
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
		done <- outcome{value, err}
	}()
	select {
	case result := <-done:
		if !errors.Is(result.err, ErrRuntimeRefreshDeferred) || !result.value.Deferred || result.value.Changed || len(control.sent) != 0 || ctx.Err() != nil {
			t.Fatalf("busy room did not defer maintenance: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("busy room blocked maintenance until its deadline")
	}
	if _, err := manager.UninstallWorld(locked, catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	release()
	value, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
	if err != nil || value.State != string(InstallStateMissing) || value.Changed || len(control.sent) != 0 {
		t.Fatalf("maintenance re-enabled Runtime on retry: %+v %v", value, err)
	}
}
