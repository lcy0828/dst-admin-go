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
		L.SetField(module, "encode_compliant", L.NewFunction(func(L *lua.LState) int {
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

func TestWorldStateLuaWritesAllMetricsAndRotatesSlots(t *testing.T) {
	state := lua.NewState()
	defer state.Close()

	writtenPaths := make([]string, 0, 2)
	var encodedPayload *lua.LTable
	state.PreloadModule("json", func(L *lua.LState) int {
		module := L.NewTable()
		state.SetField(module, "encode_compliant", state.NewFunction(func(L *lua.LState) int {
			encodedPayload = L.CheckTable(1)
			L.Push(lua.LString("{}"))
			return 1
		}))
		L.Push(module)
		return 1
	})
	theSim := state.NewTable()
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
		writtenPaths = append(writtenPaths, L.CheckString(2))
		callback := L.CheckFunction(5)
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue); err != nil {
			L.RaiseError("write callback: %v", err)
		}
		return 0
	}))
	state.SetGlobal("TheSim", theSim)
	theNet := state.NewTable()
	state.SetField(theNet, "GetSessionIdentifier", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("SESSION")); return 1 }))
	state.SetField(theNet, "GetClientTable", state.NewFunction(func(L *lua.LState) int {
		clients := L.NewTable()
		host := L.NewTable()
		L.SetField(host, "performance", lua.LNumber(1))
		clients.Append(host)
		L.Push(clients)
		return 1
	}))
	state.SetGlobal("TheNet", theNet)
	theShard := state.NewTable()
	state.SetField(theShard, "GetShardId", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("1")); return 1 }))
	state.SetGlobal("TheShard", theShard)

	scheduled := make([]*lua.LFunction, 0, 1)
	scheduler := state.NewTable()
	state.SetField(scheduler, "ExecuteInTime", state.NewFunction(func(L *lua.LState) int {
		scheduled = append(scheduled, L.CheckFunction(3))
		canceled := 0
		L.Push(luaTask(state, &canceled))
		return 1
	}))
	state.SetGlobal("scheduler", scheduler)
	periodicCanceled := 0
	var periodicCallback *lua.LFunction
	theWorld := state.NewTable()
	state.SetField(theWorld, "ismastersim", lua.LTrue)
	worldValues := state.NewTable()
	for name, value := range map[string]lua.LValue{
		"season": lua.LString("autumn"), "phase": lua.LString("day"), "cycles": lua.LNumber(48),
		"elapseddaysinseason": lua.LNumber(6), "remainingdaysinseason": lua.LNumber(14), "seasonprogress": lua.LNumber(.3),
		"time": lua.LNumber(.34), "timeinphase": lua.LNumber(.57), "isacidraining": lua.LTrue, "moonphase": lua.LString("new"),
		"temperature": lua.LNumber(18.5), "wetness": lua.LNumber(.18), "moisture": lua.LNumber(18), "moistureceil": lua.LNumber(100),
		"precipitationrate": lua.LNumber(.25), "nightmarephase": lua.LString("warn"), "nightmaretimeinphase": lua.LNumber(.46),
	} {
		state.SetField(worldValues, name, value)
	}
	state.SetField(theWorld, "state", worldValues)
	state.SetField(theWorld, "components", state.NewTable())
	state.SetField(theWorld, "DoPeriodicTask", state.NewFunction(func(L *lua.LState) int {
		periodicCallback = L.CheckFunction(3)
		L.Push(luaTask(state, &periodicCanceled))
		return 1
	}))
	state.SetGlobal("TheWorld", theWorld)

	module := loadLuaModule(t, state, "worldstate.lua")
	callLuaMethod(t, state, module, "Start", true)
	callLuaMethod(t, state, module, "Start", true)
	if len(scheduled) != 0 {
		t.Fatalf("ready world scheduled %d readiness tasks, want 0", len(scheduled))
	}
	if len(writtenPaths) != 0 || periodicCallback == nil {
		t.Fatalf("module start writes = %#v, periodic callback = %v", writtenPaths, periodicCallback != nil)
	}
	callLuaFunction(t, state, periodicCallback)
	if len(writtenPaths) != 1 || writtenPaths[0] != "mod_config_data/dst-admin/worldstate-a.json" {
		t.Fatalf("first periodic world state writes = %#v", writtenPaths)
	}
	if encodedPayload == nil {
		t.Fatal("world state payload was not encoded")
	}
	for name, expected := range map[string]string{
		"season": "autumn", "phase": "day", "precipitation": "acid_rain", "moonPhase": "new", "nightmarePhase": "warn",
		"sessionId": "SESSION", "shardId": "1", "producerVersion": RuntimeVersion,
	} {
		if actual := state.GetField(encodedPayload, name).String(); actual != expected {
			t.Fatalf("payload %s = %q, want %q", name, actual, expected)
		}
	}
	for name, expected := range map[string]float64{
		"cycles": 48, "elapsedDaysInSeason": 6, "remainingDaysInSeason": 14, "seasonProgress": .3,
		"dayProgress": .34, "phaseProgress": .57, "temperature": 18.5, "wetness": .18, "moisture": 18,
		"moistureCeil": 100, "precipitationRate": .25, "nightmareProgress": .46, "hostPerformance": 1,
	} {
		if actual := float64(lua.LVAsNumber(state.GetField(encodedPayload, name))); actual != expected {
			t.Fatalf("payload %s = %v, want %v", name, actual, expected)
		}
	}
	if !lua.LVAsBool(state.GetField(encodedPayload, "complete")) || luaIntField(state, encodedPayload, "schemaVersion") != ProtocolVersion {
		t.Fatalf("payload envelope = %v", encodedPayload)
	}
	status := callLuaTableMethod(t, state, module, "Status")
	if luaIntField(state, status, "sequence") != 1 || !lua.LVAsBool(state.GetField(status, "ready")) {
		t.Fatalf("world state status = %v", status)
	}
	callLuaFunction(t, state, periodicCallback)
	if len(writtenPaths) != 2 || writtenPaths[1] != "mod_config_data/dst-admin/worldstate-b.json" {
		t.Fatalf("rotated world state writes = %#v", writtenPaths)
	}
	callLuaMethod(t, state, module, "Stop", true)
	callLuaMethod(t, state, module, "Stop", true)
	if periodicCanceled != 1 {
		t.Fatalf("Stop canceled periodic tasks %d times, want 1", periodicCanceled)
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

func TestCommandsLuaExecutesManagedScriptAndKeepsSuccessOutOfServerLog(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadJSONHarness(state)
	printed := 0
	state.SetGlobal("print", state.NewFunction(func(L *lua.LState) int { printed++; return 0 }))
	theSim := state.NewTable()
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
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
	state.SetGlobal("TheNet", theNet)
	theShard := state.NewTable()
	state.SetField(theShard, "GetShardId", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("1")); return 1 }))
	state.SetGlobal("TheShard", theShard)

	module := loadLuaModule(t, state, "commands.lua")
	execute := requireLuaFunction(t, state.GetField(module, "Execute"), "Execute")
	request := state.NewTable()
	state.SetField(request, "requestId", lua.LString("managed-script-1234"))
	state.SetField(request, "action", lua.LString("console.execute"))
	arguments := state.NewTable()
	state.SetField(arguments, "script", lua.LString(`DST_ADMIN_MANAGED_TEST = "executed"`))
	state.SetField(request, "arguments", arguments)
	if err := state.CallByParam(lua.P{Fn: execute, NRet: 1, Protect: true}, request); err != nil {
		t.Fatal(err)
	}
	result := requireLuaTable(t, state.Get(-1), "managed command result")
	if !lua.LVAsBool(state.GetField(result, "ok")) || state.GetField(result, "code").String() != "COMMAND_EXECUTED" || state.GetGlobal("DST_ADMIN_MANAGED_TEST").String() != "executed" {
		t.Fatalf("managed result=%v value=%v", result, state.GetGlobal("DST_ADMIN_MANAGED_TEST"))
	}
	state.Pop(1)
	if printed != 0 {
		t.Fatalf("successful managed command printed %d server log lines", printed)
	}
}

func TestCommandsLuaLoadsCommandDocumentAndKeepsReceiptIdentity(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadJSONHarness(state)
	writtenPath, writtenJSON, loadedPath, erasedPath := "", "", "", ""
	theSim := state.NewTable()
	state.SetField(theSim, "GetPersistentString", state.NewFunction(func(L *lua.LState) int {
		loadedPath = L.CheckString(2)
		callback := L.CheckFunction(3)
		source := `{"requestId":"file-command-1234","action":"console.execute","arguments":{"script":"DST_ADMIN_FILE_TEST=true"}}`
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue, lua.LString(source)); err != nil {
			L.RaiseError("read callback: %v", err)
		}
		return 0
	}))
	state.SetField(theSim, "ErasePersistentString", state.NewFunction(func(L *lua.LState) int {
		erasedPath = L.CheckString(2)
		callback := L.CheckFunction(3)
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue); err != nil {
			L.RaiseError("erase callback: %v", err)
		}
		return 0
	}))
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
		writtenPath, writtenJSON = L.CheckString(2), L.CheckString(3)
		callback := L.CheckFunction(5)
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue); err != nil {
			L.RaiseError("write callback: %v", err)
		}
		return 0
	}))
	state.SetGlobal("TheSim", theSim)
	theNet := state.NewTable()
	state.SetField(theNet, "GetSessionIdentifier", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("SESSION")); return 1 }))
	state.SetGlobal("TheNet", theNet)
	theShard := state.NewTable()
	state.SetField(theShard, "GetShardId", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("1")); return 1 }))
	state.SetGlobal("TheShard", theShard)

	module := loadLuaModule(t, state, "commands.lua")
	executeFile := requireLuaFunction(t, state.GetField(module, "ExecuteFile"), "ExecuteFile")
	if err := state.CallByParam(lua.P{Fn: executeFile, NRet: 1, Protect: true}, lua.LString("file-command-1234"), lua.LString("console.execute")); err != nil {
		t.Fatal(err)
	}
	result := requireLuaTable(t, state.Get(-1), "file command result")
	if !lua.LVAsBool(state.GetField(result, "ok")) || state.GetGlobal("DST_ADMIN_FILE_TEST") != lua.LTrue ||
		loadedPath != "../dst-admin/command-requests/file-command-1234.json" || erasedPath != loadedPath ||
		writtenPath != "mod_config_data/dst-admin/command-receipt-a.json" ||
		!strings.Contains(writtenJSON, `"requestId":"file-command-1234"`) || !strings.Contains(writtenJSON, `"action":"console.execute"`) {
		t.Fatalf("result=%v loaded=%q erased=%q written=%q receipt=%s value=%v", result, loadedPath, erasedPath, writtenPath, writtenJSON, state.GetGlobal("DST_ADMIN_FILE_TEST"))
	}
}

func TestCommandsLuaSerializesFileSignalsAndPersistsMismatch(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadJSONHarness(state)
	readCallbacks := map[string]*lua.LFunction{}
	erasedPaths := []string{}
	writtenJSON := []string{}
	writeCallbacks := []*lua.LFunction{}
	theSim := state.NewTable()
	state.SetField(theSim, "GetPersistentString", state.NewFunction(func(L *lua.LState) int {
		readCallbacks[L.CheckString(2)] = L.CheckFunction(3)
		return 0
	}))
	state.SetField(theSim, "ErasePersistentString", state.NewFunction(func(L *lua.LState) int {
		erasedPaths = append(erasedPaths, L.CheckString(2))
		callback := L.CheckFunction(3)
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue); err != nil {
			L.RaiseError("erase callback: %v", err)
		}
		return 0
	}))
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
		writtenJSON = append(writtenJSON, L.CheckString(3))
		writeCallbacks = append(writeCallbacks, L.CheckFunction(5))
		return 0
	}))
	state.SetGlobal("TheSim", theSim)
	theNet := state.NewTable()
	state.SetField(theNet, "GetSessionIdentifier", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("SESSION")); return 1 }))
	state.SetGlobal("TheNet", theNet)
	theShard := state.NewTable()
	state.SetField(theShard, "GetShardId", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("1")); return 1 }))
	state.SetGlobal("TheShard", theShard)

	module := loadLuaModule(t, state, "commands.lua")
	executeFile := requireLuaFunction(t, state.GetField(module, "ExecuteFile"), "ExecuteFile")
	firstID, secondID := "queued-file-command-1234", "queued-file-command-5678"
	for _, requestID := range []string{firstID, secondID} {
		if err := state.CallByParam(lua.P{Fn: executeFile, NRet: 1, Protect: true}, lua.LString(requestID), lua.LString("console.execute")); err != nil {
			t.Fatal(err)
		}
		result := requireLuaTable(t, state.Get(-1), "queued file result")
		if !lua.LVAsBool(state.GetField(result, "ok")) {
			t.Fatalf("request %s was rejected: %v", requestID, result)
		}
		state.Pop(1)
	}
	firstPath := "../dst-admin/command-requests/" + firstID + ".json"
	secondPath := "../dst-admin/command-requests/" + secondID + ".json"
	if readCallbacks[firstPath] == nil || readCallbacks[secondPath] != nil {
		t.Fatalf("initial reads=%v", readCallbacks)
	}
	firstSource := `{"requestId":"` + firstID + `","action":"console.execute","arguments":{"script":"DST_ADMIN_QUEUED_FILE=true"}}`
	if err := state.CallByParam(lua.P{Fn: readCallbacks[firstPath], NRet: 0, Protect: true}, lua.LTrue, lua.LString(firstSource)); err != nil {
		t.Fatal(err)
	}
	if state.GetGlobal("DST_ADMIN_QUEUED_FILE") != lua.LTrue || len(writtenJSON) != 1 || readCallbacks[secondPath] != nil {
		t.Fatalf("first value=%v receipts=%v reads=%v", state.GetGlobal("DST_ADMIN_QUEUED_FILE"), writtenJSON, readCallbacks)
	}
	if err := state.CallByParam(lua.P{Fn: writeCallbacks[0], NRet: 0, Protect: true}, lua.LTrue); err != nil {
		t.Fatal(err)
	}
	if readCallbacks[secondPath] == nil {
		t.Fatalf("second document was not dispatched after first receipt: %v", readCallbacks)
	}
	secondSource := `{"requestId":"` + secondID + `","action":"system.ping","arguments":{}}`
	if err := state.CallByParam(lua.P{Fn: readCallbacks[secondPath], NRet: 0, Protect: true}, lua.LTrue, lua.LString(secondSource)); err != nil {
		t.Fatal(err)
	}
	if len(writtenJSON) != 2 || !strings.Contains(writtenJSON[0], `"code":"COMMAND_EXECUTED"`) ||
		!strings.Contains(writtenJSON[1], `"code":"COMMAND_DOCUMENT_MISMATCH"`) || len(erasedPaths) != 2 {
		t.Fatalf("receipts=%v erased=%v", writtenJSON, erasedPaths)
	}
	if err := state.CallByParam(lua.P{Fn: writeCallbacks[1], NRet: 0, Protect: true}, lua.LTrue); err != nil {
		t.Fatal(err)
	}
	status := callLuaTableMethod(t, state, module, "Status")
	if lua.LVAsBool(state.GetField(status, "busy")) || luaIntField(state, status, "pending") != 0 {
		t.Fatalf("final status=%v", status)
	}
}

func TestCommandsLuaDoesNotExecuteWhenDocumentCleanupFails(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadJSONHarness(state)
	writtenJSON := ""
	theSim := state.NewTable()
	state.SetField(theSim, "GetPersistentString", state.NewFunction(func(L *lua.LState) int {
		callback := L.CheckFunction(3)
		source := `{"requestId":"cleanup-file-command-1234","action":"console.execute","arguments":{"script":"DST_ADMIN_CLEANUP_TEST=true"}}`
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue, lua.LString(source)); err != nil {
			L.RaiseError("read callback: %v", err)
		}
		return 0
	}))
	state.SetField(theSim, "ErasePersistentString", state.NewFunction(func(L *lua.LState) int {
		callback := L.CheckFunction(3)
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LFalse); err != nil {
			L.RaiseError("erase callback: %v", err)
		}
		return 0
	}))
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
		path := L.CheckString(2)
		callback := L.CheckFunction(5)
		if strings.Contains(path, "command-requests/") {
			if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LFalse); err != nil {
				L.RaiseError("blank callback: %v", err)
			}
			return 0
		}
		writtenJSON = L.CheckString(3)
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue); err != nil {
			L.RaiseError("receipt callback: %v", err)
		}
		return 0
	}))
	state.SetGlobal("TheSim", theSim)
	theNet := state.NewTable()
	state.SetField(theNet, "GetSessionIdentifier", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("SESSION")); return 1 }))
	state.SetGlobal("TheNet", theNet)
	theShard := state.NewTable()
	state.SetField(theShard, "GetShardId", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("1")); return 1 }))
	state.SetGlobal("TheShard", theShard)

	module := loadLuaModule(t, state, "commands.lua")
	executeFile := requireLuaFunction(t, state.GetField(module, "ExecuteFile"), "ExecuteFile")
	if err := state.CallByParam(lua.P{Fn: executeFile, NRet: 1, Protect: true}, lua.LString("cleanup-file-command-1234"), lua.LString("console.execute")); err != nil {
		t.Fatal(err)
	}
	if state.GetGlobal("DST_ADMIN_CLEANUP_TEST") == lua.LTrue || !strings.Contains(writtenJSON, `"code":"COMMAND_DOCUMENT_CLEANUP_FAILED"`) {
		t.Fatalf("value=%v receipt=%s", state.GetGlobal("DST_ADMIN_CLEANUP_TEST"), writtenJSON)
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
	if len(scheduled) != 0 {
		t.Fatalf("ready world scheduled %d readiness tasks, want 0", len(scheduled))
	}
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

func TestBarriersLuaProvesSaveCallbackAndPreservesDelayedShutdown(t *testing.T) {
	state := lua.NewState()
	defer state.Close()

	var receipt *lua.LTable
	state.PreloadModule("json", func(L *lua.LState) int {
		module := L.NewTable()
		state.SetField(module, "encode_compliant", state.NewFunction(func(L *lua.LState) int {
			receipt = L.CheckTable(1)
			L.Push(lua.LString("{}"))
			return 1
		}))
		L.Push(module)
		return 1
	})
	theSim := state.NewTable()
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int {
		if path := L.CheckString(2); path != "mod_config_data/dst-admin/snapshot-barrier.json" {
			L.RaiseError("unexpected receipt path %s", path)
		}
		callback := L.CheckFunction(5)
		if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}, lua.LTrue); err != nil {
			L.RaiseError("receipt callback: %v", err)
		}
		return 0
	}))
	state.SetGlobal("TheSim", theSim)
	snapshot := 40
	theNet := state.NewTable()
	state.SetField(theNet, "GetSessionIdentifier", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("SESSION")); return 1 }))
	state.SetField(theNet, "GetCurrentSnapshot", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LNumber(snapshot)); return 1 }))
	state.SetGlobal("TheNet", theNet)
	theShard := state.NewTable()
	state.SetField(theShard, "GetShardId", state.NewFunction(func(L *lua.LState) int { L.Push(lua.LString("1")); return 1 }))
	state.SetGlobal("TheShard", theShard)

	var timeoutCallback *lua.LFunction
	timeoutCanceled := 0
	theWorld := state.NewTable()
	state.SetField(theWorld, "ismastersim", lua.LTrue)
	state.SetField(theWorld, "ismastershard", lua.LTrue)
	state.SetField(theWorld, "DoTaskInTime", state.NewFunction(func(L *lua.LState) int {
		timeoutCallback = L.CheckFunction(3)
		L.Push(luaTask(state, &timeoutCanceled))
		return 1
	}))
	state.SetGlobal("TheWorld", theWorld)

	saveCalls := 0
	shutdownValues := make([]bool, 0, 2)
	shardGameIndex := state.NewTable()
	originalSave := state.NewFunction(func(L *lua.LState) int {
		saveCalls++
		shutdownValues = append(shutdownValues, lua.LVAsBool(L.Get(3)))
		snapshot++
		if callback, ok := L.Get(2).(*lua.LFunction); ok {
			if err := L.CallByParam(lua.P{Fn: callback, NRet: 0, Protect: true}); err != nil {
				L.RaiseError("save callback: %v", err)
			}
		}
		return 0
	})
	state.SetField(shardGameIndex, "SaveCurrent", originalSave)
	state.SetGlobal("ShardGameIndex", shardGameIndex)
	state.SetField(theWorld, "PushEvent", state.NewFunction(func(L *lua.LState) int {
		if L.CheckString(2) != "ms_save" {
			L.RaiseError("unexpected world event")
		}
		save := requireLuaFunction(t, state.GetField(shardGameIndex, "SaveCurrent"), "wrapped SaveCurrent")
		if err := L.CallByParam(lua.P{Fn: save, NRet: 0, Protect: true}, shardGameIndex, lua.LNil, lua.LFalse); err != nil {
			L.RaiseError("trigger save: %v", err)
		}
		return 0
	}))

	module := loadLuaModule(t, state, "barriers.lua")
	callLuaMethod(t, state, module, "Start", true)
	callLuaMethodWithString(t, state, module, "Prepare", "hot-barrier-123", true)
	if receipt == nil || state.GetField(receipt, "state").String() != "prepared" || timeoutCallback == nil {
		t.Fatalf("prepared receipt=%v timeout=%v", receipt, timeoutCallback != nil)
	}
	callLuaMethodWithString(t, state, module, "Commit", "hot-barrier-123", true)
	if saveCalls != 1 || state.GetField(receipt, "state").String() != "completed" || state.GetField(receipt, "proof").String() != "save_current_callback" || luaIntField(state, receipt, "snapshotAfter") != 41 {
		t.Fatalf("saveCalls=%d receipt=%v", saveCalls, receipt)
	}
	status := callLuaTableMethod(t, state, module, "Status")
	if !lua.LVAsBool(state.GetField(status, "holding")) {
		t.Fatalf("barrier status=%v", status)
	}

	delayedCallbackCalls := 0
	delayedCallback := state.NewFunction(func(L *lua.LState) int { delayedCallbackCalls++; return 0 })
	wrappedSave := requireLuaFunction(t, state.GetField(shardGameIndex, "SaveCurrent"), "wrapped SaveCurrent")
	if err := state.CallByParam(lua.P{Fn: wrappedSave, NRet: 0, Protect: true}, shardGameIndex, delayedCallback, lua.LTrue); err != nil {
		t.Fatal(err)
	}
	if saveCalls != 1 {
		t.Fatalf("held shutdown save reached original SaveCurrent: %d", saveCalls)
	}
	callLuaMethodWithString(t, state, module, "Release", "hot-barrier-123", true)
	if saveCalls != 2 || delayedCallbackCalls != 1 || len(shutdownValues) != 2 || !shutdownValues[1] || timeoutCanceled == 0 {
		t.Fatalf("saveCalls=%d callback=%d shutdown=%v timeoutCanceled=%d", saveCalls, delayedCallbackCalls, shutdownValues, timeoutCanceled)
	}

	callLuaMethodWithString(t, state, module, "Prepare", "hot-timeout-456", true)
	callLuaFunction(t, state, timeoutCallback)
	status = callLuaTableMethod(t, state, module, "Status")
	if lua.LVAsBool(state.GetField(status, "busy")) || state.GetField(receipt, "state").String() != "cancelled" {
		t.Fatalf("timeout status=%v receipt=%v", status, receipt)
	}
	callLuaMethod(t, state, module, "Stop", true)
	if state.GetField(shardGameIndex, "SaveCurrent") != originalSave {
		t.Fatal("Stop did not restore the original SaveCurrent function")
	}
}

func TestBarriersLuaWaitsForWorldBeforeWrappingSave(t *testing.T) {
	state := lua.NewState()
	defer state.Close()

	preloadStaticJSONHarness(state)
	theSim := state.NewTable()
	state.SetField(theSim, "SetPersistentString", state.NewFunction(func(L *lua.LState) int { return 0 }))
	state.SetGlobal("TheSim", theSim)

	readyCallbacks := make([]*lua.LFunction, 0, 1)
	readyCanceled := 0
	scheduler := state.NewTable()
	state.SetField(scheduler, "ExecuteInTime", state.NewFunction(func(L *lua.LState) int {
		readyCallbacks = append(readyCallbacks, L.CheckFunction(3))
		L.Push(luaTask(state, &readyCanceled))
		return 1
	}))
	state.SetGlobal("scheduler", scheduler)

	module := loadLuaModule(t, state, "barriers.lua")
	callLuaMethod(t, state, module, "Start", true)
	status := callLuaTableMethod(t, state, module, "Status")
	if !lua.LVAsBool(state.GetField(status, "running")) || lua.LVAsBool(state.GetField(status, "ready")) || len(readyCallbacks) != 1 {
		t.Fatalf("waiting barrier status=%v callbacks=%d", status, len(readyCallbacks))
	}

	theWorld := state.NewTable()
	state.SetField(theWorld, "ismastersim", lua.LTrue)
	state.SetGlobal("TheWorld", theWorld)
	originalSave := state.NewFunction(func(L *lua.LState) int { return 0 })
	shardGameIndex := state.NewTable()
	state.SetField(shardGameIndex, "SaveCurrent", originalSave)
	state.SetGlobal("ShardGameIndex", shardGameIndex)
	callLuaFunction(t, state, readyCallbacks[0])

	status = callLuaTableMethod(t, state, module, "Status")
	if !lua.LVAsBool(state.GetField(status, "ready")) || state.GetField(shardGameIndex, "SaveCurrent") == originalSave {
		t.Fatalf("ready barrier status=%v wrapped=%v", status, state.GetField(shardGameIndex, "SaveCurrent") != originalSave)
	}
	callLuaMethod(t, state, module, "Stop", true)
	if state.GetField(shardGameIndex, "SaveCurrent") != originalSave || readyCanceled != 0 {
		t.Fatalf("barrier stop restored=%v canceled=%d", state.GetField(shardGameIndex, "SaveCurrent") == originalSave, readyCanceled)
	}
}

func preloadStaticJSONHarness(state *lua.LState) {
	state.PreloadModule("json", func(L *lua.LState) int {
		module := L.NewTable()
		state.SetField(module, "encode_compliant", state.NewFunction(func(L *lua.LState) int {
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
		state.SetField(module, "encode_compliant", state.NewFunction(func(L *lua.LState) int {
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
			if marker := `"script":"`; strings.Contains(source, marker) {
				value := source[strings.Index(source, marker)+len(marker):]
				if end := strings.Index(value, `"`); end >= 0 {
					state.SetField(arguments, "script", lua.LString(value[:end]))
				}
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
	worldStateSource := `local running=false; return {
Start=function() running=true; return true end,
Stop=function() running=false; return true end,
Status=function() return {running=running} end,
EmitOnce=function(callback) refresh_order=(refresh_order or "").."worldstate,"; if callback then callback(true) end; return true end
}`
	theSim := state.NewTable()
	state.SetField(theSim, "GetPersistentString", state.NewFunction(func(L *lua.LState) int {
		path := L.CheckString(2)
		callback := L.CheckFunction(3)
		source := lifecycleSource
		if path == "../dst-admin/telemetry.lua" {
			source = telemetrySource
		} else if path == "../dst-admin/worldstate.lua" {
			source = worldStateSource
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
	if state.GetGlobal("refresh_order").String() != "worldstate,telemetry," {
		t.Fatalf("initial refresh order = %q", state.GetGlobal("refresh_order").String())
	}
	state.SetGlobal("refresh_order", lua.LString(""))
	callLuaMethod(t, state, first, "Start", true)
	if luaInt(state, "runtime_starts") != 1 {
		t.Fatalf("duplicate Start called telemetry again: %d", luaInt(state, "runtime_starts"))
	}

	callLuaMethod(t, state, first, "Reload", true)
	second := requireLuaTable(t, state.GetGlobal("DSTAdmin"), "reloaded DSTAdmin")
	if second == first || luaInt(state, "runtime_generation") != 2 || luaInt(state, "runtime_starts") != 2 || luaInt(state, "runtime_stops") != 1 {
		t.Fatalf("successful reload did not swap cleanly: generation=%d starts=%d stops=%d", luaInt(state, "runtime_generation"), luaInt(state, "runtime_starts"), luaInt(state, "runtime_stops"))
	}
	if state.GetGlobal("refresh_order").String() != "worldstate,telemetry," {
		t.Fatalf("reload refresh order = %q", state.GetGlobal("refresh_order").String())
	}
	state.SetGlobal("refresh_order", lua.LString(""))
	callLuaMethod(t, state, second, "Refresh", true)
	if state.GetGlobal("refresh_order").String() != "worldstate,telemetry," {
		t.Fatalf("refresh order = %q", state.GetGlobal("refresh_order").String())
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
function M.EmitOnce(callback)
    refresh_order = (refresh_order or "") .. "telemetry,"
    if callback then callback(true) end
    return true
end
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

func callLuaMethodWithString(t *testing.T, state *lua.LState, table *lua.LTable, name, value string, expected bool) {
	t.Helper()
	function := requireLuaFunction(t, state.GetField(table, name), name)
	if err := state.CallByParam(lua.P{Fn: function, NRet: 1, Protect: true}, lua.LString(value)); err != nil {
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

func luaIntField(state *lua.LState, table *lua.LTable, name string) int {
	return int(lua.LVAsNumber(state.GetField(table, name)))
}
