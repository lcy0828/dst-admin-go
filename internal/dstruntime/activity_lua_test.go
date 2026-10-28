package dstruntime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func activityHarness(t testing.TB) *lua.LState {
	t.Helper()
	L := lua.NewState()
	preloadGameJSONHarness(L)
	L.SetGlobal("activityDecode", L.NewFunction(func(L *lua.LState) int {
		var v any
		if err := json.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
			L.RaiseError("decode: %v", err)
			return 0
		}
		var convert func(any) lua.LValue
		convert = func(v any) lua.LValue {
			switch x := v.(type) {
			case map[string]any:
				tb := L.NewTable()
				for k, v := range x {
					tb.RawSetString(k, convert(v))
				}
				return tb
			case []any:
				tb := L.NewTable()
				for _, v := range x {
					tb.Append(convert(v))
				}
				return tb
			case string:
				return lua.LString(x)
			case float64:
				return lua.LNumber(x)
			case bool:
				return lua.LBool(x)
			default:
				return lua.LNil
			}
		}
		L.Push(convert(v))
		return 1
	}))
	if err := L.DoString(`
require("json").decode=activityDecode
now=1800000000;os.time=function()return now end
writes={};files={};timers={}
function entity(id,prefab,x)
 local e={userid=id,prefab=prefab,name="Player",GUID=x,listeners={},x=x}
 e.Transform={GetWorldPosition=function()return e.x,0,10 end}
 function e:ListenForEvent(event,callback)self.listeners[event]=callback end
 function e:RemoveEventCallback(event,callback)if self.listeners[event]==callback then self.listeners[event]=nil end end
 function e:Push(event,data)if self.listeners[event] then self.listeners[event](self,data) end end
 function e:HasTag(tag)return self.epic==true and tag=="epic" end
 return e
end
TheWorld=entity(nil,"world",0);TheWorld.ismastersim=true
TheWorld.Map={GetTileAtPoint=function()return 1 end}
function TheWorld:DoStaticTaskInTime(delay,callback)
 local t={callback=callback,at=now+delay};function t:Cancel()self.cancelled=true end;timers[#timers+1]=t;return t
end
TheNet={GetSessionIdentifier=function()return "SESSION" end};TheShard={GetShardId=function()return "1" end}
TheSim={GetPersistentString=function(self,name,cb)cb(files[name]~=nil,files[name] or "")end,
 SetPersistentString=function(self,name,value,encode,cb)
 assert(name:sub(1,26)=="mod_config_data/dst-admin/")
 files[name]=value;writes[#writes+1]={name=name,value=value};if cb then cb(true) end
 end}
AllPlayers={}
function tick(seconds)
 now=now+seconds
 local pending=timers;timers={}
 for _,task in ipairs(pending)do if not task.cancelled then if task.at<=now then task.callback() else timers[#timers+1]=task end end end
 activity.Poll()
end
function activeTimers()local count=0;for _,task in ipairs(timers)do if not task.cancelled then count=count+1 end end;return count end
`); err != nil {
		L.Close()
		t.Fatal(err)
	}
	// Use the same inlined scheduler as deployed Runtime modules.
	source, err := assetData("activity.lua")
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := L.LoadString(string(source))
	if err != nil {
		t.Fatal(err)
	}
	if err = L.CallByParam(lua.P{Fn: chunk, NRet: 1, Protect: true}); err != nil {
		t.Fatal(err)
	}
	L.SetGlobal("activity", L.Get(-1))
	L.Pop(1)
	return L
}
func TestActivityLuaAllPlayersGroupingCoordinatesAndDisable(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	if err := L.DoString(`
assert(activity.Start())
assert(activeTimers()==0) -- no player timer while idle
p=entity("KU_TEST","wilson",20);AllPlayers={p};TheWorld:Push("ms_playerjoined",p)
local target=entity(nil,"dragonfly",100);target.epic=true
for i=1,100 do p:Push("onhitother",{target=target,damageresolved=2})end
p:Push("onhitother",{target=target,damage=999}) -- not resolved damage
for i=1,20 do p:Push("onpickupitem",{item=entity(nil,"log",i)})end
p.x=25;p:Push("attacked",{attacker=target,damageresolved=3})
p.x=40
tick(5) -- grouped actions retain x=20, not the flush position x=40
p.x=60;tick(5)
p:Push("killed",{victim=target,from_doattack=true})
tick(5)
local config={revision="changed",enabled=false,positions=true,actions=true,combat=true,positionIntervalSeconds=30,minimumDistance=10,combatIntervalSeconds=10,flushIntervalSeconds=5}
assert(activity.Configure(config))
assert(next(p.listeners)==nil)
p:Push("onpickupitem",{item=target});tick(60)
assert(activity.Status().pending==0 and activeTimers()==0)
assert(activity.Stop() and next(TheWorld.listeners)==nil)
`); err != nil {
		t.Fatal(err)
	}
	files := requireLuaTable(t, L.GetGlobal("files"), "files")
	events := []map[string]any{}
	files.ForEach(func(k, v lua.LValue) {
		if strings.Contains(k.String(), "activity-") && !strings.Contains(k.String(), "manifest") && !strings.Contains(k.String(), "activity-config") {
			var segment struct {
				Events []map[string]any `json:"events"`
			}
			if err := json.Unmarshal([]byte(v.String()), &segment); err != nil {
				t.Fatal(err)
			}
			events = append(events, segment.Events...)
		}
	})
	count := map[string]int{}
	for _, event := range events {
		kind := event["kind"].(string)
		count[kind]++
		f := event["fields"].(map[string]any)
		p, _ := event["position"].(map[string]any)
		if kind == "action.pickup" && (f["count"] != float64(20) || p["x"] != float64(20)) {
			t.Fatalf("bad action aggregation: %v", event)
		}
		if kind == "combat.attack" && (f["damage"] != float64(200) || f["unknownDamageHits"] != float64(1) || f["hits"] != float64(101) || p["x"] != float64(20)) {
			t.Fatalf("bad resolved damage or event position: %v", event)
		}
		if kind == "combat.received" && p["x"] != float64(25) {
			t.Fatalf("incoming position %v", event)
		}
	}
	for _, kind := range []string{"player.joined", "action.pickup", "combat.attack", "combat.received", "combat.kill"} {
		if count[kind] != 1 {
			t.Fatalf("expected one %s, counts=%v", kind, count)
		}
	}
}
func TestActivityLuaBoundedQueueAndNoDuplicateListeners(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	if err := L.DoString(`
p=entity("KU_TEST","wilson",10);AllPlayers={p};assert(activity.Start());assert(activity.Start())
for i=1,1000 do p:Push("death",{})end
assert(activity.Status().pending==512 and activity.Status().dropped==488)
for i=1,6 do tick(5)end
assert(activity.Status().pending==0)
TheWorld:Push("ms_playerleft",p)
tick(5)
assert(next(p.listeners)==nil)
assert(activity.Stop() and activeTimers()==0)
`); err != nil {
		t.Fatal(err)
	}
}

func TestActivityLuaPlayerChurnPreservesSamplingDeadlineAndIntervalChanges(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	if err := L.DoString(`
p=entity("KU_MAIN","wilson",1);AllPlayers={p};activity.Start()
for i=1,90 do
 p.x=p.x+20
 tick(1)
 if i%14==0 then
  local newcomer=entity("KU_NEW","wilson",2)
  TheWorld:Push("ms_playerjoined",newcomer)
  TheWorld:Push("ms_playerleft",newcomer)
 end
end
tick(5)
local samples={}
for name,data in pairs(files) do
 if name:match("activity%-%d+%.json$") then
  for _,event in ipairs(require("json").decode(data).events) do
   if event.kind=="position" and event.userId=="KU_MAIN" then samples[#samples+1]=event.occurredAtUnix end
  end
 end
end
table.sort(samples)
assert(#samples==6,"joins/leaves delayed existing player samples")
for i,at in ipairs(samples) do assert(at==1800000000+i*15,"sampling cadence changed") end

local config={revision="interval",enabled=true,positions=true,actions=true,combat=true,
 positionIntervalSeconds=5,minimumDistance=10,combatIntervalSeconds=10,flushIntervalSeconds=5}
assert(activity.Configure(config))
local previous=activity.Status().pending
p.x=p.x+100;tick(5)
assert(activity.Status().pending>previous,"new interval did not take effect")
TheWorld:Push("ms_playerleft",p)
tick(5)
assert(activeTimers()==0,"last player departure left a sampling timer")
`); err != nil {
		t.Fatal(err)
	}
}

func TestActivityLuaDrainPreservesQueuedAndGroupedEvents(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	if err := L.DoString(`
p=entity("KU_MAIN","wilson",1);AllPlayers={p};activity.Start()
for i=1,512 do p:Push("death",{}) end
for i=1,32 do
 p:Push("onhitother",{target=entity(nil,"spider",i),damageresolved=2})
 p:Push("onpickupitem",{item=entity(nil,"item_"..i,i)})
end
local completed=0
assert(activity.Drain(function(ok) assert(ok);completed=completed+1 end))
assert(completed==1 and activity.Status().pending==0 and activity.Status().dropped==0)
local count=0
for name,data in pairs(files) do
 if name:match("activity%-%d+%.json$") then count=count+#require("json").decode(data).events end
end
assert(count==576,"handover dropped an accepted event/group")
assert(activity.Stop())
assert(activity.Drain(function(ok) assert(ok);completed=completed+1 end))
assert(completed==2,"already stopped collector blocked replacement")
`); err != nil {
		t.Fatal(err)
	}
}

func TestActivityLuaDrainFailureTimeoutAndCancellationRetainRecording(t *testing.T) {
	for _, scenario := range []string{"segment_failure", "manifest_failure", "timeout", "late_timeout", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			L := activityHarness(t)
			defer L.Close()
			L.SetGlobal("scenario", lua.LString(scenario))
			if err := L.DoString(`
p=entity("KU_MAIN","wilson",1);AllPlayers={p};activity.Start();p:Push("death",{})
local write=TheSim.SetPersistentString
local held,allowed,completed=nil,true,0
TheSim.SetPersistentString=function(self,name,value,encode,callback)
 if name:match("activity%-%d+%.json$") and scenario=="segment_failure" then callback(false);return end
 if name:match("activity%-manifest%.json$") and scenario=="manifest_failure" then callback(false);return end
 return write(self,name,value,encode,function(written)
  if name:match("activity%-%d+%.json$") then held=function()callback(written)end
  elseif callback then callback(written) end
 end)
end
assert(activity.Drain(function(ok) assert(not ok);completed=completed+1 end,function()return allowed end))
if scenario=="manifest_failure" then held() end
if scenario=="timeout" then tick(5) end
if scenario=="late_timeout" then now=now+5;held() end
if scenario=="cancel" then allowed=false;held() end
assert(completed==1,"failed/cancelled drain did not finish")
TheSim.SetPersistentString=write
if scenario=="timeout" then held() end
assert(completed==1,"retired write completed the drain twice")
assert(not activity.Status().busy and activity.Status().running)
assert(p.listeners.onpickupitem~=nil,"drain disabled game event input")
p:Push("death",{})
assert(activity.Drain(function(ok) assert(ok);completed=completed+1 end))
assert(completed==2 and activity.Status().pending==0 and activity.Status().dropped==0)
local count=0
for name,data in pairs(files) do
 if name:match("activity%-%d+%.json$") then count=count+#require("json").decode(data).events end
end
assert(count==2,"drain failure lost or duplicated queued events")
`); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestActivityLuaDrainBoundsWorkWhenNewEventsKeepArriving(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	if err := L.DoString(`
p=entity("KU_MAIN","wilson",1);AllPlayers={p};activity.Start();p:Push("death",{})
local write=TheSim.SetPersistentString
local batches,completed=0,0
TheSim.SetPersistentString=function(self,name,value,encode,callback)
 return write(self,name,value,encode,function(written)
  if name:match("activity%-%d+%.json$") then batches=batches+1;p:Push("death",{}) end
  if callback then callback(written) end
 end)
end
assert(activity.Drain(function(ok)assert(not ok);completed=completed+1 end))
assert(batches==8 and completed==1,"drain performed unbounded maintenance work")
assert(not activity.Status().busy and activity.Status().pending==1 and activity.Status().dropped==0)
TheSim.SetPersistentString=write
assert(activity.Drain(function(ok)assert(ok);completed=completed+1 end))
assert(completed==2 and activity.Status().pending==0)
local count=0
for name,data in pairs(files) do
 if name:match("activity%-%d+%.json$") then count=count+#require("json").decode(data).events end
end
assert(count==9,"bounded drain lost or duplicated events")
`); err != nil {
		t.Fatal(err)
	}
}

func activityBootstrap(t *testing.T, L *lua.LState) string {
	t.Helper()
	files := requireLuaTable(t, L.GetGlobal("files"), "files")
	for _, name := range managedAssetNames {
		if name == "bootstrap.lua" {
			continue
		}
		source := fmt.Sprintf(`return {version=%q,Start=function()return true end,Stop=function()return true end,
Status=function()return {running=true,ready=true}end,EmitOnce=function(callback)if callback then callback(true)end;return true end}`, RuntimeVersion)
		if name == "activity.lua" {
			data, err := assetData(name)
			if err != nil {
				t.Fatal(err)
			}
			source = string(data)
		}
		L.SetField(files, "mod_config_data/dst-admin/runtime/"+name, lua.LString(source))
	}
	bootstrap, err := assetData("bootstrap.lua")
	if err != nil {
		t.Fatal(err)
	}
	return string(bootstrap)
}

func TestActivityLuaBootstrapReplacesRuntimeDuringContinuousCombat(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	bootstrap := activityBootstrap(t, L)
	if err := L.DoString(`
p=entity("KU_MAIN","wilson",1);AllPlayers={p};activity.Start()
local a=entity(nil,"dragonfly",100);local b=entity(nil,"spider",101)
local hits=0
local function hit(target)p:Push("onhitother",{target=target,damageresolved=1});hits=hits+1 end
hit(a)
for i=1,180 do tick(1);hit(a);hit(b) end
assert(activity.Status().pending>0)
previous={state="running",Activity=activity}
function previous.Stop()
 assert(activity.Status().pending==0 and activity.Status().dropped==0,"replacement discarded accepted records")
 previous.state="stopped";return activity.Stop()
end
function previous.Start()previous.state="running";return activity.Start()end
DSTAdmin=previous
expectedHits=hits
`); err != nil {
		t.Fatal(err)
	}
	if err := L.DoString(bootstrap); err != nil {
		t.Fatal(err)
	}
	if err := L.DoString(`
assert(DSTAdmin~=previous and previous.state=="stopped" and DSTAdmin.state=="running")
local hits=0
for name,data in pairs(files) do
 if name:match("activity%-%d+%.json$") then
  for _,event in ipairs(require("json").decode(data).events) do
   if event.kind=="combat.attack" then hits=hits+event.fields.hits end
  end
 end
end
assert(hits==expectedHits,"handover lost or duplicated combat records")
assert(DSTAdmin.Activity.Status().generation==activity.Status().generation,"new collector lost spool generation")
-- A later Reload after a maintenance token was cleared must also remain usable.
local current=DSTAdmin
assert(current.Reload() and DSTAdmin~=current)
assert(DSTAdmin.Activity.Status().dropped==0)
`); err != nil {
		t.Fatal(err)
	}
}

func TestActivityLuaBootstrapRechecksAsyncDrainBeforeReplacement(t *testing.T) {
	for _, scenario := range []string{"success", "cancel", "timeout", "barrier", "reload_superseded", "after_token_cleared"} {
		t.Run(scenario, func(t *testing.T) {
			L := activityHarness(t)
			defer L.Close()
			bootstrap := activityBootstrap(t, L)
			L.SetGlobal("scenario", lua.LString(scenario))
			if err := L.DoString(`
p=entity("KU_MAIN","wilson",1);AllPlayers={p};activity.Start();p:Push("death",{})
previous={state="running",Activity=activity}
function previous.Stop()previous.state="stopped";return activity.Stop()end
function previous.Start()previous.state="running";return activity.Start()end
DSTAdmin=previous
if scenario~="reload_superseded" then DSTAdminPendingUpdate={id="activity-update"} end
local write=TheSim.SetPersistentString
local hold=true
TheSim.SetPersistentString=function(self,name,value,encode,callback)
 return write(self,name,value,encode,function(written)
  if hold and name:match("activity%-%d+%.json$") then
   hold=false;held=function()callback(written)end
  elseif callback then callback(written) end
 end)
end
`); err != nil {
				t.Fatal(err)
			}
			if err := L.DoString(bootstrap); err != nil {
				t.Fatal(err)
			}
			if err := L.DoString(`
assert(DSTAdmin==previous and previous.state=="running" and held~=nil)
p:Push("death",{}) -- Input arriving during the asynchronous drain must survive.
if scenario=="cancel" then DSTAdminPendingUpdate.aborted=true end
if scenario=="timeout" then tick(5) end
if scenario=="barrier" then previous.Barriers={Status=function()return {holding=true}end} end
if scenario=="reload_superseded" then DSTAdminPendingUpdate={id="newer-update"} end
held()
if scenario=="success" or scenario=="after_token_cleared" then
 assert(DSTAdmin~=previous and DSTAdmin.state=="running" and previous.state=="stopped")
 local count=0
 for name,data in pairs(files) do
  if name:match("activity%-%d+%.json$") then count=count+#require("json").decode(data).events end
 end
 assert(count==2,"input received while draining was lost or duplicated")
 if scenario=="after_token_cleared" then
  DSTAdminPendingUpdate=nil
  local current=DSTAdmin
  assert(current.Reload() and DSTAdmin~=current,"retired maintenance token blocked later reload")
 end
else
 assert(DSTAdmin==previous and previous.state=="running" and activity.Status().running)
 assert(not activity.Status().busy,"failed drain blocked future collection")
 assert(p.listeners.onpickupitem~=nil)
 tick(5)
 assert(activity.Status().pending==0 and activity.Status().dropped==0)
end
`); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func BenchmarkActivityLua100Hits(b *testing.B) {
	L := activityHarness(b)
	defer L.Close()
	if err := L.DoString(`p=entity("KU_TEST","wilson",10);AllPlayers={p};activity.Start();target=entity(nil,"spider",20);function hitBatch()for i=1,100 do p:Push("onhitother",{target=target,damageresolved=2})end end`); err != nil {
		b.Fatal(err)
	}
	fn := L.GetGlobal("hitBatch")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestActivityLuaRetiredReadCannotReviveStoppedCollector(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	if err := L.DoString(`
local read=TheSim.GetPersistentString
local first=true
TheSim.GetPersistentString=function(self,name,callback)
 if first then first=false;retiredRead=callback;return end
 return read(self,name,callback)
end
assert(activity.Start())
assert(activity.Status().busy and not activity.Status().ready)
assert(activity.Stop())
assert(activity.Start() and activity.Status().ready)
local count=#writes
retiredRead(false,"")
assert(activity.Status().ready and #writes==count)
assert(activity.Stop() and activeTimers()==0)
`); err != nil {
		t.Fatal(err)
	}
}

func TestActivityLuaModOutliersDoNotPoisonWholeBatch(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	if err := L.DoString(`
p=entity("KU_TEST","wilson",1);AllPlayers={p};activity.Start()
local target=entity(nil,"mod_boss",2)
p:Push("onhitother",{target=target,damageresolved=1e100})
p.x=1e100;p:Push("death",{})
tick(10)
`); err != nil {
		t.Fatal(err)
	}
	files := requireLuaTable(t, L.GetGlobal("files"), "files")
	data := L.GetField(files, "mod_config_data/dst-admin/activity-01.json").String()
	var segment struct {
		Events []struct {
			Kind     string         `json:"kind"`
			Position any            `json:"position"`
			Fields   map[string]any `json:"fields"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(data), &segment); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range segment.Events {
		if event.Kind == "combat.attack" {
			found = true
			if event.Fields["damage"] != nil || event.Fields["unknownDamageHits"] != float64(1) {
				t.Fatal("outlier claimed as confirmed damage")
			}
		}
		if event.Kind == "player.death" && event.Position != nil {
			t.Fatal("outlier coordinate retained")
		}
	}
	if !found {
		t.Fatal("outlier blocked valid batch")
	}
}

func TestActivityLuaDelayedDiskWriteKeepsFenceUntilCompletion(t *testing.T) {
	for _, path := range []string{"segment", "manifest", "config"} {
		t.Run(path, func(t *testing.T) {
			L := activityHarness(t)
			defer L.Close()
			L.SetGlobal("delayPath", lua.LString(path))
			if err := L.DoString(`
p=entity("KU_MAIN","wilson",1);AllPlayers={p};activity.Start()
local write=TheSim.SetPersistentString
local waiting,held,writesWhileHeld=nil,false,0
TheSim.SetPersistentString=function(self,name,value,encode,callback)
 if waiting then writesWhileHeld=writesWhileHeld+1 end
 local selected=delayPath=="segment" and name:match("activity%-%d+%.json$") or delayPath=="manifest" and name:match("activity%-manifest%.json$") or delayPath=="config" and name:match("activity%-config%.json$")
 if selected and not held then
  held=true
  -- Actual disk mutation and callback both occur later.
  waiting=function()waiting=nil;write(self,name,value,encode,callback)end
  return
 end
 return write(self,name,value,encode,callback)
end
local cfg={revision="delayed",enabled=true,positions=true,actions=true,combat=true,positionIntervalSeconds=15,minimumDistance=10,combatIntervalSeconds=10,flushIntervalSeconds=5}
if delayPath=="config" then assert(activity.Configure(cfg)) else p:Push("death",{});tick(5) end
assert(waiting~=nil and activity.Status().busy)
for i=1,20 do tick(5);p:Push("death",{}) end
assert(activity.Status().writeUnconfirmed)
assert(not activity.Configure(cfg) and not activity.Drain(function()error("drain overlapped pending I/O")end))
assert(not activity.Stop(),"stopped while persistent write still unresolved")
assert(writesWhileHeld==0,"new writes overlapped uncertain old disk I/O")
waiting()
assert(not activity.Status().busy and not activity.Status().writeUnconfirmed)
TheSim.SetPersistentString=write
assert(activity.Drain(function(ok)assert(ok)end))
local m=require("json").decode(files["mod_config_data/dst-admin/activity-manifest.json"])
for _,slot in ipairs(m.slots) do
 local seg=require("json").decode(files["mod_config_data/dst-admin/"..string.format("activity-%02d.json",slot.slot)])
 assert(seg.generation==slot.generation and seg.firstSequence==slot.firstSequence,"late write overwrote committed files")
end
assert(activity.Status().pending==0 and activity.Stop())
`); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestActivityLuaManifestFailureRetriesSameBatchBeforeNewEvents(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	if err := L.DoString(`
p=entity("KU_MAIN","wilson",1);AllPlayers={p};activity.Start()
local write=TheSim.SetPersistentString;local failed=false;local segmentWrites=0;local firstManifest
TheSim.SetPersistentString=function(self,name,value,encode,cb)
 if name:match("activity%-%d+%.json$") then segmentWrites=segmentWrites+1 end
 if name:match("activity%-manifest%.json$") and not failed then failed=true;firstManifest=value;cb(false);return end
 if name:match("activity%-manifest%.json$") and firstManifest then assert(value==firstManifest,"pending manifest bytes changed");firstManifest=nil end
 return write(self,name,value,encode,cb)
end
p:Push("death",{});tick(5)
assert(activity.Status().generation==0 and activity.Status().pending==1)
p:Push("death",{});tick(5)
assert(activity.Status().generation==1 and activity.Status().pending==1 and segmentWrites==1)
tick(5)
assert(activity.Status().generation==2 and activity.Status().pending==0 and segmentWrites==2)
`); err != nil {
		t.Fatal(err)
	}
}

func TestActivityLuaThrowingQueuedWriteCannotUnlockFence(t *testing.T) {
	L := activityHarness(t)
	defer L.Close()
	if err := L.DoString(`
p=entity("KU_MAIN","wilson",1);AllPlayers={p};activity.Start()
local write=TheSim.SetPersistentString;local waiting
TheSim.SetPersistentString=function(self,name,data,encode,callback)
 waiting=function()write(self,name,data,encode,callback)end
 error("native API threw after enqueue")
end
p:Push("death",{});tick(5);tick(15)
assert(activity.Status().writeUnconfirmed and activity.Status().busy)
local first=waiting
tick(60)
assert(waiting==first,"throwing call released the unresolved I/O fence")
TheSim.SetPersistentString=write
first()
assert(not activity.Status().busy and activity.Status().generation==1)
`); err != nil {
		t.Fatal(err)
	}
}
