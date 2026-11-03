local PlayerSettings = (function()
    local M, owned = {}, {}
    local function native(c,name,method)return c and type(c[method])=="function" and c[method]==require("components/"..name)[method]end
    local source = {} -- module-owned source, never remove another mod's modifier
    local function notifyAge(o)
        -- Native StopDamageOverTime brackets healthdelta with this transient
        -- flag. Restore it even when a mod's event handler raises an error.
        local previous=o._taking_time_damage
        local ok,err=pcall(o.StopDamageOverTime,o)
        o._taking_time_damage=previous
        if not ok then error(err) end
    end
    function M.Plan(p,action,value,amount)
        local c=p.components or {}
        if p:HasTag("playerghost") or not c.health or c.health:IsDead() then error("PLAYER_NOT_ALIVE") end
        if action=="set_player_hunger_rate" then
            local h=c.hunger;local m=h and h.burnratemodifiers
            local modifiers=require("util/sourcemodifierlist")
            if not m or m._base~=1 or m._fn~=modifiers.multiply or m.SetModifier~=modifiers.SetModifier
                or m.RemoveModifier~=modifiers.RemoveModifier or m.Get~=modifiers.Get then error("HUNGER_UNSUPPORTED") end
            if value~="set" and value~="remove" or type(amount)~="number" or amount~=amount or amount<0 or amount>5 then error("SETTING_ARGUMENT_INVALID") end
            local entry=m._modifiers and m._modifiers[source]
            local current=entry and entry.modifiers and entry.modifiers.hunger or 1
            return {component=h,modifiers=m,before=current,after=value=="set" and amount or 1,steps=1,units=0,spawns=0,destination="none"}
        elseif action=="set_wanda_age" then
            local h,o=c.health,c.oldager
            if p.prefab~="wanda" or not native(o,"oldager","StopDamageOverTime") or not native(h,"health","SetPercent") or not p.sg
                or not native(h,"health","SetVal") or not native(h,"health","DoDelta") or o._taking_time_damage
                or not ({young=true,normal=true,old=true})[p.age_state]
                or p.sg:HasStateTag("nomorph") or p.sg:HasStateTag("busy") or o.damage_remaining~=0 then error("WANDA_AGE_UNAVAILABLE") end
            local target=({young=.9,normal=.5,old=.1})[value]
            local youngest,oldest=TUNING.WANDA_MIN_YEARS_OLD,TUNING.WANDA_MAX_YEARS_OLD
            if not target or type(youngest)~="number" or type(oldest)~="number" or h.maxhealth~=oldest-youngest
                or h.maxhealth~=TUNING.WANDA_OLDAGER or h:GetMaxWithPenalty()~=h.maxhealth
                or not (target==.9 and target>=TUNING.WANDA_AGE_THRESHOLD_YOUNG
                    or target==.5 and target>TUNING.WANDA_AGE_THRESHOLD_OLD and target<TUNING.WANDA_AGE_THRESHOLD_YOUNG
                    or target==.1 and target<=TUNING.WANDA_AGE_THRESHOLD_OLD) then error("WANDA_AGE_UNAVAILABLE") end
            return {component=o,health=h,before=youngest+h.maxhealth-h.currenthealth,after=youngest+h.maxhealth-h.maxhealth*target,healthBefore=h.currenthealth,healthAfter=h.maxhealth*target,percent=target,steps=2,units=0,spawns=0,destination="none"}
        elseif action=="restore_woodie_human" then
            if p.prefab~="woodie" or not native(c.wereness,"wereness","SetPercent") or not p:HasTag("wereplayer") or not p.sg
                or p.sg:HasStateTag("busy") or p.sg:HasStateTag("nomorph") or p.sg:HasStateTag("silentmorph")
                or c.rider and c.rider:IsRiding() or p.GetCurrentPlatform and p:GetCurrentPlatform() then error("WOODIE_TRANSFORM_UNAVAILABLE") end
            return {component=c.wereness,before=c.wereness:GetPercent(),after=0,steps=1,units=0,spawns=0,destination="none"}
        end
        error("SETTING_UNSUPPORTED")
    end
    function M.Same(p,action,value,amount,plan)
        local current=M.Plan(p,action,value,amount)
        return current.component==plan.component and current.health==plan.health and current.before==plan.before and current.after==plan.after and current.modifiers==plan.modifiers
    end
    function M.Apply(p,action,value,amount,plan)
        if action=="set_player_hunger_rate" then
            if value=="remove" then
                plan.modifiers:RemoveModifier(source,"hunger")
                if owned[p] then p:RemoveEventCallback("onremove",owned[p].onremove) end
                owned[p]=nil
            else
                if not owned[p] then
                    local onremove=function()local entry=owned[p];if entry then entry.modifiers:RemoveModifier(source,"hunger");owned[p]=nil end end
                    p:ListenForEvent("onremove",onremove)
                    owned[p]={modifiers=plan.modifiers,onremove=onremove}
                end
                plan.modifiers:SetModifier(source,amount,"hunger");owned[p].value=amount
            end
            local mods=plan.modifiers._modifiers
            local entry=mods and mods[source]
            local actual=entry and entry.modifiers and entry.modifiers.hunger
            local expected=value=="set" and amount~=1 and amount or nil
            return actual==expected and "verified" or "unconfirmed","",0
        elseif action=="set_wanda_age" then
            -- Native Wanda health is an inverse age meter (60 years, age 20–80).
            -- SetPercent uses SetVal and emits the native HUD/age-state update;
            -- no invented oldager:SetAge method and no cooldown changes.
            plan.health:SetPercent(plan.percent,false,"debug_key")
            notifyAge(plan.component)
            local verified=math.abs(plan.health.currenthealth-plan.healthAfter)<.001 and p.age_state==value
            return verified and "verified" or "unconfirmed",verified and "" or "TRANSITION_PENDING",0
        elseif action=="restore_woodie_human" then
            plan.component:SetPercent(0)
            local verified=not p:HasTag("wereplayer")
            return verified and "verified" or "unconfirmed",verified and "" or "TRANSITION_PENDING",0
        end
    end
    function M.Transition(p,action,value,plan)
        if action=="restore_woodie_human" then return not p:HasTag("wereplayer") end
        if action=="set_wanda_age" then
            if math.abs(plan.health.currenthealth-plan.healthAfter)>.001 then return false end
            if p.age_state~=value and not p.sg:HasStateTag("nomorph") and not plan.notified then
                plan.notified=true;notifyAge(plan.component)
            end
            return p.age_state==value
        end
        return false
    end
    function M.Stop()
        local ok=true
        for p,entry in pairs(owned) do
            local cleaned=pcall(function()entry.modifiers:RemoveModifier(source,"hunger");p:RemoveEventCallback("onremove",entry.onremove)end)
            if not cleaned then ok=false end
            owned[p]=nil
        end
        return ok
    end
    return M
end)()
