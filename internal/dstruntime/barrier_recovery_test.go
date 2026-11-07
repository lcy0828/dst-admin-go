package dstruntime

import (
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func TestBarrierRecoveryRejectsUnrelatedAndInvalidAttempts(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
assert(B.Prepare("hot-current-attempt"));assert(B.Arm("hot-current-attempt",7187))
ShardGameIndex:SaveCurrent();CompleteSave()
for _,id in ipairs({"hot-stale-attempt","",123,"../invalid"}) do
 assert(not B.ForceRollback(id),"unrelated rollback accepted")
 assert(B.Status().holding and B.Status().barrierId=="hot-current-attempt")
end
assert(not B.Cancel("hot-stale-attempt"))
assert(not B.Release("hot-stale-attempt"))
assert(B.Status().holding)
-- A rejected request must not cancel the current attempt's expiry timer.
now=1181;B.Poll()
assert(not B.Status().holding and receipt.state=="failed")
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierRecoveryDrainsNativeWritesAndDelayedShutdown(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
paused=false;endSaveCalls=0
TheNet.IsServerPaused=function()return paused end
TheNet.GetClientTable=function()return {{userid="KU_EXAMPLE",name="example"}}end
TheNet.EndWorldSave=function()endSaveCalls=endSaveCalls+1;paused=false end
assert(B.Prepare("hot-current-write"));assert(B.Arm("hot-current-write",7187))
ShardGameIndex:SaveCurrent();paused=true
called=0;ShardGameIndex:SaveCurrent(function()called=called+1 end,true)
assert(saveCalls==1)
assert(B.ForceRollback("hot-current-write"))
assert(saveCalls==1 and endSaveCalls==0,"recovery raced an unfinished native write")
assert(B.Status().inFlight and receipt.busy and not receipt.holding)
assert(not B.Stop(),"Runtime reload allowed during a write")
assert(not B.Prepare("hot-next-attempt"),"next backup hid an unfinished recovery")
CompleteSave()
assert(saveCalls==2 and endSaveCalls==1 and not paused and savedShutdown[2])
assert(called==0 and receipt.inFlight and not B.Stop())
CompleteSave()
assert(called==1 and not B.Status().busy and not receipt.inFlight)
assert(B.ForceRollback("hot-current-write") and endSaveCalls==1)
assert(B.Stop())
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierRecoveryNeedsObservedUnpauseAndCanBeRetried(t *testing.T) {
	for _, implementation := range []string{
		`function()error("native unpause failed")end`,
		`function()return true end`,
		`function()return false end`,
	} {
		t.Run(implementation, func(t *testing.T) {
			state := barrierRoundState(t)
			state.SetGlobal("recoveryFunctionSource", lua.LString(implementation))
			if err := state.DoString(`
paused=false
TheNet.IsServerPaused=function()return paused end
TheNet.GetClientTable=function()return {{userid="KU_EXAMPLE",name="example"}}end
local failed=assert(loadstring("return "..recoveryFunctionSource))()
TheNet.EndWorldSave=failed;TheNet.SetServerPaused=failed;SetServerPaused=failed
assert(B.Prepare("hot-unpause-failure"));assert(B.Arm("hot-unpause-failure",7187))
ShardGameIndex:SaveCurrent();CompleteSave();paused=true
assert(not B.Release("hot-unpause-failure"),"unverified pause recovery was reported as successful")
assert(paused and receipt.busy and receipt.code=="PAUSE_RESTORE_FAILED")
assert(not B.Prepare("hot-next-attempt"))
assert(not B.Stop())
-- The first native API still fails; the fallback must be verified by reading.
SetServerPaused=function(value)paused=value end
assert(B.Release("hot-unpause-failure"))
assert(not paused and not receipt.busy and receipt.message=="" and receipt.code=="")
assert(B.Prepare("hot-next-attempt"))
`); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBarrierRecoveryPreservesPriorPauseAndUnknownEmptyServerSleep(t *testing.T) {
	for _, prior := range []string{"true", "nil"} {
		t.Run(prior, func(t *testing.T) {
			state := barrierRoundState(t)
			if err := state.DoString(`
paused=` + prior + `;unpauseCalls=0
TheNet.IsServerPaused=function()return paused end
TheNet.EndWorldSave=function()end
TheNet.SetServerPaused=function()unpauseCalls=unpauseCalls+1 end
SetServerPaused=function()unpauseCalls=unpauseCalls+1 end
assert(B.Prepare("hot-preserve-pause"));assert(B.Arm("hot-preserve-pause",7187))
ShardGameIndex:SaveCurrent();CompleteSave()
assert(B.ForceRollback("hot-preserve-pause"))
assert(unpauseCalls==0 and paused==` + prior + ` and not B.Status().busy)
`); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBarrierCapturesPauseAtSaveInsteadOfPrepare(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
paused=false;unpauseCalls=0
TheNet.IsServerPaused=function()return paused end
TheNet.GetClientTable=function()return {{userid="KU_EXAMPLE",name="example"}}end
TheNet.SetServerPaused=function()unpauseCalls=unpauseCalls+1 end
assert(B.Prepare("hot-pause-before-save"));assert(B.Arm("hot-pause-before-save",7187))
paused=true
ShardGameIndex:SaveCurrent();CompleteSave()
assert(B.Release("hot-pause-before-save"))
assert(paused and unpauseCalls==0)
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierReleaseDoesNotRepeatNativeUnpauseAfterSuccessfulSave(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
TheNet.IsServerPaused=function()return false end
TheNet.GetClientTable=function()return {{userid="KU_EXAMPLE",name="example"}}end
calls=0
TheNet.EndWorldSave=function()calls=calls+1;error("native save already ended")end
assert(B.Prepare("hot-already-resumed"));assert(B.Arm("hot-already-resumed",7187))
ShardGameIndex:SaveCurrent();CompleteSave()
assert(B.Release("hot-already-resumed"))
assert(calls==0 and not B.Status().busy)
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierRecoveryLogWaitsForDurableDrainedReceipt(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
notices={};print=function(value)notices[#notices+1]=value end
writes={}
TheSim.SetPersistentString=function(self,path,data,encoded,callback)writes[#writes+1]=callback end
assert(B.Prepare("hot-log-recovery"));assert(B.Arm("hot-log-recovery",7187))
ShardGameIndex:SaveCurrent();CompleteSave()
assert(B.Release("hot-log-recovery"))
assert(#notices==0 and #writes==1)
table.remove(writes,1)(true)
assert(#notices==0 and #writes==1,"old prepared receipt announced recovery")
table.remove(writes,1)(false)
assert(#notices==0,"failed receipt write announced recovery")
assert(B.Release("hot-log-recovery") and #notices==0)
table.remove(writes,1)(true)
assert(#notices==1 and string.find(notices[1],"snapshot barrier recovered id=hot%-log%-recovery"))
assert(B.Release("hot-log-recovery"));table.remove(writes,1)(true)
assert(#notices==1,"idempotent release duplicated recovery notice")
`); err != nil {
		t.Fatal(err)
	}
}

func TestBarrierRecoveryRetainsOriginalSaveFailure(t *testing.T) {
	state := barrierRoundState(t)
	if err := state.DoString(`
paused=false
TheNet.IsServerPaused=function()return paused end
TheNet.GetClientTable=function()return {{userid="KU_EXAMPLE",name="example"}}end
assert(B.Prepare("hot-preserve-error"));assert(B.Arm("hot-preserve-error",7187))
ShardGameIndex:SaveCurrent();CompleteSave()
-- Model a prior native save rejection and a secondary pause recovery failure.
receipt.state="failed";receipt.code="SAVE_WRITE_FAILED";receipt.message="disk unavailable"
paused=true
assert(not B.Cancel("hot-preserve-error"))
assert(receipt.code=="PAUSE_RESTORE_FAILED" and receipt.busy)
TheNet.EndWorldSave=function()paused=false end
assert(B.Cancel("hot-preserve-error"))
assert(not receipt.busy and receipt.state=="failed")
assert(receipt.code=="SAVE_WRITE_FAILED" and receipt.message=="disk unavailable")
`); err != nil {
		t.Fatal(err)
	}
}
