package dstruntime

import (
	"fmt"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func TestTelemetryLuaStartAndStopAreIdempotent(t *testing.T) {
	state := lua.NewState()
	defer state.Close()

	scheduled, canceled, writes := 0, 0, 0
	state.PreloadModule("json", func(L *lua.LState) int {
		module := L.NewTable()
		L.SetField(module, "encode", L.NewFunction(func(L *lua.LState) int {
			L.Push(lua.LString("{}"))
			return 1
		}))
		L.Push(module)
		return 1
	})
	theSim := state.NewTable()
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
		writes++
		return 0
	}))
	state.SetGlobal("TheSim", theSim)
	scheduler := state.NewTable()
	state.SetField(scheduler, "ExecuteInTime", state.NewFunction(func(L *lua.LState) int {
		scheduled++
		task := L.NewTable()
		state.SetField(task, "Cancel", state.NewFunction(func(L *lua.LState) int {
			canceled++
			return 0
		}))
		L.Push(task)
		return 1
	}))
	state.SetGlobal("scheduler", scheduler)

	module := loadLuaModule(t, state, "telemetry.lua")
	callLuaMethod(t, state, module, "Start", true)
	callLuaMethod(t, state, module, "Start", true)
	if scheduled != 1 {
		t.Fatalf("Start scheduled %d readiness tasks, want 1", scheduled)
	}
	callLuaMethod(t, state, module, "Stop", true)
	callLuaMethod(t, state, module, "Stop", true)
	if canceled != 1 {
		t.Fatalf("Stop canceled %d readiness tasks, want 1", canceled)
	}
	if writes != 2 {
		t.Fatalf("Stop wrote %d health snapshots, want one per idempotent call", writes)
	}
}

func TestCommandsLuaUsesAllowlistAndWritesStructuredReceipt(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	writtenPath, writtenJSON, kicked := "", "", ""
	preloadJSONHarness(state)
	theSim := state.NewTable()
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
		writtenPath, writtenJSON = L.CheckString(2), L.CheckString(3)
		if callback, ok := L.Get(5).(*lua.LFunction); ok {
			if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue); err != nil {
				L.RaiseError("write callback: %v", err)
			}
		}
		return 0
	}))
	state.SetGlobal("TheSim", theSim)
	theNet := state.NewTable()
	state.SetField(theNet, "GetSessionIdentifier", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("SESSION")); return 1 }))
	state.SetField(theNet, "Kick", state.NewFunction(func(L *lua.LState) int { kicked = L.CheckString(2); return 0 }))
	state.SetGlobal("TheNet", theNet)
	theShard := state.NewTable()
	state.SetField(theShard, "GetShardId", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("1")); return 1 }))
	state.SetGlobal("TheShard", theShard)
	state.SetGlobal("UserToPlayer", state.NewFunction(func(L *lua.LState) int { L.Push(state.NewTable()); return 1 }))

	module := loadLuaModule(t, state, "commands.lua")
	executeJSON := requireLuaFunction(t, state.GetField(module, "ExecuteJSON"), "ExecuteJSON")
	request := `{"requestId":"request-1234567890","action":"player.kick","arguments":{"userId":"KU_TEST"}}`
	if err := state.CallByParam(lua.P{Fn: executeJSON, NRet: 1, Protect: true}, lua.LString(request)); err != nil {
		t.Fatal(err)
	}
	result := requireLuaTable(t, state.Get(-1), "command result")
	if !lua.LVAsBool(state.GetField(result, "ok")) || kicked != "KU_TEST" || writtenPath != "mod_config_data/dst-admin/command-receipt-a.json" || !strings.Contains(writtenJSON, `"requestId":"request-1234567890"`) {
		t.Fatalf("result=%v kicked=%q path=%q receipt=%s", result, kicked, writtenPath, writtenJSON)
	}
	state.Pop(1)

	denied := `{"requestId":"request-0987654321","action":"lua.execute","arguments":{}}`
	if err := state.CallByParam(lua.P{Fn: executeJSON, NRet: 1, Protect: true}, lua.LString(denied)); err != nil {
		t.Fatal(err)
	}
	deniedResult := requireLuaTable(t, state.Get(-1), "denied result")
	if lua.LVAsBool(state.GetField(deniedResult, "ok")) || state.GetField(deniedResult, "code").String() != "ACTION_NOT_ALLOWED" {
		t.Fatalf("denied result = %v", deniedResult)
	}
}

func TestEventsLuaLifecycleDoesNotLeakTasksOrListeners(t *testing.T) {
	state := lua.NewState()
	defer state.Close()

	preloadStaticJSONHarness(state)
	scheduled := make([]*lua.LFunction, 0, 2)
	readyCanceled, flushCanceled := 0, 0
	listenerCallbacks := make(map[string]*lua.LFunction)
	watchCallbacks := make(map[string]*lua.LFunction)
	listenersAdded, listenersRemoved := 0, 0
	watchesAdded, watchesRemoved := 0, 0

	scheduler := state.NewTable()
	state.SetField(scheduler, "ExecuteInTime", state.NewFunction(func(L *lua.LState) int {
		scheduled = append(scheduled, L.CheckFunction(3))
		L.Push(luaTask(state, &readyCanceled))
		return 1
	}))
	state.SetGlobal("scheduler", scheduler)

	theSim := state.NewTable()
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int { return 0 }))
	state.SetGlobal("TheSim", theSim)
	theWorld := state.NewTable()
	state.SetField(theWorld, "ismastersim", lua.LTrue)
	state.SetField(theWorld, "ListenForEvent", state.NewFunction(func(L *lua.LState) int {
		listenersAdded++
		listenerCallbacks[L.CheckString(2)] = L.CheckFunction(3)
		return 0
	}))
	state.SetField(theWorld, "RemoveEventCallback", state.NewFunction(func(L *lua.LState) int {
		listenersRemoved++
		return 0
	}))
	state.SetField(theWorld, "WatchWorldState", state.NewFunction(func(L *lua.LState) int {
		watchesAdded++
		watchCallbacks[L.CheckString(2)] = L.CheckFunction(3)
		return 0
	}))
	state.SetField(theWorld, "StopWatchingWorldState", state.NewFunction(func(L *lua.LState) int {
		watchesRemoved++
		return 0
	}))
	state.SetField(theWorld, "DoTaskInTime", state.NewFunction(func(L *lua.LState) int {
		L.Push(luaTask(state, &flushCanceled))
		return 1
	}))
	state.SetGlobal("TheWorld", theWorld)

	module := loadLuaModule(t, state, "events.lua")
	callLuaMethod(t, state, module, "Start", true)
	callLuaMethod(t, state, module, "Start", true)
	if len(scheduled) != 1 {
		t.Fatalf("Start scheduled %d readiness tasks, want 1", len(scheduled))
	}
	callLuaFunction(t, state, scheduled[0])
	if listenersAdded != 5 || watchesAdded != 4 {
		t.Fatalf("capture registered listeners=%d watches=%d, want 5 and 4", listenersAdded, watchesAdded)
	}
	callLuaMethod(t, state, module, "Start", true)
	if listenersAdded != 5 || watchesAdded != 4 {
		t.Fatal("duplicate Start registered duplicate event handlers")
	}
	callLuaFunction(t, state, listenerCallbacks["ms_save"], theWorld)
	callLuaMethod(t, state, module, "Stop", true)
	callLuaMethod(t, state, module, "Stop", true)
	if readyCanceled != 0 || flushCanceled != 1 {
		t.Fatalf("canceled readiness=%d flush=%d, want 0 and 1", readyCanceled, flushCanceled)
	}
	if listenersRemoved != 5 || watchesRemoved != 4 {
		t.Fatalf("removed listeners=%d watches=%d, want 5 and 4", listenersRemoved, watchesRemoved)
	}

	callLuaMethod(t, state, module, "Start", true)
	callLuaFunction(t, state, scheduled[1])
	callLuaMethod(t, state, module, "Stop", true)
	if listenersAdded != 10 || listenersRemoved != 10 || watchesAdded != 8 || watchesRemoved != 8 {
		t.Fatalf("restart lifecycle leaked handlers: listeners=%d/%d watches=%d/%d", listenersAdded, listenersRemoved, watchesAdded, watchesRemoved)
	}
	_ = watchCallbacks
}

func TestDiagnosticsLuaCancelsReadinessAndAutomaticallyStopsPerformanceSampling(t *testing.T) {
	state := lua.NewState()
	defer state.Close()

	preloadStaticJSONHarness(state)
	scheduled := make([]*lua.LFunction, 0, 2)
	readyCanceled, sampleCanceled, writes := 0, 0, 0
	writtenPath := ""
	scheduler := state.NewTable()
	state.SetField(scheduler, "ExecuteInTime", state.NewFunction(func(L *lua.LState) int {
		scheduled = append(scheduled, L.CheckFunction(3))
		L.Push(luaTask(state, &readyCanceled))
		return 1
	}))
	state.SetGlobal("scheduler", scheduler)

	theSim := state.NewTable()
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
		writes++
		writtenPath = L.CheckString(2)
		callback := L.CheckFunction(5)
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue); err != nil {
			L.RaiseError("write callback: %v", err)
		}
		return 0
	}))
	state.SetGlobal("TheSim", theSim)

	var periodicCallback *lua.LFunction
	theWorld := state.NewTable()
	state.SetField(theWorld, "ismastersim", lua.LFalse)
	state.SetField(theWorld, "state", state.NewTable())
	state.SetField(theWorld, "DoPeriodicTask", state.NewFunction(func(L *lua.LState) int {
		periodicCallback = L.CheckFunction(3)
		L.Push(luaTask(state, &sampleCanceled))
		return 1
	}))
	state.SetGlobal("TheWorld", theWorld)
	state.SetGlobal("TheNet", state.NewTable())
	state.SetGlobal("TheShard", state.NewTable())

	currentTime := 0.0
	state.SetGlobal("GetTime", state.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(currentTime))
		currentTime += 0.5
		return 1
	}))

	module := loadLuaModule(t, state, "diagnostics.lua")
	callLuaMethod(t, state, module, "Start", true)
	callLuaMethod(t, state, module, "Start", true)
	if len(scheduled) != 1 {
		t.Fatalf("Start scheduled %d readiness tasks, want 1", len(scheduled))
	}
	callLuaMethod(t, state, module, "Stop", true)
	callLuaMethod(t, state, module, "Stop", true)
	if readyCanceled != 1 {
		t.Fatalf("Stop canceled %d readiness tasks, want 1", readyCanceled)
	}

	state.SetField(theWorld, "ismastersim", lua.LTrue)
	callLuaMethod(t, state, module, "Start", true)
	callLuaFunction(t, state, scheduled[1])
	request := state.NewTable()
	state.SetField(request, "requestId", lua.LString("request-1234567890"))
	state.SetField(request, "profile", lua.LString("performance"))
	state.SetField(request, "durationSeconds", lua.LNumber(1))
	capture := requireLuaFunction(t, state.GetField(module, "Capture"), "Capture")
	if err := state.CallByParam(lua.P{Fn: capture, NRet: 1, Protect: true}, request); err != nil {
		t.Fatal(err)
	}
	accepted := requireLuaTable(t, state.Get(-1), "diagnostic accepted")
	if !lua.LVAsBool(state.GetField(accepted, "ok")) {
		t.Fatalf("Capture result = %v", accepted)
	}
	state.Pop(1)
	callLuaFunction(t, state, periodicCallback)
	callLuaFunction(t, state, periodicCallback)
	if sampleCanceled != 1 || writes != 1 || writtenPath != "mod_config_data/dst-admin/diagnostic-a.json" {
		t.Fatalf("sampling canceled=%d writes=%d path=%q", sampleCanceled, writes, writtenPath)
	}
	status := callLuaTableMethod(t, state, module, "Status")
	if lua.LVAsBool(state.GetField(status, "busy")) {
		t.Fatal("performance sampling remained busy after automatic completion")
	}

	currentTime = 0
	if err := state.CallByParam(lua.P{Fn: capture, NRet: 1, Protect: true}, request); err != nil {
		t.Fatal(err)
	}
	state.Pop(1)
	callLuaMethod(t, state, module, "Stop", true)
	callLuaMethod(t, state, module, "Stop", true)
	if sampleCanceled != 2 {
		t.Fatalf("Stop canceled active sampling %d times, want exactly 1 additional cancellation", sampleCanceled)
	}
}

func preloadStaticJSONHarness(state *lua.LState) {
	state.PreloadModule("json", func(L *lua.LState) int {
		module := L.NewTable()
		state.SetField(module, "encode", state.NewFunction(func(L *lua.LState) int {
			L.Push(lua.LString("{}"))
			return 1
		}))
		L.Push(module)
		return 1
	})
}

func luaTask(state *lua.LState, canceled *int) *lua.LTable {
	task := state.NewTable()
	state.SetField(task, "Cancel", state.NewFunction(func(L *lua.LState) int {
		*canceled++
		return 0
	}))
	return task
}

func preloadJSONHarness(state *lua.LState) {
	state.PreloadModule("json", func(L *lua.LState) int {
		module := L.NewTable()
		state.SetField(module, "encode", state.NewFunction(func(L *lua.LState) int {
			value := L.CheckTable(1)
			requestID := state.GetField(value, "requestId").String()
			action := state.GetField(value, "action").String()
			code := state.GetField(value, "code").String()
			L.Push(lua.LString(fmt.Sprintf(`{"requestId":"%s","action":"%s","code":"%s"}`, requestID, action, code)))
			return 1
		}))
		state.SetField(module, "decode", state.NewFunction(func(L *lua.LState) int {
			source := L.CheckString(1)
			request := state.NewTable()
			for _, field := range []string{"requestId", "action"} {
				marker := `"` + field + `":"`
				start := strings.Index(source, marker)
				if start >= 0 {
					value := source[start+len(marker):]
					if end := strings.Index(value, `"`); end >= 0 {
						state.SetField(request, field, lua.LString(value[:end]))
					}
				}
			}
			arguments := state.NewTable()
			if strings.Contains(source, `"userId":"KU_TEST"`) {
				state.SetField(arguments, "userId", lua.LString("KU_TEST"))
			}
			state.SetField(request, "arguments", arguments)
			L.Push(request)
			return 1
		}))
		L.Push(module)
		return 1
	})
}

func requireLuaFunction(t *testing.T, value lua.LValue, label string) *lua.LFunction {
	t.Helper()
	function, ok := value.(*lua.LFunction)
	if !ok {
		t.Fatalf("%s = %s, want function", label, value.Type())
	}
	return function
}

func TestBootstrapLuaReloadSwapsOnlyAStartedCandidate(t *testing.T) {
	state := lua.NewState()
	defer state.Close()

	telemetrySource := runtimeTelemetryHarnessSource(false)
	commandsSource := "return { Execute = function() return { ok = true } end }"
	lifecycleSource := `local running=false; return { Start=function() running=true; return true end, Stop=function() running=false; return true end, Status=function() return {running=running} end }`
	theSim := state.NewTable()
	state.SetField(theSim, "GetPersistentString", state.NewFunction(func(L *lua.LState) int {
		path := L.CheckString(2)
		callback := L.CheckFunction(3)
		source := lifecycleSource
		if path == "../dst-admin/telemetry.lua" {
			source = telemetrySource
		} else if path == "../dst-admin/commands.lua" {
			source = commandsSource
		}
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue, lua.LString(source)); err != nil {
			L.RaiseError("persistent callback: %v", err)
		}
		return 0
	}))
	state.SetGlobal("TheSim", theSim)

	bootstrap, err := assetData("bootstrap.lua")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.DoString(string(bootstrap)); err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	first := requireLuaTable(t, state.GetGlobal("DSTAdmin"), "initial DSTAdmin")
	if luaInt(state, "runtime_generation") != 1 || luaInt(state, "runtime_starts") != 1 {
		t.Fatalf("initial counters: generation=%d starts=%d", luaInt(state, "runtime_generation"), luaInt(state, "runtime_starts"))
	}
	callLuaMethod(t, state, first, "Start", true)
	if luaInt(state, "runtime_starts") != 1 {
		t.Fatalf("duplicate Start called telemetry again: %d", luaInt(state, "runtime_starts"))
	}

	callLuaMethod(t, state, first, "Reload", true)
	second := requireLuaTable(t, state.GetGlobal("DSTAdmin"), "reloaded DSTAdmin")
	if second == first || luaInt(state, "runtime_generation") != 2 || luaInt(state, "runtime_starts") != 2 || luaInt(state, "runtime_stops") != 1 {
		t.Fatalf("successful reload did not swap cleanly: generation=%d starts=%d stops=%d", luaInt(state, "runtime_generation"), luaInt(state, "runtime_starts"), luaInt(state, "runtime_stops"))
	}

	telemetrySource = "local broken = ("
	callLuaMethod(t, state, second, "Reload", true)
	if state.GetGlobal("DSTAdmin") != second || luaInt(state, "runtime_stops") != 1 {
		t.Fatal("compile failure replaced or stopped the active runtime")
	}

	telemetrySource = runtimeTelemetryHarnessSource(true)
	callLuaMethod(t, state, second, "Reload", true)
	if state.GetGlobal("DSTAdmin") != second {
		t.Fatal("start failure replaced the active runtime")
	}
	if luaInt(state, "runtime_generation") != 3 || luaInt(state, "runtime_starts") != 4 || luaInt(state, "runtime_stops") != 2 {
		t.Fatalf("failed activation did not restore the previous runtime: generation=%d starts=%d stops=%d", luaInt(state, "runtime_generation"), luaInt(state, "runtime_starts"), luaInt(state, "runtime_stops"))
	}
}

func runtimeTelemetryHarnessSource(failStart bool) string {
	return fmt.Sprintf(`
runtime_generation = (runtime_generation or 0) + 1
local running = false
local M = { generation = runtime_generation }
function M.Start()
    if running then return true end
    runtime_starts = (runtime_starts or 0) + 1
    if %t then return false end
    running = true
    return true
end
function M.Stop()
    if not running then return true end
    runtime_stops = (runtime_stops or 0) + 1
    running = false
    return true
end
function M.Status() return { running = running } end
return M
`, failStart)
}

func loadLuaModule(t *testing.T, state *lua.LState, name string) *lua.LTable {
	t.Helper()
	source, err := assetData(name)
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := state.LoadString(string(source))
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	if err := state.CallByParam(lua.P{Fn: chunk, NRet: 1, Protect: true}); err != nil {
		t.Fatalf("execute %s: %v", name, err)
	}
	return requireLuaTable(t, state.Get(-1), name)
}

func requireLuaTable(t *testing.T, value lua.LValue, label string) *lua.LTable {
	t.Helper()
	table, ok := value.(*lua.LTable)
	if !ok {
		t.Fatalf("%s = %s, want table", label, value.Type())
	}
	return table
}

func callLuaMethod(t *testing.T, state *lua.LState, table *lua.LTable, name string, expected bool) {
	t.Helper()
	function, ok := state.GetField(table, name).(*lua.LFunction)
	if !ok {
		t.Fatalf("%s is not a function", name)
	}
	if err := state.CallByParam(lua.P{Fn: function, NRet: 1, Protect: true}); err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	actual := lua.LVAsBool(state.Get(-1))
	state.Pop(1)
	if actual != expected {
		t.Fatalf("%s returned %v, want %v", name, actual, expected)
	}
}

func callLuaFunction(t *testing.T, state *lua.LState, function *lua.LFunction, arguments ...lua.LValue) {
	t.Helper()
	if function == nil {
		t.Fatal("Lua function is nil")
	}
	if err := state.CallByParam(lua.P{Fn: function, NRet: 0, Protect: true}, arguments...); err != nil {
		t.Fatalf("call Lua function: %v", err)
	}
}

func callLuaTableMethod(t *testing.T, state *lua.LState, table *lua.LTable, name string) *lua.LTable {
	t.Helper()
	function := requireLuaFunction(t, state.GetField(table, name), name)
	if err := state.CallByParam(lua.P{Fn: function, NRet: 1, Protect: true}); err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	result := requireLuaTable(t, state.Get(-1), name+" result")
	state.Pop(1)
	return result
}

func luaInt(state *lua.LState, name string) int {
	return int(lua.LVAsNumber(state.GetGlobal(name)))
}
