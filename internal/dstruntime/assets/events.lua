local json = require("json")

local M = {}
local SCHEMA_VERSION = 1
local PRODUCER_VERSION = "2.3.0"
local OUTPUT_ROOT = "mod_config_data/dst-admin/"
local FLUSH_DELAY = 1
local MAX_BATCH = 128
local READY_RETRY_SECONDS = 1
local READY_RETRY_LIMIT = 60

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
    state.flushTask = TheWorld:DoTaskInTime(FLUSH_DELAY, function()
        state.flushTask = nil
        flush()
    end)
end

local function emit(kind, fields)
    if not state.running or not state.ready then return end
    state.sequence = state.sequence + 1
    if #state.pending >= MAX_BATCH then table.remove(state.pending, 1) end
    state.pending[#state.pending + 1] = {
        sequence = state.sequence,
        kind = safe_text(kind, 64),
        occurredAtUnix = os.time(),
        fields = fields or {},
    }
    schedule_flush()
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
    local encoded_ok, encoded = pcall(json.encode, payload)
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
    if state.readyAttempts >= READY_RETRY_LIMIT then
        state.readyTask = nil
        state.lastError = "world readiness timed out"
        return
    end
    state.readyTask = scheduler:ExecuteInTime(READY_RETRY_SECONDS, await_world, "dst-admin-events-ready")
end

function M.Start()
    if state.running then return true end
    state.running = true
    state.ready = false
    state.readyAttempts = 0
    state.lastError = nil
    state.readyTask = scheduler:ExecuteInTime(0, await_world, "dst-admin-events-ready")
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

function M.Emit(kind, fields) emit(kind, fields) end

function M.Status()
    return {
        running = state.running,
        ready = state.ready,
        busy = state.writing,
        sequence = state.sequence,
        lastError = state.lastError,
    }
end

return M
