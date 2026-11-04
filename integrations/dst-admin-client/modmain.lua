-- Independent implementation against Klei's public client components.
-- Never loads on a dedicated server and never sends a server command or RPC.
local G=GLOBAL
if G.TheNet:IsDedicated() then return end
local showing=false
local function show(title,message,buttons)
    if showing then return end
    local Dialog=require("screens/popupdialog")
    showing=true
    local function close()showing=false;G.TheFrontEnd:PopScreen()end
    local choices={}
    for _,b in ipairs(buttons or {})do local action=b.action;choices[#choices+1]={text=b.text,cb=function()close();if action then action()end end}end
    if #choices==0 then choices={{text="知道了",cb=close}}end
    G.TheFrontEnd:PushScreen(Dialog(title,message,choices))
end
G.TheInput:AddKeyUpHandler(G.KEY_F8,function()
    local p=G.ThePlayer
    if not p or not p.prefab or not p.HUD or not G.TheSkillTree then return end
    local defs=require("prefabs/skilltree_defs").SKILLTREE_DEFS
    if defs[p.prefab]~=nil then show("此角色已有技能树","不会清除这个角色的洞察点。请先从面板同步技能数据；仍异常时重新进入游戏。");return end
    show("修复错误技能提示","仅用于当前没有技能树的角色。确认后会清除本角色客户端中误存的洞察点和提示，不影响其他角色。",{
        {text="取消"},
        {text="确认修复",action=function()
            if G.ThePlayer~=p or defs[p.prefab]~=nil then return end
            G.TheSkillTree.skillxp[p.prefab]=0
            G.TheSkillTree:UpdateSaveState(p.prefab)
            p.new_skill_available_popup=nil
            local controls=p.HUD and p.HUD.controls
            local notice=controls and controls.skilltree_notification
            if notice and notice.UpdateElements then notice:UpdateElements()end
        end},
    })
end)
