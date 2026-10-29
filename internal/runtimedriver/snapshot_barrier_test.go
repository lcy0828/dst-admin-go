package runtimedriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dont/internal/dstruntime"
	lua "github.com/yuin/gopher-lua"
)

// Exercise the actual publisher and both production readers together. Mocked
// Go receipts and a Lua JSON stub returning "{}" cannot catch protocol drift.
func TestSnapshotBarrierReadsActualLuaReceiptsOnBothTransports(t *testing.T) {
	for _, scenario := range []struct {
		name, script string
		terminal     string
	}{
		{"master completes", `
TheWorld.ismastershard=true
assert(B.Prepare("hot-master-round"));assert(B.Arm("hot-master-round",0))
assert(B.Commit("hot-master-round"));assert(B.Commit("hot-master-round"));assert(saveCalls==1)
CompleteSave();assert(B.Release("hot-master-round"));assert(not B.Status().busy)
`, "released"},
		{"master cancels during write", `
TheWorld.ismastershard=true
assert(B.Prepare("hot-cancel-round"));assert(B.Arm("hot-cancel-round",0))
assert(B.Commit("hot-cancel-round"));assert(B.Cancel("hot-cancel-round"))
assert(B.Status().inFlight);CompleteSave();assert(not B.Status().busy)
`, "cancelled"},
		{"master save fails", `
TheWorld.ismastershard=true
assert(B.Prepare("hot-failed-round"));assert(B.Arm("hot-failed-round",0))
assert(B.Commit("hot-failed-round"));CompleteSave(false)
assert(B.Cancel("hot-failed-round"));assert(not B.Status().busy)
`, "cancelled"},
		{"secondary completes without commit marker", `
assert(B.Prepare("hot-caves-round"));assert(B.Arm("hot-caves-round",7187))
ShardGameIndex:SaveCurrent();CompleteSave();assert(B.Release("hot-caves-round"))
assert(not B.Status().busy)
`, "released"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			payloads := barrierLuaReceipts(t, scenario.script)
			root := t.TempDir()
			path := filepath.Join(root, "Cluster", "Master", "save", "mod_config_data", "dst-admin", "snapshot-barrier.json")
			if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
				t.Fatal(err)
			}
			control := &nativeLifecycleControl{}
			native, err := NewNative(root, control)
			if err != nil {
				t.Fatal(err)
			}
			for index, payload := range payloads {
				var published struct {
					BarrierID       string `json:"barrierId"`
					State           string `json:"state"`
					CommitRequested bool   `json:"commitRequested"`
				}
				if err := json.Unmarshal(payload, &published); err != nil {
					t.Fatal(err)
				}
				data := append([]byte("KLEI     1 "), payload...)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				remote, err := NewAgent(barrierArtifactExecutor{data: data})
				if err != nil {
					t.Fatal(err)
				}
				for name, driver := range map[string]SnapshotBarrierDriver{"local": native, "agent": remote} {
					targetID := LocalTargetID
					if name == "agent" {
						targetID = "agent:node"
					}
					receipt, err := driver.SnapshotBarrier(context.Background(), Target{TargetID: targetID, InstallationID: "default", Cluster: "Cluster", Shard: "Master"}, published.BarrierID)
					if err != nil {
						t.Fatalf("%s receipt %d (%s): %v", name, index, published.State, err)
					}
					if receipt.State != published.State || receipt.ProducerVersion != dstruntime.RuntimeVersion || receipt.CommitRequested != published.CommitRequested {
						t.Fatalf("%s receipt lost publisher fields: %+v", name, receipt)
					}
					if receipt.State == "completed" && (receipt.SnapshotAfter != 7187 || receipt.Proof != "save_current_callback" || !receipt.Holding || receipt.InFlight) {
						t.Fatalf("save completion proof lost: %+v", receipt)
					}
					if index == len(payloads)-1 && (receipt.State != scenario.terminal || receipt.Busy || receipt.Holding || receipt.InFlight) {
						t.Fatalf("terminal release proof lost: %+v", receipt)
					}
				}
			}
			if control.starts != 0 || control.stops != 0 || control.index != 0 {
				t.Fatalf("receipt reads changed world lifecycle: %+v", control)
			}
		})
	}
}

func barrierLuaReceipts(t *testing.T, script string) [][]byte {
	t.Helper()
	L := lua.NewState()
	defer L.Close()
	var payloads [][]byte
	L.PreloadModule("json", func(L *lua.LState) int {
		module := L.NewTable()
		L.SetField(module, "encode_compliant", L.NewFunction(func(L *lua.LState) int {
			fields := map[string]interface{}{}
			L.CheckTable(1).ForEach(func(k, v lua.LValue) {
				switch value := v.(type) {
				case lua.LString:
					fields[k.String()] = string(value)
				case lua.LBool:
					fields[k.String()] = bool(value)
				case lua.LNumber:
					fields[k.String()] = float64(value)
				default:
					L.RaiseError("unexpected receipt field %s: %s", k, v.Type())
				}
			})
			data, err := json.Marshal(fields)
			if err != nil {
				L.RaiseError("encode receipt: %v", err)
			}
			L.Push(lua.LString(data))
			return 1
		}))
		L.Push(module)
		return 1
	})
	L.SetGlobal("RecordWrite", L.NewFunction(func(L *lua.LState) int {
		payloads = append(payloads, []byte(L.CheckString(1)))
		return 0
	}))
	if err := L.DoString(`
now=1000;os.time=function()return now end
snapshot=7186;savedCallbacks={};saveCalls=0
TheNet={GetSessionIdentifier=function()return "SESSION"end,GetCurrentSnapshot=function()return snapshot end}
TheShard={GetShardId=function()return "1"end}
TheSim={SetPersistentString=function(self,path,data,encoded,cb)RecordWrite(data);cb(true)end}
TheWorld={ismastersim=true,ismastershard=false,DoStaticTaskInTime=function()return {Cancel=function()end}end}
ShardGameIndex={SaveCurrent=function(self,cb)saveCalls=saveCalls+1;savedCallbacks[#savedCallbacks+1]={callback=cb,before=snapshot}end}
TheWorld.PushEvent=function(self,event)assert(event=="ms_save");ShardGameIndex:SaveCurrent()end
function CompleteSave(advance)
 local item=table.remove(savedCallbacks,1);assert(item)
 if advance~=false then snapshot=item.before+1 end
 if item.callback then item.callback() end
end
`); err != nil {
		t.Fatal(err)
	}
	clock, err := os.ReadFile("../dstruntime/assets/clock.lua")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../dstruntime/assets/barriers.lua")
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := L.LoadString(string(clock) + "\n" + string(source))
	if err != nil {
		t.Fatal(err)
	}
	if err := L.CallByParam(lua.P{Fn: chunk, NRet: 1, Protect: true}); err != nil {
		t.Fatal(err)
	}
	L.SetGlobal("B", L.Get(-1))
	L.Pop(1)
	if err := L.DoString("assert(B.Start())\n" + script); err != nil {
		t.Fatal(err)
	}
	if len(payloads) < 5 {
		t.Fatalf("missing lifecycle receipts: %d", len(payloads))
	}
	return payloads
}

func TestSnapshotBarrierReceiptStillRejectsMalformedOrUnknownFields(t *testing.T) {
	base := fmt.Sprintf(`{"schemaVersion":2,"producerVersion":%q,"producerInstanceId":"instance","barrierId":"hot-test-0001","state":"armed","sessionId":"SESSION","shardId":"1","snapshotBefore":7,"preparedAtUnix":1000`, dstruntime.RuntimeVersion)
	for _, tail := range []string{`,"commitRequested":"true"}`, `,"unexpectedField":true}`, `,"commitRequested":true} {}`, `,"commitRequested":`} {
		driver, err := NewAgent(barrierArtifactExecutor{data: []byte(base + tail)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = driver.SnapshotBarrier(context.Background(), Target{TargetID: "agent:node", InstallationID: "default", Cluster: "Cluster", Shard: "Master"}, "hot-test-0001")
		if !errors.Is(err, ErrSnapshotReceiptInvalid) || RetryableSnapshotBarrier(err) {
			t.Fatalf("invalid receipt was not classified: %s: %v", tail, err)
		}
	}
	driver, _ := NewAgent(barrierArtifactExecutor{data: []byte(base + `,"commitRequested":"true"}`)})
	_, err := driver.SnapshotBarrier(context.Background(), Target{TargetID: "agent:node", InstallationID: "default", Cluster: "Cluster", Shard: "Master"}, "hot-test-0001")
	var mismatch *json.UnmarshalTypeError
	if !errors.As(err, &mismatch) || !strings.Contains(err.Error(), "commitRequested") {
		t.Fatalf("lost original decode evidence: %v", err)
	}
}
