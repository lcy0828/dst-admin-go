package dstruntime

import (
	lua "github.com/yuin/gopher-lua"
	"strings"
	"testing"
)

func workbenchLua(t *testing.T) *lua.LState {
	t.Helper()
	L := lua.NewState()
	t.Cleanup(L.Close)
	preloadGameJSONHarness(L)
	source, e := embeddedAssets.ReadFile("assets/workbench.lua")
	if e != nil {
		t.Fatal(e)
	}
	var modules string
	for _, name := range []string{"nearby_adapters.lua", "player_settings.lua", "workbench_v2.lua", "workbench_expansion.lua"} {
		b, _ := embeddedAssets.ReadFile("assets/" + name)
		modules += string(b) + "\n"
	}
	if e = L.DoString(modules + string(source) + `;W=Workbench
now=100;os.time=function()return now end;os.clock=function()return 0 end
TheWorld={};mutations=0;last=nil;print=function(text)last=text end
p={Transform={GetWorldPosition=function()return 0,0,0 end}};UserToPlayer=function(id)if id=="KU_EXAMPLE"then return p end end
function entity(x,tags)
 local e={prefab="spider",valid=true,tags=tags or {},Transform={GetWorldPosition=function()return x,0,0 end},components={}}
 function e:IsValid()return self.valid end;function e:HasTag(t)return self.tags[t]==true end
 function e:Remove()mutations=mutations+1;self.valid=false end
 e.components.burnable={burning=true,IsBurning=function(s)return s.burning end,Extinguish=function(s)mutations=mutations+1;s.burning=false end}
 e.components.freezable={AddColdness=function()mutations=mutations+1 end,IsFrozen=function()return false end}
 return e
end
entities={entity(1),entity(2)};TheSim={FindEntities=function()return entities end}
function run(action,phase,id) return W.Nearby("KU_EXAMPLE",action,10,"spider",phase,id or "token","token","caller-a")end
`); e != nil {
		t.Fatal(e)
	}
	return L
}
func TestNearbyPreviewHasNoWritesAndExecutionUsesFrozenTargets(t *testing.T) {
	L := workbenchLua(t)
	if e := L.DoString(`local r=run("extinguish","preview");assert(r.matched==2 and mutations==0)
local old=entities;entities={entity(3)};p.Transform.GetWorldPosition=function()return 100,0,0 end
local r=run("extinguish","execute");assert(r.state=="completed" and r.verified==2 and mutations==2)
assert(entities[1].components.burnable.burning==true)
assert(not pcall(run,"extinguish","execute"));assert(mutations==2)`); e != nil {
		t.Fatal(e)
	}
}
func TestNearbyLifecycleAndLimitsNeverReplay(t *testing.T) {
	for _, scenario := range []string{
		`run("extinguish","preview");now=131;assert(not pcall(run,"extinguish","execute"))`,
		`run("extinguish","preview");TheWorld={};assert(not pcall(run,"extinguish","execute"))`,
		`run("extinguish","preview");p={Transform=p.Transform};assert(not pcall(run,"extinguish","execute"))`,
		`run("extinguish","preview");assert(not pcall(run,"delete","execute"))`,
		`run("extinguish","preview");assert(run("extinguish","cancel").state=="cancelled");assert(not pcall(run,"extinguish","execute"))`,
		`entities={};for i=1,41 do entities[i]=entity(1)end;assert(not pcall(run,"extinguish","preview"))`,
		`entities={};for i=1,513 do entities[i]=entity(1)end;assert(not pcall(run,"extinguish","preview"))`,
		`entities={entity(1,{player=true}),entity(1,{epic=true}),entity(1,{structure=true})};assert(run("delete","preview").matched==0)`,
	} {
		t.Run(scenario, func(t *testing.T) {
			L := workbenchLua(t)
			if e := L.DoString(scenario + `;assert(mutations==0)`); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestNearbyPartialEffectsAreReportedWithoutClaimingFrozen(t *testing.T) {
	L := workbenchLua(t)
	if e := L.DoString(`run("freeze","preview");entities[2].valid=false;local r=run("freeze","execute");assert(r.state=="partial" and r.verified==0 and r.unconfirmed==1 and r.skipped==1 and mutations==1);assert(not pcall(run,"freeze","execute"))`); e != nil {
		t.Fatal(e)
	}
}
func TestModeOwnershipPreservesOtherSourcesAndOnlyRemovesOwnSetter(t *testing.T) {
	for name, scenario := range map[string]string{
		"baseline true":        `h.invincible=true;M.Begin(p);M.Apply(p);assert(M.End(p));assert(h.invincible==true)`,
		"baseline false":       `M.Begin(p);M.Apply(p);assert(M.End(p));assert(h.invincible==false and h.SetInvincible==original)`,
		"external request":     `M.Begin(p);M.Apply(p);h:SetInvincible(true);assert(M.End(p));assert(h.invincible==true)`,
		"external replacement": `M.Begin(p);M.Apply(p);local foreign=function()end;h.SetInvincible=foreign;assert(not M.End(p));assert(h.SetInvincible==foreign)`,
		"direct off":           `h.invincible=true;M.Begin(p);M.Apply(p);h.invincible=false;assert(M.End(p));assert(h.invincible==false)`,
		"overlapping modes":    `M.Begin(p);M.SetInvincibility(p,true);assert(M.End(p));assert(h.invincible==true);M.SetInvincibility(p,false);assert(h.invincible==false)`,
		"reverse release":      `M.SetInvincibility(p,true);M.Begin(p);M.SetInvincibility(p,false);assert(h.invincible==true);assert(M.End(p));assert(h.invincible==false)`,
		"repeat enable":        `M.Begin(p);M.Apply(p);M.Begin(p);assert(M.End(p));assert(h.invincible==false)`,
	} {
		t.Run(name, func(t *testing.T) {
			L := lua.NewState()
			defer L.Close()
			source, _ := embeddedAssets.ReadFile("assets/mode_ownership.lua")
			if e := L.DoString(string(source) + `;M=ModeOwnership;h={invincible=false,SetInvincible=function(s,v)s.invincible=v end};original=h.SetInvincible;p={components={health=h}};` + scenario); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestPlayerModeQueryPreservesFalseUnknownAndReadOnlyBehavior(t *testing.T) {
	L, out := queryLuaState(t, 0)
	defer L.Close()
	if e := L.DoString(`AllPlayers={{userid="KU_EXAMPLE",components={health={_={invincible={false}}},builder={freebuildmode=true}},_dst_admin_god_owned=false},{userid="KU_OTHER",components={}}}` + queryScript(t, "player_modes", "")); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(*out, `"invincible":"disabled"`) || !strings.Contains(*out, `"creative":"enabled"`) || !strings.Contains(*out, `"invincible":"unknown"`) {
		t.Fatal(*out)
	}
}

func TestNearbyCallerAndBoundaryExpiry(t *testing.T) {
	L := workbenchLua(t)
	if err := L.DoString(`run("extinguish","preview");assert(not pcall(W.Nearby,"KU_EXAMPLE","extinguish",10,"spider","execute","token","unused","caller-b"));assert(mutations==0);now=130;assert(not pcall(run,"extinguish","execute"));assert(mutations==0)`); err != nil {
		t.Fatal(err)
	}
}

func TestMotionOwnershipDoesNotResetExternalPhysicsOrDrowning(t *testing.T) {
	for name, scenario := range map[string]string{
		"restore":            `assert(M.Motion(p,"god",true));assert(phys.mask==1);assert(M.Motion(p,"god",false));assert(phys.mask==15 and d.ShouldDrown==original)`,
		"external physics":   `M.Motion(p,"god",true);phys.mask=7;assert(not M.Motion(p,"god",false));assert(phys.mask==7)`,
		"external drowning":  `M.Motion(p,"god",true);local foreign=function()return true end;d.ShouldDrown=foreign;assert(not M.Motion(p,"god",false));assert(d.ShouldDrown==foreign)`,
		"overlap":            `M.Motion(p,"water",true);M.Motion(p,"god",true);M.Motion(p,"god",false);assert(phys.mask==11 and d.ShouldDrown~=original);M.Motion(p,"water",false);assert(phys.mask==15 and d.ShouldDrown==original)`,
		"other order":        `M.Motion(p,"god",true);M.Motion(p,"water",true);M.Motion(p,"water",false);assert(phys.mask==1);M.Motion(p,"god",false);assert(phys.mask==15)`,
		"component replaced": `M.Motion(p,"god",true);p.Physics={};assert(not M.Motion(p,"god",false));assert(p.Physics.mask==nil)`,
	} {
		t.Run(name, func(t *testing.T) {
			L := lua.NewState()
			defer L.Close()
			source, _ := embeddedAssets.ReadFile("assets/mode_ownership.lua")
			if e := L.DoString(string(source) + `;M=ModeOwnership;COLLISION={GROUND=1,LIMITS=4};bit={bnot=function()return 11 end,band=function()return 11 end};phys={mask=15,GetCollisionMask=function(s)return s.mask end,SetCollisionMask=function(s,v)s.mask=v end,GetMass=function()return 1 end};original=function()return true end;d={ShouldDrown=original};p={Physics=phys,components={drownable=d},HasTag=function()return false end,AddTag=function()end,RemoveTag=function()end};` + scenario); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestCreativeOwnershipPreservesBaselineAndRejectsUnknownRelease(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	source, _ := embeddedAssets.ReadFile("assets/mode_ownership.lua")
	if e := L.DoString(string(source) + `;M=ModeOwnership;b={freebuildmode=true};p={components={builder=b}};M.Creative(p,true);M.Creative(p,false);assert(b.freebuildmode==true);assert(not pcall(M.Creative,p,false));b.freebuildmode=false;M.Creative(p,true);M.Creative(p,true);M.Creative(p,false);assert(b.freebuildmode==false)`); e != nil {
		t.Fatal(e)
	}
}
