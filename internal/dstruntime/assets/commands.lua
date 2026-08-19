local json = require("json")

local M = {}
local SCHEMA_VERSION = 1
local PRODUCER_VERSION = "2.4.0"
local OUTPUT_ROOT = "mod_config_data/dst-admin/"

local state = {
    sequence = 0,
    nextSlot = "a",
    writing = false,
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

local function result(ok, code, message, details)
    return {
        ok = ok == true,
        code = safe_text(code, 64),
        message = safe_text(message, 512),
        details = details,
    }
end

local function valid_user_id(value)
    return type(value) == "string" and #value >= 6 and #value <= 128
        and string.match(value, "^KU_[A-Za-z0-9_-]+$") ~= nil
end

local function player_argument(arguments)
    local user_id = type(arguments) == "table" and arguments.userId or nil
    if not valid_user_id(user_id) then
        return nil, result(false, "INVALID_USER_ID", "player userId is invalid")
    end
    local player = UserToPlayer ~= nil and UserToPlayer(user_id) or nil
    if player == nil then
        return nil, result(false, "PLAYER_NOT_FOUND", "player is not online in this shard")
    end
    return player, nil
end

local handlers = {}

handlers["telemetry.emit"] = function()
    local runtime = rawget(_G, "DSTAdmin")
    local emitted = runtime ~= nil and runtime.Telemetry ~= nil and runtime.Telemetry.EmitOnce()
    return result(emitted == true, emitted and "TELEMETRY_EMITTED" or "TELEMETRY_BUSY", "")
end

handlers["player.kick"] = function(arguments)
    local player, failure = player_argument(arguments)
    if player == nil then return failure end
    TheNet:Kick(arguments.userId)
    return result(true, "PLAYER_KICKED", "", { userId = arguments.userId })
end

handlers["player.announce"] = function(arguments)
    local player, failure = player_argument(arguments)
    if player == nil then return failure end
    local message = type(arguments.message) == "string" and arguments.message or ""
    if message == "" or #message > 1024 then
        return result(false, "INVALID_MESSAGE", "announcement message is invalid")
    end
    c_announce(message)
    return result(true, "PLAYER_ANNOUNCED", "", { userId = arguments.userId })
end

handlers["player.kill"] = function(arguments)
    local player, failure = player_argument(arguments)
    if player == nil then return failure end
    player:PushEvent("death")
    return result(true, "PLAYER_KILLED", "", { userId = arguments.userId })
end

handlers["player.god_mode"] = function(arguments)
    local player, failure = player_argument(arguments)
    if player == nil then return failure end
    if type(arguments.enabled) ~= "boolean" then
        return result(false, "INVALID_ENABLED", "enabled must be a boolean")
    end
    if player.components == nil or player.components.health == nil then
        return result(false, "COMPONENT_UNAVAILABLE", "player health component is unavailable")
    end
    player.components.health:SetInvincible(arguments.enabled)
    if player.components.talker ~= nil then
        player.components.talker:Say(arguments.enabled and "无敌模式已开启" or "无敌模式已关闭")
    end
    return result(true, "PLAYER_GOD_MODE_CHANGED", "", { userId = arguments.userId, enabled = arguments.enabled })
end

handlers["player.creative_mode"] = function(arguments)
    local player, failure = player_argument(arguments)
    if player == nil then return failure end
    if type(arguments.enabled) ~= "boolean" then
        return result(false, "INVALID_ENABLED", "enabled must be a boolean")
    end
    if player.components == nil or player.components.builder == nil then
        return result(false, "COMPONENT_UNAVAILABLE", "player builder component is unavailable")
    end
    player.components.builder.freebuildmode = arguments.enabled
    if player.components.talker ~= nil then
        player.components.talker:Say(arguments.enabled and "制作模式已开启" or "制作模式已关闭")
    end
    return result(true, "PLAYER_CREATIVE_MODE_CHANGED", "", { userId = arguments.userId, enabled = arguments.enabled })
end

handlers["player.resurrect"] = function(arguments)
    local player, failure = player_argument(arguments)
    if player == nil then return failure end
    player:PushEvent("respawnfromghost")
    player.rezsource = "DST-ADMIN-GO控制台"
    return result(true, "PLAYER_RESURRECTED", "", { userId = arguments.userId })
end

handlers["player.change_character"] = function(arguments)
    local player, failure = player_argument(arguments)
    if player == nil then return failure end
    c_despawn(player)
    c_announce("管理员已将玩家重置，该玩家可以重新选择角色")
    return result(true, "PLAYER_CHARACTER_RESET", "", { userId = arguments.userId })
end

handlers["player.ban"] = function(arguments)
    if type(arguments) ~= "table" or not valid_user_id(arguments.userId) then
        return result(false, "INVALID_USER_ID", "player userId is invalid")
    end
    TheNet:Ban(arguments.userId)
    return result(true, "PLAYER_BANNED", "", { userId = arguments.userId })
end

handlers["player.unban"] = function(arguments)
    if type(arguments) ~= "table" or not valid_user_id(arguments.userId) then
        return result(false, "INVALID_USER_ID", "player userId is invalid")
    end
    TheNet:Unban(arguments.userId)
    return result(true, "PLAYER_UNBANNED", "", { userId = arguments.userId })
end

M.allowedActions = {}
for action, _ in pairs(handlers) do M.allowedActions[action] = true end

local function persist(request, action_result)
    state.sequence = state.sequence + 1
    local session_id, shard_id = runtime_identity()
    local receipt = {
        schemaVersion = SCHEMA_VERSION,
        producerVersion = PRODUCER_VERSION,
        producerInstanceId = instance_id,
        sessionId = session_id,
        shardId = shard_id,
        sequence = state.sequence,
        requestId = request.requestId,
        action = request.action,
        ok = action_result.ok == true,
        code = safe_text(action_result.code, 64),
        message = safe_text(action_result.message, 512),
        details = action_result.details,
        completedAtUnix = os.time(),
    }
    local encoded_ok, encoded = pcall(json.encode, receipt)
    if not encoded_ok or type(encoded) ~= "string" then
        state.writing = false
        print("[DST-ADMIN-COMMAND ERROR] code=RECEIPT_ENCODE_FAILED")
        return false
    end
    local slot = state.nextSlot
    state.writing = true
    TheSim:SetPersistentString(OUTPUT_ROOT .. "command-receipt-" .. slot .. ".json", encoded, false, function(written)
        state.writing = false
        if written then state.nextSlot = slot == "a" and "b" or "a" end
    end)
    print(string.format("[DST-ADMIN-COMMAND RECEIPT] request=%s action=%s code=%s", request.requestId, request.action, receipt.code))
    return true
end

function M.Execute(request)
    if state.writing then
        return result(false, "COMMAND_BUSY", "a previous command receipt is still being written")
    end
    if type(request) ~= "table" or type(request.requestId) ~= "string"
        or #request.requestId < 16 or #request.requestId > 80
        or string.match(request.requestId, "^[A-Za-z0-9_-]+$") == nil
        or type(request.action) ~= "string" then
        return result(false, "INVALID_REQUEST", "requestId and action are required")
    end
    local handler = handlers[request.action]
    if handler == nil then
        local denied = result(false, "ACTION_NOT_ALLOWED", "action is not allowed")
        persist(request, denied)
        return denied
    end
    local executed, action_result = xpcall(function()
        return handler(type(request.arguments) == "table" and request.arguments or {})
    end, debug.traceback)
    if not executed then
        action_result = result(false, "ACTION_FAILED", safe_text(action_result, 512))
    elseif type(action_result) ~= "table" then
        action_result = result(false, "INVALID_ACTION_RESULT", "action returned an invalid result")
    end
    persist(request, action_result)
    return action_result
end

function M.ExecuteJSON(source)
    if type(source) ~= "string" or #source > 4096 then
        return result(false, "INVALID_REQUEST", "request JSON is invalid")
    end
    local decoded, request = pcall(json.decode, source)
    if not decoded then
        return result(false, "INVALID_REQUEST", "request JSON could not be decoded")
    end
    return M.Execute(request)
end

function M.Status()
    return { running = true, ready = TheWorld ~= nil and TheNet ~= nil, busy = state.writing, sequence = state.sequence }
end

return M
