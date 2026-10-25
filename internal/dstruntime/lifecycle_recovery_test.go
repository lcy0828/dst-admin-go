package dstruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/runtimefiles"
	"dont/shared"
	lua "github.com/yuin/gopher-lua"
)

func TestReloadRestoresPreviousWhenInitialRefreshFails(t *testing.T) {
	for _, failure := range []string{"return false", `error("snapshot write failed")`} {
		t.Run(failure, func(t *testing.T) {
			state := lua.NewState()
			installStaticSchedulerStub(state)
			defer state.Close()
			lifecycle := `local running=false; return {Start=function() running=true; return true end, Stop=function() running=false; return true end, Status=function() return {running=running} end}`
			worldState := `local running=false; return {Start=function() running=true; return true end, Stop=function() running=false; return true end, Status=function() return {running=running} end, EmitOnce=function(callback) if callback then callback(true) end; return true end}`
			sim := state.NewTable()
			state.SetField(sim, "GetPersistentString", state.NewFunction(func(L *lua.LState) int {
				source := lifecycle
				switch L.CheckString(2) {
				case "mod_config_data/dst-admin/runtime/telemetry.lua":
					source = runtimeTelemetryHarnessSource(false)
				case "mod_config_data/dst-admin/runtime/worldstate.lua":
					source = worldState
				case "mod_config_data/dst-admin/runtime/commands.lua":
					source = "return { Execute=function() end }"
				}
				source = fmt.Sprintf("local module=(function() %s end)(); module.version=%q; return module", source, RuntimeVersion)
				if err := L.CallByParam(lua.P{Fn: L.CheckFunction(3), NRet: 0, Protect: true}, lua.LTrue, lua.LString(source)); err != nil {
					L.RaiseError("load module: %v", err)
				}
				return 0
			}))
			state.SetGlobal("TheSim", sim)
			bootstrap, err := assetData("bootstrap.lua")
			if err != nil {
				t.Fatal(err)
			}
			if err := state.DoString(string(bootstrap)); err != nil {
				t.Fatal(err)
			}
			previous := requireLuaTable(t, state.GetGlobal("DSTAdmin"), "previous runtime")
			worldState = `local running=false; return {Start=function() running=true; return true end, Stop=function() running=false; return true end, Status=function() return {running=running} end, EmitOnce=function() ` + failure + ` end}`
			callLuaMethod(t, state, previous, "Reload", true)
			if state.GetGlobal("DSTAdmin") != previous || state.GetField(previous, "state").String() != "running" {
				t.Fatalf("failed initial refresh replaced previous runtime: activeChanged=%v previousState=%s", state.GetGlobal("DSTAdmin") != previous, state.GetField(previous, "state"))
			}
		})
	}
}

func TestMissingExecutionScriptsMustNotBeReportedAsWaiting(t *testing.T) {
	manager, catalog, root := newRuntimeTestManager(t)
	if _, err := manager.InstallWorld(context.Background(), catalog.room.ID, catalog.worlds[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "Cluster_1", "Master", executionDirectory, "bootstrap.lua")); err != nil {
		t.Fatal(err)
	}
	value, err := runtimefiles.ReadWorldState(context.Background(), root, "Cluster_1", "Master", shared.ShardRuntimeStatus{State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if value.ReadError == "" {
		t.Fatal("missing execution bootstrap is indistinguishable from awaiting first sample")
	}
}

func TestLateActivationFailureCannotReplaceNewerRuntime(t *testing.T) {
	state := lua.NewState()
	installStaticSchedulerStub(state)
	defer state.Close()
	if err := state.DoString(`oldRuntime={Start=function() return true end,Stop=function() return true end};DSTAdmin=oldRuntime`); err != nil {
		t.Fatal(err)
	}
	sim := state.NewTable()
	state.SetField(sim, "GetPersistentString", state.NewFunction(func(L *lua.LState) int {
		source := `local running=false; return {Start=function() running=true; return true end, Stop=function() running=false; return true end, Status=function() return {running=running} end}`
		switch L.CheckString(2) {
		case "mod_config_data/dst-admin/runtime/telemetry.lua":
			source = runtimeTelemetryHarnessSource(false)
		case "mod_config_data/dst-admin/runtime/worldstate.lua":
			source = `local running=false; return {Start=function() running=true; return true end, Stop=function() running=false; return true end, Status=function() return {running=running} end, EmitOnce=function(callback) delayed_refresh=callback; return true end}`
		case "mod_config_data/dst-admin/runtime/commands.lua":
			source = "return { Execute=function() end }"
		}
		source = fmt.Sprintf("local module=(function() %s end)(); module.version=%q; return module", source, RuntimeVersion)
		callLuaFunction(t, state, L.CheckFunction(3), lua.LTrue, lua.LString(source))
		return 0
	}))
	state.SetGlobal("TheSim", sim)
	bootstrap, err := assetData("bootstrap.lua")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.DoString(string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if state.GetGlobal("delayed_refresh") == lua.LNil {
		t.Fatal("no pending initial snapshot")
	}
	if err := state.DoString(`DSTAdmin.Stop(); newerRuntime={state="running"}; DSTAdmin=newerRuntime; delayed_refresh(false)`); err != nil {
		t.Fatal(err)
	}
	if state.GetGlobal("DSTAdmin") != state.GetGlobal("newerRuntime") {
		t.Fatal("late failed activation replaced the newer runtime")
	}
}

func TestLateReloadCannotReplaceNewerRuntime(t *testing.T) {
	for _, test := range []struct {
		name, action string
	}{
		{"newer activation", `DSTAdmin.Stop(); DSTAdmin={state="running"}`},
		{"explicit stop", `DSTAdmin.Stop()`},
		{"automatic update started", `DSTAdminPendingUpdate={id="new-update"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, root := newRuntimeTestManager(t)
			worldPath := filepath.Join(root, "Cluster_1", "Master")
			control := newMaintenanceLuaControl(t, worldPath)
			assets, _, err := desiredAssets()
			if err != nil {
				t.Fatal(err)
			}
			if err := writeExecutionAssets(worldPath, assets); err != nil {
				t.Fatal(err)
			}
			if err := control.state.DoString(string(assets["bootstrap.lua"])); err != nil {
				t.Fatal(err)
			}
			previous := requireLuaTable(t, control.state.GetGlobal("DSTAdmin"), "previous runtime")
			control.delayLoads = true
			callLuaMethod(t, control.state, previous, "Reload", true)
			if len(control.loads) == 0 {
				t.Fatal("reload did not start an asynchronous module read")
			}
			// Another lifecycle action wins while the previous instance's module
			// reads are pending. Their late success must not publish a Runtime.
			if err := control.state.DoString(test.action); err != nil {
				t.Fatal(err)
			}
			current := control.state.GetGlobal("DSTAdmin")
			currentState := control.state.GetField(current, "state")
			control.delayLoads = false
			for _, load := range control.loads {
				load()
			}
			if control.state.GetGlobal("DSTAdmin") != current || control.state.GetField(current, "state") != currentState {
				t.Fatal("late successful reload overrode the newer lifecycle action")
			}
			if control.state.GetField(previous, "reloadInProgress") != lua.LFalse {
				t.Fatal("abandoned reload kept the previous instance busy")
			}
		})
	}
}

func TestLateStartupBootstrapCannotReplaceNewerActivation(t *testing.T) {
	_, _, root := newRuntimeTestManager(t)
	worldPath := filepath.Join(root, "Cluster_1", "Master")
	control := newMaintenanceLuaControl(t, worldPath)
	assets, _, err := desiredAssets()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeExecutionAssets(worldPath, assets); err != nil {
		t.Fatal(err)
	}
	control.delayLoads = true
	if err := control.state.DoString(string(assets["bootstrap.lua"])); err != nil {
		t.Fatal(err)
	}
	if len(control.loads) == 0 {
		t.Fatal("startup bootstrap did not start asynchronous module reads")
	}
	control.delayLoads = false
	if err := control.state.DoString(string(assets["bootstrap.lua"])); err != nil {
		t.Fatal(err)
	}
	current := requireLuaTable(t, control.state.GetGlobal("DSTAdmin"), "new activation")
	for _, load := range control.loads {
		load()
	}
	if control.state.GetGlobal("DSTAdmin") != current || control.state.GetField(current, "state") != lua.LString("running") {
		t.Fatal("late startup bootstrap replaced the newer runtime")
	}
}
