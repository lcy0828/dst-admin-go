-- Versioned fixed interactions. All state is private to this Runtime instance.
-- Idle cost: none. Cleanup/rate budgets are evaluated only on a request.
local WorkbenchV2 = (function()
    local M, plans, limits = {}, {}, {}
    local uncertain=setmetatable({}, {__mode="k"})
    local nearby={pick=true,repair=true,fertilize=true,grow=true,ripen=true,harvest=true,repair_burnt=true}
    local active=false
    local awaiting=false
    local observation=nil
    local function encode(out)
        local encoded=require("json").encode_compliant(out)
        if #encoded>12240 then error("BATCH_OUTPUT_LIMIT") end
        return "[DST-ADMIN-BATCH-V2] "..encoded
    end
    local function emit(out) print(encode(out));return out end
    local function base(action,request,world)
        return {version=2,action=action,requestId=request,worldId=world,state="preview",matched=0,attempted=0,verified=0,unconfirmed=0,skipped=0,failed=0,items={},detailsComplete=true,units=0}
    end
    local function identity()
        return tostring(TheWorld.GUID or "world")
    end
    local function cleanup(now)
        local count=0
        for key,plan in pairs(plans) do if plan.untilTime<=now then plans[key]=nil else count=count+1 end end
        for key,v in pairs(limits) do if v.untilTime<=now then limits[key]=nil end end
        return count
    end
    local function throttle(caller,now)
        local function take(key,window,max)
            local v=limits[key]
            if not v then v={untilTime=now+window,count=0};limits[key]=v end
            if v.count>=max then error("BATCH_RATE_LIMIT") end
            v.count=v.count+1
        end
        take("world",10,4);take(caller,2,2)
    end
    local function previewBudget(caller,now)
        if cleanup(now)>=16 then error("BATCH_BUSY") end
        local mine=0;for _,v in pairs(plans) do if v.caller==caller then mine=mine+1 end end
        if mine>=4 then error("BATCH_BUSY") end
        throttle(caller,now)
    end
    -- V1 and V2 share admission limits in the upgraded Runtime. V1's wire
    -- format and its own frozen object cache remain unchanged.
    function M.ReserveLegacyPreview(token,caller)
        local now=os.time();previewBudget(caller,now)
        plans[token]={legacy=true,caller=caller,untilTime=now+30}
    end
    function M.ReleaseLegacyPreview(token)
        if plans[token] and plans[token].legacy then plans[token]=nil end
    end
    function M.IsBusy()return active or awaiting end
    function M.BeginExternal()if M.IsBusy()then error("BATCH_BUSY")end;active=true end
    function M.EndExternal()active=false end
    function M.ContinueExternal(fn)
        if active then error("BATCH_BUSY")end
        active=true;local ok,result=pcall(fn);active=false;if not ok then error(result)end;return result
    end
    function M.ObserveExternal(fn)if awaiting then error("BATCH_BUSY")end;awaiting=true;observation=fn end
    local function valid(e)
        return e and e.IsValid and e:IsValid() and e.Transform and not e:HasTag("INLIMBO") and not e:HasTag("player") and not e:HasTag("playerghost") and not e:HasTag("FX")
    end
    local function nearbyPlan(p,action,radius,prefab,deadline)
        local x,y,z=p.Transform:GetWorldPosition()
        local entities=TheSim:FindEntities(x,y,z,radius,nil,{"INLIMBO","FX","player","playerghost"})
        if #entities>512 then error("BATCH_SCAN_LIMIT") end
        local chosen={};local steps,units,spawns=0,0,0
        for _,e in ipairs(entities) do
            if os.clock()>deadline then error("BATCH_CPU_LIMIT") end
            if valid(e) and (prefab=="" or e.prefab==prefab) then
                local adapter=NearbyAdapters.Plan(e,action,p)
                if adapter then
                    if uncertain[e] then error("BATCH_PREVIOUS_UNCONFIRMED") end
                    chosen[#chosen+1]={entity=e,prefab=e.prefab,adapter=adapter}
                    steps=steps+adapter.steps;units=units+adapter.units;spawns=spawns+adapter.spawns
                    if #chosen>(action=="repair_burnt" and 10 or 40) or steps>40 or units>200 or spawns>40 then error("BATCH_TARGET_LIMIT") end
                end
            end
        end
        table.sort(chosen,function(a,b)
            local ax,_,az=a.entity.Transform:GetWorldPosition();local bx,_,bz=b.entity.Transform:GetWorldPosition()
            local ad,bd=(ax-x)^2+(az-z)^2,(bx-x)^2+(bz-z)^2
            if ad==bd then return tostring(a.entity.GUID)<tostring(b.entity.GUID) end
            return ad<bd
        end)
        return chosen,x,y,z
    end
    local function run(p,action,value,amount,radius,prefab,phase,token,fresh,caller,request,isNearby)
        if type(caller)~="string" or #caller<1 or #caller>128 or type(request)~="string" or #request~=36 then error("BATCH_CALLER_REQUIRED") end
        local now=os.time();cleanup(now)
        if phase~="preview" and phase~="execute" and phase~="cancel" then error("BATCH_PHASE_INVALID") end
        local worldId=identity();local out=base(action,request,worldId)
        if phase=="preview" then
            previewBudget(caller,now)
            if not p or not p.Transform or p.migration then error("PLAYER_LOCATION_UNCERTAIN") end
            local deadline=os.clock()+.004
            local chosen,x,y,z
            if isNearby then chosen,x,y,z=nearbyPlan(p,action,radius,prefab,deadline)
            else
                if uncertain[p] then error("BATCH_PREVIOUS_UNCONFIRMED") end
                local adapter=PlayerSettings.Plan(p,action,value,amount)
                chosen={{entity=p,prefab=p.prefab,adapter=adapter}};x,y,z=p.Transform:GetWorldPosition()
            end
            plans[fresh]={caller=caller,world=TheWorld,player=p,action=action,value=value,amount=amount,radius=radius,prefab=prefab,chosen=chosen,x=x,y=y,z=z,untilTime=now+30}
            out.previewId=fresh;out.expiresIn=30;out.observedAt=now;out.expiresAt=now+30;out.x=x;out.z=z;out.matched=#chosen
            for i,v in ipairs(chosen) do
                out.items[i]={prefab=v.prefab,state="prepared",destination=v.adapter.destination,units=v.adapter.units,before=v.adapter.before,after=v.adapter.after}
            end
            return emit(out)
        end
        local plan=plans[token]
        if not plan or plan.caller~=caller or plan.world~=TheWorld or plan.player~=p or plan.action~=action
            or plan.radius~=radius or plan.prefab~=prefab or plan.value~=value or plan.amount~=amount then error("BATCH_PREVIEW_EXPIRED") end
        plans[token]=nil -- consume before mutation, including a refused duplicate
        out.matched=#plan.chosen
        if phase=="cancel" then out.state="cancelled";return emit(out) end
        if M.IsBusy() then error("BATCH_BUSY") end
        active=true
        local ok,err=pcall(function()
            out.state="completed";local deadline=os.clock()+.004;local stop=false
            for i,v in ipairs(plan.chosen) do
                local e,adapter=v.entity,v.adapter
                local row={prefab=v.prefab,state="skipped",destination=adapter.destination,units=0,before=adapter.before,after=adapter.after}
                out.items[i]=row
                if stop or os.clock()>deadline then row.reason="CPU_BUDGET"
                else
                    local checked,same=pcall(function()
                        if uncertain[e] then return false end
                        if not p or p.migration or not p:IsValid() or p~=UserToPlayer(p.userid) then return false end
                        if isNearby then
                            if not valid(e) or e.prefab~=v.prefab then return false end
                            local ex,_,ez=e.Transform:GetWorldPosition()
                            if (ex-plan.x)^2+(ez-plan.z)^2>radius^2 then return false end
                            if action=="pick" or action=="harvest" then
                                local px,_,pz=p.Transform:GetWorldPosition()
                                if (px-plan.x)^2+(pz-plan.z)^2>4 then return false end
                            end
                            return NearbyAdapters.Same(e,action,p,adapter)
                        end
                        return PlayerSettings.Same(p,action,value,amount,adapter)
                    end)
                    if not checked then row.reason="TARGET_UNAVAILABLE"
                    elseif not same then row.reason="TARGET_CHANGED"
                    else
                        out.attempted=out.attempted+1
                        local applied,state,reason,units
                        if isNearby then applied,state,reason,units=pcall(NearbyAdapters.Apply,e,action,p,adapter,deadline)
                        else applied,state,reason,units=pcall(PlayerSettings.Apply,p,action,value,amount,adapter) end
                        -- Calls can throw after changing state. Never label such
                        -- an exception as a proven no-effect failure.
                        if not applied then row.state="unconfirmed";row.reason="EFFECT_UNCONFIRMED"
                        else row.state=state;row.reason=reason~="" and reason or nil;row.units=units or 0;if state=="skipped" then out.attempted=out.attempted-1 end end
                        if action=="repair_burnt" and row.state=="unconfirmed" then stop=true end
                    end
                end
                if row.state=="unconfirmed" then
                    uncertain[e]=true
                    if adapter.uncertainReplacement then uncertain[adapter.uncertainReplacement]=true end
                end
                out[row.state]=out[row.state]+1;out.units=out.units+row.units
            end
            if out.verified~=out.matched then out.state="partial" end
        end)
        active=false
        if not ok then error(err) end
        if (action=="restore_woodie_human" or action=="set_wanda_age") and out.unconfirmed==1 and out.items[1].reason=="TRANSITION_PENDING" then
            awaiting=true
            observation=function()
                if not p:IsValid() or p.migration or p~=UserToPlayer(p.userid) then return out end
                if not PlayerSettings.Transition(p,action,value,plan.chosen[1].adapter) then return nil end
                out.state="completed";out.verified=1;out.unconfirmed=0;out.items[1].state="verified";out.items[1].reason=nil;uncertain[p]=nil
                return out
            end
        end
        return emit(out)
    end
    function M.Nearby(player,action,radius,prefab,phase,token,fresh,caller,request)
        if not nearby[action] or type(radius)~="number" or radius<1 or radius>64 or radius%1~=0 or type(prefab)~="string" or #prefab>80 or prefab~="" and not prefab:match("^[a-z0-9_]+$") then error("BATCH_ARGUMENT_INVALID") end
        return run(UserToPlayer(player),action,"",0,radius,prefab,phase,token,fresh,caller,request,true)
    end
    function M.Setting(player,action,value,amount,phase,token,fresh,caller,request)
        return run(UserToPlayer(player),action,value,amount,0,"",phase,token,fresh,caller,request,false)
    end
    function M.TakeObservation() local f=observation;observation=nil;return f end
    function M.FinishObservation() awaiting=false end
    function M.IsBlocked(e)return uncertain[e]==true end
    function M.Block(e)uncertain[e]=true end
    M.Encode=encode
    function M.Stop() plans={};observation=nil;awaiting=false;return PlayerSettings.Stop() end
    return M
end)()
