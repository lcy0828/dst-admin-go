-- Opt-in, per-player mode ownership. No periodic work while no mode is enabled.
local ManagedModes = (function()
    local M,owners,memories={},setmetatable({}, {__mode="k"}),{}
    local names={invincible=true,super_god=true,creative=true,health_lock=true,one_hit=true,invisible=true,water_walk=true,noclip=true}
    local path="mod_config_data/dst-admin/mode-memory.json"
    local started,loaded,loading,writing,dirty=false,false,false,false,false
    local loadGeneration=0
    local revision,persisted=0,0
    local loadFailure=false
    local join,leave,world
    local function updateMarkers()
        for _,p in ipairs(AllPlayers or {})do
            p._dst_admin_mode_memory=loaded and not loadFailure and not writing and not dirty and persisted==revision and (memories[p.userid]~=nil) or nil
            if loaded and not loadFailure and not writing and not dirty and persisted==revision and memories[p.userid]==nil then p._dst_admin_mode_memory=false end
        end
    end
    local function command()local r=rawget(_G,"DSTAdmin");if not r or not r.Commands then error("WORKBENCH_UPDATE_REQUIRED")end;return r.Commands end
    local function write()
        dirty=true
        if writing or not loaded then return end
        local encoded=require("json").encode_compliant({version=1,players=memories})
        if #encoded>32768 then error("MODE_MEMORY_LIMIT") end
        writing=true;dirty=false;local writingRevision=revision
        updateMarkers()
        local called=false
        local function completed(ok)
            if called then return end;called=true
            writing=false
            if ok then persisted=writingRevision end
            if not ok then print("[DST-ADMIN-RUNTIME ERROR] code=MODE_MEMORY_WRITE_FAILED") end
            if dirty and started then write() end
            updateMarkers()
        end
        local ok=pcall(TheSim.SetPersistentString,TheSim,path,encoded,false,completed)
        if not ok then completed(false)end
    end
    local function cleanup(p)
        local o=owners[p];if not o then return end
        for name in pairs(names)do if o.modes[name] then pcall(M.Set,p,name,false,true) end end
        owners[p]=nil;p._dst_admin_managed_modes=nil
    end
    function M.Status(p,name)local o=owners[p];if name=="clear_all"then return o and next(o.modes)~=nil or false end;return o and o.modes[name]==true or false end
    function M.Remembering(p)return memories[p.userid]~=nil end
    function M.MemoryVerified(p,enabled)return loaded and not loadFailure and not writing and not dirty and persisted==revision and M.Remembering(p)==enabled end
    function M.Busy()return loading or writing or dirty end
    function M.Memory(p,enabled)
        if not loaded then error("MODE_MEMORY_LOADING") end
        if loadFailure then error("MODE_MEMORY_INVALID")end
        if enabled then
            local count=0;for _ in pairs(memories)do count=count+1 end
            if count>=64 and not memories[p.userid] then error("MODE_MEMORY_LIMIT") end
            local copy={};local o=owners[p];for k,v in pairs(o and o.modes or {})do if names[k] and v then copy[k]=true end end
            memories[p.userid]=copy
        else memories[p.userid]=nil end
        p._dst_admin_mode_memory=nil;revision=revision+1
        write()
    end
    function M.Set(p,name,enabled,cleaning)
        if name=="clear_all" and enabled==false then
            local safe=true
            for key in pairs(names)do if not pcall(M.Set,p,key,false,cleaning)then safe=false end end
            if not safe then error("MODE_OWNERSHIP_UNCERTAIN")end
            return
        end
        if not names[name] or type(enabled)~="boolean" or not p or not p.components or not p.components.health then error("MODE_ARGUMENT_INVALID") end
        local c=command();local m=c.ModeOwnership
        local o=owners[p]
        if not o then o={modes={}};owners[p]=o;p._dst_admin_managed_modes=o.modes end
        if (o.modes[name]==true)==enabled then return end
        local health,combat=p.components.health,p.components.combat
        if name=="invincible" then m.SetInvincibility(p,enabled)
        elseif name=="super_god" then c.SetGodMode(p,enabled)
        elseif name=="creative" then m.Creative(p,enabled)
        elseif name=="water_walk" then if not m.Motion(p,"water",enabled) then error("MODE_OWNERSHIP_UNCERTAIN") end
        elseif name=="noclip" then if not m.Motion(p,"noclip",enabled) then error("MODE_OWNERSHIP_UNCERTAIN") end
        elseif name=="health_lock" then
            if enabled then
                o.health=health;o.minBefore=health.minhealth;health:SetMinHealth(1)
            else
                if health~=o.health or health.minhealth~=1 then error("MODE_OWNERSHIP_UNCERTAIN") end
                health:SetMinHealth(o.minBefore or 0)
            end
        elseif name=="one_hit" then
            if not combat then error("COMPONENT_UNAVAILABLE") end
            if enabled then
                o.combat=combat;o.damageBefore=combat.CalcDamage
                o.damage=function(self,target,...)if target and target:HasTag("player") then return o.damageBefore(self,target,...) end;return 999999999 end
                combat.CalcDamage=o.damage
            else
                if combat~=o.combat or combat.CalcDamage~=o.damage then error("MODE_OWNERSHIP_UNCERTAIN") end
                combat.CalcDamage=o.damageBefore
            end
        elseif name=="invisible" then
            if enabled then
                o.notarget=p:HasTag("notarget");o.invisible=p:HasTag("invisible");p:AddTag("notarget");p:AddTag("invisible")
                local x,_,z=p.Transform:GetWorldPosition();local found=TheSim:FindEntities(x,0,z,40,nil,{"INLIMBO","FX"})
                for i=1,math.min(#found,128)do local cc=found[i].components and found[i].components.combat;if cc and cc.target==p then cc:SetTarget(nil) end end
            else
                if not o.notarget then p:RemoveTag("notarget") end;if not o.invisible then p:RemoveTag("invisible")end
            end
        end
        o.modes[name]=enabled and true or nil
        if not cleaning and memories[p.userid] then
            memories[p.userid][name]=enabled and true or nil;revision=revision+1;write()
        end
    end
    function M.Start()
        if started then return end
        if not TheWorld or not TheWorld.ListenForEvent or not TheWorld.RemoveEventCallback then loaded=true;return end
        started=true;world=TheWorld
        join=function(_,p)
            if not started or not loaded or not p or not memories[p.userid] then return end
            p._dst_admin_mode_memory=persisted==revision and not writing and not dirty and true or nil
            local wanted={};for k,v in pairs(memories[p.userid])do wanted[k]=v end
            -- One event-driven attempt; never retry effects after an exception.
            for name in pairs(wanted)do
                local ok=pcall(M.Set,p,name,true,true)
                if not ok then print("[DST-ADMIN-RUNTIME ERROR] code=MODE_MEMORY_RESTORE_UNCONFIRMED mode="..name) end
            end
        end
        leave=function(_,p)cleanup(p)end
        world:ListenForEvent("ms_playerjoined",join);world:ListenForEvent("ms_playerleft",leave)
        if loaded then
            for _,p in ipairs(AllPlayers or {})do join(world,p)end
            updateMarkers();return
        end
        loadGeneration=loadGeneration+1
        local epoch=loadGeneration
        loading=true
        TheSim:GetPersistentString(path,function(ok,source)
            if not started or epoch~=loadGeneration then return end
            loading=false
            if ok and type(source)=="string" and #source<=32768 and source~="" then
                local decoded,value=pcall(require("json").decode,source)
                if decoded and type(value)=="table" and value.version==1 and type(value.players)=="table" then
                    local n=0;local restored={}
                    for uid,modes in pairs(value.players)do
                        n=n+1;if n>64 then loadFailure=true;break end
                        if type(uid)=="string" and #uid<=80 and uid:match("^KU_[%w_-]+$") and type(modes)=="table" then
                            local clean={};for name,v in pairs(modes)do if not names[name] or v~=true then loadFailure=true;break end;clean[name]=true end
                            restored[uid]=clean
                        else loadFailure=true;break end
                    end
                    if not loadFailure then memories=restored end
                else loadFailure=true end
            elseif ok and source~="" then loadFailure=true end
            loaded=true
            if not loadFailure then for _,p in ipairs(AllPlayers or {})do join(world,p) end end
            updateMarkers()
        end)
    end
    function M.Stop()
        started=false
        loadGeneration=loadGeneration+1;loading=false
        if world and world.RemoveEventCallback then if join then world:RemoveEventCallback("ms_playerjoined",join)end;if leave then world:RemoveEventCallback("ms_playerleft",leave)end end
        for p in pairs(owners)do cleanup(p) end
        world=nil
    end
    return M
end)()
