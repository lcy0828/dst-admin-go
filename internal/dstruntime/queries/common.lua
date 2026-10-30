local json=require("json")
local function emit(data,complete,reason,scanned)
    local text=json.encode_compliant({protocolVersion=1,runId=request_id,queryId=query_id,complete=complete,data=data,reason=reason,scannedEntities=scanned})
    if #text>8192 then error("QUERY_OUTPUT_LIMIT") end
    print(text)
end
local function text(v,n)
    if type(v)~="string" then return "" end
    if #v<=n then return v end
    while n>0 and string.byte(v,n+1)>=128 and string.byte(v,n+1)<192 do n=n-1 end
    return string.sub(v,1,n)
end
