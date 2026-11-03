-- Fixed adapters; never evaluate directory metadata or caller supplied code.
local NearbyAdapters = (function()
    local M = {}
    local plants={grass="cutgrass",sapling="twigs",sapling_moon="twigs",berrybush="berries",berrybush2="berries",reeds="cutreeds"}
    local crops={carrot=true,corn=true,pumpkin=true,eggplant=true,durian=true,pomegranate=true,dragonfruit=true,watermelon=true,tomato=true,potato=true,asparagus=true,onion=true,garlic=true,pepper=true}
    local function native(c,name,method)
        return c and type(c[method])=="function" and c[method] == require("components/"..name)[method]
    end
    local function stack(e) return e.components.stackable and e.components.stackable:StackSize() or 1 end
    local function ground(e)
        local c=e.components.inventoryitem
        return c and c.canbepickedup and c.cangoincontainer and not c.owner and not c:IsHeld()
            and not (e.entity and e.entity:GetParent()) and not e:HasTag("trap") and not e:HasTag("mine")
    end
    -- Choose one main inventory slot. Do not route into another player's chest,
    -- specialized storage or recursively walk containers. One bounded source split
    -- preserves partial stacks on the ground instead of dropping them elsewhere.
    local function slotFor(inv,e)
        if not native(inv,"inventory","GiveItem") or type(inv.maxslots)~="number" or inv.maxslots>80 then return nil end
        for i=1,inv.maxslots do
            local item=inv.itemslots[i]
            if item and item.components.stackable and e.components.stackable
                and native(item.components.stackable,"stackable","Put")
                and item.components.stackable:CanStackWith(e) then
                local n=item.components.stackable:RoomLeft()
                if n>0 then return i,math.min(n,stack(e)),item end
            end
        end
        for i=1,inv.maxslots do
            if not inv.itemslots[i] and inv:CanTakeItemInSlot(e,i) then return i,stack(e) end
        end
    end
    local function plant(e)
        local c=e.components.pickable
        return plants[e.prefab] and c and c.product==plants[e.prefab] and c.numtoharvest==1
            and not c.droppicked and not c.use_lootdropper_for_product and not c.remove_when_picked
            and native(c,"pickable","Pick") and native(c,"pickable","FinishGrowing") and native(c,"pickable","Fertilize") and c
    end
    function M.Plan(e,action,p)
        local c=e.components or {}
        if action=="pick" then
            if not ground(e) or not p.components.inventory or not native(c.inventoryitem,"inventoryitem","OnPickup")
                or c.inventoryitem.onpickupfn then return nil end
            if c.stackable and (not native(c.stackable,"stackable","Get") or not native(c.stackable,"stackable","StackSize")) then return nil end
            local slot,n,receiver=slotFor(p.components.inventory,e)
            return {component=c.inventoryitem,count=stack(e),slot=slot,units=n or 0,receiver=receiver,steps=1,spawns=1,destination="inventory"}
        elseif action=="repair" then
            if not ground(e) then return nil end
            local parts={}
            for _,name in ipairs({"finiteuses","armor"}) do
                local v=c[name]
                if native(v,name,"SetPercent") and native(v,name,"GetPercent") and v:GetPercent()<1 then parts[#parts+1]={name=name,ref=v,before=v:GetPercent()} end
            end
            if #parts>0 then return {parts=parts,steps=#parts,units=0,spawns=0,destination="none"} end
        elseif action=="fertilize" or action=="grow" or action=="ripen" or action=="harvest" then
            if e:HasTag("burnt") or e:HasTag("burning") then return nil end
            local g=c.growable
            local crop=e.prefab and e.prefab:match("^farm_plant_(.+)$")
            if (action=="grow" or action=="ripen") and crops[crop] and native(g,"growable","DoGrowth") and native(g,"growable","SetStage")
                and type(g.stage)=="number" and g.stage>=1 and g.stage<5 and g.stage%1==0 and g.stages and #g.stages==6
                and not g.growonly and (g.targettime or g.pausedremaining) then
                for i,name in ipairs({"seed","sprout","small","med","full","rotten"}) do if not g.stages[i] or g.stages[i].name~=name then return nil end end
                return {component=g,crop=true,before=g.stage,after=action=="ripen" and 5 or g.stage+1,steps=action=="ripen" and 5-g.stage or 1,units=0,spawns=0,destination="none"}
            end
            local v=plant(e);if not v then return nil end
            if action=="fertilize" and v:CanBeFertilized() then
                return {component=v,before=v.cycles_left,steps=1,units=0,spawns=1,destination="none"}
            elseif (action=="grow" or action=="ripen") and not v:CanBePicked() and not v:CanBeFertilized()
                and not v.paused and not e:HasTag("withered") and (v.task or v.useexternaltimer and v.regentimerexists(e)) then
                return {component=v,steps=1,units=0,spawns=0,destination="none"}
            elseif action=="harvest" and v:CanBePicked() and v.caninteractwith and not v.stuck then
                return {component=v,steps=1,units=1,spawns=1,destination="inventory"}
            end
        elseif action=="repair_burnt" then
            -- Only simple native research stations. Burnt structures have lost
            -- components/data: this is a rebuild, never recovery of lost contents.
            if (e.prefab=="researchlab" or e.prefab=="researchlab2") and e:HasTag("burnt")
                and not c.container and not c.childspawner and not c.stewer and not c.prototyper
                and e.Transform.GetRotation and e.Remove==EntityScript.Remove then
                local x,y,z=e.Transform:GetWorldPosition()
                return {steps=1,units=0,spawns=1,destination="rebuild",rotation=e.Transform:GetRotation(),skin=e.skinname,x=x,y=y,z=z}
            end
        end
    end
    function M.Same(e,action,p,old)
        local v=M.Plan(e,action,p)
        if not v or v.component~=old.component or v.count~=old.count or v.units>old.units or v.spawns>old.spawns or v.steps>old.steps then return false end
        if old.parts then
            if #v.parts~=#old.parts then return false end
            for i,part in ipairs(old.parts) do if v.parts[i].ref~=part.ref or v.parts[i].before~=part.before then return false end end
        end
        return v.rotation==old.rotation and v.skin==old.skin and v.before==old.before and v.x==old.x and v.y==old.y and v.z==old.z
    end
    function M.Apply(e,action,p,plan,deadline)
        local c=e.components
        if action=="pick" then
            local inv=p.components.inventory;local slot,n,dest=slotFor(inv,e)
            if not slot or n<1 then return "skipped","INVENTORY_FULL",0 end
            if n>plan.units or dest~=plan.receiver then return "skipped","TARGET_CHANGED",0 end
            local before=stack(e);local destBefore=dest and stack(dest) or 0
            -- Clear stale pickup routing on the bounded source, not on unrelated
            -- inventory objects. The native slot path is used after the split.
            local item=e
            if n<before then item=c.stackable:Get(n) end
            item.prevslot=nil;item.prevcontainer=nil
            inv:GiveItem(item,slot)
            local receiver=inv.itemslots[slot]
            local received=dest and receiver==dest and stack(dest)-destBefore==n and not item:IsValid()
                or not dest and receiver==item and item:IsValid() and item.components.inventoryitem.owner==p and stack(item)==n
            local consumed=n==before and (not e:IsValid() or e.components.inventoryitem.owner==p)
                or n<before and e:IsValid() and ground(e) and stack(e)==before-n
            return received and consumed and "verified" or "unconfirmed","",received and consumed and n or 0
        elseif action=="repair" then
            for _,part in ipairs(plan.parts) do part.ref:SetPercent(1) end
            for _,part in ipairs(plan.parts) do if part.ref:GetPercent()~=1 then return "unconfirmed","EFFECT_UNCONFIRMED",0 end end
            return "verified","",0
        elseif action=="fertilize" then
            local f=SpawnPrefab("compostwrap");if not f then return "failed","MATERIAL_UNAVAILABLE",0 end
            local ok,err=pcall(function()plan.component:Fertilize(f,p)end)
            local cleaned=pcall(function()if f:IsValid() then f:Remove() end end)
            if not ok or not cleaned then return "unconfirmed","EFFECT_UNCONFIRMED",0 end
            return not plan.component:CanBeFertilized() and "verified" or "unconfirmed","",0
        elseif action=="grow" or action=="ripen" then
            if plan.crop then
                deadline=deadline or os.clock()+.004
                for i=1,plan.steps do
                    if os.clock()>deadline or not e:IsValid() or plan.component.stage~=plan.before+i-1 then return "unconfirmed","EFFECT_UNCONFIRMED",0 end
                    plan.component:DoGrowth()
                end
                local ready=e:IsValid() and c.pickable and c.pickable:CanBePicked()
                return e:IsValid() and plan.component.stage==plan.after and (action~="ripen" or ready) and "verified" or "unconfirmed","",0
            end
            plan.component:FinishGrowing()
            return plan.component:CanBePicked() and "verified" or "unconfirmed","",0
        elseif action=="harvest" then
            local inv=p.components.inventory
            if not inv or not native(inv,"inventory","GiveItem") then return "skipped","INVENTORY_UNAVAILABLE",0 end
            -- Probe the native product's capacity without spawning an item. This
            -- first adapter requires an empty main slot; full/stack-only cases
            -- wait for a new preview instead of risking an unbounded loot route.
            local hasRoom=false
            if type(inv.maxslots)~="number" or inv.maxslots>80 then return "skipped","INVENTORY_UNAVAILABLE",0 end
            for i=1,inv.maxslots do if not inv.itemslots[i] then hasRoom=true;break end end
            if not hasRoom then return "skipped","INVENTORY_FULL",0 end
            local success,loot=plan.component:Pick(p)
            local item=type(loot)=="table" and loot[1]
            if success and not plan.component:CanBePicked() and item and item:IsValid()
                and item.prefab==plants[e.prefab] and item.components.inventoryitem and item.components.inventoryitem.owner==p and stack(item)==1 then
                return "verified","",1
            end
            return "unconfirmed","OUTPUT_UNCONFIRMED",0
        elseif action=="repair_burnt" then
            local x,y,z=e.Transform:GetWorldPosition()
            local replacement=SpawnPrefab(e.prefab,plan.skin)
            if not replacement then return "failed","REBUILD_UNAVAILABLE",0 end
            -- Cleanup is itself observable, and can fail in a mod callback. A
            -- surviving replacement must never be reported as a clean failure.
            local function discard()
                local ok=pcall(function()if replacement:IsValid() then replacement:Remove() end end)
                if replacement:IsValid() then plan.uncertainReplacement=replacement end
                return ok and not replacement:IsValid()
            end
            local ok=pcall(function()
                assert(replacement:IsValid() and replacement.prefab==e.prefab and replacement.skinname==plan.skin
                    and replacement.components.prototyper and replacement.components.burnable and not replacement:HasTag("burnt"))
                replacement.Transform:SetPosition(x,y,z);replacement.Transform:SetRotation(plan.rotation)
                local nx,ny,nz=replacement.Transform:GetWorldPosition()
                assert(nx==x and ny==y and nz==z and replacement.Transform:GetRotation()==plan.rotation)
            end)
            if not ok then
                if discard() then return "failed","REBUILD_UNAVAILABLE",0 end
                return "unconfirmed","REBUILD_UNCONFIRMED",0
            end
            -- Retain the original until replacement construction has succeeded.
            local removed=pcall(function()e:Remove()end)
            if e:IsValid() then discard();return "unconfirmed","REBUILD_UNCONFIRMED",0 end
            if not removed then plan.uncertainReplacement=replacement;return "unconfirmed","REBUILD_UNCONFIRMED",0 end
            return replacement:IsValid() and not replacement:HasTag("burnt") and "verified" or "unconfirmed","",0
        end
        return "failed","UNSUPPORTED",0
    end
    return M
end)()
