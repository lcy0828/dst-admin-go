-- Inlined into managed modules so old Runtime backup layouts stay readable.
local clock = {}

local tasks = {}
function clock.Poll()
    local due = {}
    local now = os.time()
    for task in pairs(tasks) do
        if task.deadline <= now then due[#due + 1] = task end
    end
    for _, task in ipairs(due) do task:Run() end
end

function clock.After(delay, callback, name)
    local task = { deadline = os.time() + delay }
    function task:Cancel()
        tasks[self] = nil
        if self.timer ~= nil then self.timer:Cancel(); self.timer = nil end
    end
    function task:Run(from_timer)
        if tasks[self] == nil then return end
        if from_timer then self.timer = nil end
        self:Cancel()
        callback()
    end
    tasks[task] = true
    if TheWorld ~= nil and type(TheWorld.DoStaticTaskInTime) == "function" then
        task.timer = TheWorld:DoStaticTaskInTime(delay, function() task:Run(true) end)
    elseif staticScheduler ~= nil and type(staticScheduler.ExecuteInTime) == "function" then
        task.timer = staticScheduler:ExecuteInTime(delay, function() task:Run(true) end, name)
    else
        tasks[task] = nil
        error("DST Admin requires the static scheduler")
    end
    return task
end

function clock.Paused()
    if TheNet ~= nil and type(TheNet.IsServerPaused) == "function" then
        local ok, paused = pcall(TheNet.IsServerPaused, TheNet)
        if ok and paused == true then return true end
    end
    if type(IsSimPaused) == "function" then
        local ok, paused = pcall(IsSimPaused)
        if ok and paused == true then return true end
    end
    -- False from the Lua pause APIs does not rule out native empty-server
    -- sleep. Only an active real client supplies evidence of an awake world.
    if TheNet ~= nil and type(TheNet.GetClientTable) == "function" then
        local ok, clients = pcall(TheNet.GetClientTable, TheNet)
        if ok and type(clients) == "table" then
            for _, client in ipairs(clients) do
                if client.userid ~= nil and client.name ~= "[Host]" then return false end
            end
        end
    end
    return nil
end

function clock.Now()
    if type(GetStaticTime) == "function" then return GetStaticTime() end
    return os.time()
end

function clock.ReadyDelay(attempt)
    return (attempt or 0) < 60 and 1 or 5
end
