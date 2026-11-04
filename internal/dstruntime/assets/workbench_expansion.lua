-- Code-owned world editing. All scans and plans are demand driven and bounded.
local WorkbenchExpansion = (function()
    local M,plans={},{}
    local generation=0
    local tasks={}
    local dummies=setmetatable({}, {__mode="k"})
    local function finite(n)return type(n)=="number" and n==n and math.abs(n)<=16000 end
    local function valid(e)return e and e.IsValid and e:IsValid() end
    local function player(id)
        local p=id and UserToPlayer(id)
        if id and (not valid(p) or p.migration or not p.Transform) then error("PLAYER_LOCATION_UNCERTAIN") end
        return p
    end
    local function safe(x,z,ocean)
        if not finite(x) or not finite(z) then return false end
        local map=TheWorld.Map
        if not map or not map.IsOceanAtPoint or not map.IsPassableAtPoint or not map.IsAboveGroundAtPoint then error("TERRAIN_CHECK_UNSUPPORTED") end
        if map.GetPlatformAtPoint and map:GetPlatformAtPoint(x,z) then return false end
        if ocean then return map:IsOceanAtPoint(x,0,z,true) end
        return map:IsAboveGroundAtPoint(x,0,z,false) and map:IsPassableAtPoint(x,0,z,false,true) and not map:IsOceanAtPoint(x,0,z,true)
            and (not map.IsGroundTargetBlocked or not map:IsGroundTargetBlocked(Vector3(x,0,z)))
    end
    local function checkpoint(deadline)if os.clock()>deadline then error("BATCH_CPU_LIMIT") end end
    -- A full-world traversal continues its original iterator across frames.
    -- Nothing is spawned and no confirmation exists until the traversal ends.
    local function scanWork(prefab,held)
        local entities=Ents or {};local key=nil;local rows,n,units={},0,0
        local world,epoch,untilTime=TheWorld,generation,os.time()+4
        return function()
            if generation~=epoch or TheWorld~=world or Ents~=entities or os.time()>=untilTime then error("BATCH_SCAN_EXPIRED")end
            local deadline=os.clock()+.004
            while true do
                local e;key,e=next(entities,key)
                if key==nil then
                    return true,rows,units
                end
                n=n+1;if n>50000 then error("BATCH_SCAN_LIMIT")end
                if e.prefab==prefab and valid(e) then
                    local c=e.components or {};local inventory=c.inventoryitem
                    if held or not e:HasTag("INLIMBO") and not(inventory and inventory.owner)then
                        rows[#rows+1]=e
                        local amount=c.stackable and c.stackable:StackSize()or 1
                        if type(amount)~="number"or amount~=math.floor(amount)or amount<1 or amount>100000 then error("BATCH_COUNT_INVALID")end
                        units=units+amount
                    end
                end
                if n%64==0 and os.clock()>=deadline then return false end
            end
        end
    end
    local function countLine(value)
        return "[DST-ADMIN-COUNT-V1] "..require("json").encode_compliant({requestId=value.requestId,prefab=value.prefab,entities=value.entities,quantity=value.quantity,includeInventory=value.includeInventory,complete=value.complete})
    end
    function M.EncodeObservation(value)
        if value._count then return countLine(value),"count"end
        return WorkbenchV2.Encode(value),"batch"
    end
    local function row(e,check,apply,verify,before,after)
        return {entity=e,prefab=e.prefab or "world",check=check,apply=apply,verify=verify,before=before,after=after}
    end
    local function one(e,check,apply,verify,before,after)return {row(e,check,apply,verify,before,after)} end
    local function components(p,...)
        if not p or p:HasTag("playerghost") then error("PLAYER_GHOST") end
        for _,name in ipairs({...})do if not p.components[name] then error("COMPONENT_UNAVAILABLE") end end
    end
    local function heal(p)
        local c=p.components
        if c.health and not p:HasTag("health_as_oldage") then c.health:SetPercent(1) end
        if c.hunger then c.hunger:SetPercent(1) end
        if c.sanity then c.sanity:SetPercent(1) end
        if c.moisture then c.moisture:SetPercent(0) end
        if c.temperature then c.temperature:SetTemperature(25) end
    end
    local function healed(p)
        local c=p.components
        return (not c.health or p:HasTag("health_as_oldage") or c.health:GetPercent()>=.999)
            and (not c.hunger or c.hunger:GetPercent()>=.999) and (not c.sanity or c.sanity:GetPercent()>=.999)
    end
    local function spawnAllowed(prefab,skin,p)
        if not Prefabs or not Prefabs[prefab] then error("PREFAB_UNAVAILABLE") end
        if prefab=="world" or prefab=="forest" or prefab=="cave" or prefab:match("_network$") or prefab:match("_placer$") or prefab:match("^world") then error("PROTECTED_PREFAB") end
        if prefab=="beefalo" and TheWorld:HasTag("cave") then error("PREFAB_REQUIRES_FOREST") end
        if skin and skin~="" and (not p or not ValidateRecipeSkinRequest or ValidateRecipeSkinRequest(p.userid,prefab,skin)~=skin) then error("SKIN_NOT_OWNED") end
    end
    local function spawn(prefab,x,z,skin,p)
        -- A native constructor is atomic. Yield between rows, never after a row
        -- has started: a budget exception here could lose a confirmed unit.
        spawnAllowed(prefab,skin,p)
        if not Prefabs or not Prefabs[prefab] then error("PREFAB_UNAVAILABLE") end
        -- Constructors which require the forest ecosystem must not run in caves.
        if prefab=="beefalo" and TheWorld:HasTag("cave") then error("PREFAB_REQUIRES_FOREST") end
        local e=SpawnPrefab(prefab,skin~="" and skin or nil,nil,p and p.userid)
        if not valid(e) or not e.Transform then error("SPAWN_UNCONFIRMED") end
        e.Transform:SetPosition(x,0,z)
        print("[DST-ADMIN-EDIT-SPAWN] "..require("json").encode_compliant({prefab=prefab,entityGuid=tostring(e.GUID),x=x,z=z}))
        return e
    end
    local function spawnRows(p,prefab,points,skin,ocean)
        spawnAllowed(prefab,skin,p)
        local out={}
        for _,point in ipairs(points)do
            local x,z=point.x,point.z
            if not safe(x,z,ocean) then error("NO_LANDING_POINT") end
            local created
            local r=row(p,function()spawnAllowed(prefab,skin,p);return safe(x,z,ocean)end,function()
                created=spawn(prefab,x,z,skin,p)
                if prefab=="beebox" and created.components.harvestable then local c=created.components.harvestable;c.produce=c.maxproduce or 5;c:Grow()end
                if prefab=="moon_altar_astral" and created.set_stage_fn then created:set_stage_fn(2)end
            end,function()
                if not valid(created) then return false end
                local xx,_,zz=created.Transform:GetWorldPosition();return (xx-x)^2+(zz-z)^2<.01
            end)
            r.prefab=prefab;r.units=1;out[#out+1]=r
        end
        return out
    end
    local function plan(id,a,p,deadline,scanned,scanUnits)
        local world=TheWorld;local same=function()return TheWorld==world end
        if a.expected_session and a.expected_session~="" and (not world.meta or world.meta.session_identifier~=a.expected_session)then error("MAP_SESSION_CHANGED")end
        if id=="set_daylight_mode" then
            local clock=world.net and world.net.components.clock or world.components.clock
            if not clock or not clock.OnSave then error("CLOCK_UNAVAILABLE") end
            local original=clock:OnSave().segs
            local requested=a.mode=="restore" and M.daylightBefore or a.mode=="day" and {day=16,dusk=0,night=0} or {day=0,dusk=0,night=16}
            if not requested then error("MODE_NOT_OWNED") end
            if a.mode=="restore" and M.daylightApplied and (original.day~=M.daylightApplied.day or original.dusk~=M.daylightApplied.dusk or original.night~=M.daylightApplied.night) then error("MODE_OWNERSHIP_UNCERTAIN") end
            return one(world,function()local current=clock:OnSave().segs;return current.day==original.day and current.dusk==original.dusk and current.night==original.night end,function()
                if a.mode~="restore" and not M.daylightBefore then M.daylightBefore=original end
                world:PushEvent("ms_setclocksegs",requested);M.daylightApplied=requested
                if a.mode=="restore" then M.daylightBefore=nil;M.daylightApplied=nil end
            end,function()local v=clock:OnSave().segs;return v.day==requested.day and v.dusk==requested.dusk and v.night==requested.night end)
        elseif id=="give_turf" or id=="give_character_supplies" then
            components(p,"inventory");local prefab=a.prefab
            if id=="give_turf" then local tile=WORLD_TILES[a.tile];local def=require("worldtiledefs").turf[tile];prefab=def and "turf_"..def.name end
            if not prefab or not Prefabs[prefab] then error("PREFAB_UNAVAILABLE") end
            if id=="give_character_supplies" then
                if p.prefab~=a.character then error("CHARACTER_UNSUPPORTED") end
                local choices={wilson={beardhair=true},wolfgang={dumbbell=true},wendy={abigail_flower=true},wx78={gears=true},wormwood={livinglog=true},walter={slingshot=true,slingshotammo_rock=true},willow={willow_ember=true,lighter=true,bernie_inactive=true},wickerbottom={papyrus=true},woodie={monstermeat=true,log=true},wes={balloon=true,balloonspeed=true,balloonparty=true},waxwell={nightmarefuel=true},wathgrithr={wathgrithrhat=true,spear_wathgrithr=true},webber={silk=true,spidereggsack=true,monstermeat=true},winona={winona_catapult_item=true,winona_spotlight_item=true,winona_battery_high_item=true,bluegem=true,sewing_tape=true,purebrilliance=true,horrorfuel=true},warly={portablecookpot_item=true,voltgoatjelly=true,glowberrymousse=true,spice_sugar=true,spice_salt=true,spice_chili=true,spice_garlic=true},wortox={wortox_soul=true,bee=true,panflute=true,wortox_souljar=true,killerbee=true,wortox_nabbag=true},wurt={merm=true,mermguard=true,mermking=true},wanda={nightmarefuel=true,pocketwatch_heal=true}}
                if not choices[a.character] or not choices[a.character][prefab] then error("CHARACTER_ITEM_UNAVAILABLE") end
            end
            local count=id=="give_turf" and 20 or a.count;local out={}
            for _=1,count do
                local item,before
                local r=row(p,function()return p.components.inventory~=nil end,function()
                    local inv=p.components.inventory;local _,n=inv:Has(prefab,0,false);before=n
                    local x,_,z=p.Transform:GetWorldPosition();item=spawn(prefab,x,z,"",p)
                    if item.components.inventoryitem then inv:GiveItem(item)end
                end,function()
                    if before~=nil then local _,n=p.components.inventory:Has(prefab,0,false);if n>=before+1 then return true end end
                    return valid(item) and (not item.components.inventoryitem or item.components.inventoryitem.owner==nil)
                end)
                r.prefab=prefab;r.units=1;r.destination=function()return valid(item)and item.components.inventoryitem and item.components.inventoryitem.owner==nil and "none"or "inventory"end
                out[#out+1]=r
            end
            return out
        elseif id=="fertilize_farm_soil" then
            local manager=world.components.farming_manager;if not manager then error("FARMING_UNAVAILABLE") end
            local x,_,z=p.Transform:GetWorldPosition();local tx,ty=world.Map:GetTileCoordsAtPoint(x,0,z);local r=math.ceil(a.radius/4);local out={}
            for dx=-r,r do for dz=-r,r do local xx,zz=tx+dx,ty+dz;local px,_,pz=world.Map:GetTileCenterPoint(xx,zz)
                if (px-x)^2+(pz-z)^2<=a.radius^2 and world.Map:IsFarmableSoilAtPoint(px,0,pz) then
                    out[#out+1]=row(world,function()return world.Map:IsFarmableSoilAtPoint(px,0,pz)end,function()manager:SetTileNutrients(xx,zz,100,100,100);manager:AddSoilMoistureAtPoint(px,0,pz,TUNING.SOIL_MAX_MOISTURE_VALUE)end,function()local n1,n2,n3=manager:GetTileNutrients(xx,zz);return n1>=99 and n2>=99 and n3>=99 and manager:IsSoilMoistAtPoint(px,0,pz)end)
                    if #out>25 then error("BATCH_TARGET_LIMIT") end
                end
            end end;return out
        elseif id=="set_nearby_movement" then
            local x,_,z=p.Transform:GetWorldPosition();local found=TheSim:FindEntities(x,0,z,a.radius,nil,{"INLIMBO","FX","player","playerghost"});if #found>512 then error("BATCH_SCAN_LIMIT") end
            local out={}
            for _,e in ipairs(found)do if e.components and e.components.locomotor then
                local l=e.components.locomotor;out[#out+1]=row(e,function()return valid(e) and e.components.locomotor==l end,function()
                    if a.enabled then l:RemoveExternalSpeedMultiplier(e,"dst_admin_movement")else l:SetExternalSpeedMultiplier(e,"dst_admin_movement",0)end
                end,function()local current=l:GetExternalSpeedMultiplier(e,"dst_admin_movement");return a.enabled and current==1 or not a.enabled and current==0 end)
                if #out>40 then error("BATCH_TARGET_LIMIT") end
            end end;return out
        elseif id=="edit_world_tiles" then
            local map=world.Map;local tile=WORLD_TILES[a.tile]
            if not tile or not map.SetTile or not map.CanTerraformAtPoint then error("TERRAIN_CHECK_UNSUPPORTED") end
            local tx,ty=map:GetTileCoordsAtPoint(a.x,0,a.z);local out={};local offset=math.floor(a.size/2)
            for dx=0,a.size-1 do for dz=0,a.size-1 do
                local xx,zz=tx+dx-offset,ty+dz-offset;local x,_,z=map:GetTileCenterPoint(xx,zz)
                if not safe(x,z,false) or not map:CanTerraformAtPoint(x,0,z) then error("TILE_NOT_EDITABLE") end
                local before=map:GetTile(xx,zz)
                out[#out+1]=row(world,function()return map:GetTile(xx,zz)==before and safe(x,z,false)end,function()
                    map:SetTile(xx,zz,tile)
                    for _,e in ipairs(map:GetEntitiesOnTileAtPoint(x,0,z))do if e:HasTag("soil") then e:PushEvent("collapsesoil") end end
                end,function()return map:GetTile(xx,zz)==tile end,before,tile)
            end end
            return out
        elseif id=="spawn_entity_at" or id=="spawn_near_player" then
            local x,z=a.x,a.z
            if id=="spawn_near_player" then local px,_,pz=p.Transform:GetWorldPosition();x=px+3;z=pz+3 end
            local points={};for i=1,a.count do local index=i-1;points[i]={x=x+(index%5)*1.5,z=z+math.floor(index/5)*1.5} end
            return spawnRows(p,a.prefab,points,a.skin or "",false)
        elseif id=="spawn_entity_array" then
            local prefabs={rock_tree_1="tree_rock1",rock_tree_2="tree_rock",beebox="beebox",pond="pond_cave"}
            local ocean=false;local x,_,z=p.Transform:GetWorldPosition();local points={};local width=math.ceil(math.sqrt(a.count))
            for i=1,a.count do
                if a.layout=="beebox" or a.layout=="pond" then local angle=(i-1)*2*math.pi/a.count;points[i]={x=x+math.cos(angle)*a.spacing,z=z+math.sin(angle)*a.spacing}
                else points[i]={x=x+(i-1)%width*a.spacing+6,z=z+math.floor((i-1)/width)*a.spacing+6} end
            end
            local rows=spawnRows(p,prefabs[a.layout],points,"",ocean)
            if a.layout=="beebox" then
                local extra=spawnRows(p,"firesuppressor",{{x=x,z=z}},"",false);rows[#rows+1]=extra[1]
            end
            return rows
        elseif id=="remove_world_entities" then
            local found=scanned;if #found>40 then error("BATCH_TARGET_LIMIT") end
            local out={}
            for _,e in ipairs(found)do
                if e==world or e:HasTag("player") or e:HasTag("playerghost") or e:HasTag("world") or e.components and e.components.worldmigrator then error("PROTECTED_ENTITY") end
                local owner=e.components and e.components.inventoryitem and e.components.inventoryitem.owner
                out[#out+1]=row(e,function()return valid(e) and e.prefab==a.prefab and (e.components and e.components.inventoryitem and e.components.inventoryitem.owner or nil)==owner end,
                    function()e:Remove()end,function()return not valid(e)end)
            end
            return out
        elseif id=="announce_entity_count" then
            local found,units=scanned,scanUnits
            return one(world,same,function()TheNet:Announce(a.prefab.."："..tostring(units).." 个（"..tostring(#found).." 个实体，含背包和箱子）")end,function()return false end)
        elseif id=="cycle_entity_teleport" or id=="teleport_world_center" then
            components(p,"health");if not p.Physics or not p.Physics.Teleport then error("MOVEMENT_UNSUPPORTED") end
            local x,z=0,0;local guid=nil
            if id=="cycle_entity_teleport" then
                local found=scanned;if #found==0 then error("ENTITY_NOT_FOUND") end
                local previous=p._dst_admin_cycle_guid or 0;local first,following=nil,nil
                for _,e in ipairs(found)do
                    if not first or e.GUID<first.GUID then first=e end
                    if e.GUID>previous and(not following or e.GUID<following.GUID)then following=e end
                end
                local selected=following or first
                local sx,_,sz=selected.Transform:GetWorldPosition();x,z=sx,sz;guid=selected.GUID
            end
            local chosen=nil
            for i=0,512 do checkpoint(deadline);local angle=i*math.pi/4;local r=8*math.ceil(i/8);local xx,zz=x+math.cos(angle)*r,z+math.sin(angle)*r;if safe(xx,zz,false)then chosen={x=xx,z=zz};break end end
            if not chosen then error("NO_LANDING_POINT") end
            return one(p,function()return safe(chosen.x,chosen.z,false) and not p.migration and not p:HasTag("playerghost")end,function()
                p.Physics:Teleport(chosen.x,0,chosen.z);if guid then p._dst_admin_cycle_guid=guid end
            end,function()local xx,_,zz=p.Transform:GetWorldPosition();return (xx-chosen.x)^2+(zz-chosen.z)^2<1 end)
        elseif id=="migrate_player" then
            local dest=a.destination_shard
            if not TheShard or tostring(TheShard:GetShardId())==dest or not Shard_IsWorldAvailable or not Shard_IsWorldAvailable(dest) then error("DESTINATION_WORLD_UNAVAILABLE") end
            return one(p,function()return not p.migration and Shard_IsWorldAvailable(dest)end,function()
                world:PushEvent("ms_playerdespawnandmigrate",{player=p,worldid=dest,portalid=a.portal_id})
            end,function()return false end) -- Source disappearance is NOT arrival evidence.
        elseif id=="unlock_player_recipe" then
            components(p,"builder");local recipe=AllRecipes and AllRecipes[a.recipe];if not recipe then error("RECIPE_UNAVAILABLE") end
            return one(p,function()return p.components.builder~=nil end,function()p.components.builder:AddRecipe(a.recipe)end,function()return p.components.builder:KnowsRecipe(a.recipe)end)
        elseif id=="give_starter_kit" then
            components(p,"inventory");local out={};for _,prefab in ipairs({"axe","pickaxe","shovel"})do
                local item;out[#out+1]=row(p,function()return p.components.inventory~=nil end,function()local x,_,z=p.Transform:GetWorldPosition();item=spawn(prefab,x,z,"",p);p.components.inventory:GiveItem(item)end,function()return valid(item)end)
            end;return out
        elseif id=="restore_player_stats" then
            components(p,"health");return one(p,function()return not p:HasTag("playerghost")end,function()heal(p)end,function()return healed(p)end)
        elseif id=="set_player_health_value" then
            components(p,"health");if p:HasTag("health_as_oldage") then error("CHARACTER_UNSUPPORTED") end
            local c=p.components.health;local target=math.min(a.value,c:GetMaxWithPenalty())
            return one(p,function()return p.components.health==c and not p:HasTag("playerghost")end,function()c:SetVal(target,"dst_admin_go")end,function()return math.abs(c.currenthealth-target)<.01 end,c.currenthealth,target)
        elseif id=="match_world_temperature" then
            components(p,"temperature");local c=p.components.temperature;local t=world.state.temperature
            if not finite(t) then error("WORLD_TEMPERATURE_UNAVAILABLE") end
            return one(p,function()return p.components.temperature==c end,function()c:SetTemperature(t)end,function()return math.abs(c:GetCurrent()-t)<.01 end,c:GetCurrent(),t)
        elseif id=="apply_all_players" then
            local out={};for _,target in ipairs(AllPlayers or {})do
                local e=target;if #out>=40 then error("BATCH_TARGET_LIMIT") end
                out[#out+1]=row(e,function()return valid(e) and not e.migration and e.components.health~=nil end,function()
                    if a.action=="restore" then heal(e)
                    elseif a.action=="kill" and not e:HasTag("playerghost") then e.components.health:Kill()
                    elseif a.action=="resurrect" and e:HasTag("playerghost") then e.rezsource="dst-admin-go";e:PushEvent("respawnfromghost") end
                end,function()return a.action=="restore" and healed(e) or a.action=="kill" and (e:HasTag("playerghost") or e.components.health:IsDead()) or a.action=="resurrect" and not e:HasTag("playerghost")end)
            end;return out
        elseif id=="set_world_day" then
            local before=world.state.cycles;local delta=a.day-1-before
            if delta<0 or delta>200 then error("WORLD_DAY_OUT_OF_RANGE") end
            return one(world,function()return world.state.cycles==before end,function()for _=1,delta do world:PushEvent("ms_nextcycle")end end,function()return world.state.cycles==a.day-1 end,before+1,a.day)
        elseif id=="set_nightmare_segments" then
            local clock=world.components.nightmareclock or world.net and world.net.components.nightmareclock
            if not clock or not clock.OnSave then error("NIGHTMARE_CLOCK_UNAVAILABLE") end
            return one(world,same,function()world:PushEvent("ms_setnightmaresegs",{calm=a.calm,warn=a.warn,wild=a.wild,dawn=a.dawn})end,function()local v=clock:OnSave().lengths;return v.calm==a.calm and v.warn==a.warn and v.wild==a.wild and v.dawn==a.dawn end)
        elseif id=="remove_world_roads" then
            if type(Roads)~="table" then error("ROADS_UNAVAILABLE") end
            local before=Roads;local startedGeneration=generation
            if not ShardGameIndex or not ShardGameIndex.SaveCurrent or not world.DoStaticTaskInTime then error("WORLD_SAVE_UNAVAILABLE")end
            return one(world,function()return Roads==before end,function()
                Roads={}
                -- Save completion, not a guessed download/timer delay, permits reload.
                ShardGameIndex:SaveCurrent(function()
                    if generation~=startedGeneration or TheWorld~=world or next(Roads)~=nil then return end
                    local task;task=world:DoStaticTaskInTime(1,function()tasks[task]=nil;c_reset()end);tasks[task]=true
                end)
            end,function()return false end,#before,0)
        elseif id=="announce_game_value" then
            local value
            if a.value=="luck" then local c=p.components.luckuser;if not c or not c.GetLuck then error("COMPONENT_UNAVAILABLE") end;value=c:GetLuck()
            elseif a.value=="naughtiness" then local c=world.components.kramped;if not c or not c.GetDebugString then error("COMPONENT_UNAVAILABLE") end
                local target="Player "..tostring(p).." - ";local dump=c:GetDebugString();if #dump>32768 then error("BATCH_OUTPUT_LIMIT") end
                for line in dump:gmatch("[^\n]+")do if line:sub(1,#target)==target then value=tonumber(line:match("Actions:%s*(%d+)"));break end end
            else local manager=world.components.hermitcrab_relocation_manager;local house=manager and manager:GetPearlsHouse();local c=house and house.components.pearldecorationscore;if c then value=c:GetScore() end end
            if type(value)~="number" then error("VALUE_UNAVAILABLE") end
            local labels={luck="幸运值",naughtiness="淘气值",pearl_decoration="隐士装饰评分"}
            return one(world,same,function()TheNet:Announce(labels[a.value].."："..tostring(value))end,function()return false end)
        elseif id=="move_nearby_creatures" or id=="ripen_giant_crops" then
            local x,_,z=p.Transform:GetWorldPosition();local entities=TheSim:FindEntities(x,0,z,a.radius,nil,{"INLIMBO","FX","player","playerghost"})
            if #entities>512 then error("BATCH_SCAN_LIMIT") end
            if id=="move_nearby_creatures" and not safe(a.x,a.z,false) then error("NO_LANDING_POINT") end
            local out={}
            for _,e in ipairs(entities)do checkpoint(deadline)
                local c=e.components or {}
                if id=="move_nearby_creatures" and c.locomotor and c.health and not c.health:IsDead() then
                    out[#out+1]=row(e,function()return valid(e) and e.components.locomotor==c.locomotor and safe(a.x,a.z,false)end,function()c.locomotor:GoToPoint(Vector3(a.x,0,a.z))end,function()return false end)
                elseif id=="ripen_giant_crops" and e.prefab and e.prefab:match("^farm_plant_") and c.growable and c.farmplantstress and e.plant_def and e.plant_def.product_oversized and c.growable.stage<5 then
                    local stage=c.growable.stage
                    out[#out+1]=row(e,function()return valid(e) and c.growable.stage==stage end,function()
                        local old=e.force_oversized;e.force_oversized=true
                        local ok,err=pcall(function()c.farmplantstress.stress_points=0;for _=1,5 do if c.growable.stage>=5 then break end;c.growable:DoGrowth()end end)
                        e.force_oversized=old;if not ok then error(err) end
                    end,function()return e.is_oversized==true and c.growable.stage==5 end)
                end
                if #out>40 then error("BATCH_TARGET_LIMIT") end
            end;return out
        elseif id=="spawn_special_structure" then
            local x,_,z=p.Transform:GetWorldPosition()
            if a.kind=="wormhole_pair" then
                if not safe(x+5,z,false) or not safe(a.x,a.z,false) or (x+5-a.x)^2+(z-a.z)^2<36 then error("NO_LANDING_POINT") end
                local first,second
                return one(p,function()return safe(x+5,z,false) and safe(a.x,a.z,false)end,function()
                    first=spawn("wormhole",x+5,z,"",p);second=spawn("wormhole",a.x,a.z,"",p)
                    if not first.components.teleporter or not second.components.teleporter then error("COMPONENT_UNAVAILABLE") end
                    first.components.teleporter:Target(second);second.components.teleporter:Target(first)
                end,function()return valid(first) and valid(second) and first.components.teleporter:GetTarget()==second and second.components.teleporter:GetTarget()==first end)
            elseif a.kind=="scrap_daywalker" then
                local c=world.components.forestdaywalkerspawner;local shared=world.shard and world.shard.components.shard_daywalkerspawner
                if not c or not shared or c:HasDaywalker() or world:HasTag("cave") then error("DAYWALKER_UNAVAILABLE")end
                local junk=c.bigjunk
                return one(world,function()return not c:HasDaywalker() and c.bigjunk==junk end,function()
                    shared:SetLocation("forestjunkpile");c.days_to_spawn=0
                    if junk then junk:StartDaywalkerBuried()else c:OnDayChange()end
                end,function()return c:HasDaywalker()end)
            elseif a.kind=="celestial_materials" then
                components(p,"inventory");local out={}
                local structures={{"moon_altar",x+8,z},{"moon_altar_astral",x+14,z},{"moon_altar_cosmic",x+11,z+5}}
                for _,v in ipairs(structures)do
                    local rows=spawnRows(p,v[1],{{x=v[2],z=v[3]}},"",false)
                    out[#out+1]=rows[1]
                end
                for _,v in ipairs({{"moon_altar_idol",1},{"wagpunk_bits",4},{"moonstorm_spark",10},{"moonglass_charged",30},{"moonstorm_static_item",1},{"moonrockseed",1}})do
                    local prefab,count=v[1],v[2];local made={};local before
                    out[#out+1]=row(p,function()return p.components.inventory~=nil end,function()
                        local _,n=p.components.inventory:Has(prefab,0,false);before=n
                        local left=count
                        while left>0 do local item=spawn(prefab,x,z,"",p);local stack=item.components.stackable;local amount=stack and math.min(left,stack.maxsize)or 1;if stack then stack:SetStackSize(amount)end;made[#made+1]=item;p.components.inventory:GiveItem(item);left=left-amount end
                    end,function()
                        if before==nil then return false end
                        local _,n=p.components.inventory:Has(prefab,0,false);local dropped=0
                        for _,item in ipairs(made)do if valid(item)and item.components.inventoryitem and item.components.inventoryitem.owner==nil then dropped=dropped+(item.components.stackable and item.components.stackable:StackSize()or 1)end end
                        return n-before+dropped>=count
                    end)
                end
                return out
            elseif a.kind=="nightmare_werepig" then
                if not world:HasTag("cave")then error("PREFAB_REQUIRES_CAVE")end
                local boss,pillars
                local points={{x=x+6,z=z},{x=x+8,z=z+3},{x=x+4,z=z+3}}
                for _,v in ipairs(points)do if not safe(v.x,v.z,false)then error("NO_LANDING_POINT")end end
                return one(p,function()for _,v in ipairs(points)do if not safe(v.x,v.z,false)then return false end end;return true end,function()
                    boss=spawn("daywalker",x+6,z,"",p);pillars={}
                    for _,v in ipairs(points)do local e=spawn("daywalker_pillar",v.x,v.z,"",p);if not e.SetPrisoner then error("COMPONENT_UNAVAILABLE")end;e:SetPrisoner(boss);pillars[#pillars+1]=e end
                end,function()if not valid(boss)or #pillars~=3 then return false end;for _,e in ipairs(pillars)do if not valid(e)then return false end end;return true end)
            else
                local made
                if not safe(x+6,z,false)then error("NO_LANDING_POINT")end
                return one(p,function()return safe(x+6,z,false)end,function()
                    made=spawn("punchingbag",x+6,z,"",p)
                    if not made.components.health or not made.components.combat then error("COMPONENT_UNAVAILABLE")end
                    local started,last,total=nil,nil,0
                    local listener=function(_,data)
                        if not data or type(data.amount)~="number" or data.amount>=0 then return end
                        local now=GetTime();if not last or now-last>10 then started=now;total=0 end
                        local interval=last and now-last or 0;last=now;total=total-data.amount
                        local dps=now>started and total/(now-started)or 0
                        local text=string.format("伤害 %.1f · 间隔 %.2fs · DPS %.1f",-data.amount,interval,dps)
                        if made.components.talker then made.components.talker:Say(text)end
                        print("[DST-ADMIN-TRAINING] "..text)
                    end
                    if not made.components.talker then made:AddComponent("talker")end
                    made:ListenForEvent("healthdelta",listener);dummies[made]=listener
                end,function()return valid(made)and dummies[made]~=nil end)
            end
        elseif id=="set_wurt_form" then
            components(p,"health","hunger","sanity","skinner");if p.prefab~="wurt" then error("CHARACTER_UNSUPPORTED") end
            local royal=a.form=="royal"
            return one(p,function()return p.prefab=="wurt" and not p:HasTag("playerghost")end,function()
                -- Match the native RoyalUpgrade/RoyalDowngrade component contract.

                local h,u,s=p.components.health,p.components.hunger,p.components.sanity
                local hp,up,sp=h:GetPercent(),u:GetPercent(),s:GetPercent()
                p.royal=royal and true or nil;p.overrideskinmode=royal and "powerup" or nil;p.overrideskinmodebuild=royal and "wurt_stage2" or nil
                h:SetMaxHealth(royal and TUNING.WURT_HEALTH_KINGBONUS or TUNING.WURT_HEALTH)
                u:SetMax(royal and TUNING.WURT_HUNGER_KINGBONUS or TUNING.WURT_HUNGER)
                s:SetMax(royal and TUNING.WURT_SANITY_KINGBONUS or TUNING.WURT_SANITY)
                h:SetPercent(hp);u:SetPercent(up);s:SetPercent(sp);p.components.skinner:SetSkinMode(royal and "powerup" or "normal_skin",royal and "wurt_stage2" or "wurt")
            end,function()return (p.royal==true)==royal end)
        elseif id=="repair_skill_sync" then
            local c=p.components.skilltreeupdater
            if not c or not c.SendFromSkillTreeBlob then error("COMPONENT_UNAVAILABLE") end
            return one(p,function()return p.components.skilltreeupdater==c end,function()c:SendFromSkillTreeBlob(p)end,function()return false end)
        elseif id=="reveal_player_map" then
            local map=world.Map;local explorer=p.player_classified and p.player_classified.MapExplorer
            if not explorer or not explorer.RevealArea or not explorer.IsTileSeeable or not map.GetSize then error("MAP_EXPLORER_UNAVAILABLE")end
            local width,height=map:GetSize()
            if not finite(width)or not finite(height)or width<1 or height<1 or width>1024 or height>1024 then error("BATCH_SCAN_LIMIT")end
            local done,verified=false,false
            return one(p,function()return p.player_classified and p.player_classified.MapExplorer==explorer end,function()
                local tx,ty=0,0;local epoch=generation;local expires=os.time()+4
                local checking=false;local allSeen=true
                local function slice()
                    if generation~=epoch or TheWorld~=world or not valid(p)or p.migration or UserToPlayer(p.userid)~=p or os.time()>=expires then done=true;return end
                    local deadline=os.clock()+.004;local steps=0
                    while tx<width and steps<512 and os.clock()<deadline do
                        local x,y=tx,ty
                        -- Consume each coordinate before the native call. An error
                        -- may have changed exploration and must never replay it.
                        ty=ty+7;if ty>=height then ty=0;tx=tx+7 end;steps=steps+1
                        local tile=map:GetTile(x,y)
                        if tile~=WORLD_TILES.IMPASSABLE and tile~=WORLD_TILES.INVALID then
                            local px,_,pz=map:GetTileCenterPoint(x,y)
                            if checking then
                                if not explorer:IsTileSeeable(x,y)then allSeen=false end
                            else explorer:RevealArea(px,0,pz)end
                        end
                    end
                    if tx>=width then
                        if checking then done=true;verified=allSeen;return end
                        -- The native explorer may apply visibility on the next
                        -- frame. Read back once after all writes, never replay
                        -- RevealArea when a receipt remains uncertain.
                        checking=true;tx=0;ty=0
                    end
                    local task;task=world:DoStaticTaskInTime(1/30,function()
                        tasks[task]=nil
                        local ok=pcall(slice);if not ok then done=true end
                    end);tasks[task]=true
                end
                slice()
            end,function()return done and verified end)
        elseif id=="set_player_mode" or id=="set_mode_memory" then
            local modes=DSTAdmin and DSTAdmin.Commands and DSTAdmin.Commands.ManagedModes
            if not modes then error("WORKBENCH_UPDATE_REQUIRED") end
            return one(p,function()return valid(p)end,function()
                if id=="set_player_mode" then modes.Set(p,a.mode,a.enabled)else modes.Memory(p,a.enabled)end
            end,function()return id=="set_player_mode" and modes.Status(p,a.mode)==a.enabled or id=="set_mode_memory" and modes.MemoryVerified(p,a.enabled)end)
        end
        error("WORKBENCH_ACTION_UNKNOWN")
    end
    local function output(id,request)
        return {version=2,action=id,requestId=request,worldId=tostring(TheWorld.GUID),state="preview",matched=0,attempted=0,verified=0,unconfirmed=0,skipped=0,failed=0,units=0,items={},detailsComplete=true}
    end
    function M.Run(id,a,phase,token,fresh,caller,request)
        if type(caller)~="string" or #caller~=64 or type(request)~="string" or #request~=36 then error("BATCH_CALLER_REQUIRED") end
        local copied={};for k,v in pairs(a)do if type(k)~="string" or (type(v)~="string" and type(v)~="number" and type(v)~="boolean")then error("BATCH_ARGUMENT_INVALID")end;copied[k]=v end;a=copied
        local now=os.time();for key,v in pairs(plans)do if v.expires<=now then plans[key]=nil end end
        if id=="count_world_entities" then
            if WorkbenchV2.IsBusy()then error("BATCH_BUSY")end
            WorkbenchV2.ReserveLegacyPreview(fresh,caller);WorkbenchV2.ReleaseLegacyPreview(fresh)
            local value={_count=true,requestId=request,prefab=a.prefab,entities=0,quantity=0,includeInventory=a.include_inventory,complete=false}
            local work=scanWork(a.prefab,a.include_inventory)
            local function read()
                local done,found,units=work()
                if not done then return nil end
                value.entities=#found;value.quantity=units;value.complete=true;return value
            end
            if not read()then WorkbenchV2.ObserveExternal(read)end
            print(countLine(value));return value
        end
        local p=player(a.player_id);local out=output(id,request);local deferred=false;local feedbackSent=false
        local function feedback()
            if feedbackSent or out.verified==0 then return end;feedbackSent=true
            local message="面板操作已核对："..tostring(out.verified).." / "..tostring(out.matched)
            if a.feedback=="world_chat"then pcall(TheNet.Announce,TheNet,message)
            elseif a.feedback=="nearby_speech"and valid(p)and p.components.talker then pcall(p.components.talker.Say,p.components.talker,message)end
        end
        local function verifiedUnits(v,r)
            r.units=v.units or 0;r.destination=v.destination and v.destination()or "none"
            out.units=out.units+r.units
        end
        local signature=require("json").encode_compliant(a)
        -- JSON key order is not a contract; compare validated scalar arguments.
        local function sameArgs(b)for k,v in pairs(a)do if b[k]~=v then return false end end;for k,v in pairs(b)do if a[k]~=v then return false end end;return true end
        if phase=="preview" then
            if WorkbenchV2.IsBusy()then error("BATCH_BUSY")end
            WorkbenchV2.ReserveLegacyPreview(fresh,caller)
            local function prepare(scanned,units)
                local ok,rows=pcall(plan,id,a,p,os.clock()+.004,scanned,units)
                if not ok then WorkbenchV2.ReleaseLegacyPreview(fresh);error(rows)end
                if #rows>40 then WorkbenchV2.ReleaseLegacyPreview(fresh);error("BATCH_TARGET_LIMIT")end
                for _,v in ipairs(rows)do if WorkbenchV2.IsBlocked(v.entity)then WorkbenchV2.ReleaseLegacyPreview(fresh);error("BATCH_PREVIOUS_UNCONFIRMED")end end
                local x,_,z=0,0,0;if p then x,_,z=p.Transform:GetWorldPosition()end
                plans[fresh]={caller=caller,world=TheWorld,player=p,args=a,id=id,session=TheWorld.meta and TheWorld.meta.session_identifier,rows=rows,expires=now+30,signature=signature}
                out.previewId=fresh;out.expiresIn=30;out.observedAt=now;out.expiresAt=now+30;out.x=x;out.z=z;out.matched=#rows
                for i,v in ipairs(rows)do out.items[i]={prefab=v.prefab,state="prepared",destination="none",units=0,before=v.before,after=v.after}end
                return out
            end
            if id=="remove_world_entities"or id=="announce_entity_count"or id=="cycle_entity_teleport"then
                local work=scanWork(a.prefab,id=="announce_entity_count"or a.include_inventory==true)
                local function read()
                    local done,found,units=work();if not done then return nil end
                    return prepare(found,units)
                end
                if not read()then WorkbenchV2.ObserveExternal(read)end
            else prepare()end
        else
            if phase~="execute" and phase~="cancel" then error("BATCH_PHASE_INVALID") end
            local prepared=plans[token]
            if not prepared or prepared.expires<=now or prepared.caller~=caller or prepared.world~=TheWorld or prepared.player~=p or prepared.session~=(TheWorld.meta and TheWorld.meta.session_identifier) or prepared.id~=id or not sameArgs(prepared.args) then error("BATCH_PREVIEW_EXPIRED") end
            plans[token]=nil;WorkbenchV2.ReleaseLegacyPreview(token);out.matched=#prepared.rows
            if phase=="cancel" then out.state="cancelled"
            else
                WorkbenchV2.BeginExternal()
                local index=1;local stopAt=os.time()+4;local startedGeneration=generation
                out.state="partial";out.skipped=#prepared.rows
                for i,v in ipairs(prepared.rows)do out.items[i]={prefab=v.prefab,state="skipped",reason="CPU_BUDGET",destination="none",units=0,before=v.before,after=v.after}end
                local function step()
                    if generation~=startedGeneration then return end
                    local deadline=os.clock()+.004
                    local ok,err=pcall(function()
                        while index<=#prepared.rows and os.clock()<=deadline do
                            local i=index;local v=prepared.rows[i];index=index+1
                            local r=out.items[i];out.skipped=out.skipped-1;r.reason=nil
                            local checked,accept=pcall(function()return valid(v.entity) and not WorkbenchV2.IsBlocked(v.entity) and (not p or valid(p) and not p.migration and p==UserToPlayer(p.userid)) and v.check()end)
                            if not checked or not accept then r.reason="TARGET_CHANGED"
                            else
                                out.attempted=out.attempted+1
                                local applied=pcall(v.apply);v.applied=applied
                                local observed,verified=false,false;if applied then observed,verified=pcall(v.verify)end
                                r.state=observed and verified==true and "verified" or "unconfirmed"
                                if r.state=="unconfirmed"then r.reason="EFFECT_UNCONFIRMED"else verifiedUnits(v,r)end
                            end
                            out[r.state]=out[r.state]+1
                        end
                    end)
                    if not ok then error(err)end
                    out.state=out.verified==out.matched and "completed"or "partial"
                end
                local ok,err=pcall(step);WorkbenchV2.EndExternal();if not ok then error(err)end
                if index<=#prepared.rows or out.unconfirmed>0 then
                    deferred=true
                    WorkbenchV2.ObserveExternal(function()
                        if generation~=startedGeneration then return out end
                        if os.time()<stopAt and index<=#prepared.rows then WorkbenchV2.ContinueExternal(step)end
                        local deadline=os.clock()+.004
                        for i,v in ipairs(prepared.rows)do
                            local r=out.items[i]
                            if os.clock()>deadline then break end
                            if r.state=="unconfirmed" and v.applied and valid(v.entity) then
                                local observed,verified=pcall(v.verify)
                                if observed and verified==true then r.state="verified";r.reason=nil;out.unconfirmed=out.unconfirmed-1;out.verified=out.verified+1;verifiedUnits(v,r)end
                            end
                        end
                        out.state=out.verified==out.matched and "completed"or "partial"
                        if index>#prepared.rows and out.unconfirmed==0 or os.time()>=stopAt then feedback();return out end
                        return nil
                    end)
                end
            end
        end
        if phase=="execute"and not deferred then feedback()end
        print(WorkbenchV2.Encode(out));return out
    end
    function M.Stop()
        generation=generation+1
        plans={}
        for task in pairs(tasks)do task:Cancel()end;tasks={}
        for e,fn in pairs(dummies)do if valid(e)then e:RemoveEventCallback("healthdelta",fn)end end
    end
    return M
end)()
