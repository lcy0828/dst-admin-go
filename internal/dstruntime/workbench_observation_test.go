package dstruntime

import (
	"encoding/json"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// Run the shipped dispatcher with a controllable static clock. Observation may
// update the original receipt, but must never invoke the mutation twice.
func TestWorkbenchObservationKeepsOriginalReceiptAndStopsTasks(t *testing.T) {
	for _, scenario := range []string{"confirmed", "timeout", "observer-error", "runtime-stop"} {
		t.Run(scenario, func(t *testing.T) {
			L := lua.NewState()
			defer L.Close()
			preloadGameJSONHarness(L)
			if err := L.DoString(`
queue={};writes={};mutations=0;checks=0;released=0
TheSim={SetPersistentString=function(self,path,text,encoded,callback)writes[#writes+1]=text;callback(true)end}
TheNet={GetSessionIdentifier=function()return "isolated-session"end}
TheShard={GetShardId=function()return "1"end}
TheWorld={DoStaticTaskInTime=function(self,delay,fn)
 local task={fn=fn};function task:Cancel()self.cancelled=true end;queue[#queue+1]=task;return task end}
function step()local task=table.remove(queue,1);if task and not task.cancelled then task.fn()end end
`); err != nil {
				t.Fatal(err)
			}
			L.SetGlobal("commands", loadLuaModule(t, L, "commands.lua"))
			if err := L.DoString(`
commands.Workbench.FinishObservation=function()released=released+1 end
commands.Workbench.Encode=function(out)return "[DST-ADMIN-BATCH-V2] final"end
commands.Workbench.TakeObservation=function()return function()
 checks=checks+1
 if observerError then error("observation failed")end
 if confirmed and checks==3 then return {}end
end end
request={requestId="observation-request-00001",action="console.execute",arguments={script='mutations=mutations+1;print("[DST-ADMIN-BATCH-V2] pending")'}}
`); err != nil {
				t.Fatal(err)
			}
			L.SetGlobal("confirmed", lua.LBool(scenario == "confirmed"))
			L.SetGlobal("observerError", lua.LBool(scenario == "observer-error"))
			code := `commands.Execute(request);commands.Execute(request);assert(mutations==1)`
			if scenario == "runtime-stop" {
				code += `;commands.ClearCatalog();for i=1,30 do step()end;assert(checks==1)`
			} else {
				code += `;for i=1,30 do step()end`
			}
			code += `;assert(#writes==1 and released==1 and mutations==1);commands.Execute(request);assert(mutations==1 and #writes==1)`
			if err := L.DoString(code); err != nil {
				t.Fatal(err)
			}
			var receipt CommandReceipt
			if err := json.Unmarshal([]byte(L.GetGlobal("writes").(*lua.LTable).RawGetInt(1).String()), &receipt); err != nil {
				t.Fatal(err)
			}
			text, _ := outputText(t, receipt)
			want := "[DST-ADMIN-BATCH-V2] pending\n"
			if scenario == "confirmed" {
				want = "[DST-ADMIN-BATCH-V2] final\n"
			}
			if receipt.RequestID != "observation-request-00001" || text != want {
				t.Fatalf("wrong original receipt: %+v", receipt)
			}
		})
	}
}
