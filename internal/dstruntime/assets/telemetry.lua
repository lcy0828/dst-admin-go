local json = require("json")

local M = {}
local SCHEMA_VERSION = 2
local PRODUCER_VERSION = "2.4.0"
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
    if ok and finite_number(value) then
        return value
    end
    return nil
end

local function safe_text(value, limit)
    if value == nil then
        return ""
    end
    value = tostring(value)
    if #value > limit then
        return string.sub(value, 1, limit)
    end
    return value
end

local function virtual_host(client)
    return client ~= nil
        and client.name == "[Host]"
        and (client.prefab == nil or client.prefab == "")
        and (client.netid == nil or client.netid == "")
end

local function capture_player(client)
    if client == nil or virtual_host(client) then
        return nil
    end
    local user_id = safe_text(client.userid, 128)
    if user_id == "" then
        return nil
    end
    local player = UserToPlayer ~= nil and UserToPlayer(user_id) or nil
    local record = {
        id = user_id,
        name = safe_text(client.name, 256),
        prefab = safe_text(client.prefab, 128),
        admin = client.admin == true,
        age = math.max(0, tonumber(client.playerage) or 0),
        netId = safe_text(client.netid, 128),
    }
    if record.name == "" then
        record.name = user_id
    end
    local score = tonumber(client.performance)
    if score ~= nil and score >= 0 then
        record.netScore = score
    end
    if player ~= nil and player.components ~= nil then
        if player.components.health ~= nil then
            record.healthPercent = read_number(function() return player.components.health:GetPercent() * 100 end)
        end
        if player.components.hunger ~= nil then
            record.hungerPercent = read_number(function() return player.components.hunger:GetPercent() * 100 end)
        end
        if player.components.sanity ~= nil then
            record.sanityPercent = read_number(function() return player.components.sanity:GetPercent() * 100 end)
        end
        if player.components.temperature ~= nil then
            record.temperature = read_number(function() return player.components.temperature.current end)
        end
        if player.components.moisture ~= nil then
            record.moisture = read_number(function() return player.components.moisture:GetMoisture() end)
        end
    end
    return record
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

local function health_payload()
    local session_id, shard_id = runtime_identity()
    local modules = {}
    local runtime = rawget(_G, "DSTAdmin")
    if runtime ~= nil then
        for _, name in ipairs({ "WorldState", "Commands", "Events", "Diagnostics", "Barriers" }) do
            local module = runtime[name]
            if module ~= nil and type(module.Status) == "function" then
                local ok, status = pcall(module.Status)
                if ok and type(status) == "table" then modules[string.lower(name)] = status end
            end
        end
    end
    return {
        schemaVersion = 1,
        producerVersion = PRODUCER_VERSION,
        producerInstanceId = instance_id,
        sessionId = session_id,
        shardId = shard_id,
        running = state.running,
        ready = state.ready,
        writing = state.writing,
        sequence = state.sequence,
        lastCapturedAtUnix = state.lastCapturedAtUnix,
        lastWrittenAtUnix = state.lastWrittenAtUnix,
        lastDurationMilliseconds = state.lastDurationMilliseconds,
        lastError = state.lastError,
        consecutiveFailures = state.consecutiveFailures,
        modules = modules,
    }
end

local function write_health()
    local ok, encoded = pcall(json.encode, health_payload())
    if ok then
        TheSim:SetPersistentString(OUTPUT_ROOT .. "health.json", encoded, false)
    end
end

local function complete(callback, written)
    if type(callback) == "function" then
        local ok, callback_error = xpcall(function() callback(written) end, debug.traceback)
        if not ok then
            print("[DST-ADMIN-RUNTIME ERROR] code=TELEMETRY_CALLBACK_FAILED message=" .. safe_text(callback_error, 1024))
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
        local players = {}
        local clients = TheNet:GetClientTable() or {}
        for _, client in ipairs(clients) do
            local captured = capture_player(client)
            if captured ~= nil then
                players[#players + 1] = captured
                if #players > 64 then
                    error("player snapshot exceeds 64 entries")
                end
            end
        end
        local session_id, shard_id = runtime_identity()
        state.sequence = state.sequence + 1
        return {
            schemaVersion = SCHEMA_VERSION,
            producerVersion = PRODUCER_VERSION,
            producerInstanceId = instance_id,
            sessionId = session_id,
            shardId = shard_id,
            sequence = state.sequence,
            capturedAtUnix = captured_at,
            complete = true,
            players = players,
        }
    end, debug.traceback)
    if not ok then
        state.writing = false
        state.lastError = safe_text(payload, 1024)
        state.consecutiveFailures = state.consecutiveFailures + 1
        write_health()
        complete(callback, false)
        return false
    end
    local encoded_ok, encoded = pcall(json.encode, payload)
    if not encoded_ok or type(encoded) ~= "string" then
        state.writing = false
        state.lastError = "snapshot JSON encoding failed"
        state.consecutiveFailures = state.consecutiveFailures + 1
        write_health()
        complete(callback, false)
        return false
    end
    local slot = state.nextSlot
    TheSim:SetPersistentString(OUTPUT_ROOT .. "players-" .. slot .. ".json", encoded, false, function(written)
        state.writing = false
        state.lastCapturedAtUnix = captured_at
        state.lastDurationMilliseconds = math.max(0, ((GetTime ~= nil and GetTime() or started) - started) * 1000)
        if written then
            state.lastWrittenAtUnix = os.time()
            state.lastError = nil
            state.consecutiveFailures = 0
            state.nextSlot = slot == "a" and "b" or "a"
        else
            state.lastError = "snapshot persistence failed"
            state.consecutiveFailures = state.consecutiveFailures + 1
        end
        write_health()
        complete(callback, written == true)
    end)
    return true
end

local function ready_for_capture()
    return TheWorld ~= nil
        and TheWorld.ismastersim == true
        and TheNet ~= nil
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
        write_health()
        return
    end
    state.readyTask = scheduler:ExecuteInTime(READY_RETRY_SECONDS, await_world, "dst-admin-telemetry-ready")
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
    write_health()
    return true
end

function M.Status()
    return health_payload()
end

return M
