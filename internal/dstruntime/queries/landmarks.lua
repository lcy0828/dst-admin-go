local items,seen={},{}
local complete,reason=true,nil
local deadline=os.clock()+0.005
local scanned=0
local function add(kind,ref,x,z)
    if type(x)~="number"or type(z)~="number"or x~=x or z~=z or math.abs(x)>100000 or math.abs(z)>100000 then return end
    local id=kind..":"..ref
    if seen[id]then return end
    if #items>=48 then complete=false;reason="OUTPUT_LIMIT";return end
    seen[id]=true;items[#items+1]={id=id,landmarkId=kind,x=x,z=z}
end
local kinds={multiplayer_portal="spawn",multiplayer_portal_moonrock="spawn",pigking="pig_king",oasislake="oasis",dragonfly_spawner="dragonfly",beequeenhive="bee_queen",beequeenhivegrown="bee_queen"}
local ents=rawget(_G,"Ents")
if type(ents)~="table"then error("QUERY_ENTITIES_UNAVAILABLE")end
for guid,e in pairs(ents)do
    if scanned>=100000 then complete=false;reason="SCAN_LIMIT";break end
    if scanned%128==0 and os.clock()>=deadline then complete=false;reason="CPU_BUDGET";break end
    scanned=scanned+1
    local kind=type(e)=="table"and kinds[rawget(e,"prefab")]
    if kind and e.Transform and e.IsValid and e:IsValid()and not(e.HasTag and e:HasTag("INLIMBO"))then
        local x,_,z=e.Transform:GetWorldPosition();add(kind,tostring(guid),x,z)
    end
end
local topology=TheWorld and TheWorld.topology
local nodes=topology and topology.nodes
if type(nodes)=="table"then
    local count=0
    local fallback=nil
    local baths=false
    for index,node in ipairs(nodes)do
        count=count+1
        if count>4096 or os.clock()>=deadline then complete=false;reason="CPU_BUDGET";break end
        local id=topology.ids and topology.ids[index]
        if type(id)=="string"and string.find(id,"MoonIsland_Baths",1,true)and type(node)=="table"then
            add("lunar_island",tostring(index),node.x,node.y)
            baths=true
        elseif not fallback and type(id)=="string"and string.find(id,"MoonIsland_",1,true)and type(node)=="table"then
            fallback={index=index,x=node.x,z=node.y}
        end
    end
    if not baths and fallback then add("lunar_island",tostring(fallback.index),fallback.x,fallback.z)end
end
emit({items=items,count=#items},complete,reason,scanned)
