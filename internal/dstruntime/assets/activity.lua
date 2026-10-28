local json = require("json")
local M = {}
local VERSION = "2.4.19"
local ROOT = "mod_config_data/dst-admin/"
local MAX_PENDING, MAX_BATCH, MAX_TARGETS = 512, 128, 32
local defaults = {enabled=true, positions=true, actions=true, combat=true, positionIntervalSeconds=15,
    minimumDistance=10, combatIntervalSeconds=10, flushIntervalSeconds=5, revision="default"}
local config = {}
for k,v in pairs(defaults) do config[k]=v end
local state = {epoch=0,running=false,ready=false,loading=false,writing=false,sequence=0,generation=0,slots={},pending={},
    players={},worldListeners={},dropped=0,lastError=nil}
local instance = string.format("%d-%06d",os.time(),math.random(0,999999))
local flush, schedule_flush, schedule_positions, attach, detach, publish_manifest, flush_actions, schedule_heartbeat

local function text(v,n)
    local s=tostring(v or ""):gsub("[%z\1-\31]"," ")
    if #s<=n then return s end
    local endpos=n
    while endpos>0 and string.byte(s,endpos)>=128 and string.byte(s,endpos)<192 do endpos=endpos-1 end
    local b=string.byte(s,endpos) or 0
    local size=b>=240 and 4 or b>=224 and 3 or b>=192 and 2 or 1
    if endpos+size-1>n then endpos=endpos-1 else endpos=n end
    return string.sub(s,1,endpos)
end
local function finite(v) return type(v)=="number" and v==v and v~=math.huge and v~=-math.huge end
local function position(e)
    if e==nil or e.Transform==nil then return nil end
    local ok,x,y,z=pcall(e.Transform.GetWorldPosition,e.Transform)
    if ok and finite(x) and finite(y) and finite(z) and math.abs(x)<=1e9 and math.abs(y)<=1e9 and math.abs(z)<=1e9 then
        local p={x=x,y=y,z=z}
        if TheWorld.Map and TheWorld.Map.GetTileAtPoint then
            local found,tile=pcall(TheWorld.Map.GetTileAtPoint,TheWorld.Map,x,y,z)
            if found and finite(tile) and math.abs(tile)<=2147483647 and tile==math.floor(tile) then p.tile=tile end
        end
        return p
    end
end
local function identity()
    local session,shard="",""
    if TheNet and TheNet.GetSessionIdentifier then local ok,v=pcall(TheNet.GetSessionIdentifier,TheNet);if ok then session=text(v,128) end end
    if TheShard and TheShard.GetShardId then local ok,v=pcall(TheShard.GetShardId,TheShard);if ok then shard=text(v,64) end end
    return session,shard
end
local function listen(list,source,event,callback)
    if source==nil or type(source.ListenForEvent)~="function" then return end
    local guarded=function(...) local ok=pcall(callback,...);if not ok then state.lastError="activity event unavailable";state.dropped=state.dropped+1 end end
    local ok=pcall(source.ListenForEvent,source,event,guarded)
    if ok then list[#list+1]={source=source,event=event,callback=guarded} end
end
local function unlisten(list)
    for _,v in ipairs(list) do
        if v.source.RemoveEventCallback then pcall(v.source.RemoveEventCallback,v.source,v.event,v.callback) end
    end
end
local function emit(player,kind,fields,critical,observedPosition,occurredAt)
    if not state.running or not state.ready or not config.enabled or not player or not player.userid then return end
    state.sequence=state.sequence+1
    if #state.pending>=MAX_PENDING or not critical and #state.pending>=MAX_PENDING-128 then state.dropped=state.dropped+1;return end
    local event={sequence=state.sequence,occurredAtUnix=occurredAt or os.time(),kind=kind,userId=text(player.userid,128),
        name=text(player.name,128),prefab=text(player.prefab,80),fields=fields or {}}
    if observedPosition==nil then event.position=position(player) elseif observedPosition~=false then event.position=observedPosition end
    state.pending[#state.pending+1]=event
    schedule_flush()
end
local function object_fields(e)
    local fields={}
    if e then
        fields.targetPrefab=text(e.prefab,80)
        fields.targetGuid=text(e.GUID,32)
        if e.HasTag then local ok,v=pcall(e.HasTag,e,"epic");if ok then fields.epic=v==true end end
        if e.components and e.components.stackable then
            local ok,n=pcall(e.components.stackable.StackSize,e.components.stackable)
            if ok and finite(n) and n>=0 and n<=1e15 then fields.quantity=n end
        end
    end
    return fields
end
-- Exactly one unresolved persistent write is allowed. A timeout reports
-- uncertainty but does not cancel the engine I/O or release this fence.
local function write_persistent(path,data,callback)
    if state.io then return false end
    local entry={path=path,startedAt=os.time(),epoch=state.epoch}
    state.io=entry
    local started=pcall(TheSim.SetPersistentString,TheSim,path,data,false,function(written)
        if state.io~=entry then return end
        state.io=nil
        if not state.running or state.epoch~=entry.epoch then return end
        callback(written==true)
    end)
    -- A throwing native call may have queued I/O before throwing. Without a
    -- terminal callback, retain the fence instead of guessing cancellation.
    if not started and state.io==entry then state.lastError="activity write completion unconfirmed" end
    return true
end
local function manifest_data(commit)
    local session,shard=identity()
    local count=0;for _ in pairs(state.players) do count=count+1 end
    local ordered={}
    for key,v in pairs(state.slots) do
        if not commit or key~=commit.slot then ordered[#ordered+1]=v end
    end
    if commit then ordered[#ordered+1]=commit.descriptor end
    table.sort(ordered,function(a,b) return a.generation<b.generation end)
    local manifest={schemaVersion=1,producerVersion=VERSION,producerInstanceId=instance,sessionId=session,shardId=shard,
        generation=commit and commit.generation or state.generation,slots=ordered,dropped=state.dropped,config=config,playerCount=count,
        pending=math.max(0,#state.pending-(commit and commit.count or 0)),updatedAtUnix=os.time(),lastError=state.lastError,
        publicationPhase="committed",writeUnconfirmed=false}
    local ok,data=pcall(json.encode_compliant,manifest)
    if ok then return data end
    state.lastError="activity manifest encode failed"
end
publish_manifest=function(callback,commit)
    local data=commit and commit.manifestData or manifest_data()
    if not data then if callback then callback(false) end;return end
    write_persistent(ROOT.."activity-manifest.json",data,function(written)
        if not written then state.lastError="activity manifest write failed" end
        if callback then callback(written) end
    end)
end
flush_actions=function(item,retain)
    for key,action in pairs(item.actions) do
        -- Maintenance drains accepted groups in batches without consuming
        -- the queue's critical-event reserve or dropping an unflushed group.
        if retain and #state.pending>=MAX_PENDING-128 then return end
        action.fields.endedAtUnix=action.lastAt
        emit(item.player,action.kind,action.fields,false,action.position or false,action.lastAt)
        item.actions[key]=nil
    end
end
local function flush_combat(force,retain)
    local now=os.time()
    for _,item in pairs(state.players) do
        for key,battle in pairs(item.battles) do
            if force or now-battle.startedAt>=config.combatIntervalSeconds then
                if retain and #state.pending>=MAX_PENDING-128 then return end
                battle.fields.startedAtUnix=battle.startedAt
                battle.fields.endedAtUnix=battle.lastHitAt
                emit(item.player,battle.incoming and "combat.received" or "combat.attack",battle.fields,false,battle.position or false,battle.lastHitAt)
                item.battles[key]=nil
            end
        end
    end
end
flush=function(completed)
    if not state.running or state.writing or state.draining and not completed then
        if completed then completed(false) end
        return
    end
    -- Retry the original transaction before collecting any new batch. Neither
    -- bytes nor generation are rebuilt after a manifest publication failure.
    local commit=state.commit
    if not commit then
        flush_combat(completed~=nil,completed~=nil)
        for _,item in pairs(state.players) do flush_actions(item,completed~=nil) end
        if #state.pending==0 then if completed then completed(true) end;return end
        local batch={}
        for i=1,math.min(MAX_BATCH,#state.pending) do batch[#batch+1]=state.pending[i] end
        local session,shard=identity()
        local generation=state.generation+1
        local slot=(generation-1)%64+1
        local payload={schemaVersion=1,producerVersion=VERSION,producerInstanceId=instance,sessionId=session,shardId=shard,
            generation=generation,firstSequence=batch[1].sequence,lastSequence=batch[#batch].sequence,events=batch}
        local ok,data=pcall(json.encode_compliant,payload)
        while ok and #data>65536 and #batch>1 do
            local count=math.max(1,math.floor(#batch/2))
            while #batch>count do table.remove(batch) end
            payload.lastSequence=batch[#batch].sequence
            ok,data=pcall(json.encode_compliant,payload)
        end
        if not ok or #data>65536 then
            state.lastError="activity batch exceeded encoding limit"
            if completed then completed(false);return end
            state.dropped=state.dropped+#batch
            for i=1,#batch do table.remove(state.pending,1) end
            schedule_flush();return
        end
        commit={slot=slot,generation=generation,count=#batch,data=data,phase="segment_pending",
            descriptor={slot=slot,generation=generation,producerInstanceId=instance,sessionId=session,shardId=shard,
                firstSequence=payload.firstSequence,lastSequence=payload.lastSequence}}
        state.commit=commit
    end
    state.writing=true
    local function finish(ok)
        state.writing=false
        schedule_flush()
        if completed then completed(ok) end
    end
    local function publish()
        commit.phase="manifest_pending"
        if not commit.manifestData then commit.manifestData=manifest_data(commit) end
        if not commit.manifestData then finish(false);return end
        publish_manifest(function(published)
            if published then
                state.slots[commit.slot]=commit.descriptor
                state.generation=commit.generation
                for i=1,commit.count do table.remove(state.pending,1) end
                state.commit=nil;state.lastError=nil
            end
            finish(published)
        end,commit)
    end
    if commit.phase=="manifest_pending" then publish();return end
    write_persistent(ROOT..string.format("activity-%02d.json",commit.slot),commit.data,function(written)
        if not written then state.lastError="activity segment write failed";finish(false);return end
        publish()
    end)
end
local function has_battles()
    for _,item in pairs(state.players) do if next(item.battles) or next(item.actions) then return true end end
    return false
end
schedule_flush=function()
    if not state.running or state.writing or state.draining or state.flushTask or (#state.pending==0 and not has_battles()) then return end
    state.flushTask=clock.After(config.flushIntervalSeconds,function() state.flushTask=nil;flush();schedule_flush() end,"dst-admin-activity-flush")
end
schedule_positions=function(reset)
    local active=state.running and state.ready and config.enabled and config.positions and next(state.players)
    if (reset or not active) and state.positionTask then state.positionTask:Cancel();state.positionTask=nil end
    if not active or state.positionTask then return end
    state.positionTask=clock.After(config.positionIntervalSeconds,function()
        state.positionTask=nil
        for _,item in pairs(state.players) do
            local p=position(item.player)
            local last=item.lastPosition
            if p and (last==nil or (p.x-last.x)^2+(p.z-last.z)^2>=config.minimumDistance^2 and (p.x~=last.x or p.z~=last.z or p.y~=last.y)) then
                emit(item.player,"position",{})
                item.lastPosition=p
            end
        end
        schedule_positions()
    end,"dst-admin-activity-positions")
end
local function combat_event(item,data,incoming)
    if not config.combat or not data then return end
    local target=incoming and data.attacker or data.target
    if target==nil then return end
    local fields=object_fields(target)
    local key=(incoming and "in:" or "out:")..fields.targetGuid..":"..fields.targetPrefab
    local battle=item.battles[key]
    if battle==nil then
        local count=0;for _ in pairs(item.battles) do count=count+1 end
        if count>=MAX_TARGETS then state.dropped=state.dropped+1;return end
        fields.hits=0;fields.knownDamageHits=0;fields.unknownDamageHits=0
        battle={fields=fields,startedAt=os.time(),incoming=incoming}
        item.battles[key]=battle
    end
    local f=battle.fields
    battle.position=position(item.player);battle.lastHitAt=os.time()
    f.hits=f.hits+1
    if finite(data.damageresolved) and data.damageresolved>=0 and data.damageresolved<=1e15 and (f.damage or 0)+data.damageresolved<=1e15 then
        f.damage=(f.damage or 0)+data.damageresolved;f.knownDamageHits=f.knownDamageHits+1
    else f.unknownDamageHits=f.unknownDamageHits+1 end
    if data.weapon then f.weapon=text(data.weapon.prefab,80) end
    schedule_flush()
end
detach=function(player,record)
    local id=player and player.userid
    local item=id and state.players[id]
    if not item or item.player~=player then return end
    for _,battle in pairs(item.battles) do
        battle.fields.startedAtUnix=battle.startedAt;battle.fields.endedAtUnix=os.time()
        emit(player,battle.incoming and "combat.received" or "combat.attack",battle.fields,false,battle.position or false,battle.lastHitAt)
    end
    flush_actions(item)
    if record then emit(player,"player.left",{},true) end
    unlisten(item.listeners)
    state.players[id]=nil
    schedule_positions();schedule_heartbeat()
end
attach=function(player,record)
    if not config.enabled or not player or type(player.userid)~="string" or player.userid=="" then return end
    if state.players[player.userid] and state.players[player.userid].player==player then return end
    if state.players[player.userid] then detach(state.players[player.userid].player,false) end
    local item={player=player,listeners={},battles={},actions={}}
    state.players[player.userid]=item
    if record then emit(player,"player.joined",{},true) end
    listen(item.listeners,player,"death",function(_,data) emit(player,"player.death",object_fields(data and data.afflicter),true) end)
    listen(item.listeners,player,"ms_respawnedfromghost",function() emit(player,"player.respawned",{},true) end)
    listen(item.listeners,player,"onremove",function() detach(player,true) end)
    local actions={onpickupitem={"action.pickup","item"},dropitem={"action.drop","item"},
        picksomething={"action.harvest","object"},oneat={"action.eat","food"},
        builditem={"action.build_item","item"},buildstructure={"action.build_structure","item"},finishedwork={"action.work","target"}}
    for event,description in pairs(actions) do
        local kind,field=description[1],description[2]
        listen(item.listeners,player,event,function(_,data)
            if config.actions then
                local fields=object_fields(data and data[field])
                if data and data.action then fields.action=text(data.action.id,40) end
                local key=kind..":"..(fields.targetPrefab or "")..":"..(fields.action or "")
                local action=item.actions[key]
                if not action then
                    local count=0;for _ in pairs(item.actions) do count=count+1 end
                    if count>=32 then state.dropped=state.dropped+1;return end
                    fields.count=0;fields.startedAtUnix=os.time()
                    action={kind=kind,fields=fields};item.actions[key]=action
                end
                if action.fields.targetGuid~=fields.targetGuid then action.fields.targetGuid="" end
                action.fields.count=action.fields.count+1
                -- The last event position/time belongs to the action, never the later flush.
                action.position=position(player);action.lastAt=os.time()
                schedule_flush()
            end
        end)
    end
    listen(item.listeners,player,"onhitother",function(_,data) combat_event(item,data,false) end)
    listen(item.listeners,player,"attacked",function(_,data) combat_event(item,data,true) end)
    listen(item.listeners,player,"killed",function(_,data)
        if config.combat then local fields=object_fields(data and data.victim);fields.fromAttack=data and data.from_doattack==true;emit(player,"combat.kill",fields,true) end
    end)
    schedule_positions();schedule_heartbeat()
end
local function valid_config(input)
    if type(input)~="table" or type(input.revision)~="string" or #input.revision<1 or #input.revision>80 then return nil end
    local nextconfig={revision=input.revision}
    for _,name in ipairs({"enabled","positions","actions","combat"}) do
        if type(input[name])~="boolean" then return nil end;nextconfig[name]=input[name]
    end
    for name,limits in pairs({positionIntervalSeconds={5,300},minimumDistance={0,100},combatIntervalSeconds={5,120},flushIntervalSeconds={2,30}}) do
        local n=input[name]
        if not finite(n) or n<limits[1] or n>limits[2] or n~=math.floor(n) then return nil end
        nextconfig[name]=n
    end
    return nextconfig
end
local function apply(nextconfig)
    local was=config.enabled
    local interval=config.positionIntervalSeconds
    -- Preserve accepted actions/combat before changing category switches.
    flush_combat(true)
    for _,item in pairs(state.players) do flush_actions(item) end
    config=nextconfig
    if not config.enabled then
        for _,item in pairs(state.players) do unlisten(item.listeners) end
        state.players={}
        if state.positionTask then state.positionTask:Cancel();state.positionTask=nil end
        if state.flushTask then state.flushTask:Cancel();state.flushTask=nil end
    elseif not was then for _,p in ipairs(AllPlayers or {}) do attach(p,false) end end
    schedule_positions(interval~=config.positionIntervalSeconds);schedule_flush();schedule_heartbeat()
end
function M.Configure(input)
    if not state.running or not state.ready or state.writing or state.commit or state.draining then return false,"ACTIVITY_BUSY" end
    local nextconfig=valid_config(input)
    if not nextconfig then return false,"INVALID_ACTIVITY_CONFIG" end
    local ok,data=pcall(json.encode_compliant,nextconfig)
    if not ok then return false,"INVALID_ACTIVITY_CONFIG" end
    state.writing=true
    write_persistent(ROOT.."activity-config.json",data,function(written)
        if written then apply(nextconfig) else state.lastError="activity config persistence failed" end
        publish_manifest(function() state.writing=false;schedule_flush() end)
    end)
    return true,"ACTIVITY_CONFIG_ACCEPTED"
end
schedule_heartbeat=function()
    if not state.running or not next(state.players) then
        if state.heartbeatTask then state.heartbeatTask:Cancel();state.heartbeatTask=nil end
        return
    end
    if state.heartbeatTask then return end
    state.heartbeatTask=clock.After(60,function()
        state.heartbeatTask=nil
        if not state.running then return end
        if next(state.players) and not state.writing and not state.commit and not state.draining then
            state.writing=true;publish_manifest(function() state.writing=false;schedule_flush() end)
        end
        schedule_heartbeat()
    end,"dst-admin-activity-heartbeat")
end
-- Drain only for an explicit Runtime handover. Ordinary recording keeps its
-- batching cadence. Input stays attached while asynchronous writes complete;
-- replacement runs in the same Lua turn as the final empty-queue check.
function M.Drain(callback,canContinue)
    if type(callback)~="function" then return false end
    if not state.running then callback(true);return true end
    if not state.ready or state.writing or state.draining then return false end
    local entry={epoch=state.epoch,batches=0,deadline=os.time()+5}
    state.draining=entry
    if state.flushTask then state.flushTask:Cancel();state.flushTask=nil end
    local function finish(ok)
        if state.draining~=entry then return end
        state.draining=nil
        if entry.timer then entry.timer:Cancel() end
        schedule_flush()
        pcall(callback,ok)
    end
    entry.finish=finish
    entry.timer=clock.After(5,function() finish(false) end,"dst-admin-activity-drain")
    local function advance(written)
        if state.draining~=entry then return end
        local allowed=true
        if canContinue then local ok,value=pcall(canContinue);allowed=ok and value==true end
        if not written or not allowed or not state.running or state.epoch~=entry.epoch or os.time()>=entry.deadline then finish(false);return end
        if #state.pending==0 and not has_battles() then finish(true);return end
        if entry.batches>=8 then finish(false);return end
        entry.batches=entry.batches+1
        flush(advance)
    end
    advance(true)
    return true
end
local function begin()
    if not state.running then return end
    state.ready=true
    listen(state.worldListeners,TheWorld,"ms_playerjoined",function(_,p) attach(p,true) end)
    listen(state.worldListeners,TheWorld,"ms_playerleft",function(_,p) detach(p,true) end)
    for _,p in ipairs(AllPlayers or {}) do attach(p,false) end
    state.writing=true
    publish_manifest(function() state.writing=false;schedule_flush() end)

end
local function load()
    state.readyTask=nil
    if not state.running or state.ready or state.loading then return end
    if not TheWorld or not TheWorld.ismastersim or not TheSim or not TheSim.GetPersistentString then
        state.readyTask=clock.After(1,load,"dst-admin-activity-ready");return
    end
    state.loading=true
    local epoch=state.epoch
    TheSim:GetPersistentString(ROOT.."activity-config.json",function(ok,source)
        if not state.running or state.epoch~=epoch then return end
        if ok and type(source)=="string" and #source<=4096 then local found,value=pcall(json.decode,source);local checked=found and valid_config(value);if checked then config=checked end end
        TheSim:GetPersistentString(ROOT.."activity-manifest.json",function(found,source)
            if not state.running or state.epoch~=epoch then return end
            if found and type(source)=="string" and #source<=65536 then
                local parsed,value=pcall(json.decode,source)
                if parsed and type(value)=="table" and value.schemaVersion==1 and finite(value.generation) and value.generation>=0 and value.generation<1e12 and type(value.slots)=="table" then
                    state.generation=math.floor(value.generation)
                    for _,slot in pairs(value.slots) do
                        if type(slot)=="table" and finite(slot.slot) and slot.slot>=1 and slot.slot<=64 and slot.slot==math.floor(slot.slot)
                            and finite(slot.generation) and slot.generation>=1 and slot.generation<=state.generation
                            and (slot.generation-1)%64+1==slot.slot and type(slot.producerInstanceId)=="string"
                            and #slot.producerInstanceId<=128 and type(slot.sessionId)=="string" and #slot.sessionId<=128
                            and type(slot.shardId)=="string" and #slot.shardId<=64 and finite(slot.firstSequence)
                            and finite(slot.lastSequence) and slot.firstSequence>=1 and slot.lastSequence>=slot.firstSequence then
                            state.slots[slot.slot]=slot
                        end
                    end
                end
            end
            state.loading=false
            begin()
        end)
    end)
end
function M.Start()
    if state.running then return true end
    state.epoch=state.epoch+1
    state.running=true;state.ready=false;load();return true
end
function M.Poll()
    if state.running and not state.ready and not state.loading and TheWorld and TheWorld.ismastersim then
        if state.readyTask then state.readyTask:Cancel();state.readyTask=nil end
        load()
    end
    clock.Poll()
end
function M.Stop()
    if not state.running then return true end
    if state.io or state.writing or state.commit then return false end
    state.dropped=state.dropped+#state.pending
    for _,item in pairs(state.players) do
        for _ in pairs(item.actions) do state.dropped=state.dropped+1 end
        for _ in pairs(item.battles) do state.dropped=state.dropped+1 end
    end
    state.pending={}
    state.epoch=state.epoch+1
    state.running=false;state.ready=false;state.loading=false;state.writing=false
    if state.draining then state.draining.finish(false) end
    for _,name in ipairs({"readyTask","positionTask","flushTask","heartbeatTask"}) do if state[name] then state[name]:Cancel();state[name]=nil end end
    unlisten(state.worldListeners);state.worldListeners={}
    for _,item in pairs(state.players) do unlisten(item.listeners) end
    state.players={}
    return true
end
function M.Status()
    return {producerInstanceId=instance,running=state.running,ready=state.ready,busy=state.writing or state.loading or state.draining~=nil,pending=#state.pending+(has_battles() and 1 or 0),
        dropped=state.dropped,generation=state.generation,lastError=state.lastError,
        publicationPhase=state.commit and state.commit.phase or state.io and "publishing" or "committed",
        writeUnconfirmed=state.io~=nil and os.time()-state.io.startedAt>=12}
end
M.version=VERSION
return M
