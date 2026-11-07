package console

import (
	"os"
	"path/filepath"
	"testing"

	"dont/pkg/commands/builtin"

	lua "github.com/yuin/gopher-lua"
)

// Execute the shipped Lua and the legacy Go-rendered commands in one game
// fixture. This catches shared-state failures which renderer string checks miss.
func playerModesRuntime(t *testing.T) *lua.LState {
	t.Helper()
	L := lua.NewState()
	t.Cleanup(L.Close)
	L.PreloadModule("json", func(L *lua.LState) int {
		module := L.NewTable()
		L.SetField(module, "encode_compliant", L.NewFunction(func(L *lua.LState) int {
			L.Push(lua.LString("{}"))
			return 1
		}))
		L.Push(module)
		return 1
	})
	if err := L.DoString(`
json=require("json");COLLISION={WORLD=2,GROUND=1,LIMITS=4}
test_time=1700000000;os.time=function()return test_time end
bit={bnot=function()return 11 end,band=function()return 11 end}
TheWorld={GUID=100,meta={session_identifier="SESSION"}}
TheNet={GetSessionIdentifier=function()return "SESSION" end}
TheShard={GetShardId=function()return "1" end}
TheSim={FindEntities=function()return {} end,GetPersistentString=function(_,_,cb)cb(false,"")end,
 SetPersistentString=function(_,_,_,_,cb)if cb then cb(true)end end}
h={invincible=false,minhealth=0,SetInvincible=function(s,v)s.invincible=v end,
 SetMinHealth=function(s,v)s.minhealth=v end,SetPercent=function()end}
b={freebuildmode=false};combat={CalcDamage=function()return 7 end,SetTarget=function()end}
phys={mask=15,GetCollisionMask=function(s)return s.mask end,
 SetCollisionMask=function(s,v)s.mask=v end,GetMass=function()return 1 end}
p={GUID=1,userid="KU_EXAMPLE",prefab="wilson",Physics=phys,tags={},
 components={health=h,builder=b,combat=combat},Transform={GetWorldPosition=function()return 0,0,0 end}}
function p:IsValid()return true end;function p:HasTag(tag)return self.tags[tag]==true end
function p:AddTag(tag)self.tags[tag]=true end;function p:RemoveTag(tag)self.tags[tag]=nil end
function p:Show()end;function p:Hide()end
function p:DoPeriodicTask()return {Cancel=function()end}end
AllPlayers={p};UserToPlayer=function(id)if id==p.userid then return p end end
print=function()end
`); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("..", "dstruntime", "assets", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	source := read("commands.lua")
	for _, name := range []string{"mode_ownership.lua", "managed_modes.lua", "teleport.lua", "workbench.lua", "workbench_expansion.lua", "workbench_v2.lua", "player_settings.lua", "nearby_adapters.lua"} {
		source = read(name) + "\n" + source
	}
	chunk, err := L.LoadString(read("clock.lua") + "\n" + source)
	if err != nil {
		t.Fatal(err)
	}
	if err := L.CallByParam(lua.P{Fn: chunk, NRet: 1, Protect: true}); err != nil {
		t.Fatal(err)
	}
	L.SetGlobal("C", L.Get(-1))
	L.Pop(1)
	if err := L.DoString(`
DSTAdmin={Commands=C};C.StartModes();M=C.ManagedModes
caller=string.rep("a",64);sequence=0
function mode(name,on)
 test_time=test_time+61
 sequence=sequence+1
 local token=string.format("00000000-0000-4000-8000-%012d",sequence)
 local request=string.format("10000000-0000-4000-8000-%012d",sequence)
 local a={player_id=p.userid,mode=name,enabled=on,feedback="off"}
 local preview=C.Workbench.Expansion("set_player_mode",a,"preview",nil,token,caller,request)
 local result=C.Workbench.Expansion("set_player_mode",a,"execute",preview.previewId,token,caller,request)
 return result
end
function dedicated(action,on)
 sequence=sequence+1
 return C.Execute({requestId=string.format("mode-command-%08d",sequence),action=action,
 arguments={userId=p.userid,enabled=on}})
end
`); err != nil {
		t.Fatal(err)
	}
	return L
}

func modeLua(t *testing.T, L *lua.LState, source string) {
	t.Helper()
	if err := L.DoString(source); err != nil {
		t.Fatal(err)
	}
}

func legacyAbility(t *testing.T, L *lua.LState, name string, enabled bool) {
	t.Helper()
	script, err := renderSetPlayerAbility(map[string]interface{}{"player_id": "KU_EXAMPLE", "ability": name, "enabled": enabled})
	if err != nil {
		t.Fatal(err)
	}
	modeLua(t, L, script)
}

func TestPlayerModesAllEntrancesShareOneSwitch(t *testing.T) {
	for _, tc := range []struct{ mode, legacy, dedicated string }{
		{"one_hit", "one_hit_kill", ""}, {"water_walk", "water_walk", ""},
		{"creative", "", "player.creative_mode"}, {"super_god", "", "player.god_mode"},
		{"invincible", "", ""},
	} {
		for _, firstLegacy := range []bool{true, false} {
			t.Run(tc.mode+map[bool]string{true: "/legacy-first", false: "/workbench-first"}[firstLegacy], func(t *testing.T) {
				L := playerModesRuntime(t)
				legacy := func(on bool) {
					if tc.legacy != "" {
						legacyAbility(t, L, tc.legacy, on)
					} else if tc.dedicated != "" {
						L.SetGlobal("action", lua.LString(tc.dedicated))
						L.SetGlobal("enabled", lua.LBool(on))
						modeLua(t, L, `assert(dedicated(action,enabled).ok)`)
					} else {
						script, err := builtinTemplates()["set_player_invincibility"].render(map[string]interface{}{"player_id": "KU_EXAMPLE", "enabled": on})
						if err != nil {
							t.Fatal(err)
						}
						modeLua(t, L, script)
					}
				}
				L.SetGlobal("name", lua.LString(tc.mode))
				if firstLegacy {
					legacy(true)
				} else {
					modeLua(t, L, `assert(mode(name,true).verified==1)`)
				}
				modeLua(t, L, `assert(M.Status(p,name)==true)`)
				if firstLegacy {
					modeLua(t, L, `assert(mode(name,false).verified==1)`)
				} else {
					legacy(false)
				}
				modeLua(t, L, `assert(M.Status(p,name)==false);assert(mode(name,false).verified==1)`)
				legacy(true)
				modeLua(t, L, `assert(mode("clear_all",false).verified==1);assert(M.Status(p,name)==false)`)
			})
		}
	}
}

func TestOneHitProtectsBlockedAndPlayerTargetsAndReportsReplacement(t *testing.T) {
	L := playerModesRuntime(t)
	legacyAbility(t, L, "one_hit_kill", true)
	modeLua(t, L, `
function target(tag)return {HasTag=function(_,name)return tag==name end}end
assert(combat:CalcDamage(target("player"))==7)
assert(combat:CalcDamage(target("alwaysblock"))==0)
assert(combat:CalcDamage(target("spider"))==999999999)
local foreign=function()return 12 end;combat.CalcDamage=foreign
assert(M.Status(p,"one_hit")==nil)
assert(not pcall(M.Set,p,"one_hit",true));assert(not pcall(M.Set,p,"one_hit",false))
assert(combat.CalcDamage==foreign)
assert(mode("one_hit",true).verified==0)
function emit(v)query_result=v end
`)
	query, err := os.ReadFile(filepath.Join("..", "dstruntime", "queries", "player_modes.lua"))
	if err != nil {
		t.Fatal(err)
	}
	modeLua(t, L, string(query)+`;assert(query_result.players[1].panelModes.one_hit=="unknown")`)
}

func TestOldUnidentifiableOneHitRequiresPlayerReconnectWithoutOverwriting(t *testing.T) {
	L := playerModesRuntime(t)
	modeLua(t, L, `
local legacy=function()return 100 end
combat._dst_admin_calc_damage=combat.CalcDamage;combat.CalcDamage=legacy
for _,on in ipairs({true,false})do
 local ok,err=pcall(M.Set,p,"one_hit",on)
 assert(not ok and tostring(err):find("MODE_LEGACY_STATE_UNCERTAIN",1,true))
 assert(combat.CalcDamage==legacy)
end
assert(not pcall(M.Set,p,"clear_all",false));assert(combat.CalcDamage==legacy)
`)
}

func TestStealthAndModeTagsReleaseOnlyTheirOwnEffects(t *testing.T) {
	for _, order := range []bool{true, false} {
		t.Run(map[bool]string{true: "stealth-first", false: "modes-first"}[order], func(t *testing.T) {
			L := playerModesRuntime(t)
			if order {
				legacyAbility(t, L, "stealth", true)
			}
			modeLua(t, L, `assert(mode("invincible",true).verified==1);assert(mode("invisible",true).verified==1)`)
			if !order {
				legacyAbility(t, L, "stealth", true)
			}
			legacyAbility(t, L, "no_hate", true)
			legacyAbility(t, L, "stealth", false)
			modeLua(t, L, `assert(h.invincible and M.Status(p,"invincible"));assert(p:HasTag("invisible") and p:HasTag("notarget") and p:HasTag("debugnoattack"))`)
			modeLua(t, L, `assert(mode("clear_all",false).verified==1);assert(not h.invincible and not p:HasTag("invisible"))`)
			legacyAbility(t, L, "no_hate", false)
			modeLua(t, L, `assert(not p:HasTag("debugnoattack"))`)
		})
	}
}

func TestHealthLocksShareMinimumWithoutClearingAnotherSource(t *testing.T) {
	L := playerModesRuntime(t)
	set := func(on bool) {
		script, err := renderSetPlayerLock(map[string]interface{}{"player_id": "KU_EXAMPLE", "resource": "health", "value": 50, "enabled": on})
		if err != nil {
			t.Fatal(err)
		}
		modeLua(t, L, script)
	}
	modeLua(t, L, `assert(mode("health_lock",true).verified==1);assert(h.minhealth==1)`)
	set(true)
	modeLua(t, L, `assert(h.minhealth==50 and M.Status(p,"health_lock"))`)
	set(false)
	modeLua(t, L, `assert(h.minhealth==1 and M.Status(p,"health_lock"))`)
	set(true)
	modeLua(t, L, `assert(mode("health_lock",false).verified==1);assert(h.minhealth==50)`)
	set(false)
	modeLua(t, L, `assert(h.minhealth==0)`)
}

func TestPlayerModeEffectsAndMemoryAreSharedWithoutDowngradingGodMode(t *testing.T) {
	L := playerModesRuntime(t)
	modeLua(t, L, `
assert(mode("invincible",true).verified==1)
assert(h.invincible and not p._dst_admin_god_task and phys.mask==15)
assert(dedicated("player.god_mode",true).ok)
assert(M.Status(p,"super_god") and M.Status(p,"invincible") and phys.mask==1)
assert(mode("super_god",false).verified==1)
assert(h.invincible and M.Status(p,"invincible") and not p._dst_admin_god_task and phys.mask==15)
assert(mode("invincible",false).verified==1 and not h.invincible)
assert(dedicated("player.god_mode",true).ok)
assert(dedicated("player.creative_mode",true).ok)
M.Memory(p,true);assert(M.MemoryVerified(p,true))
M.Stop();assert(not h.invincible and not b.freebuildmode and not p._dst_admin_god_task)
TheWorld.ListenForEvent=function()end;TheWorld.RemoveEventCallback=function()end
M.Start();assert(M.Status(p,"super_god") and M.Status(p,"creative"))
assert(mode("clear_all",false).verified==1)
assert(not h.invincible and not b.freebuildmode and phys.mask==15)
M.Stop();M.Start();assert(not M.Status(p,"clear_all"))
`)
}

func TestPlayerModeInspectionDoesNotConfirmReplacedEffectsOrRememberThem(t *testing.T) {
	for name, replace := range map[string]string{
		"invincible":  `h.invincible=false`,
		"super_god":   `h.SetInvincible=function()end`,
		"creative":    `b.freebuildmode=false`,
		"water_walk":  `phys.mask=7`,
		"noclip":      `phys.mask=7`,
		"health_lock": `h.minhealth=15`,
		"invisible":   `p:RemoveTag("invisible")`,
	} {
		t.Run(name, func(t *testing.T) {
			L := playerModesRuntime(t)
			L.SetGlobal("name", lua.LString(name))
			modeLua(t, L, `M.Set(p,name,true);`+replace+`
assert(M.Status(p,name)==nil)
assert(not pcall(M.Set,p,name,true))
assert(not pcall(M.Memory,p,true));assert(not M.Remembering(p))
function emit(v)query_result=v end
`)
			query, err := os.ReadFile(filepath.Join("..", "dstruntime", "queries", "player_modes.lua"))
			if err != nil {
				t.Fatal(err)
			}
			modeLua(t, L, string(query)+`;assert(query_result.players[1].panelModes[name]=="unknown")`)
			if name == "super_god" {
				modeLua(t, L, `assert(query_result.players[1].panelGod=="unknown")`)
			}
		})
	}
}

func TestPlayerModeAdoptsIdentifiableEffectsAndPreservesGameBaselines(t *testing.T) {
	L := playerModesRuntime(t)
	modeLua(t, L, `
h.invincible=true;b.freebuildmode=true;h.minhealth=12
p:AddTag("invisible");p:AddTag("notarget")
C.ModeOwnership.SetInvincibility(p,true);C.ModeOwnership.Creative(p,true)
assert(mode("health_lock",true).verified==1 and h.minhealth==12)
assert(mode("invisible",true).verified==1)
assert(mode("clear_all",false).verified==1)
assert(h.invincible and b.freebuildmode and h.minhealth==12)
assert(p:HasTag("invisible") and p:HasTag("notarget"))
assert(M.Status(p,"invincible")==false and M.Status(p,"creative")==false)
`)
}

func TestLegacyCommandLibraryUsesCanonicalImmunity(t *testing.T) {
	L := playerModesRuntime(t)
	modeLua(t, L, `c_select=function(name)assert(name=="example-player");return p end`)
	for _, cmd := range builtin.GetBuiltinCommands() {
		if cmd.ID == "godmode_player" {
			modeLua(t, L, cmd.GenerateScript("example-player", "true"))
			modeLua(t, L, `assert(M.Status(p,"invincible") and not p._dst_admin_god_task);assert(mode("invincible",false).verified==1);assert(not h.invincible)`)
			modeLua(t, L, `assert(mode("invincible",true).verified==1)`)
			modeLua(t, L, cmd.GenerateScript("example-player", "false"))
			modeLua(t, L, `assert(not M.Status(p,"invincible") and not h.invincible)`)
			return
		}
	}
	t.Fatal("legacy mode command missing")
}
