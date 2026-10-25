package dstruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/shards"
	"dont/shared"
	lua "github.com/yuin/gopher-lua"
)

type maintenanceLuaControl struct {
	state      *lua.LState
	status     shards.RuntimeStatus
	sent       []string
	failWrites bool
	abortErr   error
	delayLoads bool
	loads      []func()
	beforeSend func(string)
	afterSend  func(string)
}

func (c *maintenanceLuaControl) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	return c.status, nil
}
func (c *maintenanceLuaControl) Send(_ context.Context, _, _ string, script string) error {
	if len(script) > shared.MaximumRuntimeConsoleCommandBytes {
		return errors.New("oversized console command")
	}
	c.sent = append(c.sent, script)
	if c.abortErr != nil && strings.Contains(script, "u.aborted=true") {
		return c.abortErr
	}
	if c.beforeSend != nil {
		c.beforeSend(script)
	}
	err := c.state.DoString(script)
	if err == nil && c.afterSend != nil {
		c.afterSend(script)
	}
	return err
}

func newMaintenanceLuaControl(t *testing.T, worldPath string) *maintenanceLuaControl {
	t.Helper()
	c := &maintenanceLuaControl{state: lua.NewState(), status: shards.RuntimeStatus{State: shards.RuntimeRunning}}
	t.Cleanup(c.state.Close)
	installStaticSchedulerStub(c.state)
	preloadGameJSONHarness(c.state)
	if err := c.state.DoString(`
TheNet={GetSessionIdentifier=function() return "SESSION" end, GetClientTable=function() return {{userid="KU_TEST",name="Player",prefab="wilson"}} end}
TheShard={GetShardId=function() return "1" end}
local task=function() return {Cancel=function() end} end
TheWorld={ismastersim=true,state={cycles=1,season="autumn",phase="day"},components={},DoPeriodicTask=task,DoTaskInTime=task,ListenForEvent=function() end,WatchWorldState=function() end}
ShardGameIndex={SaveCurrent=function() end}
GetTime=function() return 1 end
scheduler={ExecuteInTime=task}
`); err != nil {
		t.Fatal(err)
	}
	sim := c.state.NewTable()
	storagePath := func(name string) string {
		if !strings.HasPrefix(name, "mod_config_data/dst-admin/") || strings.Contains(name, "..") || strings.Contains(name, `\`) {
			t.Fatalf("unsafe storage path %q", name)
		}
		return filepath.Join(worldPath, "save", filepath.FromSlash(name))
	}
	c.state.SetField(sim, "GetPersistentString", c.state.NewFunction(func(L *lua.LState) int {
		path, callback := storagePath(L.CheckString(2)), L.CheckFunction(3)
		load := func() {
			data, err := os.ReadFile(path)
			callLuaFunction(t, c.state, callback, lua.LBool(err == nil), lua.LString(data))
		}
		if c.delayLoads {
			c.loads = append(c.loads, load)
		} else {
			load()
		}
		return 0
	}))
	c.state.SetField(sim, "SetPersistentString", c.state.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(2)
		var err error
		if c.failWrites {
			err = errors.New("storage unavailable")
		} else {
			err = os.WriteFile(storagePath(name), []byte(L.CheckString(3)), 0640)
		}
		if callback, ok := L.Get(5).(*lua.LFunction); ok {
			callLuaFunction(t, c.state, callback, lua.LBool(err == nil))
		}
		return 0
	}))
	c.state.SetGlobal("TheSim", sim)
	if err := os.MkdirAll(filepath.Join(worldPath, "save/session/SESSION"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worldPath, "server.ini"), []byte("[SHARD]\nid = 1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	log := "[00:00:00]: Current time: " + time.Now().Add(-time.Minute).In(time.Local).Format(time.ANSIC) + "\n"
	if err := os.WriteFile(filepath.Join(worldPath, "server_log.txt"), []byte(log), 0640); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNodeMaintenanceRejectsRecentHealthFromPreviousProcess(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%t", busy), func(t *testing.T) {
			manager, catalog, root := newRuntimeTestManager(t)
			path := filepath.Join(root, "Cluster_1", "Master")
			control := newMaintenanceLuaControl(t, path)
			request := shared.RuntimeLuaRequest{Version: RuntimeVersion, InstallIfMissing: true}
			if _, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control); err != nil {
				t.Fatal(err)
			}
			instance, count := control.state.GetGlobal("DSTAdmin"), len(control.sent)
			health, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			// The old sample is younger than the freshness window, but predates
			// the newly started process using the same persistent save session.
			old := time.Now().Add(-10 * time.Second)
			stamp := old.Unix()
			health.LastCapturedAtUnix, health.LastWrittenAtUnix = &stamp, &stamp
			module := health.Modules["barriers"]
			module.Holding = busy
			health.Modules["barriers"] = module
			data, err := json.Marshal(health)
			if err != nil {
				t.Fatal(err)
			}
			healthPath := filepath.Join(path, "save/mod_config_data/dst-admin/health-"+RuntimeVersion+".json")
			if err := os.WriteFile(healthPath, data, 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(healthPath, old, old); err != nil {
				t.Fatal(err)
			}
			log := "[00:00:00]: Current time: " + time.Now().Add(-time.Second).In(time.Local).Format(time.ANSIC) + "\n"
			if err := os.WriteFile(filepath.Join(path, "server_log.txt"), []byte(log), 0640); err != nil {
				t.Fatal(err)
			}
			value, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control)
			if err != nil || !value.Active || value.Deferred || len(control.sent) <= count || control.state.GetGlobal("DSTAdmin") == instance {
				t.Fatalf("previous process health accepted: %+v err=%v", value, err)
			}
		})
	}
}

func TestNodeMaintenanceActivatesAndDoesNotReloadCurrentRuntime(t *testing.T) {
	_, _, root := newRuntimeTestManager(t)
	path := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, path)
	user := []byte("-- custom user code\nuser_setting=true\n")
	if err := os.WriteFile(filepath.Join(path, customCommandsName), user, 0600); err != nil {
		t.Fatal(err)
	}
	request := shared.RuntimeLuaRequest{Version: RuntimeVersion, InstallIfMissing: true}
	value, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control)
	if err != nil || !value.Changed || !value.Active {
		t.Fatalf("value=%+v err=%v", value, err)
	}
	instance := control.state.GetGlobal("DSTAdmin")
	if control.state.GetGlobal("DSTAdminPendingUpdate") != lua.LNil {
		t.Fatal("committed update retained rollback state")
	}
	count := len(control.sent)
	value, err = EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control)
	if err != nil || value.Changed || !value.Active || len(control.sent) != count || control.state.GetGlobal("DSTAdmin") != instance {
		t.Fatalf("current runtime reloaded: %+v %v", value, err)
	}
	data, err := os.ReadFile(filepath.Join(path, customCommandsName))
	if err != nil || !strings.HasPrefix(string(data), string(user)) {
		t.Fatalf("user code changed: %q %v", data, err)
	}
}

func TestNodeMaintenanceKeepsPausedRuntimeButRejectsPreviousBootHealth(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	path := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, path)
	request := shared.RuntimeLuaRequest{Version: RuntimeVersion, InstallIfMissing: true}
	if _, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control); err != nil {
		t.Fatal(err)
	}
	instance, count := control.state.GetGlobal("DSTAdmin"), len(control.sent)
	paused := true
	control.status.Paused = &paused
	health, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	stamp := old.Unix()
	health.LastCapturedAtUnix, health.LastWrittenAtUnix = &stamp, &stamp
	// Published collectors without an independent heartbeat retain the legacy
	// paused/current-boot exception during upgrade.
	health.HeartbeatAtUnix, health.HeartbeatSequence = nil, 0
	data, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	healthPath := filepath.Join(path, "save/mod_config_data/dst-admin/health-"+RuntimeVersion+".json")
	if err := os.WriteFile(healthPath, data, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(healthPath, old, old); err != nil {
		t.Fatal(err)
	}
	writeBoot := func(at time.Time) {
		t.Helper()
		data := "[00:00:00]: Current time: " + at.In(time.Local).Format(time.ANSIC) + "\n"
		if err := os.WriteFile(filepath.Join(path, "server_log.txt"), []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	writeBoot(old.Add(-time.Hour))
	value, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control)
	if err != nil || !value.Active || len(control.sent) != count || control.state.GetGlobal("DSTAdmin") != instance {
		t.Fatalf("paused healthy runtime replaced: %+v err=%v", value, err)
	}
	writeBoot(time.Now().Add(-time.Minute))
	value, err = EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control)
	if err != nil || !value.Active || len(control.sent) <= count || control.state.GetGlobal("DSTAdmin") == instance {
		t.Fatalf("previous process health accepted: %+v err=%v", value, err)
	}
}

func TestNodeMaintenanceDefersDuringBarrierWithoutReplacingRuntime(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	path := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, path)
	request := shared.RuntimeLuaRequest{Version: RuntimeVersion, InstallIfMissing: true}
	if _, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control); err != nil {
		t.Fatal(err)
	}
	count := len(control.sent)
	health, err := manager.Health(catalog.room.ID, catalog.worlds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	module := health.Modules["barriers"]
	module.Holding = true
	health.Modules["barriers"] = module
	data, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "save/mod_config_data/dst-admin/health-"+RuntimeVersion+".json"), data, 0640); err != nil {
		t.Fatal(err)
	}
	value, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control)
	if !errors.Is(err, ErrRuntimeRefreshDeferred) || !value.Deferred || len(control.sent) != count {
		t.Fatalf("busy Runtime replaced: %+v err=%v", value, err)
	}
}

func TestNodeMaintenanceChecksLiveOperationsAfterPersistedHealth(t *testing.T) {
	for _, module := range []string{"Commands", "Barriers", "Diagnostics", "Telemetry", "WorldState", "Events"} {
		t.Run(module, func(t *testing.T) {
			_, _, root := newRuntimeTestManager(t)
			path := filepath.Join(root, "Cluster_1", "Master")
			control := newMaintenanceLuaControl(t, path)
			request := shared.RuntimeLuaRequest{Version: RuntimeVersion, InstallIfMissing: true}
			if _, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control); err != nil {
				t.Fatal(err)
			}
			previous := control.state.GetGlobal("DSTAdmin")
			before := []byte("-- DST-ADMIN MANAGED BLOCK BEGIN protocol=2 version=2.4.10\n-- original loader\n-- DST-ADMIN MANAGED BLOCK END\n-- preserve user code\n")
			if err := os.WriteFile(filepath.Join(path, customCommandsName), before, 0600); err != nil {
				t.Fatal(err)
			}
			// Begin work after the node has already inspected the healthy on-disk
			// snapshot. The switch must inspect the live module before stopping it.
			control.beforeSend = func(script string) {
				if !strings.Contains(script, "local __update=") {
					return
				}
				control.beforeSend = nil
				if err := control.state.DoString(`busyModule=DSTAdmin.` + module + `; originalStatus=busyModule.Status; busyModule.Status=function() return {running=true,ready=true,busy=true,writing=true,holding=true,pending=1} end`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			value, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", request, control)
			cancel()
			if !errors.Is(err, ErrRuntimeRefreshDeferred) || !value.Deferred || control.state.GetGlobal("DSTAdmin") != previous || control.state.GetField(previous, "state") != lua.LString("running") {
				t.Fatalf("live %s operation was interrupted: %+v err=%v", module, value, err)
			}
			if data, err := os.ReadFile(filepath.Join(path, customCommandsName)); err != nil || string(data) != string(before) {
				t.Fatalf("deferred update changed the original scripts: %q %v", data, err)
			}
			if control.state.GetGlobal("DSTAdminPendingUpdate") != lua.LNil {
				t.Fatal("deferred update retained a pending activation")
			}
			if err := control.state.DoString(`busyModule.Status=originalStatus`); err != nil {
				t.Fatal(err)
			}
			value, err = EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control)
			if err != nil || !value.Active || control.state.GetGlobal("DSTAdmin") == previous {
				t.Fatalf("update did not resume after the operation completed: %+v err=%v", value, err)
			}
		})
	}
}

func TestNodeMaintenanceDoesNotDeferWhenAbortFails(t *testing.T) {
	_, _, root := newRuntimeTestManager(t)
	path := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, path)
	request := shared.RuntimeLuaRequest{Version: RuntimeVersion, InstallIfMissing: true}
	if _, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", request, control); err != nil {
		t.Fatal(err)
	}
	previous := control.state.GetGlobal("DSTAdmin")
	before := []byte("-- DST-ADMIN MANAGED BLOCK BEGIN protocol=2 version=2.4.10\n-- original loader\n-- DST-ADMIN MANAGED BLOCK END\n-- preserve user code\n")
	if err := os.WriteFile(filepath.Join(path, customCommandsName), before, 0600); err != nil {
		t.Fatal(err)
	}
	control.beforeSend = func(script string) {
		if strings.Contains(script, "local __update=") {
			if err := control.state.DoString(`DSTAdmin.Barriers.Status=function() return {holding=true} end`); err != nil {
				t.Fatal(err)
			}
		}
	}
	abortErr := errors.New("console disconnected during abort")
	control.abortErr = abortErr
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	value, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", request, control)
	if value.Deferred || !errors.Is(err, ErrRuntimeActivation) || errors.Is(err, ErrRuntimeRefreshDeferred) || !errors.Is(err, abortErr) {
		t.Fatalf("recovery failure was classified as a normal deferral: %+v err=%v", value, err)
	}
	if data, err := os.ReadFile(filepath.Join(path, customCommandsName)); err != nil || string(data) != string(before) {
		t.Fatalf("failed abort prevented restoring the original scripts: %q %v", data, err)
	}
	if control.state.GetGlobal("DSTAdmin") != previous || control.state.GetField(previous, "state") != lua.LString("running") {
		t.Fatal("deferred update stopped the busy instance")
	}
}

func TestNodeMaintenanceWaitsForCoherentSnapshots(t *testing.T) {
	_, _, root := newRuntimeTestManager(t)
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, worldPath)
	var restoreDone chan error
	control.afterSend = func(script string) {
		if !strings.Contains(script, "local __update=") {
			return
		}
		control.afterSend = nil
		path := filepath.Join(worldPath, "save/mod_config_data/dst-admin/worldstate-a.json")
		coherent, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0640); err != nil {
			t.Fatal(err)
		}
		restoreDone = make(chan error, 1)
		time.AfterFunc(50*time.Millisecond, func() { restoreDone <- os.WriteFile(path, coherent, 0640) })
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	value, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion, InstallIfMissing: true}, control)
	if restoreDone == nil {
		t.Fatal("activation was not sent")
	}
	if restoreErr := <-restoreDone; restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err != nil || !value.Active {
		t.Fatalf("transient snapshot rotation caused a failed activation: %+v err=%v", value, err)
	}
}

func TestNodeMaintenanceFailureRestoresFilesAndCancelsLateBootstrap(t *testing.T) {
	for _, delay := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed persistence", true: "late asynchronous load"}[delay], func(t *testing.T) {
			manager, catalog, root := newRuntimeTestManager(t)
			if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "Cluster_1", "Master")
			before := []byte("-- DST-ADMIN MANAGED BLOCK BEGIN protocol=2 version=2.4.10\n-- original loader\n-- DST-ADMIN MANAGED BLOCK END\n-- keep user code\n")
			if err := os.WriteFile(filepath.Join(path, customCommandsName), before, 0600); err != nil {
				t.Fatal(err)
			}
			control := newMaintenanceLuaControl(t, path)
			if err := control.state.DoString(`oldRuntime={Start=function() oldRunning=true; return true end,Stop=function() oldRunning=false; return true end,Refresh=function() return true end};oldRunning=true;DSTAdmin=oldRuntime`); err != nil {
				t.Fatal(err)
			}
			control.failWrites, control.delayLoads = !delay, delay
			ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
			defer cancel()
			_, err := EnsureNodeWorld(ctx, root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
			if err == nil {
				t.Fatal("failed update reported success")
			}
			control.delayLoads = false
			for len(control.loads) > 0 {
				loads := control.loads
				control.loads = nil
				for _, load := range loads {
					load()
				}
			}
			if control.state.GetGlobal("DSTAdmin") != control.state.GetGlobal("oldRuntime") || control.state.GetGlobal("oldRunning") != lua.LTrue {
				t.Fatal("previous runtime was not restored, or late bootstrap replaced it")
			}
			got, readErr := os.ReadFile(filepath.Join(path, customCommandsName))
			if readErr != nil || string(got) != string(before) {
				t.Fatalf("original loader lost: %q %v", got, readErr)
			}
		})
	}
}

func TestNodeMaintenanceSkipsDisabledWorldAndRejectsModifiedAssets(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	path := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, path)
	value, err := EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
	if err != nil || value.State != "missing" || len(control.sent) != 0 {
		t.Fatalf("missing runtime reinstalled: %+v %v", value, err)
	}
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, executionDirectory, "telemetry.lua")
	if err := os.WriteFile(file, []byte("-- keep edited script\nreturn {}"), 0640); err != nil {
		t.Fatal(err)
	}
	_, err = EnsureNodeWorld(context.Background(), root, "Cluster_1", "Master", shared.RuntimeLuaRequest{Version: RuntimeVersion}, control)
	if !errors.Is(err, ErrManagedFileChanged) || len(control.sent) != 0 {
		t.Fatalf("modified asset overwritten: %v", err)
	}
}

func TestSchemaOneBackupRemainsRestorable(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Cluster_1", "Master")
	id, err := manager.createStateBackup(path, "legacy fixture")
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(path, backupDirectory, id, backupManifestName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.SchemaVersion = 1
	files := manifest.Files[:0]
	for _, file := range manifest.Files {
		if !strings.HasPrefix(file.Path, "save/") {
			files = append(files, file)
		}
	}
	manifest.Files = files
	data, _ = json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, executionDirectory, "bootstrap.lua")); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreStateBackup(path, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, executionDirectory, "bootstrap.lua")); err != nil {
		t.Fatal(err)
	}
}
