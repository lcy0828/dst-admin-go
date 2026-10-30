local world=rawget(_G,"TheWorld")
local state=type(world)=="table" and rawget(world,"state") or nil
if type(state)~="table" then error("QUERY_WORLD_UNAVAILABLE") end
local data={}
for _,key in ipairs({"cycles","season","phase","temperature","israining","issnowing"}) do
    local v=rawget(state,key)
    if type(v)=="number" or type(v)=="boolean" then data[key]=v
    elseif type(v)=="string" then data[key]=text(v,64) end
end
emit(data,true)
