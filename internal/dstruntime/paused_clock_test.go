package dstruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// The simulation never advances. Only the separate static queue can run work.
func pausedClockControl(t *testing.T) (*maintenanceLuaControl, string) {
	t.Helper()
	_, _, root := newRuntimeTestManager(t)
	path := filepath.Join(root, "Cluster_1", "Master")
	c := newMaintenanceLuaControl(t, path)
	assets, _, err := desiredAssets()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeExecutionAssets(path, assets); err != nil {
		t.Fatal(err)
	}
	if err := c.state.DoString(fmt.Sprintf(`
staticNow=0; wallOffset=0; simulationNow=10; simulationPaused=true; wallStart=%d
GetTime=function() return simulationNow end
GetStaticTime=function() return staticNow end
IsSimPaused=function() return simulationPaused end
os.time=function() return wallStart+math.floor(staticNow+wallOffset) end
staticQueue={}; simulationQueue={}
local function schedule(queue,delay,fn)
    local task={at=staticNow+delay,fn=fn,cancelled=false}
    function task:Cancel() self.cancelled=true end
    queue[#queue+1]=task
    return task
end
staticScheduler={ExecuteInTime=function(self,delay,fn) return schedule(staticQueue,delay,fn) end}
scheduler={ExecuteInTime=function(self,delay,fn) return schedule(simulationQueue,delay,fn) end}
TheWorld.DoStaticTaskInTime=function(self,delay,fn) return schedule(staticQueue,delay,fn) end
TheWorld.DoTaskInTime=function(self,delay,fn) return schedule(simulationQueue,delay,fn) end
TheWorld.DoPeriodicTask=function(self,delay,fn) return schedule(simulationQueue,delay,fn) end
function AdvanceStatic(seconds)
    local deadline=staticNow+seconds
    for count=1,10000 do
        local index,first=nil,nil
        for i,task in ipairs(staticQueue) do
            if not task.cancelled and task.at<=deadline and (first==nil or task.at<first.at) then index,first=i,task end
        end
        if first==nil then staticNow=deadline; return end
        table.remove(staticQueue,index);staticNow=first.at;first.fn()
    end
    error("static queue did not converge")
end
function StaticTaskCount()
    local count=0
    for _,task in ipairs(staticQueue) do if not task.cancelled then count=count+1 end end
    return count
end
`, time.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	return c, path
}

func TestPausedStaticClockCompletesLateFirstActivationAndKeepsSnapshotsFresh(t *testing.T) {
	c, path := pausedClockControl(t)
	if err := c.state.DoString(`savedWorld=TheWorld;TheWorld=nil`); err != nil {
		t.Fatal(err)
	}
	bootstrap, _ := assetData("bootstrap.lua")
	if err := c.state.DoString(string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if err := c.state.DoString(`AdvanceStatic(90);assert(DSTAdmin.state=="waiting_for_world",DSTAdmin.state);TheWorld=savedWorld;AdvanceStatic(6);assert(DSTAdmin.readyAnnounced,require("json").encode(DSTAdmin.Status()));assert(DSTAdmin.state=="running",DSTAdmin.state)`); err != nil {
		t.Fatal(err)
	}
	readHealth := func() Health {
		data, err := os.ReadFile(filepath.Join(path, "save/mod_config_data/dst-admin/health-"+RuntimeVersion+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var health Health
		if err := json.Unmarshal(data, &health); err != nil {
			t.Fatal(err)
		}
		return health
	}
	first := readHealth()
	if err := c.state.DoString(`AdvanceStatic(600);assert(simulationNow==10);assert(#simulationQueue==0);assert(StaticTaskCount()==1)`); err != nil {
		t.Fatal(err)
	}
	last := readHealth()
	if last.Sequence <= first.Sequence || last.HeartbeatSequence <= first.HeartbeatSequence || last.HeartbeatAtUnix == nil || first.HeartbeatAtUnix == nil || *last.HeartbeatAtUnix-*first.HeartbeatAtUnix < 595 || last.Paused == nil || !*last.Paused {
		t.Fatalf("paused collector stopped: first=%+v last=%+v", first, last)
	}
	if err := c.state.DoString(`DSTAdmin.Reload();assert(DSTAdmin.readyAnnounced);assert(StaticTaskCount()==1);DSTAdmin.Stop();AdvanceStatic(30);assert(StaticTaskCount()==0)`); err != nil {
		t.Fatal(err)
	}
}

func TestPausedStaticClockBoundsPerformanceDiagnostics(t *testing.T) {
	c, path := pausedClockControl(t)
	bootstrap, _ := assetData("bootstrap.lua")
	if err := c.state.DoString(string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if err := c.state.DoString(`
local request={requestId="paused-diagnostic-001",profile="performance",durationSeconds=3}
DSTAdmin.Diagnostics.Capture(request)
assert(not DSTAdmin.Diagnostics.Status().busy)
simulationPaused=false
DSTAdmin.Diagnostics.Capture(request)
assert(DSTAdmin.Diagnostics.Status().busy)
simulationPaused=true
AdvanceStatic(5)
assert(not DSTAdmin.Diagnostics.Status().busy)
assert(simulationNow==10)
`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(path, "save/mod_config_data/dst-admin/diagnostic-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report DiagnosticReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if report.OK || report.Code != "SIMULATION_PAUSED" || payload["result"] != nil {
		t.Fatalf("paused response must omit absent result (DST encodes empty tables as arrays): %s", data)
	}
	if _, err := decodeDiagnosticReportData(data, report.SessionID, report.ShardID, time.Now().UTC(), time.Time{}); err != nil {
		t.Fatalf("typed diagnostic reader rejected Lua pause response: %v", err)
	}
	report.OK = true
	report.Code = "DIAGNOSTIC_COMPLETE"
	data, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeDiagnosticReportData(data, report.SessionID, report.ShardID, time.Now().UTC(), time.Time{}); err == nil {
		t.Fatal("successful diagnostic without samples was accepted")
	}
}

func TestPausedStaticClockReleasesExpiredSaveBarrier(t *testing.T) {
	c, _ := pausedClockControl(t)
	bootstrap, _ := assetData("bootstrap.lua")
	if err := c.state.DoString(string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if err := c.state.DoString(`
TheNet.GetCurrentSnapshot=function() return 4 end
assert(DSTAdmin.Barriers.Prepare("paused-barrier-001"))
assert(DSTAdmin.Barriers.Status().busy)
AdvanceStatic(181)
assert(not DSTAdmin.Barriers.Status().busy)
assert(simulationNow==10)
`); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeClockRejectsSimulationSchedulerFallback(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadStaticJSONHarness(state)
	if err := state.DoString(`scheduler={ExecuteInTime=function() error("must not use simulation time") end}`); err != nil {
		t.Fatal(err)
	}
	module := loadLuaModule(t, state, "telemetry.lua")
	err := state.CallByParam(lua.P{Fn: requireLuaFunction(t, state.GetField(module, "Start"), "Start"), NRet: 1, Protect: true})
	if err == nil {
		t.Fatal("missing static scheduler was silently accepted")
	}
}

func TestPausedRefreshCoalescesAndHeartbeatDoesNotFakeSamples(t *testing.T) {
	c, _ := pausedClockControl(t)
	bootstrap, _ := assetData("bootstrap.lua")
	if err := c.state.DoString(string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if err := c.state.DoString(`
local realWrite=TheSim.SetPersistentString
local originalSequence=DSTAdmin.Telemetry.Status().sequence
local originalHeartbeat=DSTAdmin.Telemetry.Status().heartbeatSequence
local originalWorldSequence=DSTAdmin.WorldState.Status().sequence
local delayed=nil
TheSim.SetPersistentString=function(self,path,data,encode,callback)
 if string.find(path,"worldstate-",1,true) then delayed=function() realWrite(self,path,data,encode,callback) end
 else realWrite(self,path,data,encode,callback) end
end
local callbacks=0
for i=1,10 do assert(DSTAdmin.Refresh(function(ok) assert(ok);callbacks=callbacks+1 end)) end
assert(callbacks==0)
AdvanceStatic(30)
assert(DSTAdmin.Telemetry.Status().sequence==originalSequence)
assert(DSTAdmin.Telemetry.Status().heartbeatSequence>originalHeartbeat)
assert(DSTAdmin.WorldState.Status().sequence==originalWorldSequence+1)
TheSim.SetPersistentString=realWrite
delayed()
assert(callbacks==10)
assert(DSTAdmin.Telemetry.Status().sequence==originalSequence+1)
TheNet.GetClientTable=function() error("injected player collection failure") end
AdvanceStatic(5)
assert(DSTAdmin.Telemetry.Status().sequence==originalSequence+1)
assert(DSTAdmin.Telemetry.Status().lastError~=nil)
assert(DSTAdmin.Telemetry.Status().heartbeatSequence>originalHeartbeat+1)
assert(not DSTAdmin.refreshInProgress)
assert(StaticTaskCount()==1)
`); err != nil {
		t.Fatal(err)
	}
}

func TestStopDuringInitialPersistenceCannotReviveRuntime(t *testing.T) {
	c, _ := pausedClockControl(t)
	if err := c.state.DoString(`
local realWrite=TheSim.SetPersistentString
pendingWrites={}
TheSim.SetPersistentString=function(self,path,data,encode,callback)
 if string.find(path,"worldstate-",1,true) then pendingWrites[#pendingWrites+1]=function() realWrite(self,path,data,encode,callback) end
 else realWrite(self,path,data,encode,callback) end
end
`); err != nil {
		t.Fatal(err)
	}
	bootstrap, _ := assetData("bootstrap.lua")
	if err := c.state.DoString(string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if err := c.state.DoString(`
local candidate=DSTAdmin
assert(candidate.refreshInProgress)
assert(candidate.Stop(true))
for _,write in ipairs(pendingWrites) do write() end
AdvanceStatic(30)
assert(DSTAdmin==candidate and candidate.state=="stopped")
assert(not candidate.readyAnnounced)
assert(StaticTaskCount()==0)
`); err != nil {
		t.Fatal(err)
	}
}

func TestPausedBarrierExpiryRetainsFailureAndDrainsDelayedSaves(t *testing.T) {
	c, _ := pausedClockControl(t)
	if err := c.state.DoString(`
snapshot=4;saves=0;onsave=nil
TheNet.GetCurrentSnapshot=function() return snapshot end
ShardGameIndex.SaveCurrent=function(self,callback) saves=saves+1;snapshot=snapshot+1;if callback then callback() end end
`); err != nil {
		t.Fatal(err)
	}
	bootstrap, _ := assetData("bootstrap.lua")
	if err := c.state.DoString(string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if err := c.state.DoString(`
assert(DSTAdmin.Barriers.Prepare("held-paused-barrier"))
assert(DSTAdmin.Barriers.Arm("held-paused-barrier",5))
ShardGameIndex:SaveCurrent()
assert(DSTAdmin.Barriers.Status().holding)
ShardGameIndex:SaveCurrent(function() onsave=true end)
assert(saves==1 and onsave==nil)
AdvanceStatic(181)
assert(not DSTAdmin.Barriers.Status().holding)
assert(saves==2 and onsave==true)
assert(simulationNow==10)
`); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatFreshnessDoesNotUseCopiedFileTime(t *testing.T) {
	now := time.Now().UTC()
	current, old := now.Unix(), now.Add(-time.Minute).Unix()
	for _, test := range []struct {
		name   string
		health Health
		fresh  bool
	}{
		{"legacy fresh", Health{ReadAt: now}, true},
		{"legacy stale", Health{ReadAt: now.Add(-time.Minute)}, false},
		{"heartbeat fresh", Health{ReadAt: now, HeartbeatSequence: 1, HeartbeatAtUnix: &current}, true},
		{"old heartbeat in copied file", Health{ReadAt: now, HeartbeatSequence: 1, HeartbeatAtUnix: &old}, false},
		{"heartbeat fresh but stale transport", Health{ReadAt: now.Add(-time.Minute), HeartbeatSequence: 1, HeartbeatAtUnix: &current}, false},
		{"missing transport timestamp", Health{HeartbeatSequence: 1, HeartbeatAtUnix: &current}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.health.FreshAt(now) != test.fresh {
				t.Fatalf("health freshness=%v", test.health)
			}
		})
	}
}

func TestPostInitCompletesFirstActivationBeforeBothClocksSleep(t *testing.T) {
	c, _ := pausedClockControl(t)
	if err := c.state.DoString(`savedWorld=TheWorld;TheWorld=nil;postInitCalls=0;ModManager={SimPostInit=function()postInitCalls=postInitCalls+1 end};originalPostInit=ModManager.SimPostInit`); err != nil {
		t.Fatal(err)
	}
	bootstrap, _ := assetData("bootstrap.lua")
	if err := c.state.DoString(string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if err := c.state.DoString(`
TheWorld=savedWorld
ModManager:SimPostInit()
assert(postInitCalls==1 and DSTAdmin.readyAnnounced)
assert(StaticTaskCount()==1)
local first=DSTAdmin.Telemetry.Status().heartbeatSequence
wallOffset=600
-- Neither game scheduler runs during native empty-server sleep.
DSTAdmin.Refresh()
assert(DSTAdmin.Telemetry.Status().heartbeatSequence>first)
assert(staticNow==0 and simulationNow==10)
DSTAdmin.Stop(true)
assert(ModManager.SimPostInit==originalPostInit)
assert(StaticTaskCount()==0)
`); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSleepExpiresProtectionBeforeResumedSave(t *testing.T) {
	c, _ := pausedClockControl(t)
	if err := c.state.DoString(`snapshot=4;saves=0;TheNet.GetCurrentSnapshot=function() return snapshot end;ShardGameIndex.SaveCurrent=function(self,cb) saves=saves+1;snapshot=snapshot+1;if cb then cb() end end`); err != nil {
		t.Fatal(err)
	}
	bootstrap, _ := assetData("bootstrap.lua")
	if err := c.state.DoString(string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	if err := c.state.DoString(`
assert(DSTAdmin.Barriers.Prepare("native-sleep-barrier"))
assert(DSTAdmin.Barriers.Arm("native-sleep-barrier",5))
ShardGameIndex:SaveCurrent()
assert(DSTAdmin.Barriers.Status().holding)
wallOffset=181
ShardGameIndex:SaveCurrent()
assert(not DSTAdmin.Barriers.Status().holding)
assert(saves==2)
assert(staticNow==0 and simulationNow==10)
`); err != nil {
		t.Fatal(err)
	}
}
