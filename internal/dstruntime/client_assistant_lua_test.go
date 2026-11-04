package dstruntime

import (
	lua "github.com/yuin/gopher-lua"
	"os"
	"testing"
)

func TestOptionalClientRepairRequiresLocalConfirmationAndProtectsSkillCharacters(t *testing.T) {
	source, err := os.ReadFile("../../integrations/dst-admin-client/modmain.lua")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{
		`key();assert(#dialog.buttons==1);dialog.buttons[1].cb();assert(saves==0 and TheSkillTree.skillxp.wilson==8)`,
		`ThePlayer.prefab="wes";key();assert(#dialog.buttons==2 and saves==0);dialog.buttons[1].cb();assert(saves==0)`,
		`ThePlayer.prefab="wes";key();dialog.buttons[2].cb();assert(saves==1 and TheSkillTree.skillxp.wes==0 and TheSkillTree.skillxp.wilson==8 and updates==1)`,
		`ThePlayer.prefab="wes";key();ThePlayer={prefab="wilson"};dialog.buttons[2].cb();assert(saves==0)`,
	} {
		L := lua.NewState()
		if err := L.DoString(`GLOBAL=_G;KEY_F8=1;saves=0;updates=0;TheNet={IsDedicated=function()return false end};TheInput={AddKeyUpHandler=function(_,_,fn)key=fn end};ThePlayer={prefab="wilson",HUD={controls={skilltree_notification={UpdateElements=function()updates=updates+1 end}}}};TheSkillTree={skillxp={wilson=8,wes=7},UpdateSaveState=function()saves=saves+1 end};TheFrontEnd={PushScreen=function(_,v)dialog=v end,PopScreen=function()end};package.preload["screens/popupdialog"]=function()return function(title,message,buttons)return {buttons=buttons}end end;package.preload["prefabs/skilltree_defs"]=function()return {SKILLTREE_DEFS={wilson={}}}end;` + string(source) + "\n" + scenario); err != nil {
			L.Close()
			t.Fatal(err)
		}
		L.Close()
	}
}
