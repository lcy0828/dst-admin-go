package dstruntime

import (
	"context"
	"dont/shared"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func TestPlayerInventoryReadsPropertiesBundlesAndActiveWithoutCallbacks(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadJSONHarness(state)
	if err := state.DoString(`
 os.clock=function() return 0 end
 TheSim={SetPersistentString=function(self,path,value,encoded,cb) cb(true) end}
 TheNet={GetSessionIdentifier=function() return "SESSION" end}
 TheShard={GetShardId=function() return "1" end}
 SerializeUserSession=function() error("must not save") end
 SpawnPrefab=function() error("must not spawn") end
 STRINGS={NAMES={TWIGS="树枝"}}
 local twigs={prefab="twigs",components={stackable={_={stacksize={40}}}}}
 local food={prefab="carrot",components={perishable={_={perishremainingtime={0},perishtime={100}}}}}
 local bundle={prefab="bundle",components={unwrappable={itemdata={{prefab="axe",data={finiteuses={uses=0}}}}}}}
 local bag={prefab="backpack",components={container={numslots=8,slots={[1]=food,[2]=bundle}}}}
 AllPlayers={{userid="KU_01234567",prefab="wilson",components={inventory={maxslots=15,itemslots={[1]=twigs},equipslots={BODY=bag},activeitem={prefab="goldnugget",components={}}}}}}
 `); err != nil {
		t.Fatal(err)
	}
	module := loadLuaModule(t, state, "commands.lua")
	state.SetGlobal("commands", module)
	if err := state.DoString(`
 local r=commands.Execute({requestId="inventory-request-0001",action="player.inventory",arguments={userId="KU_01234567"}})
 assert(r.ok,r.code)
 local d=r.details
 assert(d.status=="complete" and d.slotCount==15 and not d.empty)
 assert(d.inventory[1].count==40 and d.inventory[1].name=="树枝")
 assert(d.activeItem.prefab=="goldnugget")
 local bag=d.equipment[1]
 assert(bag.slot=="BODY" and bag.slotCount==8)
 assert(bag.contents[1].metrics.perishable.ratio==0)
 local axe=bag.contents[2].contents[1]
 assert(axe.prefab=="axe" and axe.recordSource=="serialized")
 assert(axe.metrics.uses.value==0 and axe.metrics.uses.maximum==nil and axe.metrics.uses.ratio==nil)
 `); err != nil {
		t.Fatal(err)
	}
}

func TestPlayerInventoryMarksMissingSectionsAndCyclesPartial(t *testing.T) {
	state := lua.NewState()
	defer state.Close()
	preloadJSONHarness(state)
	if err := state.DoString(`os.clock=function() return 0 end; TheSim={SetPersistentString=function(self,p,v,e,cb)cb(true)end};TheNet={GetSessionIdentifier=function() return "S" end};TheShard={GetShardId=function()return "1"end};local bag={prefab="mod_bag",components={container={slots={}}}};bag.components.container.slots[1]=bag;AllPlayers={{userid="KU_01234567",components={inventory={itemslots={bag}}}}}`); err != nil {
		t.Fatal(err)
	}
	state.SetGlobal("commands", loadLuaModule(t, state, "commands.lua"))
	if err := state.DoString(`local r=commands.Execute({requestId="inventory-request-0002",action="player.inventory",arguments={userId="KU_01234567"}});assert(r.ok and r.details.status=="partial" and not r.details.empty);assert(#r.details.limitsReached>=2)`); err != nil {
		t.Fatal(err)
	}
}

type inventoryDistributedRuntime struct {
	*distributedRuntimeFixture
	calls int
}

func (f *inventoryDistributedRuntime) ReadPlayerInventory(_ context.Context, _, _ string, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	f.calls++
	return shared.PlayerInventory{Source: q.Source}, nil
}
func TestInventorySharesMailboxLockWithConsoleCommands(t *testing.T) {
	b, fixture, room, world, _ := newDistributedBridgeFixture(t)
	f := &inventoryDistributedRuntime{distributedRuntimeFixture: fixture}
	b.runtime = f
	lock := b.worldLock(room, world)
	lock.Lock()
	_, e := b.ReadPlayerInventory(context.Background(), room, world, shared.PlayerInventoryRequest{Source: "live"})
	if e == nil || e.Error() != "INVENTORY_BUSY" || f.calls != 0 {
		t.Fatal(e, f.calls)
	}
	// A saved read never uses the game mailbox and remains available while busy.
	v, e := b.ReadPlayerInventory(context.Background(), room, world, shared.PlayerInventoryRequest{Source: "save"})
	lock.Unlock()
	if e != nil || v.Source != "save" || f.calls != 1 {
		t.Fatal(v, e, f.calls)
	}
}
