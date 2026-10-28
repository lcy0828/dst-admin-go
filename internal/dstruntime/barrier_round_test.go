package dstruntime

import (
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func barrierRoundState(t *testing.T) *lua.LState {
	t.Helper()
	state := lua.NewState()
	t.Cleanup(state.Close)
	installStaticSchedulerStub(state)
	if err := state.DoString(`
package.preload.json=function() return {encode_compliant=function(value) receipt=value;return "{}" end} end
now=1000;os.time=function() return now end
snapshot=7186;saveCalls=0;savedCallbacks={};savedShutdown={}
TheNet={GetSessionIdentifier=function()return "SESSION"end,GetCurrentSnapshot=function()return snapshot end}
TheShard={GetShardId=function()return "2"end}
TheSim={SetPersistentString=function(self,path,value,encoded,callback)callback(true)end}
TheWorld={ismastersim=true,ismastershard=false,DoStaticTaskInTime=function()return {Cancel=function()end}end}
ShardGameIndex={SaveCurrent=function(self,callback,shutdown)
 saveCalls=saveCalls+1
 savedCallbacks[#savedCallbacks+1]={callback=callback,before=snapshot}
 savedShutdown[#savedShutdown+1]=shutdown==true
end}
function CompleteSave()
 local item=table.remove(savedCallbacks,1);assert(item,"no pending save")
 snapshot=item.before+1
 if item.callback then item.callback() end
end
`); err != nil {
		t.Fatal(err)
	}
	state.SetGlobal("B", loadLuaModule(t, state, "barriers.lua"))
	if err := state.DoString(`assert(B.Start())`); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestBarrierPrepareDoesNotCaptureAnUnrelatedAutosave(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
assert(B.Prepare("hot-prepare-only"))
ShardGameIndex:SaveCurrent()
CompleteSave()
assert(receipt.state=="prepared", "Prepare consumed an unrelated save callback")
assert(not B.Status().holding, "unrelated save blocked the intended next round")
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierIgnoresEarlierCaveRoundAndCapturesTheMasterTarget(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
assert(B.Prepare("hot-caves-race"))
assert(B.Arm("hot-caves-race",7188))
ShardGameIndex:SaveCurrent()
CompleteSave()
assert(snapshot==7187 and receipt.state=="armed" and not B.Status().holding)
ShardGameIndex:SaveCurrent()
assert(B.Status().busy and not B.Status().holding)
CompleteSave()
assert(receipt.state=="completed" and receipt.snapshotBefore==7187 and receipt.snapshotAfter==7188)
assert(receipt.targetSnapshotAfter==7188 and receipt.proof=="save_current_callback")
assert(B.Status().holding)
assert(B.Release("hot-caves-race"))
assert(not B.Status().busy)
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierRejectsInFlightSaveAndDoesNotLoseItsCallback(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
called=0
ShardGameIndex:SaveCurrent(function()called=called+1 end)
assert(B.Prepare("hot-inflight-race"))
assert(not B.Arm("hot-inflight-race",7188))
assert(receipt.state=="conflict" and receipt.code=="SAVE_IN_PROGRESS")
assert(B.Cancel("hot-inflight-race"))
CompleteSave()
assert(called==1 and not B.Status().busy and not receipt.inFlight)
assert(B.Prepare("hot-after-drain"))
assert(B.Arm("hot-after-drain",7188))
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierQueuesSavesWhileTargetWriteIsInFlightAndDrainsShutdown(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
assert(B.Prepare("hot-target-flight"))
assert(B.Arm("hot-target-flight",7187))
ShardGameIndex:SaveCurrent()
called=0
ShardGameIndex:SaveCurrent(function()called=called+1 end,true)
assert(saveCalls==1 and called==0)
CompleteSave()
assert(B.Status().holding)
assert(B.Release("hot-target-flight"))
assert(saveCalls==2 and savedShutdown[2] and called==0)
CompleteSave()
assert(called==1 and not B.Status().busy)
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierMasterCommitIsIdempotent(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
TheWorld.ismastershard=true
TheWorld.PushEvent=function(self,event) assert(event=="ms_save");ShardGameIndex:SaveCurrent() end
assert(B.Prepare("hot-master-round"))
assert(B.Arm("hot-master-round",0))
assert(receipt.targetSnapshotAfter==7187)
assert(B.Commit("hot-master-round"))
assert(B.Commit("hot-master-round"))
assert(saveCalls==1)
CompleteSave()
assert(B.Commit("hot-master-round"))
assert(saveCalls==1 and receipt.state=="completed")
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierCancelDuringTargetWriteDrainsQueuedCallbacksAndBlocksReload(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
assert(B.Prepare("hot-cancel-write"))
assert(B.Arm("hot-cancel-write",7187))
ShardGameIndex:SaveCurrent()
called=0
ShardGameIndex:SaveCurrent(function()called=called+1 end,true)
assert(not B.Stop())
assert(B.Cancel("hot-cancel-write"))
assert(receipt.busy and receipt.inFlight and not receipt.holding)
CompleteSave()
assert(receipt.busy and receipt.inFlight and called==0 and savedShutdown[2])
CompleteSave()
assert(called==1 and not receipt.busy and not receipt.inFlight)
assert(B.Stop())
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierWrongAttemptAndPassedTargetCannotCaptureSave(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
assert(B.Prepare("hot-owner-round"))
assert(not B.Arm("hot-other-round",7187))
assert(not B.Cancel("hot-other-round"))
assert(not B.Arm("hot-owner-round",7186))
assert(receipt.code=="TARGET_SNAPSHOT_PASSED")
ShardGameIndex:SaveCurrent()
CompleteSave()
assert(not B.Status().holding)
assert(B.Cancel("hot-owner-round"))
assert(not receipt.busy)
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierReceiptWritesAreSerializedAndCoalesced(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
pendingWrite=nil;written={};writingCount=0
package.loaded.json.encode_compliant=function(value) return value.state..":"..tostring(value.busy) end
TheSim.SetPersistentString=function(self,path,data,encoded,callback)
 assert(pendingWrite==nil,"overlapping receipt writes can publish stale state")
 pendingWrite={data=data,callback=callback};writingCount=writingCount+1
end
function FinishReceipt()
 local item=pendingWrite;pendingWrite=nil
 written[#written+1]=item.data;item.callback(true)
end
assert(B.Prepare("hot-serialize-write"))
assert(B.Arm("hot-serialize-write",7187))
ShardGameIndex:SaveCurrent()
CompleteSave()
assert(B.Cancel("hot-serialize-write"))
assert(writingCount==1)
FinishReceipt()
assert(writingCount==2)
FinishReceipt()
assert(pendingWrite==nil and written[#written]=="released:false")
assert(B.Status().lastError==nil)
-- Ordinary autosaves after release do not publish barrier receipts.
ShardGameIndex:SaveCurrent();CompleteSave()
assert(writingCount==2)
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierRejectsOverlappingEarlierWriteAtTargetEntry(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
assert(B.Prepare("hot-overlap-round"))
assert(B.Arm("hot-overlap-round",7188))
ShardGameIndex:SaveCurrent()
-- The native secondary autosaver aligns the counter before calling SaveCurrent.
snapshot=7187
ShardGameIndex:SaveCurrent()
assert(receipt.state=="conflict" and receipt.code=="SAVE_IN_PROGRESS")
CompleteSave();CompleteSave()
assert(not B.Status().holding and receipt.state=="conflict")
assert(B.Cancel("hot-overlap-round") and not receipt.busy)
`); err != nil {
		t.Fatal(err)
	}
}
