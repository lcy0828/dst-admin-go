package dstruntime

import (
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func TestRuntimeCatalogReadsRegisteredModPrefabsWithoutSpawning(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadJSONHarness(state)
	err := state.DoString(`
 TheSim={SetPersistentString=function(self,path,value,encoded,callback) callback(true) end}
 TheNet={GetSessionIdentifier=function() return "SESSION" end}
 TheShard={GetShardId=function() return "1" end}
 SpawnPrefab=function() error("catalog must never spawn") end
 local factory=function() error("catalog must never invoke prefab constructors") end
 local mod_item={fn=factory}; local overridden={fn=factory}
 Prefabs={goldnugget={fn=factory},mod_sword=mod_item,overridden=overridden,mod_root={fn=nil},["invalid-id"]={fn=factory}}
 STRINGS={NAMES={GOLDNUGGET="金块",MOD_SWORD="模组剑"}}
 local mods={first={modinfo={name="旧模组"},Prefabs={overridden={fn=factory}}},second={modinfo={name="测试模组"},Prefabs={mod_sword=mod_item,overridden=overridden}}}
 ModManager={enabledmods={"first","second"},GetMod=function(self,id) return mods[id] end}
 `)
	if err != nil {
		t.Fatal(err)
	}
	module := loadLuaModule(t, state, "commands.lua")
	execute := requireLuaFunction(t, state.GetField(module, "Execute"), "Execute")
	query := func(id, source, search string, offset, limit int) *lua.LTable {
		request := state.NewTable()
		state.SetField(request, "requestId", lua.LString(id))
		state.SetField(request, "action", lua.LString("catalog.entities"))
		args := state.NewTable()
		state.SetField(args, "source", lua.LString(source))
		state.SetField(args, "query", lua.LString(search))
		state.SetField(args, "offset", lua.LNumber(offset))
		state.SetField(args, "limit", lua.LNumber(limit))
		state.SetField(request, "arguments", args)
		if err := state.CallByParam(lua.P{Fn: execute, NRet: 1, Protect: true}, request); err != nil {
			t.Fatal(err)
		}
		result := requireLuaTable(t, state.Get(-1), "result")
		state.Pop(1)
		return result
	}
	result := query("catalog-request-0001", "all", "", 0, 1)
	if !lua.LVAsBool(state.GetField(result, "ok")) {
		t.Fatal(result)
	}
	details := requireLuaTable(t, state.GetField(result, "details"), "details")
	if state.GetField(details, "total") != lua.LNumber(3) || !lua.LVAsBool(state.GetField(details, "hasMore")) {
		t.Fatal(details)
	}
	items := requireLuaTable(t, state.GetField(details, "items"), "items")
	first := requireLuaTable(t, items.RawGetInt(1), "first")
	if state.GetField(first, "id").String() != "goldnugget" {
		t.Fatal(first)
	}
	result = query("catalog-request-0002", "mods", "测试", 0, 60)
	details = requireLuaTable(t, state.GetField(result, "details"), "details")
	if state.GetField(details, "total") != lua.LNumber(2) {
		t.Fatal(details)
	}
	first = requireLuaTable(t, requireLuaTable(t, state.GetField(details, "items"), "items").RawGetInt(1), "first")
	if state.GetField(first, "modId").String() != "second" || state.GetField(first, "nameZhCN").String() != "模组剑" {
		t.Fatal(first)
	}
	result = query("catalog-request-0003", "first", "", 0, 60)
	if state.GetField(requireLuaTable(t, state.GetField(result, "details"), "details"), "total") != lua.LNumber(0) {
		t.Fatal(result)
	}
	result = query("catalog-request-0004", "all", "", 0, 121)
	if lua.LVAsBool(state.GetField(result, "ok")) {
		t.Fatal("unbounded catalog request accepted")
	}
}

func TestRuntimeCatalogCacheLifecycleAndBoundedModWork(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadJSONHarness(state)
	if err := state.DoString(`
 local clock=1800000000
 os.time=function() return clock end
 advance=function(seconds) clock=clock+seconds end
 TheSim={SetPersistentString=function(self,path,value,encoded,callback) callback(true) end}
 Prefabs={one={fn=function() error("must not spawn") end}}
 mod_reads=0; tasks={}; work={}; last_receipt=nil
 local json=require("json"); local encode=json.encode_compliant
 json.encode_compliant=function(receipt) last_receipt=receipt; return encode(receipt) end
 ModManager={enabledmods={"mod"},GetMod=function() mod_reads=mod_reads+1; return {Prefabs={one=Prefabs.one}} end}
 TheWorld={DoTaskInTime=function() error("must work while paused") end,DoStaticTaskInTime=function(self,delay,fn)
   local task={fn=fn,Cancel=function(self) self.cancelled=true end}
   if delay==300 then tasks[#tasks+1]=task else assert(delay==1/30);work[#work+1]=task end
   return task
 end}
 `); err != nil {
		t.Fatal(err)
	}
	module := loadLuaModule(t, state, "commands.lua")
	state.SetGlobal("commands", module)
	if err := state.DoString(`
 local request_id=0
 local function read(args)
   request_id=request_id+1
   local value=commands.Execute({requestId=string.format("catalog-cache-%08d",request_id),action="catalog.entities",arguments=args or {}})
   while value.code=="COMMAND_ACCEPTED" do
     local task=table.remove(work,1);assert(task,"missing scheduled catalog work")
     if not task.cancelled then task.fn() end
     if last_receipt and last_receipt.requestId==string.format("catalog-cache-%08d",request_id) then value=last_receipt end
   end
   return value
 end
 assert(mod_reads==0 and #tasks==0, "loading must not build the catalog")
 local initial=read().details
 assert(mod_reads==1 and #tasks==1)
 assert(read({source="mods",query="one"}).details.total==1)
 assert(read({source="mods",query="one",offset=1}).details.total==1)
 assert(mod_reads==1, "search and pagination must reuse registrations")
 Prefabs.two={fn=function() error("must not spawn") end}
 advance(10)
 local cached=read().details
 assert(cached.total==1 and cached.observedAt==initial.observedAt, "cached timestamp must not advance")
 local refreshed=read({refresh=true}).details
 assert(refreshed.total==2 and refreshed.observedAt~=initial.observedAt)
 assert(tasks[1].cancelled and #tasks==2 and mod_reads==2)
 tasks[1].fn()
 assert(read().details.total==2 and mod_reads==2, "old expiry must not discard a newer index")
 tasks[2].fn()
 read()
 assert(mod_reads==3, "expiry callback must release the unused index")
 advance(300)
 read()
 assert(mod_reads==4, "age check must also enforce TTL while game tasks are paused")
 Prefabs={replacement={fn=function() end}}
 assert(read().details.items[1].id=="replacement", "replaced registry must invalidate the index")
 assert(mod_reads==5)
 ModManager.enabledmods={}
 for i=1,501 do ModManager.enabledmods[i]="mod"..i end
 assert(read({refresh=true}).code=="CATALOG_TOO_LARGE", "mod enumeration must be bounded")
 assert(mod_reads==505, "only 500 mod tables may be examined per build")
 ModManager.enabledmods={"huge"}
 local many={}
 for i=1,50001 do many["entity"..i]={} end
 ModManager.GetMod=function() return {Prefabs=many} end
 assert(read({refresh=true}).code=="CATALOG_TOO_LARGE", "total mod registration work must be bounded")
 ModManager.enabledmods={}
 assert(read().ok, "failed builds must not poison subsequent reads")
 commands.ClearCatalog()
 assert(tasks[#tasks].cancelled, "runtime stop/reload must release the index and expiry task")
 `); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCatalogYieldsAcrossStaticFramesAndCancelsOnStop(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadJSONHarness(state)
	if err := state.DoString(`
 local cpu=0;local wall=1800000000
 os.time=function() return wall end
 advance=function(seconds) wall=wall+seconds end
 os.clock=function() cpu=cpu+.001;return cpu end
 TheSim={SetPersistentString=function(self,path,value,encoded,callback) callback(true) end}
 Prefabs={};for i=1,2000 do Prefabs[string.format("prefab_%05d",i)]={fn=function() error("never spawn") end} end
 tasks={};expiry={};receipts={}
 local json=require("json");local encode=json.encode_compliant
 json.encode_compliant=function(receipt) receipts[#receipts+1]=receipt;return encode(receipt) end
 TheWorld={DoTaskInTime=function() error("simulation scheduler is paused") end,DoStaticTaskInTime=function(self,delay,fn)
   local task={fn=fn,Cancel=function(self) self.cancelled=true end}
   if delay==300 then expiry[#expiry+1]=task else tasks[#tasks+1]=task end
   return task
 end}
 `); err != nil {
		t.Fatal(err)
	}
	module := loadLuaModule(t, state, "commands.lua")
	state.SetGlobal("commands", module)
	if err := state.DoString(`
 local initial=commands.Execute({requestId="catalog-yield-00001",action="catalog.entities",arguments={limit=120}})
 assert(initial.code=="COMMAND_ACCEPTED" and #receipts==0, "cold collection must yield before completing")
 local frames=0
 while #tasks>0 do
   local task=table.remove(tasks,1);if not task.cancelled then task.fn();frames=frames+1 end
 end
 assert(frames>1 and #receipts==1 and receipts[1].ok)
 local data=receipts[1].details
 assert(data.total==2000 and #data.items==120 and #expiry==1)
 for index,item in ipairs(data.items) do assert(item.id==string.format("prefab_%05d",index),"cross-frame sorting lost order") end
 commands.Execute({requestId="catalog-yield-00002",action="catalog.entities",arguments={query="prefab_",offset=120,limit=120}})
 assert(#receipts==1, "search must also yield")
 while #tasks>0 do local task=table.remove(tasks,1);if not task.cancelled then task.fn() end end
 assert(#receipts==2 and receipts[2].details.total==2000 and receipts[2].details.items[1].id=="prefab_00121")
 local page=commands.Execute({requestId="catalog-page-00001",action="catalog.entities",arguments={query="prefab_",offset=240,limit=120}})
 assert(page.code=="CATALOG_READY" and #tasks==0, "cached pagination must not repeat filtering")
 commands.Execute({requestId="catalog-yield-00003",action="catalog.entities",arguments={refresh=true}})
 assert(#tasks==1)
 commands.ClearCatalog()
 assert(tasks[1].cancelled and expiry[1].cancelled and receipts[#receipts].code=="CATALOG_CANCELLED")
 assert(not commands.Status().busy, "stopping must release command dispatch")
 local receipt_count=#receipts
 tasks[1].fn()
 assert(#receipts==receipt_count and #tasks==1, "cancelled callbacks must not resume collection")
 tasks={}
 commands.Execute({requestId="catalog-yield-00004",action="catalog.entities",arguments={}})
 assert(#tasks==1)
 advance(10);table.remove(tasks,1).fn()
 assert(receipts[#receipts].code=="CATALOG_TIMEOUT" and not commands.Status().busy and #tasks==0)
 assert(commands.Execute({requestId="catalog-ping-000001",action="system.ping"}).ok, "deadline must release subsequent commands")
 `); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCatalogBatchesBoundEscapedBytesAndRetainEveryEntry(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadGameJSONHarness(state)
	if err := state.DoString(`
 TheSim={SetPersistentString=function(self,path,value,encoded,callback) callback(true) end}
 Prefabs={};STRINGS={NAMES={}}
 os.clock=function() return 0 end
 for i=1,6121 do Prefabs[string.format("prefab_%05d",i)]={fn=function() error("must not spawn") end} end
 `); err != nil {
		t.Fatal(err)
	}
	state.SetGlobal("commands", loadLuaModule(t, state, "commands.lua"))
	if err := state.DoString(`
 local json=require("json");local calls=0
 local function read_all()
   local offset, seen, pages=0,{},0
   repeat
     calls=calls+1;pages=pages+1
     local value=commands.Execute({requestId=string.format("catalog-batch-%08d",calls),action="catalog.entities",arguments={snapshot=true,limit=2048,offset=offset,refresh=offset==0}})
     assert(value.ok, value.code)
     local data=value.details
     assert(data.total==6121 and data.offset==offset and #data.items>0 and #data.items<=2048)
     assert(#json.encode_compliant(data.items)<=128*1024, "escaped JSON exceeded the batch budget")
     for _,item in ipairs(data.items) do assert(not seen[item.id],"duplicate item");seen[item.id]=true end
     offset=offset+#data.items
     if not data.hasMore then assert(offset==6121);break end
   until pages>200
   assert(offset==6121,"partial collection")
   return pages
 end
 assert(read_all()<10,"ordinary directory still requires too many commands")
 -- Worst-case names shrink the batch; they must not truncate the directory or
 -- accidentally calculate hasMore from the requested rather than actual size.
 for i=1,6121 do STRINGS.NAMES[string.format("PREFAB_%05d",i)]=string.rep('\1',160) end
 assert(read_all()>10)
 local rejected=commands.Execute({requestId="catalog-batch-invalid",action="catalog.entities",arguments={snapshot=true,limit=2049}})
 assert(not rejected.ok and rejected.code=="INVALID_SEARCH")
 `); err != nil {
		t.Fatal(err)
	}
}

func TestCommandCompletionCoalescesFreshHealthWithoutResamplingPlayers(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadGameJSONHarness(state)
	if err := state.DoString(`
 health_writes={};health_callbacks={};command_callback=nil;player_writes=0
 TheSim={SetPersistentString=function(self,path,value,encoded,callback)
   if string.find(path,"health.json",1,true) then
     health_writes[#health_writes+1]=value;health_callbacks[#health_callbacks+1]=callback
   elseif string.find(path,"command-receipt-",1,true) then command_callback=callback
   else player_writes=player_writes+1;callback(true) end
 end}
 TheNet={GetSessionIdentifier=function() return "SESSION" end,GetClientTable=function() return {} end}
 TheShard={GetShardId=function() return "1" end}
 TheWorld={ismastersim=true,DoPeriodicTask=function() return {Cancel=function() end} end,
   DoStaticTaskInTime=function() return {Cancel=function() end} end}
 Prefabs={one={fn=function() error("must not spawn") end}}
 DSTAdmin={}
 `); err != nil {
		t.Fatal(err)
	}
	state.SetField(state.GetGlobal("DSTAdmin"), "Telemetry", loadLuaModule(t, state, "telemetry.lua"))
	state.SetField(state.GetGlobal("DSTAdmin"), "Commands", loadLuaModule(t, state, "commands.lua"))
	if err := state.DoString(`
 DSTAdmin.Telemetry.Start()
 DSTAdmin.Commands.Execute({requestId="catalog-health-00001",action="catalog.entities",arguments={}})
 assert(DSTAdmin.Commands.Status().busy)
 DSTAdmin.Telemetry.EmitOnce()
 assert(#health_writes==1 and string.find(health_writes[1],'"busy":true',1,true))
 command_callback(true)
 assert(not DSTAdmin.Commands.Status().busy and #health_writes==1, "health writes overlapped")
 health_callbacks[1](true)
 assert(#health_writes==2 and string.find(health_writes[2],'"busy":false',1,true), "busy snapshot survived completion")
 health_callbacks[2](true)
 DSTAdmin.Commands.Execute({requestId="catalog-health-00002",action="catalog.entities",arguments={}})
 command_callback(true)
 assert(#health_writes==2 and player_writes==1,"idle completion caused redundant sampling/writes")
 `); err != nil {
		t.Fatal(err)
	}
}
