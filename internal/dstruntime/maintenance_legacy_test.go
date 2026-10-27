package dstruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/shared"
)

func installPublishedRuntimeFixture(t *testing.T, manager *Manager, catalog runtimeTestCatalog, worldPath string) {
	t.Helper()
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{managedDirectory, "save/mod_config_data/dst-admin/runtime"} {
		if err := os.Remove(filepath.Join(worldPath, directory, "activity.lua")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	installed := manifest{Version: "2.4.11", ProtocolVersion: ProtocolVersion, Assets: make(map[string]string), InstalledAt: time.Now().UTC()}
	for _, name := range []string{"bootstrap.lua", "telemetry.lua", "worldstate.lua", "commands.lua", "events.lua", "diagnostics.lua", "barriers.lua"} {
		value, err := os.ReadFile(filepath.Join("testdata/runtime-2.4.11", name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(value)
		installed.Assets[name] = hex.EncodeToString(sum[:])
		for _, directory := range []string{managedDirectory, "save/mod_config_data/dst-admin/runtime"} {
			if err := os.WriteFile(filepath.Join(worldPath, directory, name), value, 0640); err != nil {
				t.Fatal(err)
			}
		}
	}
	data, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worldPath, managedDirectory, manifestName), data, 0640); err != nil {
		t.Fatal(err)
	}
	loader := strings.ReplaceAll(managedLoader, RuntimeVersion, installed.Version) + "\n-- preserve user code\n"
	if err := os.WriteFile(filepath.Join(worldPath, customCommandsName), []byte(loader), 0640); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedRuntimeUpgradeCannotPublishRetiredHealth(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	path := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, path)
	installPublishedRuntimeFixture(t, manager, catalog, path)
	if err := control.state.DoString(managedActivationScript); err != nil {
		t.Fatal(err)
	}
	if err := control.state.DoString(`
assert(DSTAdmin.version=="2.4.11")
local write=TheSim.SetPersistentString
local hold=true
TheSim.SetPersistentString=function(self,name,value,encode,callback)
    if hold and name=="mod_config_data/dst-admin/health.json" then
        return write(self,name,value,encode,function(written)
            oldHealthCompletion=function() if callback then callback(written) end end
        end)
    end
    return write(self,name,value,encode,callback)
end
assert(DSTAdmin.Refresh())
assert(oldHealthCompletion~=nil)
hold=false
`); err != nil {
		t.Fatal(err)
	}
	result, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
	if err != nil || !result.Active || !result.Changed {
		t.Fatalf("upgrade failed before delayed callback: %+v %v", result, err)
	}
	if err := control.state.DoString(`oldHealthCompletion()`); err != nil {
		t.Fatal(err)
	}
	health, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || !health.Running || health.ProducerVersion != RuntimeVersion {
		t.Fatalf("published Runtime callback overwrote successful upgrade: producer=%s running=%v err=%v", health.ProducerVersion, health.Running, err)
	}
	// The published collector actually drained its stopped health after the
	// upgrade. The current result must not merely depend on callback ordering.
	data, err := os.ReadFile(filepath.Join(path, "save/mod_config_data/dst-admin/health.json"))
	if err != nil {
		t.Fatal(err)
	}
	retired, err := decodeHealthData(data, "SESSION", "1", time.Now())
	if err != nil || retired.Running || retired.ProducerVersion != "2.4.11" {
		t.Fatalf("legacy late callback was not exercised: %+v %v", retired, err)
	}
}

func TestFailedPublishedRuntimeUpgradeRestoresLegacyHealthSelection(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	path := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, path)
	installPublishedRuntimeFixture(t, manager, catalog, path)
	if err := control.state.DoString(managedActivationScript); err != nil {
		t.Fatal(err)
	}
	previous := control.state.GetGlobal("DSTAdmin")
	// Fail after the candidate has produced new health. The old installed
	// manifest must select old health again, despite the candidate's file.
	control.afterSend = func(script string) {
		if strings.Contains(script, "local __update=") {
			// Publish the injected failure. Stopping the module alone leaves the
			// candidate's initial healthy snapshot on disk, so the check could
			// legitimately accept it before the next heartbeat.
			if err := control.state.DoString(`DSTAdmin.WorldState.Stop(); DSTAdmin.Telemetry.Heartbeat()`); err != nil {
				t.Fatal(err)
			}
			health, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID)
			if err != nil || health.ProducerVersion != RuntimeVersion || health.Modules["worldstate"].Running {
				t.Fatalf("candidate failure was not published: %+v %v", health, err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	result, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
	if !errors.Is(err, context.DeadlineExceeded) || result.Active || result.Version != "2.4.11" || control.state.GetGlobal("DSTAdmin") != previous {
		t.Fatalf("failed update did not recover published Runtime: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(path, "save/mod_config_data/dst-admin/health-"+RuntimeVersion+".json")); err != nil {
		t.Fatalf("candidate never wrote isolated health: %v", err)
	}
	health, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID)
	if err != nil || !health.Running || health.ProducerVersion != "2.4.11" {
		t.Fatalf("restored manifest selected candidate health: %+v %v", health, err)
	}
}
