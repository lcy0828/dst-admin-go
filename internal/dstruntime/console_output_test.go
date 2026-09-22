package dstruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"dont/shared"
	lua "github.com/yuin/gopher-lua"
)

func TestDistributedConsoleOutputKeepsReceiptCorrespondence(t *testing.T) {
	bridge, fixture, room, world, now := newDistributedBridgeFixture(t)
	request := CommandRequest{RequestID: "console-output-remote-001", Action: "console.execute", Arguments: map[string]interface{}{"script": `print("中文")`}}
	fixture.onSend = func(shared.RuntimeConsoleRequest) {
		receipt := CommandReceipt{SchemaVersion: 1, ProducerVersion: RuntimeVersion, ProducerInstanceID: "remote-instance", SessionID: "REMOTE_SESSION", ShardID: "2", Sequence: 9, RequestID: request.RequestID, Action: request.Action, OK: true, Code: "COMMAND_EXECUTED", CompletedAtUnix: now.Unix(), Details: map[string]interface{}{"output": map[string]interface{}{"text": "中文\n", "truncated": false}}}
		bundle := artifactBundle(shared.ArtifactRuntimeCommand, now, "command-receipt-a.json", receipt)
		receipt.RequestID, receipt.Sequence = "another-console-request", 10
		receipt.Details = map[string]interface{}{"output": map[string]interface{}{"text": "unrelated", "truncated": false}}
		other := artifactBundle(shared.ArtifactRuntimeCommand, now, "command-receipt-b.json", receipt)
		bundle.Artifacts = append(bundle.Artifacts, other.Artifacts...)
		fixture.mu.Lock()
		fixture.bundles[shared.ArtifactRuntimeCommand] = bundle
		fixture.mu.Unlock()
	}
	receipt, err := bridge.ExecuteCommand(context.Background(), room, world, request)
	if err != nil {
		t.Fatal(err)
	}
	text, truncated := outputText(t, receipt)
	if receipt.RequestID != request.RequestID || text != "中文\n" || truncated || len(fixture.sent) != 1 {
		t.Fatalf("remote output: %#v", receipt)
	}
}

func consoleOutputHarness(t *testing.T) (*lua.LState, func(string, string) CommandReceipt) {
	t.Helper()
	state := lua.NewState()
	t.Cleanup(state.Close)
	preloadGameJSONHarness(state)
	state.SetGlobal("print", state.NewFunction(func(L *lua.LState) int { return 0 }))
	var receipt CommandReceipt
	state.SetGlobal("write_receipt", state.NewFunction(func(L *lua.LState) int {
		if err := json.Unmarshal([]byte(L.CheckString(1)), &receipt); err != nil {
			t.Fatal(err)
		}
		return 0
	}))
	if err := state.DoString(`
TheSim = { SetPersistentString = function(self, path, text, encode, callback) write_receipt(text); callback(true) end }
TheNet = { GetSessionIdentifier = function() return "session" end }
TheShard = { GetShardId = function() return "1" end }
TheWorld = { prefab = "forest", state = { cycles = 42, season = "autumn" } }
`); err != nil {
		t.Fatal(err)
	}
	module := loadLuaModule(t, state, "commands.lua")
	execute := requireLuaFunction(t, state.GetField(module, "Execute"), "Execute")
	return state, func(id, script string) CommandReceipt {
		t.Helper()
		receipt = CommandReceipt{}
		request, arguments := state.NewTable(), state.NewTable()
		state.SetField(request, "requestId", lua.LString(id))
		state.SetField(request, "action", lua.LString("console.execute"))
		state.SetField(arguments, "script", lua.LString(script))
		state.SetField(request, "arguments", arguments)
		if err := state.CallByParam(lua.P{Fn: execute, NRet: 0, Protect: true}, request); err != nil {
			t.Fatal(err)
		}
		if receipt.RequestID != id || receipt.Action != "console.execute" {
			t.Fatalf("incorrect receipt identity: %#v", receipt)
		}
		return receipt
	}
}

func outputText(t *testing.T, receipt CommandReceipt) (string, bool) {
	t.Helper()
	value, ok := receipt.Details["output"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing output: %#v", receipt)
	}
	return value["text"].(string), value["truncated"].(bool)
}

func TestConsolePrintOutputIsBoundToEachReceipt(t *testing.T) {
	state, execute := consoleOutputHarness(t)
	original := state.GetGlobal("print")
	first := execute("print-request-00001", `print("World:", TheWorld.prefab)
print("Day:", TheWorld.state.cycles + 1)
print("Season:", TheWorld.state.season)`)
	text, truncated := outputText(t, first)
	if !first.OK || text != "World:\tforest\nDay:\t43\nSeason:\tautumn\n" || truncated {
		t.Fatalf("receipt: %#v", first)
	}
	second := execute("print-request-00002", `print("second", nil, false, "中文")`)
	text, truncated = outputText(t, second)
	if text != "second\tnil\tfalse\t中文\n" || truncated || state.GetGlobal("print") != original {
		t.Fatalf("output=%q truncated=%v", text, truncated)
	}
}

func TestConsoleFailureKeepsOutputAndAlwaysRestoresPrint(t *testing.T) {
	state, execute := consoleOutputHarness(t)
	// DST's traceback handler reports a fatal game error, unlike stock Lua.
	if err := state.DoString(`fatal_errors=0; debug.traceback=function() fatal_errors=fatal_errors+1; return "fatal game error" end`); err != nil {
		t.Fatal(err)
	}
	original := state.GetGlobal("print")
	failed := execute("print-request-00003", `print("before error"); error("boom")`)
	text, _ := outputText(t, failed)
	if failed.OK || failed.Code != "COMMAND_EXECUTION_FAILED" || text != "before error\n" || !strings.Contains(failed.Message, "boom") || state.GetGlobal("print") != original {
		t.Fatalf("failure: %#v", failed)
	}
	invalid := execute("print-request-00004", `print(`)
	text, _ = outputText(t, invalid)
	if invalid.OK || invalid.Code != "COMMAND_COMPILE_FAILED" || text != "" || state.GetGlobal("print") != original {
		t.Fatalf("compile failure: %#v", invalid)
	}
	empty := execute("print-request-00005", `local value = 1`)
	text, truncated := outputText(t, empty)
	if !empty.OK || text != "" || truncated {
		t.Fatalf("empty output: %#v", empty)
	}
	badMessage := execute("print-request-00010", `print("before bad error"); error(setmetatable({}, {__tostring=function() error("bad tostring") end}))`)
	text, _ = outputText(t, badMessage)
	if badMessage.OK || text != "before bad error\n" || state.GetGlobal("fatal_errors") != lua.LNumber(0) || state.GetGlobal("print") != original {
		t.Fatalf("fatal handler invoked or output lost: %#v", badMessage)
	}
}

func TestConsoleDelayedPrintDoesNotLeakIntoOtherCommands(t *testing.T) {
	state, execute := consoleOutputHarness(t)
	logged := 0
	state.SetGlobal("print", state.NewFunction(func(L *lua.LState) int { logged++; return 0 }))
	if err := state.DoString(`function existing_helper() print("helper") end`); err != nil {
		t.Fatal(err)
	}
	first := execute("print-request-00006", `SavedPrint = print; Later = function() print("late") end; existing_helper(); print("now")`)
	if err := state.DoString(`Later(); SavedPrint("saved printer")`); err != nil {
		t.Fatal(err)
	}
	second := execute("print-request-00007", `SavedPrint("old capture"); print("next")`)
	text, _ := outputText(t, first)
	next, _ := outputText(t, second)
	if text != "helper\nnow\n" || next != "next\n" || logged != 6 {
		t.Fatalf("first=%q next=%q logCalls=%d", text, next, logged)
	}
}

func TestConsoleOutputHasBoundedUTF8Payload(t *testing.T) {
	_, execute := consoleOutputHarness(t)
	for i, script := range []string{`print(string.rep("中", 10000))`, `for i=1,20000 do print("row", i) end`} {
		receipt := execute([]string{"print-request-00008", "print-request-00009"}[i], script)
		text, truncated := outputText(t, receipt)
		if !receipt.OK || len(text) > 16<<10 || len(text) == 0 || !truncated || !utf8.ValidString(text) {
			t.Fatalf("size=%d validUTF8=%v truncated=%v code=%s", len(text), utf8.ValidString(text), truncated, receipt.Code)
		}
	}
}

func TestConsoleReceiptEscapesGameEncoderControlBytes(t *testing.T) {
	state, execute := consoleOutputHarness(t)
	if err := state.DoString(`
local json=require("json"); local encode=json.encode_compliant
json.encode_compliant=function(value)
 return string.gsub(encode(value), "\\u00(%x%x)", function(hex) return string.char(tonumber(hex,16)) end)
end
`); err != nil {
		t.Fatal(err)
	}
	receipt := execute("print-request-00011", `print("quoted \""..string.char(0,1,11,12,27).." tail")`)
	text, truncated := outputText(t, receipt)
	if !receipt.OK || text != "quoted \"\x00\x01\x0b\x0c\x1b tail\n" || truncated {
		t.Fatalf("control output corrupted: %#v", receipt)
	}
}
