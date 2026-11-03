package dstruntime

import (
	lua "github.com/yuin/gopher-lua"
	"testing"
)

func teleportLua(t *testing.T) *lua.LState {
	t.Helper()
	L := lua.NewState()
	t.Cleanup(L.Close)
	e := L.DoString(`
 Workbench={};now=100;os.time=function()return now end
 mutations=0;blocked=false;water=false;obstacles={}
 package.preload.json=function()return {encode_compliant=function(v)last=v;return "evidence"end}end
 Vector3=function(x,y,z)return {x=x,y=y,z=z}end
 TheWorld={Map={IsAboveGroundAtPoint=function()return not blocked end,GetPlatformAtPoint=function()return destination_boat end,IsPassableAtPoint=function(_,x,y,z,water,exclude_boats)assert(water==false and exclude_boats==true);return not blocked end,IsOceanAtPoint=function()return water end,IsGroundTargetBlocked=function()return false end}}
 TheNet={GetSessionIdentifier=function()return "session"end}
 TheSim={FindEntities=function()return obstacles end}
 function makeplayer(id)
 local p={userid=id,components={},tags={},x=1,z=2}
 function p:IsValid()return not self.removed end
 function p:HasTag(t)return self.tags[t] end
 function p:GetCurrentPlatform()return self.boat end
 function p:GetDisplayName()return "Example"end
 p.Transform={GetWorldPosition=function()return p.x,0,p.z end}
 p.Physics={Teleport=function(_,x,y,z)mutations=mutations+1;if not no_effect then p.x=x;p.z=z end;if after_error then error("MOD_ERROR")end end}
 return p
 end
 p=makeplayer("KU_EXAMPLE");target=makeplayer("KU_TARGET");target.x=20;target.z=30
 UserToPlayer=function(id)if id==p.userid then return p elseif id==target.userid then return target end end
 function query(phase,extras)
 local q={phase=phase,operationId="preview-id",caller="caller",playerId=p.userid,sessionId="session",x=12,z=24}
 for k,v in pairs(extras or {})do q[k]=v end
 return Workbench.Teleport(q)
 end
 `)
	if e != nil {
		t.Fatal(e)
	}
	src, e := embeddedAssets.ReadFile("assets/teleport.lua")
	if e != nil {
		t.Fatal(e)
	}
	if e = L.DoString(string(src)); e != nil {
		t.Fatal(e)
	}
	return L
}
func TestTeleportRuntimeEvidenceWalkingAndReplay(t *testing.T) {
	L := teleportLua(t)
	if e := L.DoString(`
 local r=query("prepare");assert(r.state=="prepared");assert(mutations==0);assert(r.expiresAt==160)
 p.x=8;p.z=9 -- normal walking is allowed
 r=query("execute");assert(r.state=="confirmed");assert(r.before.x==8 and r.after.x==12);assert(mutations==1)
 r=query("execute");assert(r.state=="rejected");assert(mutations==1)
 `); e != nil {
		t.Fatal(e)
	}
}
func TestTeleportRuntimeGuards(t *testing.T) {
	for _, setup := range []string{`water=true`, `blocked=true`, `destination_boat={}`, `p.boat={}`, `p.tags.playerghost=true`, `p.tags.wereplayer=true`, `p.migration={}`, `p.components.rider={IsRiding=function()return true end}`, `TheWorld.Map.IsGroundTargetBlocked=nil`} {
		t.Run(setup, func(t *testing.T) {
			L := teleportLua(t)
			if e := L.DoString(setup + `;local r=query("prepare");assert(r.state=="rejected");assert(mutations==0)`); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestTeleportRuntimeExpiryDestinationChangeAndWrongCaller(t *testing.T) {
	for _, setup := range []string{`now=160`, `blocked=true`, `p=makeplayer("KU_EXAMPLE")`, `TheWorld={Map=TheWorld.Map}`} {
		t.Run(setup, func(t *testing.T) {
			L := teleportLua(t)
			if e := L.DoString(`assert(query("prepare").state=="prepared");` + setup + `;assert(query("execute").state=="rejected");assert(mutations==0)`); e != nil {
				t.Fatal(e)
			}
		})
	}
	L := teleportLua(t)
	if e := L.DoString(`assert(query("prepare").state=="prepared");assert(query("execute",{caller="other"}).state=="rejected");assert(mutations==0);assert(query("execute").state=="confirmed")`); e != nil {
		t.Fatal(e)
	}
}
func TestTeleportRuntimeUncertainMutationNotRetried(t *testing.T) {
	for _, setup := range []string{`no_effect=true`, `after_error=true`} {
		t.Run(setup, func(t *testing.T) {
			L := teleportLua(t)
			if e := L.DoString(`assert(query("prepare").state=="prepared");` + setup + `;assert(query("execute").state=="uncertain");assert(query("execute").state=="rejected");assert(mutations==1)`); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestTeleportTargetIsFrozen(t *testing.T) {
	L := teleportLua(t)
	if e := L.DoString(`assert(query("prepare",{targetPlayerId="KU_TARGET"}).state=="prepared");target.x=100;assert(query("execute",{targetPlayerId="KU_TARGET"}).state=="rejected");assert(mutations==0)`); e != nil {
		t.Fatal(e)
	}
}

func TestCancellationReleasesOnlyItsOwnReservation(t *testing.T) {
	L := teleportLua(t)
	if e := L.DoString(`
assert(query("prepare").state=="prepared")
query("cancel",{caller="other"})
assert(query("prepare",{operationId="other-op"}).reason=="TELEPORT_BUSY")
query("cancel")
assert(query("prepare",{operationId="other-op"}).state=="prepared")
assert(query("execute").state=="rejected")
assert(mutations==0)
`); e != nil {
		t.Fatal(e)
	}
}

func TestMovementPreviewFreezesPlayersAndPositions(t *testing.T) {
	L := teleportLua(t)
	if e := L.DoString(`
local id="00000000-0000-4000-8000-000000000001"
Workbench.MovePlayers({{id=p.userid,x=20,z=30},{id=target.userid,x=1,z=2}},{phase="preview",previewId=id})
assert(last.state=="preview" and mutations==0 and #last.items==2)
p.x=8;target.x=80 -- normal walking does not change the frozen destination
assert(query("prepare").reason=="TELEPORT_BUSY")
Workbench.MovePlayers(nil,{phase="execute",previewId=id})
assert(last.state=="confirmed" and last.verified==2 and mutations==2)
assert(p.x==20 and p.z==30 and target.x==1 and target.z==2)
assert(not pcall(Workbench.MovePlayers,nil,{phase="execute",previewId=id}))
assert(mutations==2)
`); e != nil {
		t.Fatal(e)
	}
}

func TestMovementPreviewExpiryAndChangeNeverMove(t *testing.T) {
	for _, setup := range []string{`now=160`, `blocked=true`, `p=makeplayer("KU_EXAMPLE")`, `TheWorld={Map=TheWorld.Map}`} {
		t.Run(setup, func(t *testing.T) {
			L := teleportLua(t)
			if e := L.DoString(`local id="00000000-0000-4000-8000-000000000001";Workbench.MovePlayers({{id=p.userid,x=20,z=30}},{phase="preview",previewId=id});` + setup + `;assert(not pcall(Workbench.MovePlayers,nil,{phase="execute",previewId=id}));assert(mutations==0)`); e != nil {
				t.Fatal(e)
			}
		})
	}
}
