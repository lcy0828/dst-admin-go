package dstruntime

import (
	lua "github.com/yuin/gopher-lua"
	"testing"
)

func expansionLua(t *testing.T) *lua.LState {
	L := workbenchLua(t)
	if err := L.DoString(`
 TheWorld={GUID=100,prefab="forest",components={},meta={session_identifier="ABCDEF"},HasTag=function(_,tag)return false end}
 TheWorld.Map={IsOceanAtPoint=function()return false end,IsAboveGroundAtPoint=function()return true end,IsPassableAtPoint=function()return true end,CanTerraformAtPoint=function()return true end,GetPlatformAtPoint=function()end}
 p=entity(0,{player=true});p.GUID=1;p.userid="KU_EXAMPLE";p.prefab="wilson";p.components.inventory={GiveItem=function()end}
 local health={currenthealth=50,GetMaxWithPenalty=function()return 200 end,SetVal=function(s,v)mutations=mutations+1;s.currenthealth=v end}
 p.components.health=health;Ents={p};Prefabs={twigs={},beefalo={}};created={}
 SpawnPrefab=function(name)local e=entity(0);e.prefab=name;e.GUID=2+#created;e.Transform.SetPosition=function(_,x,y,z)e.x=x;e.z=z end;e.Transform.GetWorldPosition=function()return e.x or 0,0,e.z or 0 end;created[#created+1]=e;return e end
 caller=string.rep("a",64);token="00000000-0000-4000-8000-000000000001";request="00000000-0000-4000-8000-000000000002"
 function expand(id,args,phase)return W.Expansion(id,args,phase or "preview",token,token,caller,request)end
 `); err != nil {
		t.Fatal(err)
	}
	return L
}
func TestExpansionPreviewsFreezeValidatedEffectsAndAreSingleUse(t *testing.T) {
	L := expansionLua(t)
	if err := L.DoString(`local a={player_id=p.userid,value=1};local r=expand("set_player_health_value",a);assert(r.matched==1 and mutations==0);r=expand("set_player_health_value",a,"execute");assert(r.verified==1 and mutations==1 and p.components.health.currenthealth==1);assert(not pcall(expand,"set_player_health_value",a,"execute"))`); err != nil {
		t.Fatal(err)
	}
}
func TestExpansionRefusesStaleIdentityAndTamperedArguments(t *testing.T) {
	for _, body := range []string{
		`now=130`, `caller=string.rep("b",64)`, `TheWorld.meta.session_identifier="FEDCBA"`, `a.value=2`, `p.migration={}`,
	} {
		t.Run(body, func(t *testing.T) {
			L := expansionLua(t)
			if err := L.DoString(`a={player_id=p.userid,value=1};expand("set_player_health_value",a);` + body + `;local ok,r=pcall(expand,"set_player_health_value",a,"execute");assert(not ok or r.skipped==1);assert(mutations==0)`); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestExpansionSpawnPreviewDoesNotInstantiateAndRequiresOwnedSkins(t *testing.T) {
	L := expansionLua(t)
	if err := L.DoString(`local a={player_id=p.userid,prefab="twigs",count=2,x=5,z=6,expected_session="ABCDEF"};expand("spawn_entity_at",a);assert(#created==0);local r=expand("spawn_entity_at",a,"execute");assert(r.verified==2 and #created==2);now=110;a.skin="unowned";assert(not pcall(expand,"spawn_entity_at",a));assert(#created==2);a.skin="";a.expected_session="OLD";assert(not pcall(expand,"spawn_entity_at",a));assert(#created==2)`); err != nil {
		t.Fatal(err)
	}
}
func TestExpansionPartialExceptionsNeverReplayOrLeakBusyFlag(t *testing.T) {
	L := expansionLua(t)
	if err := L.DoString(`local a={player_id=p.userid,value=1};p.components.health.SetVal=function(s,v)mutations=mutations+1;s.currenthealth=v;error("mod error after mutation")end;expand("set_player_health_value",a);local r=expand("set_player_health_value",a,"execute");assert(r.unconfirmed==1 and r.verified==0 and mutations==1);assert(not pcall(expand,"set_player_health_value",a,"execute"));assert(not pcall(expand,"set_player_health_value",a));W.FinishObservation();now=110;assert(pcall(expand,"set_player_health_value",a))`); err != nil {
		t.Fatal(err)
	}
}

func TestExpansionFullWorldScanYieldsAndOnlyCreatesConfirmationAfterCompletion(t *testing.T) {
	L := expansionLua(t)
	if err := L.DoString(`
 Ents={};for i=1,1024 do local e=entity(2);e.GUID=i;e.prefab="twigs";Ents[i]=e end
 local ticks=0;os.clock=function()ticks=ticks+.001;return ticks end
 local r=expand("count_world_entities",{prefab="twigs",include_inventory=false});assert(not r.complete)
 local observe=W.TakeObservation();assert(observe);local value
 for i=1,20 do value=observe();if value then break end end
 assert(value and value.complete and value.quantity==1024 and mutations==0)
 local line,format=W.EncodeObservation(value);assert(format=="count"and line:find('"entities":1024',1,true));W.FinishObservation()
 now=110;Ents={};for i=1,1024 do local e=entity(2);e.GUID=i;e.prefab=i==1 and "twigs"or "grass";Ents[i]=e end
 local a={prefab="twigs",include_inventory=false};r=expand("remove_world_entities",a);assert(r.previewId==nil and mutations==0)
 assert(not pcall(expand,"remove_world_entities",a,"execute"))
 observe=W.TakeObservation();for i=1,20 do value=observe();if value then break end end;W.FinishObservation()
 assert(value and value.matched==1 and mutations==0)
 os.clock=function()return 0 end;r=expand("remove_world_entities",a,"execute");assert(r.verified==1 and mutations==1)
 `); err != nil {
		t.Fatal(err)
	}
}

func TestExpansionStopCancelsDeferredWritesAndSaveCallbacks(t *testing.T) {
	L := expansionLua(t)
	if err := L.DoString(`
 local ticks=0;os.clock=function()ticks=ticks+.001;return ticks end
 local a={player_id=p.userid,prefab="twigs",count=8,x=5,z=6}
 expand("spawn_entity_at",a);expand("spawn_entity_at",a,"execute");local n=#created;local observe=W.TakeObservation();assert(observe)
 W.Stop();observe();assert(#created==n)
 now=110;os.clock=function()return 0 end;TheWorld.IsValid=function()return true end;Roads={{}};ShardGameIndex={SaveCurrent=function(_,cb)saved=cb end};TheWorld.DoStaticTaskInTime=function()error("must not schedule after stop")end
 expand("remove_world_roads",{});expand("remove_world_roads",{},"execute");W.Stop();saved()
 `); err != nil {
		t.Fatal(err)
	}
}
func TestExpansionWorldDeletionProtectsPlayersAndIncludesHeldOnlyExplicitly(t *testing.T) {
	L := expansionLua(t)
	if err := L.DoString(`p.prefab="twigs";Ents={p};assert(not pcall(expand,"remove_world_entities",{prefab="twigs",include_inventory=true}));Ents={};local item=entity(2);item.GUID=2;item.prefab="twigs";item.components.inventoryitem={owner=p};Ents[2]=item;now=110;local a={prefab="twigs",include_inventory=false};local r=expand("remove_world_entities",a);assert(r.matched==0);r=expand("remove_world_entities",a,"execute");assert(r.attempted==0 and item:IsValid());now=120;a.include_inventory=true;expand("remove_world_entities",a);r=expand("remove_world_entities",a,"execute");assert(r.verified==1 and not item:IsValid())`); err != nil {
		t.Fatal(err)
	}
}
func TestExpansionBoundedScansNeverConvertFailureIntoZero(t *testing.T) {
	L := expansionLua(t)
	if err := L.DoString(`Ents={};for i=1,50001 do Ents[i]={IsValid=function()return false end}end;assert(not pcall(expand,"count_world_entities",{prefab="twigs",include_inventory=true}));Ents={};now=110;assert(pcall(expand,"count_world_entities",{prefab="twigs",include_inventory=true}));assert(last:find('"quantity":0',1,true))`); err != nil {
		t.Fatal(err)
	}
}
func TestManagedModeMemoryRequiresDiskAcknowledgement(t *testing.T) {
	L := expansionLua(t)
	source, _ := embeddedAssets.ReadFile("assets/managed_modes.lua")
	if err := L.DoString(string(source) + `;Modes=ManagedModes
 TheWorld.ListenForEvent=function()end;TheWorld.RemoveEventCallback=function()end
 TheSim.GetPersistentString=function(_,path,cb)cb(false,"")end
 TheSim.SetPersistentString=function(_,path,text,encode,cb)pending=cb end
 Modes.Start();Modes.Memory(p,true);assert(Modes.Remembering(p)and not Modes.MemoryVerified(p,true));pending(false);assert(not Modes.MemoryVerified(p,true));Modes.Memory(p,false);pending(true);assert(Modes.MemoryVerified(p,false))
 `); err != nil {
		t.Fatal(err)
	}
}

func TestExpansionContinuesOriginalBatchAcrossFramesWithoutRepeatingSpawns(t *testing.T) {
	L := expansionLua(t)
	if err := L.DoString(`
 local ticks=0;os.clock=function()ticks=ticks+.001;return ticks end
 local a={player_id=p.userid,prefab="twigs",count=8,x=5,z=6}
 expand("spawn_entity_at",a);local r=expand("spawn_entity_at",a,"execute")
 local observe=W.TakeObservation();assert(observe,"must yield")
 local value
 for i=1,20 do value=observe();if value then break end end
 W.FinishObservation()
 assert(value and value.verified==8 and value.skipped==0 and #created==8,tostring(value and value.verified)..":"..tostring(value and value.unconfirmed)..":"..tostring(#created))
 assert(not pcall(expand,"spawn_entity_at",a,"execute"))
 `); err != nil {
		t.Fatal(err)
	}
}

func TestMapRevealSlicesWithoutRepeatingAndMigrationNeverClaimsArrival(t *testing.T) {
	t.Run("map", func(t *testing.T) {
		L := expansionLua(t)
		if err := L.DoString(`
 TheWorld.IsValid=function()return true end;WORLD_TILES={IMPASSABLE=0,INVALID=-1};local seen,pending={},{ };local calls=0;local queue={}
 p.player_classified={MapExplorer={RevealArea=function(_,x,_,z)local key=x..":"..z;assert(not seen[key]and not pending[key]);pending[key]=true;calls=calls+1 end,IsTileSeeable=function(_,x,y)return seen[x..":"..y]==true end}}
 TheWorld.Map.GetSize=function()return 64,64 end;TheWorld.Map.GetTile=function()return 1 end;TheWorld.Map.GetTileCenterPoint=function(_,x,y)return x,0,y end
 TheWorld.DoStaticTaskInTime=function(_,_,fn)local task={Cancel=function(s)s.cancelled=true end};queue[#queue+1]={task=task,fn=fn};return task end
 local ticks=0;os.clock=function()ticks=ticks+.001;return ticks end
 local a={player_id=p.userid};expand("reveal_player_map",a);assert(calls==0);expand("reveal_player_map",a,"execute");local observe=W.TakeObservation();assert(observe)
 while #queue>0 do local item=table.remove(queue,1);if not item.task.cancelled then for key in pairs(pending)do seen[key]=true end;pending={};item.fn()end end
 os.clock=function()return 0 end;local r=observe();assert(r and r.verified==1 and calls==100);W.FinishObservation()
 `); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("migration", func(t *testing.T) {
		L := expansionLua(t)
		if err := L.DoString(`TheWorld.IsValid=function()return true end;TheShard={GetShardId=function()return "1"end};Shard_IsWorldAvailable=function(id)return id=="2"end;local events=0;TheWorld.PushEvent=function(_,event,data)assert(event=="ms_playerdespawnandmigrate"and data.player==p and data.worldid=="2"and data.portalid==1);events=events+1 end;local a={player_id=p.userid,destination_shard="2",portal_id=1};expand("migrate_player",a);assert(events==0);local r=expand("migrate_player",a,"execute");assert(r.unconfirmed==1 and r.verified==0 and events==1);local observe=W.TakeObservation();observe();observe();assert(events==1);W.Stop()`); err != nil {
			t.Fatal(err)
		}
	})
}
