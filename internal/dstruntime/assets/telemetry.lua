local json = require("json")

local M = {}
local SCHEMA_VERSION = 2
local PRODUCER_VERSION = "2.4.15"
local OUTPUT_ROOT = "mod_config_data/dst-admin/"

local state = {
    running = false,
    ready = false,
    writing = false,
    sequence = 0,
    nextSlot = "a",
    readyAttempts = 0,
    readyTask = nil,
    lastCapturedAtUnix = nil,
    lastWrittenAtUnix = nil,
    lastDurationMilliseconds = nil,
    lastError = nil,
    consecutiveFailures = 0,
    generation = 0,
    heartbeatSequence = 0,
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

local function gameplay_state(client, player)
    if player ~= nil then
        if player.migration ~= nil then
            return "migrating"
        end
        if player.HasTag ~= nil and player:HasTag("playerghost") then
            return "ghost"
        end
        if player.components ~= nil and player.components.health ~= nil then
            local ok, dead = pcall(player.components.health.IsDead, player.components.health)
            if ok and dead then
                return "dead"
            end
        end
        return "alive"
    end
    if client.prefab == nil or client.prefab == "" then
        return "selecting_character"
    end
    return "loading"
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
        gameplayState = gameplay_state(client, player),
        admin = client.admin == true,
        age = math.max(0, tonumber(client.playerage) or 0),
        netId = safe_text(client.netid, 128),
    }
    if record.name == "" then
        record.name = user_id
    end
    local score = tonumber(client.netscore)
    if score ~= nil and score >= 0 then
        record.netScore = score
    end
    if player ~= nil and player.components ~= nil then
        if player.components.health ~= nil then
            record.healthPercent = read_number(function() return player.components.health:GetPercent() * 100 end)
            record.health = read_number(function() return player.components.health.currenthealth end)
            record.healthMax = read_number(function()
                if type(player.components.health.GetMaxWithPenalty) == "function" then
                    return player.components.health:GetMaxWithPenalty()
                end
                return player.components.health.maxhealth
            end)
        end
        if player.components.hunger ~= nil then
            record.hungerPercent = read_number(function() return player.components.hunger:GetPercent() * 100 end)
            record.hunger = read_number(function() return player.components.hunger.current end)
            record.hungerMax = read_number(function() return player.components.hunger.max end)
        end
        if player.components.sanity ~= nil then
            record.sanityPercent = read_number(function() return player.components.sanity:GetPercent() * 100 end)
            record.sanity = read_number(function() return player.components.sanity.current end)
            record.sanityMax = read_number(function() return player.components.sanity.max end)
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
        updateId = runtime ~= nil and runtime.updateId or nil,
        producerVersion = PRODUCER_VERSION,
        producerInstanceId = instance_id,
        sessionId = session_id,
        shardId = shard_id,
        running = state.running,
        ready = state.ready,
        writing = state.writing,
        sequence = state.sequence,
        heartbeatSequence = state.heartbeatSequence,
        heartbeatAtUnix = os.time(),
        paused = clock.Paused(),
        lastCapturedAtUnix = state.lastCapturedAtUnix,
        lastWrittenAtUnix = state.lastWrittenAtUnix,
        lastDurationMilliseconds = state.lastDurationMilliseconds,
        lastError = state.lastError,
        consecutiveFailures = state.consecutiveFailures,
        modules = modules,
    }
end

local health_writing, health_pending, health_commands_busy = false, false, false
local health_callbacks = {}
local write_health
write_health = function(callback)
    -- Retired collectors may still receive persistence callbacks. They must
    -- never publish health for a replacement Runtime.
    local runtime = rawget(_G, "DSTAdmin")
    if runtime ~= nil and runtime.Telemetry ~= nil and runtime.Telemetry ~= M then
        health_pending = false
        if type(callback) == "function" then callback(false) end
        return
    end
    -- Serialize writes so an older busy snapshot cannot overwrite a newer idle
    -- notification. Coalesce overlap into one latest snapshot, with no poller.
    if type(callback) == "function" then health_callbacks[#health_callbacks + 1] = callback end
    if health_writing then health_pending = true; return end
    local callbacks = health_callbacks
    health_callbacks = {}
    local generation = state.generation
    local completed = false
    local function finish(written)
        if completed then return end
        completed = true
        health_writing = false
        for _, pending in ipairs(callbacks) do pcall(pending, written == true and state.generation == generation) end
        if health_pending then health_pending = false; write_health() end
    end
    state.heartbeatSequence = state.heartbeatSequence + 1
    local payload = health_payload()
    local ok, encoded = pcall(json.encode_compliant, payload)
    if not ok then finish(false); return end
    health_commands_busy = payload.modules.commands ~= nil and payload.modules.commands.busy == true
    health_writing = true
    local started = pcall(function()
        -- Released collectors can finish callbacks after handover. Keep their
        -- fixed health.json separate from this version's health publication.
        TheSim:SetPersistentString(OUTPUT_ROOT .. "health-" .. PRODUCER_VERSION .. ".json", encoded, false, finish)
    end)
    if not started then finish(false) end
end

function M.Heartbeat()
    if state.running then write_health() end
end

function M.CommandFinished()
    if state.running and state.ready and health_commands_busy then write_health() end
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
    local generation = state.generation
    local started = clock.Now()
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
    local encoded_ok, encoded = pcall(json.encode_compliant, payload)
    if not encoded_ok or type(encoded) ~= "string" then
        state.writing = false
        state.lastError = "snapshot JSON encoding failed"
        state.consecutiveFailures = state.consecutiveFailures + 1
        write_health()
        complete(callback, false)
        return false
    end
    local slot = state.nextSlot
    local persisted = pcall(function() TheSim:SetPersistentString(OUTPUT_ROOT .. "players-" .. slot .. ".json", encoded, false, function(written)
        if generation ~= state.generation then complete(callback, false); return end
        state.writing = false
        state.lastCapturedAtUnix = captured_at
        state.lastDurationMilliseconds = math.max(0, (clock.Now() - started) * 1000)
        if written then
            state.lastWrittenAtUnix = os.time()
            state.lastError = nil
            state.consecutiveFailures = 0
            state.nextSlot = slot == "a" and "b" or "a"
        else
            state.lastError = "snapshot persistence failed"
            state.consecutiveFailures = state.consecutiveFailures + 1
        end
        write_health(function(healthy) complete(callback, written == true and healthy) end)
    end) end)
    if not persisted then
        state.writing = false
        state.lastError = "snapshot persistence could not start"
        state.consecutiveFailures = state.consecutiveFailures + 1
        write_health()
        complete(callback, false)
        return false
    end
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
        -- Periodic capture is coordinated by bootstrap, including paused worlds.
        return
    end
    state.readyAttempts = state.readyAttempts + 1
    state.readyTask = clock.After(clock.ReadyDelay(state.readyAttempts), await_world, "dst-admin-telemetry-ready")
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
    state.generation = state.generation + 1
    state.writing = false
    state.ready = false
    state.readyAttempts = 0
    state.lastError = nil
    await_world()
    return true
end

function M.Stop(handover)
    state.running = false
    state.generation = state.generation + 1
    state.ready = false
    if state.readyTask ~= nil then
        state.readyTask:Cancel()
        state.readyTask = nil
    end
    if handover == true then
        health_pending = false
    else
        write_health()
    end
    return true
end

function M.Status()
    local status = health_payload()
    -- Keep the persisted snapshot's writing flag unchanged: that snapshot is
    -- itself written asynchronously. Live replacement must also drain health.
    status.writing = state.writing or health_writing
    return status
end

M.version = PRODUCER_VERSION
return M
