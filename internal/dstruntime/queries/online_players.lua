local players=rawget(_G,"AllPlayers")
if type(players)~="table" or TheNet==nil then error("QUERY_PLAYERS_UNAVAILABLE") end
local ids,n={},0
local deadline=os.clock()+0.005
for _,p in next,players do
    n=n+1
    if n>1024 or os.clock()>=deadline then error("QUERY_PLAYER_SCAN_LIMIT") end
    if type(p)=="table" then local id=rawget(p,"userid") if type(id)=="string" and #id<=80 then ids[id]=p end end
end
local clients=TheNet:GetClientTable()
if type(clients)~="table" then error("QUERY_PLAYERS_UNAVAILABLE") end
local items,seen,size,complete,reason={}, {},0,true,nil
n=0
for _,c in next,clients do
    n=n+1
    if n>1024 or os.clock()>=deadline then complete=false reason="CPU_BUDGET" break end
    if type(c)=="table" then
        local id,name=rawget(c,"userid"),rawget(c,"name")
        local host=name=="[Host]" and (rawget(c,"prefab")==nil or rawget(c,"prefab")=="") and (rawget(c,"netid")==nil or rawget(c,"netid")=="")
        if type(id)=="string" and #id<=80 and ids[id] and not seen[id] and not host then
            local item={id=id,name=text(name,192),prefab=text(rawget(c,"prefab"),80)}
            local bytes=#json.encode_compliant(item)+1
            if #items>=64 or size+bytes>7000 then complete=false reason="OUTPUT_LIMIT" break end
            seen[id]=true size=size+bytes items[#items+1]=item
        end
    end
end
if complete then
    for id,p in next,ids do
        if not seen[id] then
            if os.clock()>=deadline then complete=false reason="CPU_BUDGET" break end
            local item={id=id,name=text(rawget(p,"name"),192),prefab=text(rawget(p,"prefab"),80)}
            local bytes=#json.encode_compliant(item)+1
            if #items>=64 or size+bytes>7000 then complete=false reason="OUTPUT_LIMIT" break end
            seen[id]=true size=size+bytes items[#items+1]=item
        end
    end
end
-- An explicit count disambiguates an empty Lua table from a missing list.
emit({players=items,count=#items},complete,reason)
