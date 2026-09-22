local json = require("json")

local M = {}
local SCHEMA_VERSION = 1
local PRODUCER_VERSION = "2.4.10"
local OUTPUT_PATH = "mod_config_data/dst-admin/snapshot-barrier.json"
local ID_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._-]+$"
local BARRIER_TIMEOUT = 180
local READY_RETRY_SECONDS = 1
local READY_RETRY_LIMIT = 60

local state = {
    running = false,
    ready = false,
    writing = false,
    pending = nil,
    receipt = nil,
    holding = false,
    delayedSave = false,
    delayedShutdown = false,
    delayedCallbacks = {},
    originalSaveCurrent = nil,
    wrappedSaveCurrent = nil,
    lastError = nil,
    timeoutTask = nil,
    readyAttempts = 0,
    readyTask = nil,
}

local instance_id = string.format("barrier-%d-%06d", os.time(), math.random(0, 999999))

local function safe_text(value, limit)
    if value == nil then return "" end
    value = tostring(value)
    if #value > limit then return string.sub(value, 1, limit) end
    return value
end

local function identity()
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

local function current_snapshot()
    if TheNet == nil or TheNet.GetCurrentSnapshot == nil then return -1 end
    local ok, value = pcall(TheNet.GetCurrentSnapshot, TheNet)
    if not ok then return -1 end
    return tonumber(value) or -1
end

local function persist_receipt(receipt)
    state.receipt = receipt
    local ok, encoded = pcall(json.encode_compliant, receipt)
    if not ok or type(encoded) ~= "string" then
        state.lastError = "snapshot barrier JSON encoding failed"
        return false
    end
    state.writing = true
    TheSim:SetPersistentString(OUTPUT_PATH, encoded, false, function(written)
        state.writing = false
        if written then
            state.lastError = nil
        else
            state.lastError = "snapshot barrier receipt persistence failed"
        end
    end)
    return true
end

local function new_receipt(barrier_id, barrier_state, message)
    local session_id, shard_id = identity()
    return {
        schemaVersion = SCHEMA_VERSION,
        producerVersion = PRODUCER_VERSION,
        producerInstanceId = instance_id,
        barrierId = barrier_id,
        state = barrier_state,
        sessionId = session_id,
        shardId = shard_id,
        snapshotBefore = current_snapshot(),
        preparedAtUnix = os.time(),
        message = safe_text(message, 256),
    }
end

local function valid_barrier_id(value)
    return type(value) == "string" and #value >= 8 and #value <= 128 and string.match(value, ID_PATTERN) ~= nil
end

local function finish_delayed_save()
    if not state.delayedSave or state.originalSaveCurrent == nil or ShardGameIndex == nil then return end
    local callbacks = state.delayedCallbacks
    local isshutdown = state.delayedShutdown
    state.delayedSave = false
    state.delayedShutdown = false
    state.delayedCallbacks = {}
    state.originalSaveCurrent(ShardGameIndex, function(...)
        for _, callback in ipairs(callbacks) do
            if type(callback) == "function" then pcall(callback, ...) end
        end
    end, isshutdown)
end

local function cancel_timeout()
    if state.timeoutTask ~= nil then
        state.timeoutTask:Cancel()
        state.timeoutTask = nil
    end
end

local function schedule_timeout(barrier_id)
    cancel_timeout()
    state.timeoutTask = TheWorld:DoTaskInTime(BARRIER_TIMEOUT, function()
        state.timeoutTask = nil
        if state.holding and state.receipt ~= nil and state.receipt.barrierId == barrier_id then
            M.Release(barrier_id)
        elseif state.pending ~= nil and state.pending.barrierId == barrier_id then
            M.Cancel(barrier_id)
        end
    end)
end

local function wrap_save_current()
    if ShardGameIndex == nil or type(ShardGameIndex.SaveCurrent) ~= "function" then
        state.lastError = "ShardGameIndex.SaveCurrent is unavailable"
        return false
    end
    state.originalSaveCurrent = ShardGameIndex.SaveCurrent
    state.wrappedSaveCurrent = function(index, onsavedcb, isshutdown)
        if state.holding then
            state.delayedSave = true
            state.delayedShutdown = state.delayedShutdown or isshutdown == true
            if type(onsavedcb) == "function" then
                state.delayedCallbacks[#state.delayedCallbacks + 1] = onsavedcb
            end
            return
        end
        local active = state.pending
        if active == nil then
            return state.originalSaveCurrent(index, onsavedcb, isshutdown)
        end
        local before = current_snapshot()
        return state.originalSaveCurrent(index, function(...)
            local after = current_snapshot()
            local receipt = active
            if state.pending == active then
                state.pending = nil
                receipt.snapshotBefore = before
                receipt.snapshotAfter = after
                receipt.completedAtUnix = os.time()
                if after > before and after >= 0 then
                    receipt.state = "completed"
                    receipt.proof = "save_current_callback"
                    receipt.message = ""
                    state.holding = true
                else
                    receipt.state = "failed"
                    receipt.message = "snapshot did not advance"
                end
                persist_receipt(receipt)
            end
            if type(onsavedcb) == "function" then onsavedcb(...) end
        end, isshutdown)
    end
    ShardGameIndex.SaveCurrent = state.wrappedSaveCurrent
    return true
end

local function ready_to_wrap()
    return TheWorld ~= nil
        and TheWorld.ismastersim == true
        and TheSim ~= nil
        and TheSim.SetPersistentString ~= nil
        and ShardGameIndex ~= nil
        and type(ShardGameIndex.SaveCurrent) == "function"
end

local function await_world()
    if not state.running then return end
    if ready_to_wrap() then
        state.readyTask = nil
        if not wrap_save_current() then return end
        state.ready = true
        state.lastError = nil
        persist_receipt(new_receipt("runtime-start", "idle", ""))
        return
    end
    state.readyAttempts = state.readyAttempts + 1
    if state.readyAttempts >= READY_RETRY_LIMIT then
        state.readyTask = nil
        state.lastError = "world readiness timed out"
        return
    end
    state.readyTask = scheduler:ExecuteInTime(READY_RETRY_SECONDS, await_world, "dst-admin-barriers-ready")
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
    if state.holding then M.Release(state.receipt ~= nil and state.receipt.barrierId or "") end
    if ShardGameIndex ~= nil and ShardGameIndex.SaveCurrent == state.wrappedSaveCurrent then
        ShardGameIndex.SaveCurrent = state.originalSaveCurrent
    end
    state.running = false
    state.ready = false
    state.pending = nil
    if state.readyTask ~= nil then
        state.readyTask:Cancel()
        state.readyTask = nil
    end
    cancel_timeout()
    state.originalSaveCurrent = nil
    state.wrappedSaveCurrent = nil
    return true
end

function M.Prepare(barrier_id)
    if not state.running or not state.ready or not valid_barrier_id(barrier_id) then return false end
    if state.holding then
        return state.receipt ~= nil and state.receipt.barrierId == barrier_id and state.receipt.state == "completed"
    end
    if state.pending ~= nil then return state.pending.barrierId == barrier_id end
    local receipt = new_receipt(barrier_id, "prepared", "")
    if receipt.sessionId == "" or receipt.shardId == "" or receipt.snapshotBefore < 0 then
        receipt.state = "failed"
        receipt.message = "runtime identity or snapshot is unavailable"
        persist_receipt(receipt)
        return false
    end
    state.pending = receipt
    schedule_timeout(barrier_id)
    persist_receipt(receipt)
    return true
end

function M.Commit(barrier_id)
    if not state.running or state.pending == nil or state.pending.barrierId ~= barrier_id then return false end
    if TheWorld == nil or TheWorld.ismastershard ~= true then return false end
    TheWorld:PushEvent("ms_save")
    return true
end

function M.Release(barrier_id)
    if not valid_barrier_id(barrier_id) or state.receipt == nil or state.receipt.barrierId ~= barrier_id then return false end
    state.holding = false
    state.pending = nil
    cancel_timeout()
    state.receipt.state = state.receipt.state == "completed" and "released" or state.receipt.state
    state.receipt.releasedAtUnix = os.time()
    persist_receipt(state.receipt)
    finish_delayed_save()
    return true
end

function M.Cancel(barrier_id)
    if not valid_barrier_id(barrier_id) then return false end
    if state.pending ~= nil and state.pending.barrierId == barrier_id then
        state.pending.state = "cancelled"
        state.pending.message = "barrier cancelled before completion"
        persist_receipt(state.pending)
        state.pending = nil
        cancel_timeout()
        return true
    end
    if state.holding and state.receipt ~= nil and state.receipt.barrierId == barrier_id then
        return M.Release(barrier_id)
    end
    return state.receipt ~= nil and state.receipt.barrierId == barrier_id
end

function M.Status()
    return {
        running = state.running,
        ready = state.ready,
        busy = state.pending ~= nil or state.holding,
        writing = state.writing,
        holding = state.holding,
        barrierId = state.pending ~= nil and state.pending.barrierId or (state.receipt ~= nil and state.receipt.barrierId or ""),
        lastError = state.lastError,
    }
end

return M
