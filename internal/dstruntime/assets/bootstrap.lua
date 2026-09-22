local VERSION = "2.4.8"
local PROTOCOL_VERSION = 2
local MODULE_ROOT = "../dst-admin/"
local READY_RETRY_SECONDS = 0.5

local function emit_error(code, message)
    print(string.format("[DST-ADMIN-RUNTIME ERROR] code=%s message=%s", tostring(code), tostring(message)))
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
        protocolVersion = PROTOCOL_VERSION,
        state = "loading",
        reloadInProgress = false,
        readinessTask = nil,
        readyAnnounced = false,
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

    function candidate.Stop()
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
            local ok, stopped = xpcall(candidate.Telemetry.Stop, debug.traceback)
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
        candidate.state = "stopped"
        return true
    end

    function candidate.Reload()
        if candidate.reloadInProgress or rawget(_G, "DSTAdmin") ~= candidate then
            return false
        end
        candidate.reloadInProgress = true
        build_candidate(function(next_candidate, load_error)
            candidate.reloadInProgress = false
            if next_candidate == nil then
                emit_error("RELOAD_LOAD_FAILED", load_error)
                return
            end
            activate_candidate(candidate, next_candidate, "RELOAD")
        end)
        return true
    end

    function candidate.Refresh()
        if candidate.state == "waiting_for_world" and candidate_ready(candidate) then
            candidate.state = "running"
        end
        if candidate.state ~= "running" or candidate.WorldState == nil or candidate.Telemetry == nil then
            return false
        end
        local function emit_telemetry()
            local ok, accepted = xpcall(candidate.Telemetry.EmitOnce, debug.traceback)
            if not ok or accepted ~= true then
                emit_error("REFRESH_TELEMETRY_FAILED", accepted)
                return false
            end
            return true
        end
        local ok, accepted = xpcall(function()
            return candidate.WorldState.EmitOnce(function(written)
                if written ~= true then
                    emit_error("REFRESH_WORLDSTATE_FAILED", "world state snapshot was not written")
                end
                emit_telemetry()
            end)
        end, debug.traceback)
        if not ok or accepted ~= true then
            emit_error("REFRESH_WORLDSTATE_FAILED", accepted)
            return false
        end
        return true
    end

    local function announce_ready()
        if candidate.readyAnnounced then return end
        candidate.readyAnnounced = true
        print(string.format("[DST-ADMIN-RUNTIME READY] version=%s protocol=%d", VERSION, PROTOCOL_VERSION))
    end

    local function await_world()
        if rawget(_G, "DSTAdmin") ~= candidate or candidate.state == "stopped" or candidate.state == "failed" then
            candidate.readinessTask = nil
            return
        end
        if candidate_ready(candidate) then
            candidate.readinessTask = nil
            candidate.state = "running"
            if not candidate.Refresh() then
                emit_error("INITIAL_REFRESH_FAILED", "runtime became ready without coherent initial snapshots")
                return
            end
            announce_ready()
            return
        end
        candidate.state = "waiting_for_world"
        if scheduler ~= nil and type(scheduler.ExecuteInTime) == "function" then
            candidate.readinessTask = scheduler:ExecuteInTime(READY_RETRY_SECONDS, await_world, "dst-admin-bootstrap-ready")
        end
    end

    function candidate.Activate()
        if candidate_ready(candidate) then
            candidate.state = "running"
            if not candidate.Refresh() then
                emit_error("INITIAL_REFRESH_FAILED", "runtime started without coherent initial snapshots")
                return false
            end
            announce_ready()
            return true
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
    if previous ~= nil and type(previous.Stop) == "function" then
        local stopped, stop_result = xpcall(previous.Stop, debug.traceback)
        if not stopped or stop_result == false then
            emit_error(operation .. "_PREVIOUS_STOP_FAILED", stop_result)
            return false
        end
    end

    if candidate.Start() then
        rawset(_G, "DSTAdmin", candidate)
        return candidate.Activate()
    end

    candidate.Stop()
    if previous ~= nil and type(previous.Start) == "function" then
        local restored, restore_result = xpcall(previous.Start, debug.traceback)
        if not restored or restore_result == false then
            emit_error(operation .. "_ROLLBACK_FAILED", restore_result)
        elseif type(previous.Refresh) == "function" then
            local refreshed, refresh_result = xpcall(previous.Refresh, debug.traceback)
            if not refreshed or refresh_result == false then
                emit_error(operation .. "_ROLLBACK_REFRESH_FAILED", refresh_result)
            end
        end
    end
    return false
end

build_candidate(function(candidate, load_error)
    if candidate == nil then
        emit_error("RUNTIME_LOAD_FAILED", load_error)
        return
    end
    activate_candidate(rawget(_G, "DSTAdmin"), candidate, "LOAD")
end)

return true
