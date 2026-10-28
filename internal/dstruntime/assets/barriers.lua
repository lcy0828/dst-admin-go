local json = require("json")

local M = {}
local SCHEMA_VERSION = 2
local PRODUCER_VERSION = "2.4.19"
local OUTPUT_PATH = "mod_config_data/dst-admin/snapshot-barrier.json"
local ID_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._-]+$"
local BARRIER_TIMEOUT = 180

local state = {
    running = false,
    ready = false,
    writing = false,
    queuedReceipt = nil,
    pending = nil,
    receipt = nil,
    holding = false,
    capturing = false,
    inFlight = 0,
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

local flush_receipt
flush_receipt = function()
    if state.writing or state.queuedReceipt == nil then return end
    local encoded = state.queuedReceipt
    state.queuedReceipt = nil
    state.writing = true
    TheSim:SetPersistentString(OUTPUT_PATH, encoded, false, function(written)
        state.writing = false
        if written then state.lastError = nil else state.lastError = "snapshot barrier receipt persistence failed" end
        -- Serialize writes to the same path. Older prepared/completed JSON must
        -- never finish after the latest cancellation or release acknowledgement.
        flush_receipt()
    end)
end

local function persist_receipt(receipt)
    receipt.holding = state.holding
    receipt.inFlight = state.inFlight > 0
    receipt.busy = state.pending ~= nil or state.holding or state.capturing or state.inFlight > 0
    receipt.currentSnapshot = current_snapshot()
    state.receipt = receipt
    local ok, encoded = pcall(json.encode_compliant, receipt)
    if not ok or type(encoded) ~= "string" then
        state.lastError = "snapshot barrier JSON encoding failed"
        return false
    end
    state.queuedReceipt = encoded
    flush_receipt()
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
    if not state.delayedSave or state.originalSaveCurrent == nil or ShardGameIndex == nil or state.inFlight > 0 or state.holding or state.capturing then return end
    local callbacks = state.delayedCallbacks
    local isshutdown = state.delayedShutdown
    state.delayedSave = false
    state.delayedShutdown = false
    state.delayedCallbacks = {}
    local save = state.wrappedSaveCurrent or state.originalSaveCurrent
    save(ShardGameIndex, function(...)
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
    state.timeoutTask = clock.After(BARRIER_TIMEOUT, function()
        state.timeoutTask = nil
        if state.holding and state.receipt ~= nil and state.receipt.barrierId == barrier_id then
            state.receipt.state = "failed"
            state.receipt.message = "snapshot barrier expired"
            M.Release(barrier_id)
        elseif state.pending ~= nil and state.pending.barrierId == barrier_id then
            state.pending.state, state.pending.code, state.pending.message = "failed", "BARRIER_EXPIRED", "snapshot barrier expired"
            M.Release(barrier_id)
        end
    end, "dst-admin-barrier-timeout")
end

local function wrap_save_current()
    if ShardGameIndex == nil or type(ShardGameIndex.SaveCurrent) ~= "function" then
        state.lastError = "ShardGameIndex.SaveCurrent is unavailable"
        return false
    end
    state.originalSaveCurrent = ShardGameIndex.SaveCurrent
    state.wrappedSaveCurrent = function(index, onsavedcb, isshutdown)
        -- A fully sleeping server may not tick either scheduler. Expire the
        -- protection before the first resumed save can inspect the hold.
        clock.Poll()
        if state.holding or state.capturing then
            state.delayedSave = true
            state.delayedShutdown = state.delayedShutdown or isshutdown == true
            if type(onsavedcb) == "function" then
                state.delayedCallbacks[#state.delayedCallbacks + 1] = onsavedcb
            end
            return
        end
        local active = state.pending
        local before = current_snapshot()
        -- Capture the eligibility at entry. A callback from a save that began
        -- before Arm must never inherit a later arm of the same receipt table.
        local target = active ~= nil and active.state == "armed" and active.targetSnapshotAfter or nil
        local capture = target ~= nil and before == target - 1
        if capture and state.inFlight > 0 then
            capture = false
            active.state, active.code, active.message = "conflict", "SAVE_IN_PROGRESS", "an earlier save is still writing at the target round"
            persist_receipt(active)
        end
        if target ~= nil and before >= target then
            active.state = "conflict"
            active.code = "TARGET_SNAPSHOT_PASSED"
            active.message = "target snapshot already passed"
            persist_receipt(active)
        end
        if capture then state.capturing = true end
        state.inFlight = state.inFlight + 1
        if state.receipt ~= nil and (state.receipt.busy or active ~= nil) then persist_receipt(state.receipt) end
        local callback_ran = false
        local ok, result = pcall(state.originalSaveCurrent, index, function(...)
            if callback_ran then return end
            callback_ran = true
            clock.Poll()
            state.inFlight = state.inFlight - 1
            if capture then state.capturing = false end
            local publish = capture or active ~= nil or (state.receipt ~= nil and state.receipt.inFlight)
            if capture and state.pending == active and active.state == "armed" then
                local after = current_snapshot()
                active.snapshotBefore = before
                active.snapshotAfter = after
                active.completedAtUnix = os.time()
                if after == target and after > before then
                    active.state = "completed"
                    active.proof = "save_current_callback"
                    active.code, active.message = "", ""
                    state.pending = nil
                    state.holding = true
                else
                    active.state = after > target and "conflict" or "failed"
                    active.code = after > target and "TARGET_SNAPSHOT_PASSED" or "SAVE_WRITE_FAILED"
                    active.message = after > target and "save advanced beyond the target snapshot" or "snapshot did not advance"
                end
            end
            if not state.holding and not state.capturing then finish_delayed_save() end
            -- Publish after starting any delayed write. An intermediate idle
            -- receipt would let the controller retry before that write drained.
            if publish and state.receipt ~= nil then persist_receipt(state.receipt) end
            if type(onsavedcb) == "function" then onsavedcb(...) end
        end, isshutdown)
        if not ok then
            if not callback_ran then state.inFlight = state.inFlight - 1 end
            if capture then state.capturing = false end
            if active ~= nil and state.pending == active then
                active.state, active.code, active.message = "failed", "SAVE_WRITE_FAILED", safe_text(result, 256)
                persist_receipt(active)
            end
            error(result)
        end
        return result
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
    state.readyTask = clock.After(clock.ReadyDelay(state.readyAttempts), await_world, "dst-admin-barriers-ready")
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
    if state.inFlight > 0 then return false end
    if state.pending ~= nil or state.holding then M.Cancel(state.receipt ~= nil and state.receipt.barrierId or "") end
    if state.inFlight > 0 then return false end
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
    clock.Poll()
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

-- Target zero lets the master choose its next snapshot atomically. Secondary
-- shards can lag behind: earlier saves run normally until the selected round.
function M.Arm(barrier_id, target)
    clock.Poll()
    local active = state.pending
    if active == nil then
        return state.holding and state.receipt ~= nil and state.receipt.barrierId == barrier_id
            and state.receipt.state == "completed" and (target == 0 or state.receipt.targetSnapshotAfter == target)
    end
    if active.barrierId ~= barrier_id then return false end
    if active.state == "armed" then return target == 0 or active.targetSnapshotAfter == target end
    if active.state ~= "prepared" then return false end
    if state.inFlight > 0 then
        active.state, active.code, active.message = "conflict", "SAVE_IN_PROGRESS", "a previous save is still writing"
        persist_receipt(active)
        return false
    end
    local current = current_snapshot()
    if target == 0 and TheWorld ~= nil and TheWorld.ismastershard == true then target = current + 1 end
    if type(target) ~= "number" or target ~= math.floor(target) or target < 1 or target > 9007199254740991 or current < 0 then
        active.state, active.code, active.message = "failed", "INVALID_TARGET_SNAPSHOT", "target snapshot is invalid"
        persist_receipt(active)
        return false
    end
    if target <= current then
        active.state, active.code, active.message = "conflict", "TARGET_SNAPSHOT_PASSED", "target snapshot already passed"
        persist_receipt(active)
        return false
    end
    active.state, active.targetSnapshotAfter, active.snapshotBefore = "armed", target, current
    active.armedAtUnix = os.time()
    active.code, active.message = "", ""
    persist_receipt(active)
    return true
end

function M.Commit(barrier_id)
    clock.Poll()
    if not state.running or TheWorld == nil or TheWorld.ismastershard ~= true then return false end
    if state.holding and state.receipt ~= nil and state.receipt.barrierId == barrier_id and state.receipt.state == "completed" then return true end
    local active = state.pending
    if active == nil or active.barrierId ~= barrier_id or active.state ~= "armed" then return false end
    if active.commitRequested or state.capturing then return true end
    active.commitRequested = true
    persist_receipt(active)
    TheWorld:PushEvent("ms_save")
    return true
end

function M.Release(barrier_id)
    clock.Poll()
    if not valid_barrier_id(barrier_id) or state.receipt == nil or state.receipt.barrierId ~= barrier_id then return false end
    state.holding = false
    state.pending = nil
    cancel_timeout()
    state.receipt.state = state.receipt.state == "completed" and "released" or state.receipt.state
    state.receipt.releasedAtUnix = os.time()
    finish_delayed_save()
    persist_receipt(state.receipt)
    return true
end

function M.Cancel(barrier_id)
    clock.Poll()
    if not valid_barrier_id(barrier_id) then return false end
    if state.pending ~= nil and state.pending.barrierId == barrier_id then
        local active = state.pending
        state.pending = nil
        active.state = "cancelled"
        active.message = "barrier cancelled before completion"
        active.releasedAtUnix = os.time()
        cancel_timeout()
        finish_delayed_save()
        persist_receipt(active)
        return true
    end
    if state.holding and state.receipt ~= nil and state.receipt.barrierId == barrier_id then return M.Release(barrier_id) end
    if state.receipt ~= nil and state.receipt.barrierId == barrier_id then return true end
    -- A lost Prepare response may mean this node never prepared the attempt.
    -- Acknowledge cancellation only when no other attempt or write is active.
    if state.pending == nil and not state.holding and not state.capturing and state.inFlight == 0 then
        local receipt = new_receipt(barrier_id, "cancelled", "")
        receipt.releasedAtUnix = os.time()
        persist_receipt(receipt)
        return true
    end
    return false
end

function M.Status()
    return {
        running = state.running,
        ready = state.ready,
        busy = state.pending ~= nil or state.holding or state.capturing or state.inFlight > 0,
        inFlight = state.inFlight > 0,
        writing = state.writing,
        holding = state.holding,
        barrierId = state.pending ~= nil and state.pending.barrierId or (state.receipt ~= nil and state.receipt.barrierId or ""),
        lastError = state.lastError,
    }
end

M.version = PRODUCER_VERSION
return M
