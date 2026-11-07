-- No polling of its own. Track method ownership so a later Mod setter is never
-- replaced by a stale baseline. Direct Mod field writes can remain ambiguous.
local ModeOwnership = {}
function ModeOwnership.Begin(player, source)
    source = source or "god"
    local marker = source=="god" and "_dst_admin_god_owned" or source=="normal" and "_dst_admin_invincible_owned" or "_dst_admin_stealth_owned"
    player[marker] = true
    if player._dst_admin_god_owner ~= nil then player._dst_admin_god_owner.sources[source]=true;return end
    local health = player.components and player.components.health
    if health == nil then return end
    local owner = {health=health, setter=health.SetInvincible, before=health.invincible, sources={[source]=true}}
    owner.wrapper = function(self, enabled, ...)
        if not owner.applying then owner.external = enabled end
        return owner.setter(self, enabled, ...)
    end
    health.SetInvincible = owner.wrapper
    player._dst_admin_god_owner = owner
end
function ModeOwnership.Apply(player)
    local owner = player._dst_admin_god_owner
    local health = player.components and player.components.health
    if owner == nil or health ~= owner.health or health.SetInvincible ~= owner.wrapper then return false end
    owner.applying = true
    local ok, err = pcall(owner.wrapper, health, true)
    owner.applying = false
    if not ok then error(err) end
    return health.invincible == true
end
function ModeOwnership.End(player, source)
    source = source or "god"
    local marker = source=="god" and "_dst_admin_god_owned" or source=="normal" and "_dst_admin_invincible_owned" or "_dst_admin_stealth_owned"
    local was = player[marker]
    local owner = player._dst_admin_god_owner
    if owner == nil then
        if was==true then return false end
        player[marker]=false;return true
    end
    if not owner.sources[source] then player[marker]=false;return true end
    for key in pairs(owner.sources)do
        if key~=source then owner.sources[source]=nil;player[marker]=false;return true end
    end
    local health = player.components and player.components.health
    if health ~= owner.health or health.SetInvincible ~= owner.wrapper then return false end
    local restore = owner.external
    if restore == nil then restore = owner.before end
    if type(restore) ~= "boolean" then return false end
    -- Another component can remove invincibility by writing its field directly.
    -- Preserve that observed change instead of re-enabling an old value.
    if health.invincible~=false then owner.setter(health,restore) end
    health.SetInvincible=owner.setter
    player._dst_admin_god_owner=nil;player[marker]=false
    return true
end

function ModeOwnership.InvincibilityStatus(player,source)
    local o=player._dst_admin_god_owner
    if not o then
        local marker=source=="god" and "_dst_admin_god_owned" or source=="normal" and "_dst_admin_invincible_owned" or "_dst_admin_stealth_owned"
        if player[marker]==true then return nil end
        return false
    end
    if not o.sources[source] then return false end
    local h=player.components and player.components.health
    if h==o.health and h.SetInvincible==o.wrapper and h.invincible==true then return true end
    return nil
end

function ModeOwnership.SetInvincibility(player, enabled)
    if type(enabled)~="boolean" or not player or not player.components or not player.components.health then error("MODE_ARGUMENT_INVALID") end
    if enabled then ModeOwnership.Begin(player,"normal");if not ModeOwnership.Apply(player) then error("MODE_OWNERSHIP_UNCERTAIN") end
    elseif not ModeOwnership.End(player,"normal") then error("MODE_OWNERSHIP_UNCERTAIN") end
end

-- Collision masks and drowning are shared by comprehensive god mode and water
-- walking. Restore only while the exact component/value we installed survives.
function ModeOwnership.Motion(player, source, enabled)
    local o = player._dst_admin_motion_owner
    local phys = player.Physics
    if enabled and not o then
        if phys and (not phys.GetCollisionMask or not phys.SetCollisionMask) then error("MODE_OWNERSHIP_UNAVAILABLE") end
        local drown = player.components and player.components.drownable
        o = {physics=phys,before=phys and phys:GetCollisionMask(),sources={},drown=drown,
            shouldDrown=drown and drown.ShouldDrown,tag=player:HasTag("dst_admin_waterwalk")}
        o.mask = o.before
        o.wrapper = function()return false end
        player._dst_admin_motion_owner = o
    end
    if not o then return true end
    if phys~=o.physics or phys and phys:GetCollisionMask()~=o.mask then return false end
    local drown=player.components and player.components.drownable
    if drown~=o.drown or drown and drown.ShouldDrown~=o.shouldDrown and drown.ShouldDrown~=o.wrapper then return false end
    o.sources[source]=enabled and true or nil
    local active=next(o.sources)~=nil
    if phys then
            local mask=o.before
            if o.sources.noclip then mask=COLLISION.WORLD
            elseif o.sources.god then mask=phys:GetMass()>0 and COLLISION.GROUND or 0
            elseif o.sources.water then mask=bit.band(mask,bit.bnot(COLLISION.LIMITS)) end
            phys:SetCollisionMask(mask);o.mask=mask
    end
    if drown then drown.ShouldDrown=active and o.wrapper or o.shouldDrown end
    if active then player:AddTag("dst_admin_waterwalk")
    elseif not o.tag then player:RemoveTag("dst_admin_waterwalk") end
    if not active then player._dst_admin_motion_owner=nil end
    return true
end

function ModeOwnership.MotionStatus(player,source)
    local o=player._dst_admin_motion_owner
    if not o or not o.sources[source] then return false end
    local phys=player.Physics;local drown=player.components and player.components.drownable
    if phys~=o.physics or phys and phys:GetCollisionMask()~=o.mask or drown~=o.drown or drown and drown.ShouldDrown~=o.wrapper then return nil end
    return true
end

function ModeOwnership.Creative(player, enabled)
    local builder=player.components and player.components.builder
    if not builder then error("COMPONENT_UNAVAILABLE") end
    if enabled then
        if not player._dst_admin_creative_owner then
            player._dst_admin_creative_owner={builder=builder,before=builder.freebuildmode}
        end
        if player._dst_admin_creative_owner.builder~=builder then error("MODE_OWNERSHIP_UNCERTAIN") end
        builder.freebuildmode=true
    else
        local o=player._dst_admin_creative_owner
        if not o then return end
        if o.builder~=builder or type(o.before)~="boolean" then error("MODE_OWNERSHIP_UNCERTAIN") end
        if builder.freebuildmode==true then builder.freebuildmode=o.before end
        player._dst_admin_creative_owner=nil
    end
end

function ModeOwnership.CreativeStatus(player)
    local o=player._dst_admin_creative_owner
    if not o then return false end
    local b=player.components and player.components.builder
    if b==o.builder and b.freebuildmode==true then return true end
    return nil
end

-- Different panel features may hold the same tag or minimum health value.
-- Release only the requesting source, preserving the original game/Mod value.
function ModeOwnership.Tags(player,source,tags,enabled,baseline)
    local owners=player._dst_admin_tag_owners or {};player._dst_admin_tag_owners=owners
    for _,tag in ipairs(tags)do
        local o=owners[tag]
        if not o and (enabled or baseline and baseline[tag]~=nil) then
            o={before=baseline and baseline[tag] or player:HasTag(tag),sources={}}
            if baseline and baseline[tag]==false then o.before=false end
            owners[tag]=o
            if not enabled then o.sources[source]=true end
        end
        if o then
            o.sources[source]=enabled and true or nil
            if next(o.sources) then player:AddTag(tag)
            else if not o.before then player:RemoveTag(tag) end;owners[tag]=nil end
        end
    end
end

function ModeOwnership.TagsStatus(player,source,tags)
    local owners=player._dst_admin_tag_owners or {};local count=0
    for _,tag in ipairs(tags)do
        local o=owners[tag]
        if o and o.sources[source] then
            count=count+1
            if not player:HasTag(tag) then return nil end
        end
    end
    if count==0 then return false end
    if count~=#tags then return nil end
    return true
end

function ModeOwnership.HealthLock(player,source,enabled,value)
    local h=player.components and player.components.health
    if not h then error("COMPONENT_UNAVAILABLE") end
    local o=player._dst_admin_minhealth_owner
    if not o then
        if player._dst_admin_health_lock_enabled then error("MODE_LEGACY_STATE_UNCERTAIN") end
        if not enabled then return end
        o={health=h,setter=h.SetMinHealth,before=h.minhealth or 0,applied=h.minhealth,sources={}}
        player._dst_admin_minhealth_owner=o
    end
    if h~=o.health or h.SetMinHealth~=o.setter or h.minhealth~=o.applied then error("MODE_OWNERSHIP_UNCERTAIN") end
    local wanted=o.before
    for key,v in pairs(o.sources)do if key~=source then wanted=math.max(wanted,v) end end
    if enabled then
        if type(value)~="number" or value~=value or value<1 or value==math.huge then error("MODE_ARGUMENT_INVALID") end
        wanted=math.max(wanted,value)
    end
    o.setter(h,wanted);o.applied=wanted
    if h.minhealth~=wanted then error("MODE_OWNERSHIP_UNCERTAIN") end
    o.sources[source]=enabled and value or nil
    if not next(o.sources) then player._dst_admin_minhealth_owner=nil end
end

function ModeOwnership.HealthLockStatus(player,source)
    local o=player._dst_admin_minhealth_owner
    if not o or not o.sources[source] then return false end
    local h=player.components and player.components.health
    if h==o.health and h.SetMinHealth==o.setter and h.minhealth==o.applied then return true end
    return nil
end
