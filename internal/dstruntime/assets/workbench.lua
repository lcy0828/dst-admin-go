-- Loaded inside Commands; no background tasks, files or global entity scans.
local Workbench = (function()
    local M, plans = {}, {}
    local supported = {extinguish=true, freeze=true, sleep=true, pacify=true, delete=true, kill=true}
    local function emit(value)
        print("[DST-ADMIN-BATCH-V1] " .. require("json").encode_compliant(value))
        return value
    end
    local function eligible(e, action, prefab)
        if WorkbenchV2 and WorkbenchV2.IsBlocked(e) then error("BATCH_PREVIOUS_UNCONFIRMED") end
        if not e or not e:IsValid() or (prefab~="" and e.prefab~=prefab) then return false end
        if e==TheWorld or e:HasTag("player") or e:HasTag("playerghost") or e:HasTag("INLIMBO") then return false end
        local c=e.components or {}
        if action=="delete" or action=="kill" then
            if e:HasTag("FX") or e:HasTag("NOCLICK") or e:HasTag("DECOR") or e:HasTag("CLASSIFIED") or e:HasTag("placer")
                or c.inventoryitem and (c.inventoryitem.owner or c.inventoryitem:IsHeld())
                or e.entity and e.entity:GetParent() then return false end
            if e:HasTag("structure") or e:HasTag("wall") or e:HasTag("boat") or e:HasTag("epic") or e:HasTag("playerowned")
                or c.follower and c.follower:GetLeader() or c.domesticatable and c.domesticatable:IsDomesticated()
                or c.domesticatable and c.domesticatable.GetDomestication and c.domesticatable:GetDomestication()>0
                or c.rideable and c.rideable:IsBeingRidden() then return false end
        end
        if action=="delete" then return true end
        if action=="kill" then return c.health~=nil and not c.health:IsDead() end
        if action=="extinguish" then
            if e.prefab=="campfire" or e.prefab=="firepit" or e.prefab=="coldfire" or e.prefab=="coldfirepit" or e.prefab=="nightlight" then return false end
            return c.burnable~=nil and c.burnable:IsBurning()
        end
        if action=="freeze" then return c.freezable~=nil end
        if action=="sleep" then return c.sleeper~=nil end
        if action=="pacify" then return c.combat~=nil and c.combat.target~=nil and c.combat.target:HasTag("player") end
        return false
    end
    local function apply(e, action)
        local c=e.components or {}
        if action=="delete" then e:Remove();return not e:IsValid() end
        if action=="kill" then c.health:Kill();return c.health:IsDead() end
        if action=="extinguish" then c.burnable:Extinguish();return not c.burnable:IsBurning() end
        if action=="freeze" then c.freezable:AddColdness(1,60);return c.freezable.IsFrozen and c.freezable:IsFrozen() or false end
        if action=="sleep" then c.sleeper:AddSleepiness(10,20);return c.sleeper.IsAsleep and c.sleeper:IsAsleep() or false end
        if action=="pacify" then c.combat:SetTarget(nil);c.combat:GiveUp();return c.combat.target==nil end
    end
    function M.Nearby(player_id, action, radius, prefab, phase, token, fresh_token, caller)
        if type(caller)~="string" or caller=="" then error("BATCH_CALLER_REQUIRED") end
        if not supported[action] or type(radius)~="number" or radius<1 or radius>64 then error("BATCH_ARGUMENT_INVALID") end
        local now=os.time()
        local count=0
        for key, plan in pairs(plans) do
            if plan.untilTime<=now then plans[key]=nil else count=count+1 end
        end
        if phase=="cancel" then
            local plan=plans[token]
            if not plan or plan.caller~=caller or plan.playerId~=player_id or plan.action~=action or plan.prefab~=prefab or plan.radius~=radius then error("BATCH_PREVIEW_EXPIRED") end
            plans[token]=nil
            if WorkbenchV2.ReleaseLegacyPreview then WorkbenchV2.ReleaseLegacyPreview(token) end
            return emit({state="cancelled",matched=#plan.entities,attempted=0,verified=0,unconfirmed=0,skipped=0,failed=0})
        end
        local p=UserToPlayer(player_id)
        if not p or not p.Transform or p.migration then error("PLAYER_LOCATION_UNCERTAIN") end
        if phase=="preview" then
            if count>=16 then error("BATCH_BUSY") end
            if WorkbenchV2.ReserveLegacyPreview then WorkbenchV2.ReserveLegacyPreview(fresh_token,caller) end
            local x,y,z=p.Transform:GetWorldPosition()
            -- One spatial query; its native engine call is synchronous. Refuse
            -- dense results before any game mutation, never run a global scan.
            local deadline=os.clock()+.004
            local candidates=TheSim:FindEntities(x,y,z,radius,nil,{"INLIMBO","FX","player","playerghost"})
            if #candidates>512 then error("BATCH_SCAN_LIMIT") end
            local chosen={}
            for _,e in ipairs(candidates) do
                if os.clock()>deadline then error("BATCH_CPU_LIMIT") end
                local ok,accept=pcall(eligible,e,action,prefab)
                if not ok then error("BATCH_PREVIEW_FAILED") end
                if accept then chosen[#chosen+1]=e end
                if #chosen>40 then error("BATCH_TARGET_LIMIT") end
            end
            plans[fresh_token]={caller=caller,world=TheWorld,player=p,playerId=player_id,action=action,radius=radius,prefab=prefab,x=x,y=y,z=z,entities=chosen,untilTime=now+30}
            return emit({state="preview",previewId=fresh_token,matched=#chosen,attempted=0,verified=0,unconfirmed=0,skipped=0,failed=0,expiresIn=30,x=x,z=z})
        end
        if phase~="execute" then error("BATCH_PHASE_INVALID") end
        local plan=plans[token]
        if not plan or plan.caller~=caller or plan.world~=TheWorld or plan.player~=p or plan.playerId~=player_id or plan.action~=action or plan.radius~=radius or plan.prefab~=prefab then error("BATCH_PREVIEW_EXPIRED") end
        plans[token]=nil -- consume before any writes; never replay partial work
        if WorkbenchV2.ReleaseLegacyPreview then WorkbenchV2.ReleaseLegacyPreview(token) end
        if WorkbenchV2.IsBusy and WorkbenchV2.IsBusy() then error("BATCH_BUSY") end
        local out={state="completed",matched=#plan.entities,attempted=0,verified=0,unconfirmed=0,skipped=0,failed=0}
        local deadline=os.clock()+.004
        for _,e in ipairs(plan.entities) do
            if os.clock()>deadline then out.skipped=out.skipped+1;out.reason="CPU_BUDGET"
            else
                local ok, accept=pcall(function()
                    if not eligible(e,action,prefab) then return false end
                    local x,_,z=e.Transform:GetWorldPosition()
                    return (x-plan.x)^2+(z-plan.z)^2<=radius^2
                end)
                if not ok then out.failed=out.failed+1
                elseif not accept then out.skipped=out.skipped+1
                else
                    out.attempted=out.attempted+1
                    local applied,verified=pcall(apply,e,action)
                    if not applied then out.unconfirmed=out.unconfirmed+1
                    elseif verified then out.verified=out.verified+1
                    else out.unconfirmed=out.unconfirmed+1 end
                    if (not applied or not verified) and WorkbenchV2.Block then WorkbenchV2.Block(e) end
                end
            end
        end
        if out.skipped>0 or out.failed>0 or out.unconfirmed>0 then out.state="partial" end
        return emit(out)
    end
    M.ResultVersion=2
    M.NearbyV2=WorkbenchV2.Nearby
    M.Setting=WorkbenchV2.Setting
    M.TakeObservation=WorkbenchV2.TakeObservation
    M.FinishObservation=WorkbenchV2.FinishObservation
    M.Encode=WorkbenchV2.Encode
    M.Stop=WorkbenchV2.Stop
    return M
end)()
