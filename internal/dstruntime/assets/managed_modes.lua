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
    M.InspectionVersion=2
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
    local function owner(p)
        local o=owners[p]
        if not o then o={modes={}};owners[p]=o;p._dst_admin_managed_modes=o.modes end
        -- Adopt identifiable panel effects left by a dedicated/older entry.
        -- Never adopt a Mod's invincibility or free building without ownership.
        local health=p._dst_admin_god_owner;local motion=p._dst_admin_motion_owner
        if p._dst_admin_invincible_owned or health and health.sources.normal then o.modes.invincible=true end
        if p._dst_admin_god_task or health and health.sources.god or motion and motion.sources.god then o.modes.super_god=true end
        if p._dst_admin_creative_owner then o.modes.creative=true end
        if motion and motion.sources.water then o.modes.water_walk=true end
        if motion and motion.sources.noclip then o.modes.noclip=true end
        if p._dst_admin_one_hit_owner then o.modes.one_hit=true end
        local min=p._dst_admin_minhealth_owner
        if min and min.sources.mode then o.modes.health_lock=true end
        local tags=p._dst_admin_tag_owners
        if tags and tags.notarget and tags.notarget.sources.invisible then o.modes.invisible=true end
        return o
    end
    local function cleanup(p)
        local o=owner(p)
        for name in pairs(names)do if o.modes[name] then pcall(M.Set,p,name,false,true) end end
        owners[p]=nil;p._dst_admin_managed_modes=nil
    end
    function M.Status(p,name)
        if not p or not p.components then return nil end
        if name=="clear_all"then
            local unknown=false
            for key in pairs(names)do local state=M.Status(p,key);if state==true then return true elseif state==nil then unknown=true end end
            if unknown then return nil end
            return false
        end
        local m=command().ModeOwnership
        if name=="invincible" then return m.InvincibilityStatus(p,"normal")
        elseif name=="super_god" then
            local h=m.InvincibilityStatus(p,"god");local motion=m.MotionStatus(p,"god")
            if h==false and motion==false and not p._dst_admin_god_task then return false end
            if h==true and motion==true and p._dst_admin_god_task then return true end
        elseif name=="creative" then return m.CreativeStatus(p)
        elseif name=="water_walk" then return m.MotionStatus(p,"water")
        elseif name=="noclip" then return m.MotionStatus(p,"noclip")
        elseif name=="health_lock" then return m.HealthLockStatus(p,"mode")
        elseif name=="invisible" then return m.TagsStatus(p,"invisible",{"notarget","invisible"})
        elseif name=="one_hit" then
            local combat=p.components.combat
            if combat and combat._dst_admin_calc_damage~=nil then return nil end
            local hit=p._dst_admin_one_hit_owner
            if not hit then return false end
            if combat==hit.combat and combat.CalcDamage==hit.wrapper then return true end
        end
        return nil
    end
    function M.Remembering(p)return memories[p.userid]~=nil end
    function M.MemoryVerified(p,enabled)return loaded and not loadFailure and not writing and not dirty and persisted==revision and M.Remembering(p)==enabled end
    function M.Busy()return loading or writing or dirty end
    function M.Memory(p,enabled)
        if not loaded then error("MODE_MEMORY_LOADING") end
        if loadFailure then error("MODE_MEMORY_INVALID")end
        if enabled then
            local count=0;for _ in pairs(memories)do count=count+1 end
            if count>=64 and not memories[p.userid] then error("MODE_MEMORY_LIMIT") end
            local copy={};local o=owner(p)
            for k,v in pairs(o.modes)do
                if names[k] and v then
                    if M.Status(p,k)~=true then error("MODE_OWNERSHIP_UNCERTAIN") end
                    copy[k]=true
                end
            end
            memories[p.userid]=copy
        else memories[p.userid]=nil end
        p._dst_admin_mode_memory=nil;revision=revision+1
        write()
    end
    function M.Set(p,name,enabled,cleaning)
        if type(enabled)~="boolean" or not p or not p.components or not p.components.health then error("MODE_ARGUMENT_INVALID") end
        if name=="clear_all" and enabled==false then
            local safe=true
            for key in pairs(names)do if not pcall(M.Set,p,key,false,cleaning)then safe=false end end
            if not safe then error("MODE_OWNERSHIP_UNCERTAIN")end
            return
        end
        if not names[name] then error("MODE_ARGUMENT_INVALID") end
        local c=command();local m=c.ModeOwnership
        local o=owner(p)
        if name=="one_hit" and p.components.combat and p.components.combat._dst_admin_calc_damage~=nil then error("MODE_LEGACY_STATE_UNCERTAIN") end
        local observed=M.Status(p,name)
        if observed==enabled and (o.modes[name]==true)==enabled then return end
        if observed==nil and enabled then error("MODE_OWNERSHIP_UNCERTAIN") end
        local combat=p.components.combat
        if name=="invincible" then m.SetInvincibility(p,enabled)
        elseif name=="super_god" then c.ApplyGodMode(p,enabled)
        elseif name=="creative" then m.Creative(p,enabled)
        elseif name=="water_walk" then if not m.Motion(p,"water",enabled) then error("MODE_OWNERSHIP_UNCERTAIN") end
        elseif name=="noclip" then if not m.Motion(p,"noclip",enabled) then error("MODE_OWNERSHIP_UNCERTAIN") end
        elseif name=="health_lock" then
            m.HealthLock(p,"mode",enabled,1)
        elseif name=="one_hit" then
            if not combat then error("COMPONENT_UNAVAILABLE") end
            if combat._dst_admin_calc_damage~=nil then error("MODE_LEGACY_STATE_UNCERTAIN") end
            if enabled then
                local hit={combat=combat,before=combat.CalcDamage}
                hit.wrapper=function(self,target,...)
                    if target and target:HasTag("alwaysblock") then return 0 end
                    if target and target:HasTag("player") then return hit.before(self,target,...) end
                    return 999999999
                end
                combat.CalcDamage=hit.wrapper;p._dst_admin_one_hit_owner=hit
            else
                local hit=p._dst_admin_one_hit_owner
                if not hit or combat~=hit.combat or combat.CalcDamage~=hit.wrapper then error("MODE_OWNERSHIP_UNCERTAIN") end
                combat.CalcDamage=hit.before;p._dst_admin_one_hit_owner=nil
            end
        elseif name=="invisible" then
            if enabled then
                m.Tags(p,"invisible",{"notarget","invisible"},true)
                local x,_,z=p.Transform:GetWorldPosition();local found=TheSim:FindEntities(x,0,z,40,nil,{"INLIMBO","FX"})
                for i=1,math.min(#found,128)do local cc=found[i].components and found[i].components.combat;if cc and cc.target==p then cc:SetTarget(nil) end end
            else
                m.Tags(p,"invisible",{"notarget","invisible"},false)
            end
        end
        o.modes[name]=enabled and true or nil
        if M.Status(p,name)~=enabled then error("MODE_OWNERSHIP_UNCERTAIN") end
        if not cleaning and memories[p.userid] then
            memories[p.userid][name]=enabled and true or nil;revision=revision+1;write()
        end
    end
    function M.Start()
        if started then return end
        if not TheWorld or not TheWorld.ListenForEvent or not TheWorld.RemoveEventCallback then loaded=true;return end
        started=true;world=TheWorld
        join=function(_,p)
            if not started or not loaded or not p then return end
            owner(p)
            if not memories[p.userid] then return end
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
        for _,p in ipairs(AllPlayers or {})do owner(p) end
        for p in pairs(owners)do cleanup(p) end
        world=nil
    end
    return M
end)()
