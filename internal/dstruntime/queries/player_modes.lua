-- Inspect known fields and the fixed Runtime's read-only mode inspector.
-- Its only getters are native tags/physics; never save or schedule work.
local function field(o,k)
    if type(o)~="table" then return nil end
    local v=rawget(o,k)
    if v~=nil then return v end
    local props=rawget(o,"_");local prop=type(props)=="table" and rawget(props,k)
    if type(prop)=="table" then return rawget(prop,1) end
    return nil
end
local function status(v)
    if v==true then return "enabled" end
    if v==false then return "disabled" end
    return "unknown"
end
local rows={};local complete=true;local reason=nil
local runtime=field(field(rawget(_G,"DSTAdmin"),"Commands"),"ManagedModes")
local inspect=field(runtime,"InspectionVersion")==2 and field(runtime,"Status") or nil
local players=rawget(_G,"AllPlayers")
if type(players)~="table" then error("PLAYERS_UNAVAILABLE") end
for i=1,65 do
    local p=rawget(players,i)
    if p==nil then break end
    if i>64 then complete=false;reason="SCAN_LIMIT";break end
    local id=field(p,"userid")
    if type(id)=="string" and #id<=80 and field(p,"migration")==nil then
        local c=field(p,"components")
        local owned=field(p,"_dst_admin_god_owned")
        if field(p,"_dst_admin_god_task")~=nil then owned=true end
        local modes=field(p,"_dst_admin_managed_modes");local panelModes=nil
        if type(inspect)=="function" or type(modes)=="table" then
            panelModes={}
            for _,name in ipairs({"invincible","super_god","creative","health_lock","one_hit","invisible","water_walk","noclip"})do
                local value=nil
                if type(inspect)=="function" then
                    local ok,observed=pcall(inspect,p,name);if ok then value=observed end
                elseif field(modes,name)~=true then value=false end
                panelModes[name]=status(value)
            end
        end
        rows[#rows+1]={panelModes=panelModes,modeMemory=status(field(p,"_dst_admin_mode_memory")),id=id,invincible=status(field(field(c,"health"),"invincible")),creative=status(field(field(c,"builder"),"freebuildmode")),panelGod=panelModes and type(inspect)=="function" and panelModes.super_god or status(owned),panelInvincible=panelModes and type(inspect)=="function" and panelModes.invincible or status(field(p,"_dst_admin_invincible_owned"))}
        if #json.encode_compliant(rows)>7000 then rows[#rows]=nil;complete=false;reason="OUTPUT_LIMIT";break end
    end
end
emit({kind="player_modes",players=rows,count=#rows},complete,reason)
