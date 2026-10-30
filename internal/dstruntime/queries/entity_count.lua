local entities=rawget(_G,"Ents")
if type(entities)~="table" then error("QUERY_ENTITIES_UNAVAILABLE") end
local count,scanned,key=0,0,nil
local deadline=os.clock()+0.005
local complete,reason=true,nil
while true do
    local value
    key,value=next(entities,key)
    if key==nil then break end
    if scanned>=100000 then complete=false reason="SCAN_LIMIT" break end
    if scanned%128==0 and os.clock()>=deadline then complete=false reason="CPU_BUDGET" break end
    scanned=scanned+1
    if type(value)=="table" and rawget(value,"prefab")==prefab then count=count+1 end
end
if complete then emit({prefab=prefab,count=count},true,nil,scanned)
else emit({prefab=prefab,lowerBound=count},false,reason,scanned) end
