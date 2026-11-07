package dstruntime

import "testing"

func TestManagedModesPersistSeriallyAndCleanOnlyOwnedEffects(t *testing.T) {
	for name, body := range map[string]string{
		"serial writes":                   `M.Memory(p,true);M.Set(p,"invincible",true);M.Memory(p,false);assert(#callbacks==1 and p._dst_admin_mode_memory==nil);callbacks[1](true);assert(#callbacks==2 and not M.MemoryVerified(p,false));callbacks[2](true);assert(M.MemoryVerified(p,false)and p._dst_admin_mode_memory==false);M.Stop();assert(not h.invincible)`,
		"leave and restore":               `M.Set(p,"health_lock",true);assert(h.minhealth==1);M.Memory(p,true);callbacks[1](true);assert(M.MemoryVerified(p,true));events.ms_playerleft(TheWorld,p);assert(h.minhealth==0 and not M.Status(p,"health_lock"));events.ms_playerjoined(TheWorld,p);assert(h.minhealth==1 and M.MemoryVerified(p,true));M.Stop();assert(h.minhealth==0)`,
		"clear despite conflict":          `M.Set(p,"invincible",true);M.Set(p,"one_hit",true);local foreign=function()return 7 end;p.components.combat.CalcDamage=foreign;assert(not pcall(M.Set,p,"clear_all",false));assert(not h.invincible and p.components.combat.CalcDamage==foreign);M.Stop()`,
		"foreign replacement":             `M.Set(p,"one_hit",true);local foreign=function()return 7 end;p.components.combat.CalcDamage=foreign;M.Stop();assert(p.components.combat.CalcDamage==foreign)`,
		"one hit respects player targets": `local combat=p.components.combat;local other={HasTag=function()return false end};local player={HasTag=function(_,tag)return tag=="player" end};M.Set(p,"one_hit",true);assert(M.Status(p,"one_hit")and combat:CalcDamage(other)==999999999 and combat:CalcDamage(player)==1);M.Set(p,"one_hit",false);assert(not M.Status(p,"one_hit")and combat:CalcDamage(other)==1);M.Stop()`,
		"clear all restores owned modes":  `M.Set(p,"health_lock",true);M.Set(p,"one_hit",true);assert(h.minhealth==1 and M.Status(p,"clear_all"));M.Set(p,"clear_all",false);assert(h.minhealth==0 and p.components.combat:CalcDamage({HasTag=function()return false end})==1 and not M.Status(p,"clear_all"));M.Stop()`,
		"failure is unknown":              `M.Memory(p,true);callbacks[1](false);assert(not M.MemoryVerified(p,true)and p._dst_admin_mode_memory==nil);M.Stop()`,
		"restart keeps loaded memory":     `M.Set(p,"health_lock",true);M.Memory(p,true);callbacks[1](true);M.Stop();assert(h.minhealth==0);TheSim.GetPersistentString=function()error("must not reload stale disk")end;M.Start();assert(h.minhealth==1 and M.MemoryVerified(p,true));M.Stop()`,
		"busy until disk acknowledgement": `assert(not M.Busy());M.Memory(p,true);assert(M.Busy());callbacks[1](true);assert(not M.Busy());M.Stop()`,
	} {
		t.Run(name, func(t *testing.T) {
			L := expansionLua(t)
			m, _ := embeddedAssets.ReadFile("assets/managed_modes.lua")
			o, _ := embeddedAssets.ReadFile("assets/mode_ownership.lua")
			if err := L.DoString(string(o) + "\n" + string(m) + `;M=ManagedModes;h=p.components.health;h.invincible=false;h.SetInvincible=function(s,v)s.invincible=v end;h.minhealth=0;h.SetMinHealth=function(s,v)s.minhealth=v end;p.components.combat={CalcDamage=function()return 1 end};AllPlayers={p};DSTAdmin={Commands={ModeOwnership=ModeOwnership}};events={};TheWorld.ListenForEvent=function(_,key,fn)events[key]=fn end;TheWorld.RemoveEventCallback=function(_,key)events[key]=nil end;callbacks={};TheSim.GetPersistentString=function(_,_,cb)cb(false,"")end;TheSim.SetPersistentString=function(_,_,_,_,cb)callbacks[#callbacks+1]=cb end;M.Start();` + body); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedModesRejectCorruptMemoryWithoutOverwritingIt(t *testing.T) {
	L := expansionLua(t)
	source, _ := embeddedAssets.ReadFile("assets/managed_modes.lua")
	if err := L.DoString(string(source) + `;TheWorld.ListenForEvent=function()end;TheWorld.RemoveEventCallback=function()end;TheSim.GetPersistentString=function(_,_,cb)cb(true,'{"version":1,"players":{"KU_EXAMPLE":{"unknown":true}}}')end;TheSim.SetPersistentString=function()error("must not overwrite")end;ManagedModes.Start();assert(not pcall(ManagedModes.Memory,p,false));assert(not ManagedModes.MemoryVerified(p,false))`); err != nil {
		t.Fatal(err)
	}
}

func TestManagedModesIgnoreReadsFromStoppedGeneration(t *testing.T) {
	L := expansionLua(t)
	source, _ := embeddedAssets.ReadFile("assets/managed_modes.lua")
	if err := L.DoString(string(source) + `
local reads={};TheWorld.ListenForEvent=function()end;TheWorld.RemoveEventCallback=function()end
TheSim.GetPersistentString=function(_,_,cb)reads[#reads+1]=cb end
ManagedModes.Start();assert(ManagedModes.Busy());ManagedModes.Stop();assert(not ManagedModes.Busy())
ManagedModes.Start();assert(ManagedModes.Busy());reads[1](true,'{"version":1,"players":{"KU_EXAMPLE":{"unknown":true}}}')
assert(ManagedModes.Busy());reads[2](false,"");assert(not ManagedModes.Busy())
assert(not ManagedModes.Remembering(p));ManagedModes.Stop()
`); err != nil {
		t.Fatal(err)
	}
}
