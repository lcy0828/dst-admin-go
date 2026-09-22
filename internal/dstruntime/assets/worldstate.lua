local json = require("json")

local M = {}
local SCHEMA_VERSION = 2
local PRODUCER_VERSION = "2.4.8"
local SNAPSHOT_INTERVAL = 5
local READY_RETRY_SECONDS = 1
local READY_RETRY_LIMIT = 60
local OUTPUT_ROOT = "mod_config_data/dst-admin/"

local state = {
    running = false,
    ready = false,
    writing = false,
    sequence = 0,
    nextSlot = "a",
    readyAttempts = 0,
    readyTask = nil,
    periodicTask = nil,
    lastCapturedAtUnix = nil,
    lastWrittenAtUnix = nil,
    lastDurationMilliseconds = nil,
    lastError = nil,
    consecutiveFailures = 0,
}

local instance_id = string.format("%d-%06d", os.time(), math.random(0, 999999))

local function finite_number(value)
    return type(value) == "number" and value == value and value > -math.huge and value < math.huge
end

local function read_number(read)
    local ok, value = pcall(read)
    if ok then
        value = tonumber(value)
        if finite_number(value) then return value end
    end
    return nil
end

local function read_non_negative_integer(read)
    local value = read_number(read)
    if value == nil or value < 0 then return nil end
    return math.floor(value)
end

local function safe_text(value, limit)
    if value == nil then return "" end
    value = tostring(value)
    if #value > limit then return string.sub(value, 1, limit) end
    return value
end

local function read_text(read, limit)
    local ok, value = pcall(read)
    if not ok then return "" end
    return safe_text(value, limit)
end

local function call_number(object, method_name)
    if object == nil or type(object[method_name]) ~= "function" then return nil end
    return read_number(function() return object[method_name](object) end)
end

local function call_text(object, method_name, limit)
    if object == nil or type(object[method_name]) ~= "function" then return "" end
    return read_text(function() return object[method_name](object) end, limit)
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

local function precipitation(values)
    if values.isacidraining == true then return "acid_rain" end
    if values.islunarhailing == true then return "lunar_hail" end
    if values.issnowing == true then return "snow" end
    if values.israining == true then return "rain" end
    return "none"
end

local function host_performance()
    if TheNet == nil or type(TheNet.GetClientTable) ~= "function" then return nil end
    local ok, clients = pcall(TheNet.GetClientTable, TheNet)
    if not ok or type(clients) ~= "table" then return nil end
    for _, client in ipairs(clients) do
        local value = client ~= nil and tonumber(client.performance) or nil
        if finite_number(value) and value >= 0 and value <= 2 then
            return math.floor(value)
        end
    end
    return nil
end

local function capture_world_state()
    local values = TheWorld ~= nil and TheWorld.state or {}
    local components = TheWorld ~= nil and TheWorld.components or {}
    local season_manager = components ~= nil and components.seasonmanager or nil
    local nightmare_clock = components ~= nil and components.nightmareclock or nil
    local season_progress = read_number(function() return values.seasonprogress end)
    if season_progress == nil then season_progress = call_number(season_manager, "GetPercentSeason") end
    local nightmare_progress = read_number(function() return values.nightmaretimeinphase end)
    if nightmare_progress == nil then nightmare_progress = call_number(nightmare_clock, "GetTimeInPhase") end
    local nightmare_phase = read_text(function() return values.nightmarephase end, 64)
    if nightmare_phase == "" then nightmare_phase = call_text(nightmare_clock, "GetPhase", 64) end

    return {
        season = read_text(function() return values.season end, 64),
        phase = read_text(function() return values.phase end, 64),
        cycles = read_non_negative_integer(function() return values.cycles end),
        elapsedDaysInSeason = read_non_negative_integer(function() return values.elapseddaysinseason end),
        remainingDaysInSeason = read_non_negative_integer(function() return values.remainingdaysinseason end),
        seasonProgress = season_progress,
        dayProgress = read_number(function() return values.time end),
        phaseProgress = read_number(function() return values.timeinphase end),
        precipitation = precipitation(values),
        moonPhase = read_text(function() return values.moonphase end, 64),
        temperature = read_number(function() return values.temperature end),
        wetness = read_number(function() return values.wetness end),
        moisture = read_number(function() return values.moisture end),
        moistureCeil = read_number(function() return values.moistureceil end),
        precipitationRate = read_number(function() return values.precipitationrate end),
        nightmarePhase = nightmare_phase,
        nightmareProgress = nightmare_progress,
        hostPerformance = host_performance(),
    }
end

local function complete(callback, written)
    if type(callback) == "function" then
        local ok, callback_error = xpcall(function() callback(written) end, debug.traceback)
        if not ok then
            print("[DST-ADMIN-RUNTIME ERROR] code=WORLDSTATE_CALLBACK_FAILED message=" .. safe_text(callback_error, 1024))
        end
    end
end

function M.EmitOnce(callback)
    if not state.running or not state.ready or state.writing then
        complete(callback, false)
        return false
    end
    state.writing = true
    local started = GetTime ~= nil and GetTime() or 0
    local captured_at = os.time()
    local ok, payload = xpcall(function()
        local session_id, shard_id = runtime_identity()
        state.sequence = state.sequence + 1
        local result = capture_world_state()
        result.schemaVersion = SCHEMA_VERSION
        result.producerVersion = PRODUCER_VERSION
        result.producerInstanceId = instance_id
        result.sessionId = session_id
        result.shardId = shard_id
        result.sequence = state.sequence
        result.capturedAtUnix = captured_at
        result.complete = true
        return result
    end, debug.traceback)
    if not ok then
        state.writing = false
        state.lastError = safe_text(payload, 1024)
        state.consecutiveFailures = state.consecutiveFailures + 1
        complete(callback, false)
        return false
    end
    local encoded_ok, encoded = pcall(json.encode_compliant, payload)
    if not encoded_ok or type(encoded) ~= "string" then
        state.writing = false
        state.lastError = "world state JSON encoding failed"
        state.consecutiveFailures = state.consecutiveFailures + 1
        complete(callback, false)
        return false
    end
    local slot = state.nextSlot
    TheSim:SetPersistentString(OUTPUT_ROOT .. "worldstate-" .. slot .. ".json", encoded, false, function(written)
        state.writing = false
        state.lastCapturedAtUnix = captured_at
        state.lastDurationMilliseconds = math.max(0, ((GetTime ~= nil and GetTime() or started) - started) * 1000)
        if written then
            state.lastWrittenAtUnix = os.time()
            state.lastError = nil
            state.consecutiveFailures = 0
            state.nextSlot = slot == "a" and "b" or "a"
        else
            state.lastError = "world state persistence failed"
            state.consecutiveFailures = state.consecutiveFailures + 1
        end
        complete(callback, written == true)
    end)
    return true
end

local function ready_for_capture()
    return TheWorld ~= nil
        and TheWorld.ismastersim == true
        and TheWorld.state ~= nil
        and TheNet ~= nil
        and TheShard ~= nil
        and TheSim ~= nil
        and TheSim.SetPersistentString ~= nil
end

local function await_world()
    if not state.running then return end
    if ready_for_capture() then
        state.ready = true
        state.readyTask = nil
        state.periodicTask = TheWorld:DoPeriodicTask(SNAPSHOT_INTERVAL, M.EmitOnce)
        return
    end
    state.readyAttempts = state.readyAttempts + 1
    if state.readyAttempts >= READY_RETRY_LIMIT then
        state.lastError = "world readiness timed out"
        state.consecutiveFailures = state.consecutiveFailures + 1
        state.readyTask = nil
        return
    end
    state.readyTask = scheduler:ExecuteInTime(READY_RETRY_SECONDS, await_world, "dst-admin-worldstate-ready")
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
    state.running = false
    state.ready = false
    if state.readyTask ~= nil then
        state.readyTask:Cancel()
        state.readyTask = nil
    end
    if state.periodicTask ~= nil then
        state.periodicTask:Cancel()
        state.periodicTask = nil
    end
    return true
end

function M.Status()
    return {
        running = state.running,
        ready = state.ready,
        busy = state.writing,
        sequence = state.sequence,
        lastCapturedAtUnix = state.lastCapturedAtUnix,
        lastWrittenAtUnix = state.lastWrittenAtUnix,
        lastDurationMilliseconds = state.lastDurationMilliseconds,
        lastError = state.lastError,
        consecutiveFailures = state.consecutiveFailures,
    }
end

return M
