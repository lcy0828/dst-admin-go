package dstruntime

import (
	"encoding/json"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func TestRuntimeRequestPreservesLuaStringsWithGameDecoder(t *testing.T) {
	for _, source := range []string{
		`local n=1; if n>=1 then print("GE_OK") end`,
		`local n=1; if n<=5 then print("LE_OK") end`,
		`local n=1; if n>0 then print("GT_OK") end`,
		`local n=1; if n<5 then print("LT_OK") end`,
		`print("<&> 饥荒", "\\u003e", "quoted: \"", "line\nnext")`,
		"print(\"分隔\u2028符\u2029\", \"\\\\u2028\", \"\\\\u2029\")",
	} {
		for _, padding := range []int{0, 1800} {
			t.Run(source+strings.Repeat("long", padding/1800), func(t *testing.T) {
				script := source + " --" + strings.Repeat("x", padding)
				request := CommandRequest{RequestID: "encoding-test-123", Action: "console.execute", Arguments: map[string]any{"script": script}}
				payload, err := marshalRuntimeRequest(request)
				if err != nil {
					t.Fatal(err)
				}
				trigger, document := commandDelivery(request, payload)
				state := lua.NewState()
				defer state.Close()
				var delivered []byte
				commands := state.NewTable()
				commands.RawSetString("ExecuteJSON", state.NewFunction(func(L *lua.LState) int {
					delivered = []byte(L.CheckString(1))
					return 0
				}))
				commands.RawSetString("ExecuteFile", state.NewFunction(func(L *lua.LState) int {
					if document == nil || L.CheckString(1) != request.RequestID || L.CheckString(2) != request.Action {
						t.Fatal("wrong document identity")
					}
					delivered = document.Data
					return 0
				}))
				runtime := state.NewTable()
				runtime.RawSetString("Commands", commands)
				state.SetGlobal("DSTAdmin", runtime)
				if err := state.DoString(trigger); err != nil {
					t.Fatal(err)
				}
				if (padding > 0) != (document != nil) {
					t.Fatal("did not exercise both delivery paths")
				}
				var wire struct {
					Arguments map[string]json.RawMessage `json:"arguments"`
				}
				if err := json.Unmarshal(delivered, &wire); err != nil {
					t.Fatal(err)
				}
				// json.lua's decode_scanString uses loadstring on the quoted JSON
				// token. Go's json.Unmarshal would hide the actual game failure.
				if err := state.DoString("decoded = " + string(wire.Arguments["script"])); err != nil {
					t.Fatal(err)
				}
				if got := state.GetGlobal("decoded").String(); got != script {
					t.Fatalf("game decoded %q, want %q", got, script)
				}
				if err := state.DoString(state.GetGlobal("decoded").String()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
