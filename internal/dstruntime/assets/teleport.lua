-- Demand-driven movement checks shared by fixed workbench entry points.
-- No timers, file writes, world scans, automatic dismount or collision changes.
do
    local plans = {}
    local function finite(n) return type(n)=="number" and n==n and math.abs(n)<=100000 end
    local function point(p) local x,_,z=p.Transform:GetWorldPosition();return {x=x,z=z} end
    local function maintenance()
        local r=rawget(_G,"DSTAdmin")
        local b=r and r.Barriers
        if b and b.Status then local status=b.Status();if status.holding or status.inFlight then error("WORLD_MAINTENANCE") end end
    end
    local function player(id)
        local p=UserToPlayer(id)
        if not p or not p:IsValid() or not p.Transform then error("PLAYER_OFFLINE") end
        if p.migration or p:HasTag("migrating") or p:HasTag("INLIMBO") then error("PLAYER_MOVING_WORLD") end
        if p:HasTag("playerghost") or p.components.health and p.components.health:IsDead() then error("PLAYER_GHOST") end
        if p.components.rider and p.components.rider:IsRiding() then error("PLAYER_RIDING") end
        if p.GetCurrentPlatform and p:GetCurrentPlatform() then error("PLAYER_ON_BOAT") end
        if p:HasTag("wereplayer") or p:HasTag("werebeaver") or p:HasTag("weremoose") or p:HasTag("weregoose") then error("PLAYER_TRANSFORMED") end
        if not p.Physics or not p.Physics.Teleport then error("MOVEMENT_UNSUPPORTED") end
        return p
    end
    local function landing(p,x,z,deadline)
        if os.clock()>deadline then error("TELEPORT_CPU_LIMIT") end
        if not finite(x) or not finite(z) then return false end
        local map=TheWorld and TheWorld.Map
        if not map or not map.IsPassableAtPoint or not map.IsAboveGroundAtPoint or not map.IsGroundTargetBlocked or not map.IsOceanAtPoint or not map.GetPlatformAtPoint then error("TERRAIN_CHECK_UNSUPPORTED") end
        -- The engine defaults consider a boat passable and hide the ocean
        -- beneath it. Explicitly exclude platforms and visual land overhang.
        if not map:IsAboveGroundAtPoint(x,0,z,false) or not map:IsPassableAtPoint(x,0,z,false,true) or map:IsOceanAtPoint(x,0,z,true) or map:GetPlatformAtPoint(x,z) or map:IsGroundTargetBlocked(Vector3(x,0,z)) then return false end
        local entities=TheSim:FindEntities(x,0,z,3,nil,{"INLIMBO","FX","CLASSIFIED"})
        if #entities>128 then error("TELEPORT_SCAN_LIMIT") end
        for _,e in ipairs(entities) do
            if os.clock()>deadline then error("TELEPORT_CPU_LIMIT") end
            if e~=p and e:IsValid() and e.Transform then
                local c=e.components or {}
                local obstacle=e:HasTag("wall") or e:HasTag("blocker") or e:HasTag("structure") or e:HasTag("player") or c.teleporter~=nil or e:HasTag("sinkhole")
                if obstacle then
                    local ex,_,ez=e.Transform:GetWorldPosition()
                    local radius=c.teleporter and 2 or math.max(.8,e.Physics and e.Physics.GetRadius and e.Physics:GetRadius() or .8)
                    if (x-ex)^2+(z-ez)^2<(radius+.5)^2 then return false end
                end
            end
        end
        return true
    end
    local function choose(p,x,z,deadline,reserved)
        local function available(xx,zz)
            for _,at in ipairs(reserved or {}) do if (xx-at.x)^2+(zz-at.z)^2<2.25 then return false end end
            return landing(p,xx,zz,deadline)
        end
        if available(x,z) then return {x=x,z=z} end
        -- Fixed, bounded search. The returned point is shown before confirmation.
        for i=1,23 do
            local radius=2*math.ceil(i/8)
            local a=(i-1)*math.pi/4
            local xx,zz=x+radius*math.cos(a),z+radius*math.sin(a)
            if available(xx,zz) then return {x=xx,z=zz} end
        end
        error("NO_LANDING_POINT")
    end
    local function emit(out)
        out.observedAt=os.time()
        print("[DST-ADMIN-TELEPORT-V1] "..require("json").encode_compliant(out))
        return out
    end
    function Workbench.Teleport(q)
        local out={state="rejected",operationId=q.operationId,playerId=q.playerId}
        local moved=false
        local ok,why=pcall(function()
            if type(q.caller)~="string" or q.caller=="" or type(q.operationId)~="string" or #q.operationId>64 then error("TELEPORT_ARGUMENT_INVALID") end
            if q.phase=="cancel" then
                local plan=plans[q.operationId]
                if plan and plan.player and plan.caller==q.caller and plan.player.userid==q.playerId then
                    plans[q.operationId]=nil
                end
                out.state="cancelled";return
            end
            maintenance()
            local now,deadline=os.time(),os.clock()+.004
            local count=0
            for key,v in pairs(plans) do if v.expires<=now then plans[key]=nil else count=count+1 end end
            local p=player(q.playerId)
            out.playerName=p.GetDisplayName and p:GetDisplayName() or q.playerId
            out.sessionId=TheNet:GetSessionIdentifier()
            if q.sessionId~=out.sessionId then error("WORLD_CHANGED") end
            if q.phase=="capture" then out.point=point(p);out.state="captured";return end
            if q.phase=="prepare" then
                if count>=16 then error("TELEPORT_BUSY") end
                for _,v in pairs(plans) do if v.player==p or v.members and v.members[p] then error("TELEPORT_BUSY") end end
                local x,z=q.x,q.z
                local target=nil
                if q.targetPlayerId and q.targetPlayerId~="" then target=player(q.targetPlayerId);local at=point(target);x,z=at.x,at.z end
                if not finite(x) or not finite(z) then error("TELEPORT_ARGUMENT_INVALID") end
                local at=choose(p,x,z,deadline)
                local expiry=os.time()+60
                plans[q.operationId]={player=p,world=TheWorld,caller=q.caller,point=at,expires=expiry,target=target,targetPoint=target and point(target) or nil}
                out.state="prepared";out.point=at;out.expiresAt=expiry;return
            end
            local plan=plans[q.operationId]
            if not plan or plan.caller~=q.caller or plan.world~=TheWorld or plan.player~=p then error("PREVIEW_EXPIRED") end
            plans[q.operationId]=nil -- single-use, even if a subsequent check fails
            if q.phase~="execute" then error("TELEPORT_ARGUMENT_INVALID") end
            if q.notAfter and os.time()>=q.notAfter then error("PREVIEW_EXPIRED") end
            if plan.target then
                if not plan.target:IsValid() or UserToPlayer(q.targetPlayerId)~=plan.target then error("TARGET_MOVED") end
                local at=point(plan.target)
                if (at.x-plan.targetPoint.x)^2+(at.z-plan.targetPoint.z)^2>4 then error("TARGET_MOVED") end
            end
            local at=plan.point
            if not landing(p,at.x,at.z,deadline) then error("LANDING_CHANGED") end
            out.point=at;out.before=point(p)
            moved=true
            if p.components.locomotor then p.components.locomotor:Stop() end
            p.Physics:Teleport(at.x,0,at.z)
            out.after=point(p)
            out.state=(out.after.x-at.x)^2+(out.after.z-at.z)^2<=.25 and "confirmed" or "uncertain"
        end)
        if not ok then out.state=moved and "uncertain" or "rejected";out.reason=tostring(why):match("([A-Z][A-Z_]+)$") or "TELEPORT_CHECK_FAILED" end
        return emit(out)
    end
    -- Trusted legacy commands retain their existing confirmation contract, but
    -- check all targets before moving any. They do not accept untrusted scripts.
    function Workbench.MovePlayers(moves,options)
        maintenance()
        options=options or {}
        local now,count=os.time(),0
        for key,v in pairs(plans) do if v.expires<=now then plans[key]=nil else count=count+1 end end
        local deadline=os.clock()+.004
        local frozen,reserved,seen={},{},{}
        if options.phase=="execute" or options.phase=="cancel" then
            local plan=plans[options.previewId]
            if not plan or not plan.moves or plan.world~=TheWorld then error("PREVIEW_EXPIRED") end
            plans[options.previewId]=nil
            frozen=plan.moves
            if options.phase=="cancel" then
                print("[DST-ADMIN-MOVE-V1] "..require("json").encode_compliant({state="cancelled",attempted=0,verified=0,observedAt=now}))
                return
            end
            -- Check the complete frozen set before the first mutation.
            for _,m in ipairs(frozen) do
                if player(m.player.userid)~=m.player or not landing(m.player,m.point.x,m.point.z,deadline) then error("LANDING_CHANGED") end
            end
        else
          if type(moves)~="table" or #moves>40 then error("TELEPORT_TARGET_LIMIT") end
          for _,m in ipairs(moves) do
            local p=player(m.id)
            if seen[m.id] then error("TELEPORT_DUPLICATE_PLAYER") end
            seen[m.id]=true
            for _,plan in pairs(plans) do if plan.player==p or plan.members and plan.members[p] then error("TELEPORT_BUSY") end end
            local at=choose(p,m.x,m.z,deadline,reserved)
            reserved[#reserved+1]=at
            frozen[#frozen+1]={player=p,point=at}
          end
        end
        if options.phase=="preview" then
            if count>=16 then error("TELEPORT_BUSY") end
            local members,items={},{}
            for _,m in ipairs(frozen) do
                members[m.player]=true
                items[#items+1]={playerId=m.player.userid,playerName=m.player.GetDisplayName and m.player:GetDisplayName() or m.player.userid,state="prepared",point=m.point,before=point(m.player)}
            end
            local expires=os.time()+60
            plans[options.previewId]={world=TheWorld,moves=frozen,members=members,expires=expires}
            print("[DST-ADMIN-MOVE-V1] "..require("json").encode_compliant({state="preview",previewId=options.previewId,expiresAt=expires,observedAt=os.time(),attempted=0,verified=0,items=#items>0 and items or nil}))
            return
        end
        local result={state="confirmed",attempted=0,verified=0,items={},observedAt=os.time()}
        for _,m in ipairs(frozen) do
            local item={playerId=m.player.userid,playerName=m.player.GetDisplayName and m.player:GetDisplayName() or m.player.userid,point=m.point,state="rejected"}
            local attempted=false
            local ok,why=pcall(function()
                if player(m.player.userid)~=m.player or not landing(m.player,m.point.x,m.point.z,deadline) then error("LANDING_CHANGED") end
                item.before=point(m.player)
                result.attempted=result.attempted+1
                attempted=true
                if m.player.components.locomotor then m.player.components.locomotor:Stop() end
                m.player.Physics:Teleport(m.point.x,0,m.point.z)
                item.after=point(m.player)
                item.state="uncertain"
                if (item.after.x-m.point.x)^2+(item.after.z-m.point.z)^2<=.25 then item.state="confirmed";result.verified=result.verified+1 end
            end)
            if not ok then
                item.state=attempted and "uncertain" or "rejected"
                item.reason=tostring(why):match("([A-Z][A-Z_]+)$") or "TELEPORT_CHECK_FAILED"
            end
            result.items[#result.items+1]=item
        end
        if result.verified~=#frozen then result.state="partial" end
        if #frozen==0 then result.items=nil end
        print("[DST-ADMIN-MOVE-V1] "..require("json").encode_compliant(result))
        return result
    end
end
