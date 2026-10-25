package dstruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/runtimefiles"
	"dont/shared"
	lua "github.com/yuin/gopher-lua"
)

// Exercise the installed loader and real modules with the game's restricted
// PersistentStorage namespace, rather than a mock that accepts arbitrary paths.
func TestInstalledRuntimeLoadsThroughRestrictedPersistentStorage(t *testing.T) {
	for _, loader := range []string{"startup", "activation"} {
		t.Run(loader, func(t *testing.T) {
			manager, catalog, root := newRuntimeTestManager(t)
			world := catalog.worlds[0]
			if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, world.ID); err != nil {
				t.Fatal(err)
			}
			worldPath := filepath.Join(root, "Cluster_1", "Master")
			state := lua.NewState()
			defer state.Close()
			preloadGameJSONHarness(state)
			if err := state.DoString(`
TheNet = {GetSessionIdentifier=function() return "SESSION" end,
 GetClientTable=function() return {{userid="KU_TEST",name="Player",prefab="wilson"}} end}
TheShard = {GetShardId=function() return "1" end}
local task=function() return {Cancel=function() end} end
TheWorld = {ismastersim=true, state={cycles=1,season="autumn",phase="day"}, components={},
 DoPeriodicTask=task, DoTaskInTime=task, ListenForEvent=function() end, WatchWorldState=function() end}
ShardGameIndex={SaveCurrent=function() end}
GetTime=function() return 1 end
scheduler={ExecuteInTime=function() error("ready world should not wait") end}
`); err != nil {
				t.Fatal(err)
			}
			reads := make(map[string]bool)
			outputs := make(map[string][]byte)
			approved := func(path string) bool {
				return strings.HasPrefix(path, "mod_config_data/dst-admin/") && !strings.Contains(path, "..") && !strings.Contains(path, `\`)
			}
			sim := state.NewTable()
			state.SetField(sim, "GetPersistentString", state.NewFunction(func(L *lua.LState) int {
				name := L.CheckString(2)
				if !approved(name) {
					L.RaiseError("Filename [%s] is not allowed by PersistentStorage", name)
					return 0
				}
				reads[name] = true
				data, err := os.ReadFile(filepath.Join(worldPath, "save", filepath.FromSlash(name)))
				callLuaFunction(t, state, L.CheckFunction(3), lua.LBool(err == nil), lua.LString(data))
				return 0
			}))
			state.SetField(sim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
				name := L.CheckString(2)
				if !approved(name) {
					L.RaiseError("write path not allowed: %s", name)
					return 0
				}
				outputs[name] = []byte(L.CheckString(3))
				if callback, ok := L.Get(5).(*lua.LFunction); ok {
					callLuaFunction(t, state, callback, lua.LTrue)
				}
				return 0
			}))
			state.SetField(sim, "ErasePersistentString", state.NewFunction(func(L *lua.LState) int {
				name := L.CheckString(2)
				if !approved(name) {
					L.RaiseError("erase path not allowed: %s", name)
					return 0
				}
				err := os.Remove(filepath.Join(worldPath, "save", filepath.FromSlash(name)))
				if callback, ok := L.Get(3).(*lua.LFunction); ok {
					callLuaFunction(t, state, callback, lua.LBool(err == nil))
				}
				return 0
			}))
			state.SetGlobal("TheSim", sim)
			if err := state.DoString(`test_json = require("json")`); err != nil {
				t.Fatal(err)
			}
			state.SetField(state.GetGlobal("test_json"), "decode", state.NewFunction(func(L *lua.LState) int {
				var request CommandRequest
				if err := json.Unmarshal([]byte(L.CheckString(1)), &request); err != nil {
					L.RaiseError("decode: %v", err)
					return 0
				}
				value := L.NewTable()
				value.RawSetString("requestId", lua.LString(request.RequestID))
				value.RawSetString("action", lua.LString(request.Action))
				arguments := L.NewTable()
				if script, ok := request.Arguments["script"].(string); ok {
					arguments.RawSetString("script", lua.LString(script))
				}
				value.RawSetString("arguments", arguments)
				L.Push(value)
				return 1
			}))
			script := managedActivationScript
			if loader == "startup" {
				data, err := os.ReadFile(filepath.Join(worldPath, customCommandsName))
				if err != nil {
					t.Fatal(err)
				}
				script = string(data)
			}
			if err := state.DoString(script); err != nil {
				t.Fatal(err)
			}
			runtime := requireLuaTable(t, state.GetGlobal("DSTAdmin"), "DSTAdmin")
			if state.GetField(runtime, "state").String() != "running" {
				t.Fatalf("runtime not ready: %v", runtime)
			}
			for _, name := range managedAssetNames {
				if !reads["mod_config_data/dst-admin/runtime/"+name] {
					t.Errorf("module not read: %s", name)
				}
			}
			health, err := decodeHealthData(outputs["mod_config_data/dst-admin/health-"+RuntimeVersion+".json"], "SESSION", "1", time.Now())
			if err != nil || !health.Running || !health.Ready {
				t.Fatalf("health=%+v error=%v", health, err)
			}
			if _, err := decodeSnapshotData(outputs["mod_config_data/dst-admin/players-a.json"], "SESSION", "1", time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := decodeWorldStateData(outputs["mod_config_data/dst-admin/worldstate-a.json"], "SESSION", "1", time.Now(), true); err != nil {
				t.Fatal(err)
			}

			// Real document publisher and Lua consumer must agree on the same namespace.
			request := CommandRequest{RequestID: "storage-request-1234", Action: "console.execute", Arguments: map[string]interface{}{"script": "return true --" + strings.Repeat("x", 1800)}}
			payload, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			trigger, document := commandDelivery(request, payload)
			if document == nil {
				t.Fatal("expected document delivery")
			}
			if err := runtimefiles.PublishCommandDocument(context.Background(), root, "Cluster_1", "Master", *document); err != nil {
				t.Fatal(err)
			}
			if err := state.DoString(trigger); err != nil {
				t.Fatal(err)
			}
			var receipt CommandReceipt
			if err := json.Unmarshal(outputs["mod_config_data/dst-admin/command-receipt-a.json"], &receipt); err != nil || !receipt.OK || receipt.RequestID != request.RequestID {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			if _, err := os.Stat(filepath.Join(worldPath, "save/mod_config_data/dst-admin/command-requests", request.RequestID+".json")); !os.IsNotExist(err) {
				t.Fatalf("request not consumed: %v", err)
			}
			callLuaMethod(t, state, runtime, "Stop", true)
		})
	}
}

func TestMissingHealthNeverSendsAutomaticRefresh(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	world := catalog.worlds[0]
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, world.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(filepath.Join(path, "save/session/SESSION"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "server.ini"), []byte("[SHARD]\nid=1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	sender := &bridgeSender{}
	bridge, _ := NewBridge(manager, bridgeProcess{running: true}, sender)
	if _, err := bridge.RefreshSnapshots(context.Background(), catalog.room.ID, world.ID); err == nil || len(sender.scripts) != 0 {
		t.Fatalf("error=%v scripts=%v", err, sender.scripts)
	}
	distributed, fixture, roomID, worldID, _ := newDistributedBridgeFixture(t)
	delete(fixture.bundles, shared.ArtifactRuntimeHealth)
	if _, err := distributed.RefreshSnapshots(context.Background(), roomID, worldID); err == nil || len(fixture.sent) != 0 {
		t.Fatalf("error=%v sends=%v", err, fixture.sent)
	}
}

func TestExecutionRepairAndRollbackPreserveSavesAndUnknownFiles(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	world := catalog.worlds[0]
	first, err := manager.InstallWorld(context.Background(), catalog.room.ID, world.ID)
	if err != nil {
		t.Fatal(err)
	}
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	unknown := filepath.Join(worldPath, executionDirectory, "user.lua")
	save := filepath.Join(worldPath, "save", "world.save")
	for _, path := range []string{unknown, save} {
		if err := os.WriteFile(path, []byte("keep"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(worldPath, executionDirectory, "bootstrap.lua")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if statuses, _ := manager.StatusRoom(catalog.room.ID); statuses[0].State != InstallStateOutdated {
		t.Fatalf("status=%+v", statuses[0])
	}
	repair, err := manager.InstallWorld(context.Background(), catalog.room.ID, world.ID)
	if err != nil || !repair.Changed {
		t.Fatalf("repair=%+v err=%v", repair, err)
	}
	// A schema-2 backup preserves the missing execution script exactly.
	backupID := filepath.Base(repair.BackupPath)
	if _, err := manager.RollbackWorld(context.Background(), catalog.room.ID, world.ID, backupID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing script was not restored exactly: %v", err)
	}

	if _, err := manager.UninstallWorld(context.Background(), catalog.room.ID, world.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{unknown, save} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "keep" {
			t.Fatalf("modified %s: %q %v", path, data, err)
		}
	}
	if _, err := manager.RollbackWorld(context.Background(), catalog.room.ID, world.ID, filepath.Base(first.BackupPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("rollback to preinstall retained script: %v", err)
	}
}

func TestExecutionAssetsRejectSymlinksAndPreserveModifiedScripts(t *testing.T) {
	for _, scenario := range []string{"symlink", "modified"} {
		t.Run(scenario, func(t *testing.T) {
			manager, catalog, root := newRuntimeTestManager(t)
			world := catalog.worlds[0]
			if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, world.ID); err != nil {
				t.Fatal(err)
			}
			worldPath := filepath.Join(root, "Cluster_1", "Master")
			execution := filepath.Join(worldPath, executionDirectory)
			protected := filepath.Join(execution, "bootstrap.lua")
			if scenario == "symlink" {
				if err := os.RemoveAll(execution); err != nil {
					t.Fatal(err)
				}
				escaped := t.TempDir()
				if err := os.WriteFile(filepath.Join(escaped, "bootstrap.lua"), []byte("-- keep\n"), 0640); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(escaped, execution); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(protected, []byte("-- keep\n"), 0640); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, world.ID); err == nil {
				t.Fatal("unsafe/modified execution file overwritten")
			}
			if content, err := os.ReadFile(protected); err != nil || string(content) != "-- keep\n" {
				t.Fatalf("file changed: %q %v", content, err)
			}
		})
	}
}
