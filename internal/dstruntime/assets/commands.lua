local json = require("json")

local M = {}
local SCHEMA_VERSION = 1
local PRODUCER_VERSION = "2.4.10"
local OUTPUT_ROOT = "mod_config_data/dst-admin/"
local INPUT_ROOT = "../dst-admin/command-requests/"
local RECENT_REQUEST_LIMIT = 256

local state = {
    sequence = 0,
    nextSlot = "a",
    writing = false,
    loading = false,
    dispatching = false,
    signals = {},
    activeIds = {},
    recentIds = {},
    recentOrder = {},
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

handlers["system.ping"] = function()
    return result(true, "CONSOLE_RESPONSIVE", "")
end

local CONSOLE_OUTPUT_BYTES = 16 * 1024

-- Keep synchronous print output inside this request's receipt. DST runs this
-- chunk without yielding; timers/coroutines that run later are outside its scope.
-- The saved wrapper becomes a plain forwarder after execution, even if user Lua
-- retained a reference to it. Never leave a global log hook installed at idle.
local function execute_console(chunk)
    local original_print = print
    local parts, size, truncated, active = {}, 0, false, true
    local function append(text)
        local remaining = CONSOLE_OUTPUT_BYTES - size
        if #text > remaining then
            truncated = true
            local end_at = remaining
            -- Avoid splitting a UTF-8 character at the output boundary.
            while end_at > 0 and string.byte(text, end_at + 1) ~= nil
                and string.byte(text, end_at + 1) >= 128 and string.byte(text, end_at + 1) < 192 do
                end_at = end_at - 1
            end
            text = string.sub(text, 1, end_at)
        end
        if #text > 0 then
            -- Bound fragment overhead as well as payload bytes, including
            -- commands that print many tiny or empty arguments.
            if #parts >= 128 then parts = { table.concat(parts) } end
            parts[#parts + 1] = text
            size = size + #text
        end
    end
    local function collect(...)
        for index = 1, select("#", ...) do
            if truncated then return end
            if index > 1 then append("\t") end
            if truncated then return end
            append(tostring(select(index, ...)))
        end
        if not truncated then append("\n") end
    end
    _G.print = function(...)
        original_print(...)
        if active and not truncated then
            -- A hostile tostring must not prevent restoration or turn a logging
            -- failure into a second execution of the user's command.
            local ok = pcall(collect, ...)
            if not ok then truncated = true end
        end
    end
    -- DST's native debug.traceback also enters the fatal script-error path.
    -- Console errors are ordinary command results: use pcall, as the game's
    -- own ExecuteConsoleCommand does, without invoking that global handler.
    local executed, failure = pcall(chunk)
    active = false
    _G.print = original_print
    local output = { text = table.concat(parts), truncated = truncated }
    parts = nil
    return executed, failure, { output = output }
end

handlers["console.execute"] = function(arguments)
    local script = type(arguments) == "table" and arguments.script or nil
    if type(script) ~= "string" or #script < 1 or #script > 4096
        or string.find(script, "\0", 1, true) ~= nil then
        return result(false, "INVALID_SCRIPT", "console script is invalid")
    end
    local chunk, compile_error = loadstring(script, "=DST Admin command")
    if chunk == nil then
        return result(false, "COMMAND_COMPILE_FAILED", safe_text(compile_error, 512), { output = { text = "", truncated = false } })
    end
    local executed, execution_error, details = execute_console(chunk)
    if not executed then
        local described, message = pcall(safe_text, execution_error, 512)
        return result(false, "COMMAND_EXECUTION_FAILED", described and message or "Lua command failed", details)
    end
    return result(true, "COMMAND_EXECUTED", "", details)
end

-- One bounded snapshot per shard, built only on request. Search/page changes
-- reuse registration metadata; never call prefab factories to infer components.
local CATALOG_TTL = 300
local CATALOG_PREFAB_LIMIT = 50000
local CATALOG_MOD_LIMIT = 500
local catalog_cache, catalog_expiry_task, catalog_cancel
local catalog_slice_deadline
local CATALOG_SLICE_SECONDS = .003
local CATALOG_BATCH_LIMIT = 2048
local CATALOG_BATCH_BYTES = 128 * 1024

local function catalog_checkpoint(index)
    if catalog_slice_deadline ~= nil and index % 64 == 0 and os.clock() >= catalog_slice_deadline then
        coroutine.yield()
    end
end

-- Lua 5.1 cannot yield from table.sort's C comparator. An iterative merge sort
-- allows both registration walks and sorting to share the same per-frame budget.
local function sort_catalog(entries)
    local scratch, width, count = {}, 1, #entries
    while width < count do
        for first = 1, count, width * 2 do
            local middle, last = math.min(first + width, count + 1), math.min(first + width * 2 - 1, count)
            local left, right = first, middle
            for index = first, last do
                if left < middle and (right > last or entries[left].id <= entries[right].id) then
                    scratch[index] = entries[left]
                    left = left + 1
                else
                    scratch[index] = entries[right]
                    right = right + 1
                end
                catalog_checkpoint(index)
            end
        end
        entries, scratch, width = scratch, entries, width * 2
    end
    return entries
end

local function clear_catalog()
    catalog_cache = nil
    if catalog_expiry_task ~= nil then
        catalog_expiry_task:Cancel()
        catalog_expiry_task = nil
    end
end

local function build_catalog()
    local registry = Prefabs or {}
    local owners, mods, entries, search_text = {}, {}, {}, {}
    local mod_count, mod_prefab_count = 0, 0
    for _, mod_id in ipairs(ModManager ~= nil and ModManager.enabledmods or {}) do
        mod_count = mod_count + 1
        catalog_checkpoint(mod_count)
        if mod_count > CATALOG_MOD_LIMIT then
            return nil, result(false, "CATALOG_TOO_LARGE", "too many loaded mods")
        end
        local mod = ModManager:GetMod(mod_id)
        if mod ~= nil then
            local owner = { id = safe_text(mod_id, 128), name = safe_text(mod.modinfo ~= nil and mod.modinfo.name or mod_id, 160), count = 0 }
            for id, prefab in pairs(mod.Prefabs or {}) do
                mod_prefab_count = mod_prefab_count + 1
                catalog_checkpoint(mod_prefab_count)
                if mod_prefab_count > CATALOG_PREFAB_LIMIT then
                    return nil, result(false, "CATALOG_TOO_LARGE", "too many mod prefab registrations")
                end
                if registry[id] == prefab then
                    owners[id] = owner
                    owner.count = owner.count + 1
                end
            end
            mods[#mods + 1] = owner
        end
    end
    local visited = 0
    for id, prefab in pairs(registry) do
        visited = visited + 1
        catalog_checkpoint(visited)
        if visited > CATALOG_PREFAB_LIMIT then return nil, result(false, "CATALOG_TOO_LARGE", "too many registered prefabs") end
        if type(prefab) == "table" and type(id) == "string" and #id <= 80 and string.match(id, "^[a-z0-9_]+$") ~= nil and type(prefab.fn) == "function" then
            local owner = owners[id]
            local name = STRINGS ~= nil and STRINGS.NAMES ~= nil and STRINGS.NAMES[string.upper(id)] or id
            name = type(name) == "string" and safe_text(name, 160) or id
            entries[#entries + 1] = { id = id, nameZhCN = name, nameEn = name, modId = owner ~= nil and owner.id or nil, modName = owner ~= nil and owner.name or nil }
            search_text[id] = string.lower(id .. " " .. name .. " " .. (owner ~= nil and owner.name or ""))
        end
    end
    entries = sort_catalog(entries)
    return { registry = registry, manager = ModManager, entries = entries, searchText = search_text, mods = mods, observedAt = os.time() }
end

handlers["catalog.entities"] = function(arguments)
    local query = type(arguments.query) == "string" and string.lower(arguments.query) or ""
    local source = type(arguments.source) == "string" and arguments.source or ""
    local offset, limit = tonumber(arguments.offset) or 0, tonumber(arguments.limit) or 120
    local maximum = arguments.snapshot == true and CATALOG_BATCH_LIMIT or 120
    if #query > 400 or #source > 128 or offset < 0 or offset > 50000 or offset % 1 ~= 0 or limit < 1 or limit > maximum or limit % 1 ~= 0 then
        return result(false, "INVALID_SEARCH", "catalog query is invalid")
    end
    if source == "" then source = "all" end
    if arguments.refresh == true or (catalog_cache ~= nil and
        (catalog_cache.registry ~= Prefabs or catalog_cache.manager ~= ModManager or os.time() - catalog_cache.observedAt >= CATALOG_TTL)) then
        clear_catalog()
    end
    if catalog_cache == nil then
        local snapshot, failure = build_catalog()
        if snapshot == nil then return failure end
        catalog_cache = snapshot
        -- A single expiry callback releases unused indexes without a poller.
        if TheWorld ~= nil and TheWorld.DoStaticTaskInTime ~= nil then
            catalog_expiry_task = TheWorld:DoStaticTaskInTime(CATALOG_TTL, function()
                if catalog_cache == snapshot then
                    catalog_expiry_task = nil
                    catalog_cache = nil
                end
            end)
        end
    end
    local snapshot = catalog_cache
    local matches = snapshot.entries
    if query ~= "" or source ~= "all" then
        -- Keep only the latest filter, not a cache entry for every keystroke.
        if snapshot.query ~= query or snapshot.source ~= source then
            matches = {}
            for index, entry in ipairs(snapshot.entries) do
                catalog_checkpoint(index)
                local matches_source = source == "all" or (source == "vanilla" and entry.modId == nil)
                    or (source == "mods" and entry.modId ~= nil) or source == entry.modId
                if matches_source and (query == "" or string.find(snapshot.searchText[entry.id], query, 1, true) ~= nil) then
                    matches[#matches + 1] = entry
                end
            end
            snapshot.query, snapshot.source, snapshot.matches = query, source, matches
        else
            matches = snapshot.matches
        end
    else
        snapshot.query, snapshot.source, snapshot.matches = nil, nil, nil
    end
    local items, page_bytes = {}, 2
    for index = offset + 1, math.min(offset + limit, #matches) do
        local item = matches[index]
        if arguments.snapshot == true then
            -- Bound the actual escaped JSON, including long Mod/localized names.
            -- Ordinary UI search keeps its existing 120-item contract.
            local item_bytes = #json.encode_compliant(item) + 1
            if page_bytes + item_bytes > CATALOG_BATCH_BYTES then break end
            page_bytes = page_bytes + item_bytes
        end
        items[#items + 1] = item
        catalog_checkpoint(index)
    end
    return result(true, "CATALOG_READY", "", { items = items, mods = snapshot.mods, total = #matches, offset = offset, hasMore = offset + #items < #matches, observedAt = os.date("!%Y-%m-%dT%H:%M:%SZ", snapshot.observedAt) })
end

local function apply_god_waterwalk(player)
    player:AddTag("dst_admin_waterwalk")
    if player.Physics ~= nil then player.Physics:ClearCollidesWith(COLLISION.LIMITS) end
    local drownable = player.components ~= nil and player.components.drownable or nil
    if drownable ~= nil then
        if player._dst_admin_shoulddrown == nil then
            player._dst_admin_shoulddrown = drownable.ShouldDrown
        end
        drownable.ShouldDrown = function() return false end
    end
end

local function set_god_mode(player, enabled)
    if enabled then
		player:AddTag("dst_admin_god_mode")
        if player:HasTag("playerghost") then
            player:PushEvent("respawnfromghost")
            player.rezsource = "DST-ADMIN-GO控制台"
        end
        if player._dst_admin_god_task ~= nil then player._dst_admin_god_task:Cancel() end
        player._dst_admin_god_task = player:DoPeriodicTask(.1, function(inst)
            local components = inst.components or {}
            if components.health ~= nil then
                components.health:SetInvincible(true)
                components.health:SetPercent(1)
            end
            if components.hunger ~= nil then components.hunger:SetPercent(1) end
            if components.sanity ~= nil then components.sanity:SetPercent(1) end
            if components.temperature ~= nil then components.temperature:SetTemperature(35) end
            if components.moisture ~= nil then
                if components.moisture.waterproofnessmodifiers ~= nil then
                    components.moisture.waterproofnessmodifiers:SetModifier("dst_admin_god", TUNING.WATERPROOFNESS_ABSOLUTE)
                end
                components.moisture:SetPercent(0)
            end
            if components.inventory ~= nil and components.inventory.isexternallyinsulated ~= nil then
                components.inventory.isexternallyinsulated:SetModifier("dst_admin_god", true)
            end
            apply_god_waterwalk(inst)
            if RemovePhysicsColliders ~= nil then RemovePhysicsColliders(inst) end
        end)
        apply_god_waterwalk(player)
        return
    end

    if player._dst_admin_god_task ~= nil then
        player._dst_admin_god_task:Cancel()
        player._dst_admin_god_task = nil
    end
	player:RemoveTag("dst_admin_god_mode")
    local components = player.components or {}
    if components.health ~= nil and player._dst_admin_stealth_task == nil then
        components.health:SetInvincible(false)
    end
    if components.moisture ~= nil and components.moisture.waterproofnessmodifiers ~= nil then
        components.moisture.waterproofnessmodifiers:RemoveModifier("dst_admin_god")
    end
    if components.inventory ~= nil and components.inventory.isexternallyinsulated ~= nil then
        components.inventory.isexternallyinsulated:SetModifier("dst_admin_god", false)
    end
    player:RemoveTag("dst_admin_god_waterwalk")
    if player.Physics ~= nil then
        if ChangeToCharacterPhysics ~= nil then
            ChangeToCharacterPhysics(player)
        else
            player.Physics:CollidesWith(COLLISION.LIMITS)
        end
		if player:HasTag("dst_admin_waterwalk_mode") then
			player:AddTag("dst_admin_waterwalk")
            player.Physics:ClearCollidesWith(COLLISION.LIMITS)
		else
			player:RemoveTag("dst_admin_waterwalk")
        end
    end
	if components.drownable ~= nil then
		if player:HasTag("dst_admin_waterwalk_mode") then
			components.drownable.ShouldDrown = function() return false end
		elseif player._dst_admin_shoulddrown ~= nil then
			components.drownable.ShouldDrown = player._dst_admin_shoulddrown
			player._dst_admin_shoulddrown = nil
		end
    end
end

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
    set_god_mode(player, arguments.enabled)
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

local dispatch_next

local function valid_request_identity(request)
    return type(request) == "table" and type(request.requestId) == "string"
        and #request.requestId >= 16 and #request.requestId <= 80
        and string.match(request.requestId, "^[A-Za-z0-9_-]+$") ~= nil
        and type(request.action) == "string"
end

local function remember_completed(request_id)
    state.activeIds[request_id] = nil
    if state.recentIds[request_id] then return end
    state.recentIds[request_id] = true
    table.insert(state.recentOrder, request_id)
    if #state.recentOrder > RECENT_REQUEST_LIMIT then
        local expired = table.remove(state.recentOrder, 1)
        state.recentIds[expired] = nil
    end
end

local function finish_signal(request_id)
    state.writing = false
    state.loading = false
    state.dispatching = false
    remember_completed(request_id)
    -- Periodic health can capture this dispatcher mid-command. Publish the
    -- idle transition only when needed, without resampling players/worlds.
    local runtime = rawget(_G, "DSTAdmin")
    if #state.signals == 0 and runtime ~= nil and runtime.Telemetry ~= nil
        and type(runtime.Telemetry.CommandFinished) == "function" then
        pcall(runtime.Telemetry.CommandFinished)
    end
    if dispatch_next ~= nil then dispatch_next() end
end

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
    local encoded_ok, encoded = pcall(json.encode_compliant, receipt)
    if not encoded_ok or type(encoded) ~= "string" then
        print("[DST-ADMIN-COMMAND ERROR] code=RECEIPT_ENCODE_FAILED")
        finish_signal(request.requestId)
        return false
    end
    -- Klei's compliant encoder still leaves some C0 control bytes unescaped.
    -- Arbitrary console output must remain valid JSON (including NUL/ANSI).
    encoded = string.gsub(encoded, "[%z\1-\8\11\12\14-\31]", function(char)
        return string.format("\\u%04x", string.byte(char))
    end)
    local slot = state.nextSlot
    state.writing = true
    local started = pcall(function()
        TheSim:SetPersistentString(OUTPUT_ROOT .. "command-receipt-" .. slot .. ".json", encoded, false, function(written)
            if written then
                state.nextSlot = slot == "a" and "b" or "a"
            else
                print("[DST-ADMIN-COMMAND ERROR] code=RECEIPT_WRITE_FAILED")
            end
            finish_signal(request.requestId)
        end)
    end)
    if not started then
        print("[DST-ADMIN-COMMAND ERROR] code=RECEIPT_WRITE_START_FAILED")
        finish_signal(request.requestId)
        return false
    end
    return true
end

local function execute_request(signal, request)
    local handler = handlers[request.action]
    if handler == nil then
        local denied = result(false, "ACTION_NOT_ALLOWED", "action is not allowed")
        signal.result = denied
        persist(request, denied)
        return
    end
    if request.action == "catalog.entities" and TheWorld ~= nil and TheWorld.DoStaticTaskInTime ~= nil then
        local started_at, task, finished = os.time(), nil, false
        local thread = coroutine.create(function()
            return handler(type(request.arguments) == "table" and request.arguments or {})
        end)
        local function finish_catalog(action_result)
            if finished then return end
            finished = true
            catalog_cancel = nil
            signal.result = action_result
            persist(request, action_result)
        end
        local resume_catalog
        resume_catalog = function()
            if finished then return end
            task = nil
            if os.time() - started_at >= 10 then
                finish_catalog(result(false, "CATALOG_TIMEOUT", "catalog collection exceeded its time budget"))
                return
            end
            catalog_slice_deadline = os.clock() + CATALOG_SLICE_SECONDS
            local ok, action_result = coroutine.resume(thread)
            catalog_slice_deadline = nil
            if not ok then
                finish_catalog(result(false, "ACTION_FAILED", safe_text(action_result, 512)))
            elseif coroutine.status(thread) == "dead" then
                finish_catalog(action_result)
            else
                -- Static tasks continue while the empty world is auto-paused.
                task = TheWorld:DoStaticTaskInTime(1 / 30, resume_catalog)
            end
        end
        catalog_cancel = function()
            if task ~= nil then task:Cancel(); task = nil end
            finish_catalog(result(false, "CATALOG_CANCELLED", "runtime stopped during catalog collection"))
        end
        resume_catalog()
        return
    end
    local executed, action_result = xpcall(function()
        return handler(type(request.arguments) == "table" and request.arguments or {})
    end, debug.traceback)
    if not executed then
        action_result = result(false, "ACTION_FAILED", safe_text(action_result, 512))
    elseif type(action_result) ~= "table" then
        action_result = result(false, "INVALID_ACTION_RESULT", "action returned an invalid result")
    end
    signal.result = action_result
    persist(request, action_result)
end

local function erase_document(path, callback)
    local function blank_document()
        if TheSim == nil or TheSim.SetPersistentString == nil then
            callback(false)
            return
        end
        local started = pcall(function()
            TheSim:SetPersistentString(path, "", false, function(written) callback(written == true) end)
        end)
        if not started then callback(false) end
    end
    if TheSim ~= nil and TheSim.ErasePersistentString ~= nil then
        local started = pcall(function()
            TheSim:ErasePersistentString(path, function(erased)
                if erased then callback(true) else blank_document() end
            end)
        end)
        if started then return end
    end
    blank_document()
end

local function load_document(signal)
    local expected = { requestId = signal.requestId, action = signal.action }
    local path = INPUT_ROOT .. expected.requestId .. ".json"
    state.loading = true
    local started = pcall(function()
        TheSim:GetPersistentString(path, function(success, source)
            state.loading = false
            local placeholder = { requestId = expected.requestId, action = expected.action }
            if not success or type(source) ~= "string" or #source < 1 or #source > 16384 then
                persist(placeholder, result(false, "COMMAND_DOCUMENT_UNAVAILABLE", "command request document is unavailable"))
                return
            end
            local decoded, request = pcall(json.decode, source)
            erase_document(path, function(erased)
                if not erased then
                    persist(placeholder, result(false, "COMMAND_DOCUMENT_CLEANUP_FAILED", "command request document could not be removed"))
                elseif not decoded or type(request) ~= "table" then
                    persist(placeholder, result(false, "COMMAND_DOCUMENT_INVALID", "command request document is invalid"))
                elseif request.requestId ~= expected.requestId or request.action ~= expected.action then
                    persist(placeholder, result(false, "COMMAND_DOCUMENT_MISMATCH", "command request document identity does not match"))
                else
                    local executed, execution_error = xpcall(function() execute_request(signal, request) end, debug.traceback)
                    if not executed then
                        persist(placeholder, result(false, "COMMAND_DOCUMENT_EXECUTION_FAILED", safe_text(execution_error, 512)))
                    end
                end
            end)
        end)
    end)
    if not started then
        state.loading = false
        persist(expected, result(false, "COMMAND_DOCUMENT_UNAVAILABLE", "command request document could not be read"))
    end
end

dispatch_next = function()
    if state.dispatching or state.writing or state.loading then return end
    local signal = table.remove(state.signals, 1)
    if signal == nil then return end
    state.dispatching = true
    if signal.request ~= nil then
        execute_request(signal, signal.request)
    else
        load_document(signal)
    end
end

local function submit_signal(signal)
    if state.activeIds[signal.requestId] or state.recentIds[signal.requestId] then
        return false
    end
    state.activeIds[signal.requestId] = true
    table.insert(state.signals, signal)
    dispatch_next()
    return true
end

function M.Execute(request)
    if not valid_request_identity(request) then
        return result(false, "INVALID_REQUEST", "requestId and action are required")
    end
    local signal = { requestId = request.requestId, action = request.action, request = request }
    if not submit_signal(signal) then
        return result(true, "COMMAND_ALREADY_ACCEPTED", "")
    end
    return signal.result or result(true, "COMMAND_ACCEPTED", "")
end

function M.ExecuteJSON(source)
    if type(source) ~= "string" or #source > 16384 then
        return result(false, "INVALID_REQUEST", "request JSON is invalid")
    end
    local decoded, request = pcall(json.decode, source)
    if not decoded then
        return result(false, "INVALID_REQUEST", "request JSON could not be decoded")
    end
    return M.Execute(request)
end

function M.ExecuteFile(request_id, action)
    if type(request_id) ~= "string" or #request_id < 16 or #request_id > 80
        or string.match(request_id, "^[A-Za-z0-9_-]+$") == nil
        or type(action) ~= "string" or handlers[action] == nil then
        return result(false, "INVALID_REQUEST", "requestId and action are invalid")
    end
    if not submit_signal({ requestId = request_id, action = action }) then
        return result(true, "COMMAND_DOCUMENT_ALREADY_ACCEPTED", "")
    end
    return result(true, "COMMAND_DOCUMENT_ACCEPTED", "")
end

function M.ClearCatalog()
    if catalog_cancel ~= nil then catalog_cancel() end
    clear_catalog()
end

function M.Status()
    return {
        running = true,
        ready = TheWorld ~= nil and TheNet ~= nil,
        busy = state.dispatching or state.writing or state.loading or #state.signals > 0,
        pending = #state.signals,
        sequence = state.sequence,
    }
end

return M
