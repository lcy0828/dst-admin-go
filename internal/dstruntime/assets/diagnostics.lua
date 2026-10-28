local json = require("json")

local M = {}
local SCHEMA_VERSION = 1
local PRODUCER_VERSION = "2.4.19"
local OUTPUT_ROOT = "mod_config_data/dst-admin/"
local MAX_SAMPLES = 50

local state = {
    generation = 0,
    running = false,
    ready = false,
    busy = false,
    sequence = 0,
    nextSlot = "a",
    activeTask = nil,
    timeoutTask = nil,
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

local function response(ok, code, message, result)
    -- DST's encoder serializes an empty Lua table as [], not an object. Omit
    -- absent results so errors remain compatible with the typed Go reader.
    return { ok = ok == true, code = safe_text(code, 64), message = safe_text(message, 512), result = result }
end

local function persist(request, report)
    state.sequence = state.sequence + 1
    local session_id, shard_id = runtime_identity()
    local payload = {
        schemaVersion = SCHEMA_VERSION,
        producerVersion = PRODUCER_VERSION,
        producerInstanceId = instance_id,
        sessionId = session_id,
        shardId = shard_id,
        sequence = state.sequence,
        requestId = request.requestId,
        profile = request.profile,
        ok = report.ok == true,
        code = report.code,
        message = report.message,
        result = report.result,
        completedAtUnix = os.time(),
    }
    local encoded_ok, encoded = pcall(json.encode_compliant, payload)
    if not encoded_ok or type(encoded) ~= "string" then
        state.lastError = "diagnostic JSON encoding failed"
        state.busy = false
        return false
    end
    local slot = state.nextSlot
    local generation = state.generation
    local started = pcall(function() TheSim:SetPersistentString(OUTPUT_ROOT .. "diagnostic-" .. slot .. ".json", encoded, false, function(written)
        if generation ~= state.generation then return end
        state.busy = false
        if written then
            state.nextSlot = slot == "a" and "b" or "a"
            state.lastError = nil
        else
            state.lastError = "diagnostic persistence failed"
        end
    end) end)
    if not started then state.busy = false; state.lastError = "diagnostic persistence could not start"; return false end
    print(string.format("[DST-ADMIN-DIAGNOSTIC RECEIPT] request=%s profile=%s code=%s", request.requestId, request.profile, report.code))
    return true
end

local function count_entities(prefab, sample_limit)
    local count = 0
    local samples = {}
    for _, entity in pairs(Ents or {}) do
        if entity ~= nil and (prefab == nil or entity.prefab == prefab) then
            count = count + 1
            if #samples < sample_limit then
                local sample = { prefab = safe_text(entity.prefab, 128), guid = tonumber(entity.GUID) or 0 }
                if entity.Transform ~= nil and entity.Transform.GetWorldPosition ~= nil then
                    local ok, x, _, z = pcall(entity.Transform.GetWorldPosition, entity.Transform)
                    if ok then sample.x = x; sample.z = z end
                end
                samples[#samples + 1] = sample
            end
        end
    end
    return count, samples
end

local function summary_report()
    local entity_count = count_entities(nil, 0)
    local world = TheWorld ~= nil and TheWorld.state or {}
    return response(true, "DIAGNOSTIC_COMPLETE", "", {
        playerCount = #(AllPlayers or {}),
        entityCount = entity_count,
        cycles = tonumber(world.cycles) or 0,
        phase = safe_text(world.phase, 32),
        season = safe_text(world.season, 32),
        raining = world.israining == true,
    })
end

local function prefab_report(request)
    local prefab = type(request.prefab) == "string" and request.prefab or ""
    if prefab == "" or #prefab > 80 or string.match(prefab, "^[a-z0-9_]+$") == nil then
        return response(false, "INVALID_PREFAB", "prefab is invalid")
    end
    local sample_limit = math.floor(tonumber(request.sampleLimit) or 10)
    if sample_limit < 0 then sample_limit = 0 end
    if sample_limit > MAX_SAMPLES then sample_limit = MAX_SAMPLES end
    local count, samples = count_entities(prefab, sample_limit)
    return response(true, "DIAGNOSTIC_COMPLETE", "", { prefab = prefab, count = count, samples = samples })
end

local function performance_report(request)
    local duration = tonumber(request.durationSeconds) or 3
    if duration < 1 or duration > 5 then
        persist(request, response(false, "INVALID_DURATION", "durationSeconds must be between 1 and 5"))
        return
    end
    if request.simulationPaused == true or clock.Paused() == true then
        persist(request, response(false, "SIMULATION_PAUSED", "simulation is paused; performance sampling requires simulation ticks"))
        return
    end
    local generation = state.generation
    local intervals = {}
    local started = GetTime ~= nil and GetTime() or 0
    local previous = started
    local finished = false
    local function finish(ok, code, message)
        if finished or not state.running or generation ~= state.generation then return end
        finished = true
        if state.activeTask ~= nil then state.activeTask:Cancel(); state.activeTask = nil end
        if state.timeoutTask ~= nil then state.timeoutTask:Cancel(); state.timeoutTask = nil end
        local total, maximum = 0, 0
        for _, value in ipairs(intervals) do total = total + value; if value > maximum then maximum = value end end
        persist(request, response(ok, code, message, {
            durationMilliseconds = math.max(0, (previous - started) * 1000),
            sampleCount = #intervals,
            averageIntervalMilliseconds = #intervals > 0 and total / #intervals or nil,
            maximumIntervalMilliseconds = #intervals > 0 and maximum or nil,
        }))
    end
    -- This deadline is independent of the simulation being measured.
    state.timeoutTask = clock.After(duration + 1, function()
        finish(false, clock.Paused() == true and "SIMULATION_PAUSED" or "DIAGNOSTIC_TIMEOUT",
            "simulation sampling did not complete within its real-time deadline")
    end, "dst-admin-diagnostic-timeout")
    state.activeTask = TheWorld:DoPeriodicTask(0.1, function()
        if finished or not state.running or generation ~= state.generation then return end
        local current = GetTime ~= nil and GetTime() or previous
        intervals[#intervals + 1] = math.max(0, (current - previous) * 1000)
        previous = current
        if current - started >= duration or #intervals >= MAX_SAMPLES then
            finish(true, "DIAGNOSTIC_COMPLETE", "")
        end
    end)
end

function M.Capture(request)
    clock.Poll()
    if not state.running or not state.ready then return response(false, "DIAGNOSTIC_UNAVAILABLE", "diagnostics are not ready") end
    if state.busy then return response(false, "DIAGNOSTIC_BUSY", "another diagnostic is running") end
    if type(request) ~= "table" or type(request.requestId) ~= "string"
        or #request.requestId < 16 or #request.requestId > 80
        or string.match(request.requestId, "^[A-Za-z0-9_-]+$") == nil
        or type(request.profile) ~= "string" then
        return response(false, "INVALID_REQUEST", "requestId and profile are required")
    end
    state.busy = true
    if request.profile == "summary" then
        persist(request, summary_report())
    elseif request.profile == "prefab" then
        persist(request, prefab_report(request))
    elseif request.profile == "performance" then
        performance_report(request)
    else
        persist(request, response(false, "PROFILE_NOT_ALLOWED", "diagnostic profile is not allowed"))
    end
    return response(true, "DIAGNOSTIC_ACCEPTED", "")
end

function M.CaptureJSON(source)
    if type(source) ~= "string" or #source > 4096 then return response(false, "INVALID_REQUEST", "request JSON is invalid") end
    local decoded, request = pcall(json.decode, source)
    if not decoded then return response(false, "INVALID_REQUEST", "request JSON could not be decoded") end
    return M.Capture(request)
end

local await_world

function M.Poll()
    clock.Poll()
    if state.running and not state.ready and await_world ~= nil then
        if state.readyTask ~= nil then state.readyTask:Cancel(); state.readyTask = nil end
        await_world()
    end
end

function M.Start()
    if state.running then return true end
    state.running = true
    state.generation = state.generation + 1
    state.ready = false
    state.readyAttempts = 0
    state.lastError = nil
    await_world = function()
        if not state.running then return end
        if TheWorld ~= nil and TheWorld.ismastersim == true and TheSim ~= nil and TheSim.SetPersistentString ~= nil then
            state.ready = true
            state.readyTask = nil
            return
        end
        state.readyAttempts = state.readyAttempts + 1
        state.readyTask = clock.After(clock.ReadyDelay(state.readyAttempts), await_world, "dst-admin-diagnostics-ready")
    end
    await_world()
    return true
end

function M.Stop()
    state.running = false
    state.generation = state.generation + 1
    state.ready = false
    if state.readyTask ~= nil then state.readyTask:Cancel(); state.readyTask = nil end
    if state.activeTask ~= nil then state.activeTask:Cancel(); state.activeTask = nil end
    if state.timeoutTask ~= nil then state.timeoutTask:Cancel(); state.timeoutTask = nil end
    state.busy = false
    return true
end

function M.Status()
    return { running = state.running, ready = state.ready, busy = state.busy, sequence = state.sequence, lastError = state.lastError }
end

M.version = PRODUCER_VERSION
return M
