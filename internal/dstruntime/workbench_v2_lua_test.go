package dstruntime

import (
	lua "github.com/yuin/gopher-lua"
	"testing"
)

// Executes the shipped Lua adapters. Game methods below are instrumented test
// doubles; these tests do not claim actual DST engine/component acceptance.
func workbenchV2Lua(t *testing.T) *lua.LState {
	t.Helper()
	L := lua.NewState()
	t.Cleanup(L.Close)
	preloadGameJSONHarness(L)
	var source string
	for _, name := range []string{"nearby_adapters.lua", "player_settings.lua", "workbench_v2.lua"} {
		b, e := embeddedAssets.ReadFile("assets/" + name)
		if e != nil {
			t.Fatal(e)
		}
		source += string(b) + "\n"
	}
	if e := L.DoString(source + `;W=WorkbenchV2`); e != nil {
		t.Fatal(e)
	}
	const setup = `
 now=100;os.time=function()return now end;os.clock=function()return 0 end
 local native={};function def(name,t)native[name]=t;package.preload["components/"..name]=function()return t end;return t end
 mutations=0;last=nil;print=function(s)last=s end
 EntityScript={Remove=function(e)mutations=mutations+1;e.valid=false end}
 function entity(prefab,x)
  local e={GUID=x,prefab=prefab,valid=true,tags={},components={},entity={GetParent=function()end},Transform={GetWorldPosition=function()return x,0,0 end,GetRotation=function()return 0 end}}
  function e:IsValid()return self.valid end;function e:HasTag(t)return self.tags[t]==true end;e.Remove=EntityScript.Remove
  return e
 end
 p=entity("wilson",0);p.userid="KU_EXAMPLE";p.tags.player=true
 p.components.health={IsDead=function()return false end}
 UserToPlayer=function(id)if id==p.userid then return p end end
 TheWorld={GUID=100};entities={};TheSim={FindEntities=function()return entities end}
 invclass=def("inventory",{GiveItem=function(s,item,slot)
  mutations=mutations+1
  local dest=s.itemslots[slot]
  if dest then dest.components.stackable:Put(item) else s.itemslots[slot]=item;item.components.inventoryitem.owner=p end
 end})
 itemclass=def("inventoryitem",{OnPickup=function()end})
 stackclass=def("stackable",{
  StackSize=function(s)return s.n end,RoomLeft=function(s)return 40-s.n end,
  CanStackWith=function(s,item)return s.inst.prefab==item.prefab end,
  Get=function(s,n)mutations=mutations+1;s.n=s.n-n;local e=ground(s.inst.prefab,99,n);return e end,
  Put=function(s,item)mutations=mutations+1;s.n=s.n+item.components.stackable.n;item.valid=false end
 })
 function ground(prefab,x,n)
  local e=entity(prefab,x)
  e.components.inventoryitem=setmetatable({canbepickedup=true,cangoincontainer=true,IsHeld=function(s)return s.owner~=nil end},{__index=itemclass})
  e.components.stackable=setmetatable({inst=e,n=n},{__index=stackclass})
  return e
 end
 inv=setmetatable({maxslots=2,itemslots={},CanTakeItemInSlot=function()return true end},{__index=invclass});p.components.inventory=inv
 pickclass=def("pickable",{
  CanBePicked=function(s)return s.ready end, CanBeFertilized=function(s)return s.cycles_left==0 end,
  FinishGrowing=function(s)mutations=mutations+1;s.ready=true end,
  Fertilize=function(s)mutations=mutations+1;s.cycles_left=5 end,
  Pick=function(s,player)
   mutations=mutations+1;s.ready=false;local e=ground(s.product,99,1);inv:GiveItem(e,1);return true,{e}
  end
 })
 function plant(x,ready)
  local e=entity("sapling",x);e.components.pickable=setmetatable({product="twigs",numtoharvest=1,ready=ready,cycles_left=5,caninteractwith=true,task=true},{__index=pickclass});return e
 end
 request="00000000-0000-4000-8000-000000000001";token="00000000-0000-4000-8000-000000000002"
 function run(action,phase,id) return W.Nearby(p.userid,action,10,"",phase,id or token,token,"caller-a",request) end
 function setting(action,value,amount,phase)return W.Setting(p.userid,action,value,amount,phase,token,token,"caller-a",request)end
 `
	if e := L.DoString(setup); e != nil {
		t.Fatal(e)
	}
	return L
}

func TestWorkbenchV2GroundPickupEvidenceAndFrozenOwnership(t *testing.T) {
	scenarios := map[string]string{
		"whole stack":           `local e=ground("twigs",1,10);entities={e};local r=run("pick","preview");assert(r.matched==1 and mutations==0);r=run("pick","execute");assert(r.verified==1 and r.units==10 and inv.itemslots[1]==e)`,
		"merged stack":          `local e=ground("twigs",1,3);local dest=ground("twigs",0,5);dest.components.inventoryitem.owner=p;inv.itemslots[1]=dest;entities={e};run("pick","preview");local r=run("pick","execute");assert(r.verified==1 and r.units==3 and not e:IsValid() and dest.components.stackable.n==8)`,
		"partial stack":         `local e=ground("twigs",1,5);local dest=ground("twigs",0,38);dest.components.inventoryitem.owner=p;inv.itemslots[1]=dest;inv.itemslots[2]=ground("flint",0,40);entities={e};run("pick","preview");local r=run("pick","execute");assert(r.verified==1 and r.units==2 and e:IsValid() and e.components.stackable.n==3 and dest.components.stackable.n==40)`,
		"full inventory":        `entities={ground("twigs",1,2)};inv.itemslots={ground("flint",0,40),ground("flint",0,40)};run("pick","preview");local r=run("pick","execute");assert(r.skipped==1 and r.attempted==0 and mutations==0 and r.items[1].reason=="INVENTORY_FULL")`,
		"owner changed":         `entities={ground("twigs",1,2)};run("pick","preview");entities[1].components.inventoryitem.owner={};local r=run("pick","execute");assert(r.skipped==1 and mutations==0)`,
		"prefab changed":        `entities={ground("twigs",1,2)};run("pick","preview");entities[1].prefab="flint";local r=run("pick","execute");assert(r.skipped==1 and mutations==0)`,
		"count changed":         `entities={ground("twigs",1,2)};run("pick","preview");entities[1].components.stackable.n=3;local r=run("pick","execute");assert(r.skipped==1 and mutations==0)`,
		"receiver changed":      `entities={ground("twigs",1,2)};run("pick","preview");inv.itemslots[1]=ground("twigs",0,5);local r=run("pick","execute");assert(r.skipped==1 and mutations==0)`,
		"player moved":          `entities={ground("twigs",1,2)};run("pick","preview");p.Transform.GetWorldPosition=function()return 5,0,0 end;local r=run("pick","execute");assert(r.skipped==1 and mutations==0)`,
		"new targets excluded":  `entities={ground("twigs",1,2)};run("pick","preview");local old=entities[1];entities={ground("flint",1,3)};local r=run("pick","execute");assert(r.matched==1 and inv.itemslots[1]==old)`,
		"throws after mutation": `entities={ground("twigs",1,2)};run("pick","preview");invclass.GiveItem=function(s,item,slot)mutations=mutations+1;item.components.inventoryitem.owner=p;error("after write")end;local r=run("pick","execute");assert(r.unconfirmed==1 and r.failed==0 and mutations==1);assert(not pcall(run,"pick","execute"))`,
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			L := workbenchV2Lua(t)
			if e := L.DoString(scenario); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestWorkbenchV2PlantSemanticsAndBudgets(t *testing.T) {
	scenarios := map[string]string{
		"ripe":                          `entities={plant(1,false)};run("ripen","preview");local r=run("ripen","execute");assert(r.verified==1 and entities[1].components.pickable.ready)`,
		"one stage":                     `entities={plant(1,false)};run("grow","preview");local r=run("grow","execute");assert(r.verified==1 and mutations==1)`,
		"harvest":                       `entities={plant(1,true)};run("harvest","preview");local r=run("harvest","execute");assert(r.verified==1 and r.units==1 and inv.itemslots[1].prefab=="twigs")`,
		"harvest merged uncertain":      `entities={plant(1,true)};inv.itemslots[1]=ground("twigs",0,5);run("harvest","preview");local r=run("harvest","execute");assert(r.unconfirmed==1 and r.units==0)`,
		"fertilizer cleanup":            `entities={plant(1,false)};entities[1].components.pickable.cycles_left=0;local f=entity("compostwrap",0);SpawnPrefab=function()return f end;run("fertilize","preview");local r=run("fertilize","execute");assert(r.verified==1 and not f:IsValid())`,
		"fertilizer exception cleanup":  `entities={plant(1,false)};entities[1].components.pickable.cycles_left=0;local f=entity("compostwrap",0);SpawnPrefab=function()return f end;run("fertilize","preview");pickclass.Fertilize=function(s)s.cycles_left=5;error("mutated")end;local r=run("fertilize","execute");assert(r.unconfirmed==1 and r.failed==0 and not f:IsValid())`,
		"unknown component no fallback": `entities={plant(1,false)};entities[1].prefab="custom_plant";entities[1].components.timer={SetTimeLeft=function()error("timer reset")end};assert(run("grow","preview").matched==0 and mutations==0)`,
		"overridden component excluded": `entities={plant(1,true)};entities[1].components.pickable.Pick=function()error("mod")end;assert(run("harvest","preview").matched==0 and mutations==0)`,
		"dense scan":                    `for i=1,513 do entities[i]=plant(i,true)end;assert(not pcall(run,"harvest","preview") and mutations==0)`,
		"too many no truncation":        `for i=1,41 do entities[i]=plant(i,true)end;assert(not pcall(run,"harvest","preview") and mutations==0)`,
		"unit budget":                   `for i=1,6 do entities[i]=ground("twigs",i,40)end;assert(not pcall(run,"pick","preview") and mutations==0)`,
		"expiry boundary":               `entities={plant(1,false)};run("ripen","preview");now=130;assert(not pcall(run,"ripen","execute") and mutations==0)`,
		"cancel":                        `entities={plant(1,false)};run("ripen","preview");assert(run("ripen","cancel").state=="cancelled");assert(not pcall(run,"ripen","execute") and mutations==0)`,
		"world changed":                 `entities={plant(1,false)};run("ripen","preview");TheWorld={GUID=100};assert(not pcall(run,"ripen","execute") and mutations==0)`,
		"rate budget":                   `run("ripen","preview");run("ripen","preview");assert(not pcall(run,"ripen","preview") and mutations==0)`,
		"cpu budget no writes":          `local clock=0;os.clock=function()clock=clock+.005;return clock end;entities={plant(1,false)};assert(not pcall(run,"ripen","preview") and mutations==0)`,
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			L := workbenchV2Lua(t)
			if e := L.DoString(scenario); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestWorkbenchV2SettingsOwnershipAndNativeTransitions(t *testing.T) {
	const setup = `
 p.events={};p.ListenForEvent=function(s,k,fn)s.events[k]=fn end;p.RemoveEventCallback=function(s,k,fn)if s.events[k]==fn then s.events[k]=nil end end
 mods={_base=1,_modifiers={foreign={modifiers={hunger=2}}},SetModifier=function(s,source,value,key)mutations=mutations+1;if value==1 then s._modifiers[source]=nil else s._modifiers[source]={modifiers={[key]=value}} end end,RemoveModifier=function(s,source)mutations=mutations+1;s._modifiers[source]=nil end}
 mods._fn=function(a,b)return a*b end;mods.Get=function()return 1 end
 package.preload["util/sourcemodifierlist"]=function()return {multiply=mods._fn,Get=mods.Get,SetModifier=mods.SetModifier,RemoveModifier=mods.RemoveModifier}end
 p.components.hunger={burnratemodifiers=mods}
 p.sg={HasStateTag=function()return false end}
 TUNING={WANDA_MIN_YEARS_OLD=20,WANDA_MAX_YEARS_OLD=80,WANDA_OLDAGER=60,WANDA_AGE_THRESHOLD_YOUNG=.6667,WANDA_AGE_THRESHOLD_OLD=.3333}
 healthclass=def("health",{SetVal=function()end,DoDelta=function()end,SetPercent=function(s,v)mutations=mutations+1;s.currenthealth=s.maxhealth*v end})
 oldclass=def("oldager",{StopDamageOverTime=function(s)mutations=mutations+1;p.age_state=p.components.health.currenthealth>45 and "young" or p.components.health.currenthealth<15 and "old" or "normal" end})
 wereclass=def("wereness",{SetPercent=function(s,v)mutations=mutations+1;s.percent=v end,GetPercent=function(s)return s.percent end})
 function wanda()p.prefab="wanda";p.age_state="normal";p.components.health=setmetatable({currenthealth=30,maxhealth=60,GetMaxWithPenalty=function()return 60 end,IsDead=function()return false end},{__index=healthclass});p.components.oldager=setmetatable({damage_remaining=0},{__index=oldclass})end
 `
	scenarios := map[string]string{
		"hunger zero preserves foreign":          `setting("set_player_hunger_rate","set",0,"preview");assert(mutations==0);local r=setting("set_player_hunger_rate","set",0,"execute");assert(r.verified==1 and mods._modifiers.foreign.modifiers.hunger==2);local n=0;for _ in pairs(mods._modifiers)do n=n+1 end;assert(n==2)`,
		"hunger remove own only":                 `setting("set_player_hunger_rate","set",.5,"preview");setting("set_player_hunger_rate","set",.5,"execute");now=103;setting("set_player_hunger_rate","remove",0,"preview");local r=setting("set_player_hunger_rate","remove",0,"execute");assert(r.verified==1 and mods._modifiers.foreign and p.events.onremove==nil)`,
		"hunger reload cleanup":                  `setting("set_player_hunger_rate","set",.5,"preview");setting("set_player_hunger_rate","set",.5,"execute");W.Stop();assert(mods._modifiers.foreign and p.events.onremove==nil);local n=0;for _ in pairs(mods._modifiers)do n=n+1 end;assert(n==1)`,
		"hunger disconnect cleanup":              `setting("set_player_hunger_rate","set",2,"preview");setting("set_player_hunger_rate","set",2,"execute");p.events.onremove();local n=0;for _ in pairs(mods._modifiers)do n=n+1 end;assert(n==1 and mods._modifiers.foreign)`,
		"wanda native age":                       `wanda();setting("set_wanda_age","old",0,"preview");assert(mutations==0);local r=setting("set_wanda_age","old",0,"execute");assert(r.verified==1 and r.items[1].reason==nil and p.components.health.currenthealth==6 and p.age_state=="old")`,
		"hunger foreign implementation rejected": `require("util/sourcemodifierlist");mods._fn=function()return 0 end;assert(not pcall(setting,"set_player_hunger_rate","set",.5,"preview") and mutations==0)`,
		"wanda uninitialized age rejected":       `wanda();p.age_state=nil;assert(not pcall(setting,"set_wanda_age","old",0,"preview") and mutations==0)`,
		"wanda health replaced after preview":    `wanda();setting("set_wanda_age","old",0,"preview");local original=p.components.health;wanda();local r=setting("set_wanda_age","old",0,"execute");assert(r.skipped==1 and mutations==0 and original.currenthealth==30)`,
		"woodie synchronous confirmation":        `p.prefab="woodie";p.tags.wereplayer=true;p.components.wereness=setmetatable({percent=.5},{__index=wereclass});wereclass.SetPercent=function(s,v)mutations=mutations+1;s.percent=v;p.tags.wereplayer=false end;setting("restore_woodie_human","",0,"preview");local r=setting("restore_woodie_human","",0,"execute");assert(r.verified==1 and r.items[1].reason==nil and mutations==1)`,
		"wanda ignores ordinary health player":   `assert(not pcall(setting,"set_wanda_age","old",0,"preview") and mutations==0)`,
		"wanda outstanding injury":               `wanda();p.components.oldager.damage_remaining=3;assert(not pcall(setting,"set_wanda_age","old",0,"preview") and mutations==0)`,
		"wanda modified range rejected":          `wanda();TUNING.WANDA_MAX_YEARS_OLD=200;assert(not pcall(setting,"set_wanda_age","old",0,"preview") and mutations==0)`,
		"wanda exception restores damage flag":   `wanda();oldclass.StopDamageOverTime=function(s)s._taking_time_damage=true;error("hook failure")end;setting("set_wanda_age","old",0,"preview");local r=setting("set_wanda_age","old",0,"execute");assert(r.unconfirmed==1 and p.components.oldager._taking_time_damage==nil)`,
		"woodie waits for native transition":     `p.prefab="woodie";p.tags.wereplayer=true;p.components.wereness=setmetatable({percent=.5},{__index=wereclass});setting("restore_woodie_human","",0,"preview");local r=setting("restore_woodie_human","",0,"execute");assert(r.unconfirmed==1 and p.components.wereness.percent==0 and mutations==1);local observe=W.TakeObservation();assert(observe() == nil);p.tags.wereplayer=false;local final=observe();assert(final.verified==1 and mutations==1)`,
		"woodie wrong character":                 `assert(not pcall(setting,"restore_woodie_human","",0,"preview") and mutations==0)`,
		"quarantine prevents new writes":         `p.prefab="woodie";p.tags.wereplayer=true;p.components.wereness=setmetatable({percent=.5},{__index=wereclass});setting("restore_woodie_human","",0,"preview");setting("restore_woodie_human","",0,"execute");now=103;assert(not pcall(setting,"set_player_hunger_rate","set",2,"preview") and mutations==1)`,
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			L := workbenchV2Lua(t)
			if e := L.DoString(setup + scenario); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestWorkbenchV2CropStagesDoNotResetTimersOrOvershootMaturity(t *testing.T) {
	const setup = `
 growclass=def("growable",{DoGrowth=function(s)mutations=mutations+1;s.stage=s.stage+1;if s.stage==5 then s.inst.components.pickable={CanBePicked=function()return true end}end end,SetStage=function()end})
 function crop(x,stage)
  local e=entity("farm_plant_carrot",x);local stages={};for i,n in ipairs({"seed","sprout","small","med","full","rotten"})do stages[i]={name=n}end
  e.components.growable=setmetatable({inst=e,stage=stage,stages=stages,targettime=1},{__index=growclass});e.components.timer={SetTimeLeft=function()error("UNEXPECTED_TIMER_RESET")end};return e
 end
 `
	for name, scenario := range map[string]string{
		"one stage":          `entities={crop(1,1)};run("grow","preview");local r=run("grow","execute");assert(r.verified==1 and entities[1].components.growable.stage==2 and mutations==1)`,
		"ripen bounded":      `entities={crop(1,1)};run("ripen","preview");local r=run("ripen","execute");assert(r.verified==1 and entities[1].components.growable.stage==5 and mutations==4)`,
		"stage changed":      `entities={crop(1,1)};run("ripen","preview");entities[1].components.growable.stage=2;local r=run("ripen","execute");assert(r.skipped==1 and mutations==0)`,
		"step budget":        `for i=1,11 do entities[i]=crop(i,1)end;assert(not pcall(run,"ripen","preview") and mutations==0)`,
		"unsupported stages": `entities={crop(1,1)};entities[1].components.growable.stages[5].name="unknown";assert(run("ripen","preview").matched==0 and mutations==0)`,
	} {
		t.Run(name, func(t *testing.T) {
			L := workbenchV2Lua(t)
			if e := L.DoString(setup + scenario); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestWorkbenchV2RebuildCompensatesBeforeRemovingOriginal(t *testing.T) {
	const setup = `
 old=entity("researchlab",1);old.tags.burnt=true;entities={old}
 replacement=entity("researchlab",2);replacement.components.prototyper={};replacement.components.burnable={}
 replacement.Transform.SetPosition=function(_,x,y,z)replacement.Transform.GetWorldPosition=function()return x,y,z end end;replacement.Transform.SetRotation=function(_,v)replacement.Transform.GetRotation=function()return v end end
 SpawnPrefab=function()return replacement end
 `
	for name, scenario := range map[string]string{
		"rebuild verified":                    `run("repair_burnt","preview");assert(mutations==0);local r=run("repair_burnt","execute");assert(r.verified==1 and not old:IsValid() and replacement:IsValid())`,
		"invalid replacement keeps original":  `replacement.components.prototyper=nil;run("repair_burnt","preview");local r=run("repair_burnt","execute");assert(r.failed==1 and old:IsValid() and not replacement:IsValid())`,
		"remove throws after effect":          `run("repair_burnt","preview");EntityScript.Remove=function(e)e.valid=false;error("removed")end;old.Remove=EntityScript.Remove;local r=run("repair_burnt","execute");assert(r.unconfirmed==1 and r.failed==0 and replacement:IsValid())`,
		"container excluded":                  `old.components.container={};assert(run("repair_burnt","preview").matched==0 and mutations==0)`,
		"skin mismatch keeps original":        `replacement.skinname="unexpected";run("repair_burnt","preview");local r=run("repair_burnt","execute");assert(r.failed==1 and old:IsValid() and not replacement:IsValid())`,
		"position mismatch keeps original":    `replacement.Transform.SetPosition=function()end;run("repair_burnt","preview");local r=run("repair_burnt","execute");assert(r.failed==1 and old:IsValid() and not replacement:IsValid())`,
		"cleanup failure blocks both objects": `replacement.components.prototyper=nil;replacement.Remove=function()error("cleanup failed")end;run("repair_burnt","preview");local r=run("repair_burnt","execute");assert(r.unconfirmed==1 and r.failed==0 and old:IsValid() and replacement:IsValid() and W.IsBlocked(old) and W.IsBlocked(replacement))`,
		"original remove failure compensates": `EntityScript.Remove=function(e)if e==old then error("original retained")end;e.valid=false end;old.Remove=EntityScript.Remove;run("repair_burnt","preview");local r=run("repair_burnt","execute");assert(r.unconfirmed==1 and old:IsValid() and not replacement:IsValid())`,
	} {
		t.Run(name, func(t *testing.T) {
			L := workbenchV2Lua(t)
			if e := L.DoString(setup + scenario); e != nil {
				t.Fatal(e)
			}
		})
	}
}
