local json = require("json")

local M = {}
local SCHEMA_VERSION = 1
local PRODUCER_VERSION = "2.4.25"
local OUTPUT_ROOT = "mod_config_data/dst-admin/"
local FLUSH_DELAY = 1
local MAX_BATCH = 128

local state = {
    running = false,
    ready = false,
    writing = false,
    sequence = 0,
    nextSlot = "a",
    pending = {},
    listeners = {},
    watches = {},
    flushTask = nil,
    readyTask = nil,
    readyAttempts = 0,
    lastError = nil,
    noticeWindow = 0,
    noticeSources = {},
    noticeTotal = 0,
}

local instance_id = string.format("%d-%06d", os.time(), math.random(0, 999999))

local function safe_text(value, limit)
    if value == nil then return "" end
    value = tostring(value)
    if #value > limit then return string.sub(value, 1, limit) end
    return value
end

local function runtime_identity()
    local session_id = ""
    local shard_id = ""
    if TheNet ~= nil and TheNet.GetSessionIdentifier ~= nil then
        local ok, value = pcall(TheNet.GetSessionIdentifier, TheNet)
        if ok then session_id = safe_text(value, 128) end
    end
    if TheShard ~= nil and TheShard.GetShardId ~= nil then
        local ok, value = pcall(TheShard.GetShardId, TheShard)
        if ok then shard_id = safe_text(value, 64) end
    end
    return session_id, shard_id
end

local flush

local function schedule_flush()
    if not state.running or state.writing or state.flushTask ~= nil or #state.pending == 0 then return end
    state.flushTask = clock.After(FLUSH_DELAY, function()
        state.flushTask = nil
        flush()
    end, "dst-admin-events-flush")
end

local function valid_notice(fields)
    if type(fields) ~= "table" then return false end
    local count = 0
    for key in pairs(fields) do
        if key ~= "source" and key ~= "code" and key ~= "message" then return false end
        count = count + 1
    end
    if count ~= 3 then return false end
    for _, key in ipairs({"source", "code"}) do
        local v = fields[key]
        if type(v) ~= "string" or #v < 1 or #v > 64 or not v:match("^[A-Za-z0-9_][A-Za-z0-9_.-]*$") then return false end
    end
    local text = fields.message
    if type(text) ~= "string" or #text < 1 or #text > 1024 or text:match("%c") or not text:match("%S") then return false end
    -- Validate complete UTF-8 sequences; never truncate inside a character.
    local i = 1
    while i <= #text do
        local b = text:byte(i)
        local n = b < 128 and 1 or b >= 194 and b <= 223 and 2 or b >= 224 and b <= 239 and 3 or b >= 240 and b <= 244 and 4 or 0
        if n == 0 or i+n-1 > #text then return false end
        for j = 1, n-1 do local c=text:byte(i+j); if c<128 or c>191 then return false end end
        local c=text:byte(i+1)
        if b==194 and c<=159 then return false end
        if n==3 and (b==224 and c<160 or b==237 and c>159) or n==4 and (b==240 and c<144 or b==244 and c>143) then return false end
        i=i+n
    end
    local window=math.floor(os.time()/60)
    if state.noticeWindow~=window then state.noticeWindow=window;state.noticeSources={};state.noticeTotal=0 end
    local existing=state.noticeSources[fields.source] or 0
    local sources=0;for _ in pairs(state.noticeSources) do sources=sources+1 end
    if state.noticeTotal>=20 or existing>=5 or existing==0 and sources>=8 or #state.pending>=MAX_BATCH-32 then return false end
    state.noticeSources[fields.source]=existing+1;state.noticeTotal=state.noticeTotal+1
    return true
end

local function emit(kind, fields)
    if not state.running or not state.ready then return false end
    if kind=="mod.notice" and not valid_notice(fields) then return false end
    state.sequence = state.sequence + 1
    if #state.pending >= MAX_BATCH then table.remove(state.pending, 1) end
    state.pending[#state.pending + 1] = {
        sequence = state.sequence,
        kind = safe_text(kind, 64),
        occurredAtUnix = os.time(),
        fields = fields or {},
    }
    schedule_flush()
    return true
end

flush = function()
    if not state.running or state.writing or #state.pending == 0 then return false end
    local batch = state.pending
    state.pending = {}
    local session_id, shard_id = runtime_identity()
    local payload = {
        schemaVersion = SCHEMA_VERSION,
        producerVersion = PRODUCER_VERSION,
        producerInstanceId = instance_id,
        sessionId = session_id,
        shardId = shard_id,
        firstSequence = batch[1].sequence,
        lastSequence = batch[#batch].sequence,
        events = batch,
    }
    local encoded_ok, encoded = pcall(json.encode_compliant, payload)
    if not encoded_ok or type(encoded) ~= "string" then
        state.lastError = "event batch JSON encoding failed"
        for _, event in ipairs(batch) do state.pending[#state.pending + 1] = event end
        return false
    end
    state.writing = true
    local slot = state.nextSlot
    TheSim:SetPersistentString(OUTPUT_ROOT .. "events-" .. slot .. ".json", encoded, false, function(written)
        state.writing = false
        if written then
            state.nextSlot = slot == "a" and "b" or "a"
            state.lastError = nil
        else
            state.lastError = "event batch persistence failed"
            local merged = {}
            for _, event in ipairs(batch) do merged[#merged + 1] = event end
            for _, event in ipairs(state.pending) do merged[#merged + 1] = event end
            while #merged > MAX_BATCH do table.remove(merged, 1) end
            state.pending = merged
        end
        schedule_flush()
    end)
    return true
end

local function listen(source, name, callback)
    if source == nil or source.ListenForEvent == nil then return end
    local ok = pcall(source.ListenForEvent, source, name, callback)
    if ok then state.listeners[#state.listeners + 1] = { source = source, name = name, callback = callback } end
end

local function watch(name, callback)
    if TheWorld == nil or TheWorld.WatchWorldState == nil then return end
    local ok = pcall(TheWorld.WatchWorldState, TheWorld, name, callback)
    if ok then state.watches[#state.watches + 1] = { name = name, callback = callback } end
end

local function player_fields(player)
    return {
        userId = safe_text(player ~= nil and player.userid or "", 128),
        prefab = safe_text(player ~= nil and player.prefab or "", 128),
    }
end

local function begin_capture()
    state.ready = true
    listen(TheWorld, "ms_playerjoined", function(_, player) emit("player.joined", player_fields(player)) end)
    listen(TheWorld, "ms_playerleft", function(_, player) emit("player.left", player_fields(player)) end)
    listen(TheWorld, "ms_save", function() emit("world.save", {}) end)
    listen(TheWorld, "ms_worldreset", function() emit("world.reset", {}) end)
    listen(TheWorld, "ms_shutdown", function() emit("world.shutdown", {}) end)
    watch("cycles", function(_, value) emit("world.cycles", { cycles = tonumber(value) or 0 }) end)
    watch("phase", function(_, value) emit("world.phase", { phase = safe_text(value, 32) }) end)
    watch("season", function(_, value) emit("world.season", { season = safe_text(value, 32) }) end)
    watch("israining", function(_, value) emit("world.rain", { raining = value == true }) end)
end

local function ready_for_capture()
    return TheWorld ~= nil and TheWorld.ismastersim == true
        and TheSim ~= nil and TheSim.SetPersistentString ~= nil
end

local function await_world()
    if not state.running then return end
    if ready_for_capture() then
        state.readyTask = nil
        begin_capture()
        return
    end
    state.readyAttempts = state.readyAttempts + 1
    state.readyTask = clock.After(clock.ReadyDelay(state.readyAttempts), await_world, "dst-admin-events-ready")
end

function M.Poll()
    clock.Poll()
    if state.running and not state.ready then
        if state.readyTask ~= nil then state.readyTask:Cancel(); state.readyTask = nil end
        await_world()
    end
end

function M.Start()
    if state.running then return true end
    state.running = true
    state.ready = false
    state.readyAttempts = 0
    state.lastError = nil
    await_world()
    return true
end

function M.Stop()
    if not state.running then return true end
    state.running = false
    state.ready = false
    if state.readyTask ~= nil then state.readyTask:Cancel(); state.readyTask = nil end
    if state.flushTask ~= nil then state.flushTask:Cancel(); state.flushTask = nil end
    for _, listener in ipairs(state.listeners) do
        if listener.source ~= nil and listener.source.RemoveEventCallback ~= nil then
            pcall(listener.source.RemoveEventCallback, listener.source, listener.name, listener.callback)
        end
    end
    for _, item in ipairs(state.watches) do
        if TheWorld ~= nil and TheWorld.StopWatchingWorldState ~= nil then
            pcall(TheWorld.StopWatchingWorldState, TheWorld, item.name, item.callback)
        end
    end
    state.listeners = {}
    state.watches = {}
    state.pending = {}
    return true
end

function M.Emit(kind, fields) return emit(kind, fields) end

-- Stable, optional API for mod authors. No Lua, callbacks or game actions are
-- accepted. Old runtimes do not expose Notify; callers should feature-detect it.
function M.Notify(source, code, message)
    return emit("mod.notice", {source=source, code=code, message=message})
end

function M.Status()
    return {
        running = state.running,
        ready = state.ready,
        busy = state.writing,
        sequence = state.sequence,
        lastError = state.lastError,
    }
end

M.version = PRODUCER_VERSION
return M
