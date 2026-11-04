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
    player[marker] = false
    local owner = player._dst_admin_god_owner
    if owner == nil then return was == false end
    owner.sources[source]=nil
    if next(owner.sources)~=nil then return true end
    player._dst_admin_god_owner = nil
    local health = player.components and player.components.health
    if health ~= owner.health or health.SetInvincible ~= owner.wrapper then return false end
    health.SetInvincible = owner.setter
    local restore = owner.external
    if restore == nil then restore = owner.before end
    if type(restore) ~= "boolean" then return false end
    -- Another component can remove invincibility by writing its field directly.
    -- Preserve that observed change instead of re-enabling an old value.
    if health.invincible == false then return true end
    owner.setter(health, restore)
    return true
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
    if not o then return false end
    o.sources[source] = enabled and true or nil
    local active = next(o.sources)~=nil
    local safe = true
    if phys~=o.physics then safe=false
    elseif phys then
        if phys:GetCollisionMask()~=o.mask then safe=false
        else
            local mask=o.before
            if o.sources.noclip then mask=COLLISION.WORLD
            elseif o.sources.god then mask=phys:GetMass()>0 and COLLISION.GROUND or 0
            elseif o.sources.water then mask=bit.band(mask,bit.bnot(COLLISION.LIMITS)) end
            phys:SetCollisionMask(mask);o.mask=mask
        end
    end
    local drown=player.components and player.components.drownable
    if drown~=o.drown then safe=false
    elseif drown then
        if drown.ShouldDrown~=o.shouldDrown and drown.ShouldDrown~=o.wrapper then safe=false
        else drown.ShouldDrown=active and o.wrapper or o.shouldDrown end
    end
    if active then player:AddTag("dst_admin_waterwalk")
    elseif not o.tag then player:RemoveTag("dst_admin_waterwalk") end
    if not active then player._dst_admin_motion_owner=nil end
    return safe
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
        if not o then error("MODE_OWNERSHIP_UNCERTAIN") end
        player._dst_admin_creative_owner=nil
        if o.builder~=builder or type(o.before)~="boolean" then error("MODE_OWNERSHIP_UNCERTAIN") end
        if builder.freebuildmode==true then builder.freebuildmode=o.before end
    end
end
