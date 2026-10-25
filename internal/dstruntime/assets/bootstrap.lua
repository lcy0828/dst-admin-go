local VERSION = "2.4.12"
local PROTOCOL_VERSION = 2
local MODULE_ROOT = "mod_config_data/dst-admin/runtime/"
local READY_RETRY_SECONDS = 0.5
local pending_update = rawget(_G, "DSTAdminPendingUpdate")
local UPDATE_ID = pending_update ~= nil and pending_update.id or nil
local previous_runtime = rawget(_G, "DSTAdmin")

local function emit_error(code, message)
    print(string.format("[DST-ADMIN-RUNTIME ERROR] code=%s message=%s", tostring(code), tostring(message)))
end

local function update_result(state, message)
    if pending_update == nil or pending_update.aborted
        or rawget(_G, "DSTAdminPendingUpdate") ~= pending_update then return end
    -- Only the active maintenance attempt publishes this bounded acknowledgement.
    -- It does not replace health or trigger another periodic collector.
    pcall(function()
        local json = require("json")
        local payload = json.encode_compliant({schemaVersion=1, updateId=UPDATE_ID,
            state=state, message=string.sub(tostring(message or ""), 1, 512)})
        TheSim:SetPersistentString("mod_config_data/dst-admin/update-result.json", payload, false)
    end)
end

local function replacement_blocker(previous)
    if previous == nil then return nil end
    if previous.reloadInProgress then return "Reload" end
    for _, name in ipairs({"Commands", "Barriers", "Diagnostics", "Telemetry", "WorldState", "Events"}) do
        local module = previous[name]
        if module ~= nil and type(module.Status) == "function" then
            local ok, status = pcall(module.Status)
            if not ok or type(status) ~= "table" then return name .. " status unavailable" end
            if status.busy or status.writing or status.holding or (tonumber(status.pending) or 0) > 0 then return name end
        end
    end
    return nil
end

local function load_module(name, callback)
    TheSim:GetPersistentString(MODULE_ROOT .. name .. ".lua", function(success, source)
        if not success or type(source) ~= "string" then
            callback(nil, "module " .. name .. " is unavailable")
            return
        end
        local chunk, compile_error = loadstring(source)
        if chunk == nil then
            callback(nil, "module " .. name .. " failed to compile: " .. tostring(compile_error))
            return
        end
        local executed, module = xpcall(chunk, debug.traceback)
        if not executed or type(module) ~= "table" then
            callback(nil, "module " .. name .. " failed to load: " .. tostring(module))
            return
        end
        if module.version ~= VERSION then
            callback(nil, "module " .. name .. " has a different runtime version")
            return
        end
        callback(module, nil)
    end)
end

local build_candidate
local activate_candidate

local function module_ready(module)
	if module == nil then return false end
	if type(module.Status) ~= "function" then return true end
    local ok, status = xpcall(module.Status, debug.traceback)
    if not ok or type(status) ~= "table" then return false end
    -- Runtime 2.4 modules expose ready explicitly. Treat an omitted field as
    -- ready so older compatible modules can still be hot-reloaded.
    return status.running ~= false and status.ready ~= false
end

local function candidate_ready(candidate)
    return module_ready(candidate.Telemetry)
        and module_ready(candidate.WorldState)
        and module_ready(candidate.Commands)
        and module_ready(candidate.Events)
        and module_ready(candidate.Diagnostics)
        and module_ready(candidate.Barriers)
end

local function new_candidate()
    local candidate = {
        version = VERSION,
        updateId = UPDATE_ID,
        protocolVersion = PROTOCOL_VERSION,
        state = "loading",
        reloadInProgress = false,
        readinessTask = nil,
        readyAnnounced = false,
        activationCallback = nil,
    }

    function candidate.Status()
        local telemetry = candidate.Telemetry ~= nil and candidate.Telemetry.Status() or nil
        local worldstate = candidate.WorldState ~= nil and candidate.WorldState.Status() or nil
        local commands = candidate.Commands ~= nil and candidate.Commands.Status() or nil
        local events = candidate.Events ~= nil and candidate.Events.Status() or nil
        local diagnostics = candidate.Diagnostics ~= nil and candidate.Diagnostics.Status() or nil
        local barriers = candidate.Barriers ~= nil and candidate.Barriers.Status() or nil
        return {
            version = candidate.version,
            protocolVersion = candidate.protocolVersion,
            state = candidate.state,
            reloadInProgress = candidate.reloadInProgress,
            telemetry = telemetry,
            worldstate = worldstate,
            commands = commands,
            events = events,
            diagnostics = diagnostics,
            barriers = barriers,
        }
    end

    function candidate.Start()
        if candidate.state == "running" or candidate.state == "starting" then
            return true
        end
        if candidate.Telemetry == nil or candidate.WorldState == nil or candidate.Commands == nil or candidate.Events == nil or candidate.Diagnostics == nil or candidate.Barriers == nil then
            candidate.state = "failed"
            emit_error("START_FAILED", "runtime module is unavailable")
            return false
        end
        candidate.state = "starting"
        local ok, started = xpcall(candidate.Telemetry.Start, debug.traceback)
        if not ok or started == false then
            candidate.state = "failed"
            emit_error("START_FAILED", started)
            return false
        end
        ok, started = xpcall(candidate.WorldState.Start, debug.traceback)
        if not ok or started == false then
            candidate.Telemetry.Stop()
            candidate.state = "failed"
            emit_error("START_FAILED", started)
            return false
        end
        ok, started = xpcall(candidate.Events.Start, debug.traceback)
        if not ok or started == false then
            candidate.WorldState.Stop()
            candidate.Telemetry.Stop()
            candidate.state = "failed"
            emit_error("START_FAILED", started)
            return false
        end
        ok, started = xpcall(candidate.Diagnostics.Start, debug.traceback)
        if not ok or started == false then
            candidate.Events.Stop()
            candidate.WorldState.Stop()
            candidate.Telemetry.Stop()
            candidate.state = "failed"
            emit_error("START_FAILED", started)
            return false
        end
        ok, started = xpcall(candidate.Barriers.Start, debug.traceback)
        if not ok or started == false then
            candidate.Diagnostics.Stop()
            candidate.Events.Stop()
            candidate.WorldState.Stop()
            candidate.Telemetry.Stop()
            candidate.state = "failed"
            emit_error("START_FAILED", started)
            return false
        end
        candidate.state = candidate_ready(candidate) and "running" or "waiting_for_world"
        return true
    end

    function candidate.Stop(handover)
        if candidate.state == "stopped" then
            return true
        end
        if candidate.readinessTask ~= nil then
            candidate.readinessTask:Cancel()
            candidate.readinessTask = nil
        end
        if candidate.Diagnostics ~= nil then
            local ok, stopped = xpcall(candidate.Diagnostics.Stop, debug.traceback)
            if not ok or stopped == false then
                emit_error("STOP_FAILED", stopped)
                return false
            end
        end
        if candidate.Barriers ~= nil then
            local ok, stopped = xpcall(candidate.Barriers.Stop, debug.traceback)
            if not ok or stopped == false then
                emit_error("STOP_FAILED", stopped)
                return false
            end
        end
        if candidate.Events ~= nil then
            local ok, stopped = xpcall(candidate.Events.Stop, debug.traceback)
            if not ok or stopped == false then
                emit_error("STOP_FAILED", stopped)
                return false
            end
        end
        if candidate.WorldState ~= nil then
            local ok, stopped = xpcall(candidate.WorldState.Stop, debug.traceback)
            if not ok or stopped == false then
                emit_error("STOP_FAILED", stopped)
                return false
            end
        end
        if candidate.Telemetry ~= nil then
            local ok, stopped = xpcall(function() return candidate.Telemetry.Stop(handover) end, debug.traceback)
            if not ok or stopped == false then
                emit_error("STOP_FAILED", stopped)
                return false
            end
        end
        if candidate.Commands ~= nil and type(candidate.Commands.ClearCatalog) == "function" then
            local ok, failure = pcall(candidate.Commands.ClearCatalog)
            if not ok then
                emit_error("STOP_FAILED", failure)
                return false
            end
        end
        candidate.activationCallback = nil
        candidate.state = "stopped"
        return true
    end

    function candidate.Reload()
        if candidate.reloadInProgress or rawget(_G, "DSTAdmin") ~= candidate
            or rawget(_G, "DSTAdminPendingUpdate") ~= nil
            or candidate.state == "stopped" or candidate.state == "failed" then
            return false
        end
        candidate.reloadInProgress = true
        build_candidate(function(next_candidate, load_error)
            candidate.reloadInProgress = false
            -- Module reads are asynchronous. Another activation or an explicit
            -- Stop may have retired this instance while those reads completed.
            if rawget(_G, "DSTAdmin") ~= candidate or candidate.state == "stopped"
                or candidate.state == "failed" or rawget(_G, "DSTAdminPendingUpdate") ~= nil then return end
            if next_candidate == nil then
                emit_error("RELOAD_LOAD_FAILED", load_error)
                return
            end
            activate_candidate(candidate, next_candidate, "RELOAD")
        end)
        return true
    end

    function candidate.Refresh(callback)
        local completed = false
        local function finish(written)
            if completed then return end
            completed = true
            if type(callback) == "function" then callback(written == true) end
        end
        if candidate.state == "waiting_for_world" and candidate_ready(candidate) then
            candidate.state = "running"
        end
        if candidate.state ~= "running" or candidate.WorldState == nil or candidate.Telemetry == nil then
            finish(false)
            return false
        end
        local ok, accepted = xpcall(function()
            return candidate.WorldState.EmitOnce(function(written)
                if written ~= true then
                    emit_error("REFRESH_WORLDSTATE_FAILED", "world state snapshot was not written")
                    finish(false)
                    return
                end
                local emitted, result = xpcall(function()
                    return candidate.Telemetry.EmitOnce(function(persisted) finish(persisted) end)
                end, debug.traceback)
                if not emitted or result ~= true then
                    emit_error("REFRESH_TELEMETRY_FAILED", result)
                    finish(false)
                end
            end)
        end, debug.traceback)
        if not ok or accepted ~= true then
            emit_error("REFRESH_WORLDSTATE_FAILED", accepted)
            finish(false)
            return false
        end
        return true
    end

    local function announce_ready()
        if candidate.readyAnnounced then return end
        candidate.readyAnnounced = true
        print(string.format("[DST-ADMIN-RUNTIME READY] version=%s protocol=%d", VERSION, PROTOCOL_VERSION))
    end

    local function complete_activation()
        local callback = candidate.activationCallback
        candidate.activationCallback = nil
        candidate.state = "running"
        return candidate.Refresh(function(written)
            if rawget(_G, "DSTAdmin") ~= candidate or candidate.state == "stopped" then return end
            if written then
                announce_ready()
            else
                emit_error("INITIAL_REFRESH_FAILED", "runtime did not persist coherent initial snapshots")
            end
            if type(callback) == "function" then callback(written) end
        end)
    end

    local function await_world()
        if rawget(_G, "DSTAdmin") ~= candidate or candidate.state == "stopped" or candidate.state == "failed" then
            candidate.readinessTask = nil
            return
        end
        if candidate_ready(candidate) then
            candidate.readinessTask = nil
            complete_activation()
            return
        end
        candidate.state = "waiting_for_world"
        if scheduler ~= nil and type(scheduler.ExecuteInTime) == "function" then
            candidate.readinessTask = scheduler:ExecuteInTime(READY_RETRY_SECONDS, await_world, "dst-admin-bootstrap-ready")
        end
    end

    function candidate.Activate(callback)
        candidate.activationCallback = callback
        if candidate_ready(candidate) then
            return complete_activation()
        end
        candidate.state = "waiting_for_world"
        print(string.format("[DST-ADMIN-RUNTIME WAITING] state=waiting_for_world version=%s protocol=%d", VERSION, PROTOCOL_VERSION))
        await_world()
        return true
    end

    return candidate
end

build_candidate = function(callback)
    local candidate = new_candidate()
    load_module("telemetry", function(telemetry, telemetry_error)
        if telemetry == nil then
            callback(nil, telemetry_error)
            return
        end
        candidate.Telemetry = telemetry
        load_module("worldstate", function(worldstate, worldstate_error)
            if worldstate == nil then
                callback(nil, worldstate_error)
                return
            end
            candidate.WorldState = worldstate
            load_module("commands", function(commands, commands_error)
                if commands == nil then
                    callback(nil, commands_error)
                    return
                end
                candidate.Commands = commands
                load_module("events", function(events, events_error)
                    if events == nil then
                        callback(nil, events_error)
                        return
                    end
                    candidate.Events = events
                    load_module("diagnostics", function(diagnostics, diagnostics_error)
                        if diagnostics == nil then
                            callback(nil, diagnostics_error)
                            return
                        end
                        candidate.Diagnostics = diagnostics
                        load_module("barriers", function(barriers, barriers_error)
                            if barriers == nil then
                                callback(nil, barriers_error)
                                return
                            end
                            candidate.Barriers = barriers
                            callback(candidate, nil)
                        end)
                    end)
                end)
            end)
        end)
    end)
end

activate_candidate = function(previous, candidate, operation)
    if rawget(_G, "DSTAdmin") ~= previous then
        update_result("deferred", "runtime instance changed while loading modules")
        return false
    end
    -- A persisted health snapshot can predate a backup barrier, diagnostic or
    -- console command. Check the live modules in the same Lua turn as Stop.
    local blocker = replacement_blocker(previous)
    if blocker ~= nil then
        update_result("deferred", blocker)
        return false
    end
    if previous ~= nil and type(previous.Stop) == "function" then
        local stopped, stop_result = xpcall(function() return previous.Stop(true) end, debug.traceback)
        if not stopped or stop_result == false then
            update_result("failed", "previous runtime did not stop")
            emit_error(operation .. "_PREVIOUS_STOP_FAILED", stop_result)
            return false
        end
    end

    local restored = false
    local function restore_previous()
        if restored then return end
        restored = true
        local current = rawget(_G, "DSTAdmin")
        if current ~= candidate and current ~= previous then return end
        update_result("failed", "candidate activation failed; restoring previous runtime")
        pcall(candidate.Stop, true)
        rawset(_G, "DSTAdmin", previous)
        if previous ~= nil and type(previous.Start) == "function" then
            local ok, result = xpcall(previous.Start, debug.traceback)
            if not ok or result == false then
                emit_error(operation .. "_ROLLBACK_FAILED", result)
            elseif type(previous.Refresh) == "function" then
                local refreshed, refresh_result = xpcall(previous.Refresh, debug.traceback)
                if not refreshed or refresh_result == false then
                    emit_error(operation .. "_ROLLBACK_REFRESH_FAILED", refresh_result)
                end
            end
        end
    end
    local started, start_result = xpcall(candidate.Start, debug.traceback)
    if started and start_result == true then
        rawset(_G, "DSTAdmin", candidate)
        local activated, accepted = xpcall(function()
            return candidate.Activate(function(written)
                if not written then restore_previous() end
            end)
        end, debug.traceback)
        if activated and accepted == true then return not restored end
    end
    restore_previous()
    return false
end

build_candidate(function(candidate, load_error)
    if candidate == nil then
        update_result("failed", load_error)
        emit_error("RUNTIME_LOAD_FAILED", load_error)
        return
    end
    if pending_update ~= nil and (pending_update.aborted or rawget(_G, "DSTAdminPendingUpdate") ~= pending_update) then return end
    if pending_update ~= nil then pending_update.candidate = candidate end
    activate_candidate(previous_runtime, candidate, "LOAD")
end)

return true
