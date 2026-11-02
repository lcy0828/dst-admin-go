-- Only inspect known fields. Do not call Mod getters, save, or schedule work.
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
        rows[#rows+1]={id=id,invincible=status(field(field(c,"health"),"invincible")),creative=status(field(field(c,"builder"),"freebuildmode")),panelGod=status(owned),panelInvincible=status(field(p,"_dst_admin_invincible_owned"))}
        if #json.encode_compliant(rows)>7000 then rows[#rows]=nil;complete=false;reason="OUTPUT_LIMIT";break end
    end
end
emit({kind="player_modes",players=rows,count=#rows},complete,reason)
