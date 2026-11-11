package console

import (
	"fmt"
	"strings"
	"testing"
)

// Execute the real rendered Lua. The fake inventory models merging (the
// original spawned entity can disappear), full bags and failing mod components.
func TestItemGrantBatchesActualStacksAndBoundsWork(t *testing.T) {
	for _, tc := range []struct {
		name, mode, size     string
		count, stacks, units int
		failure              string
	}{
		{"100 logs", "units", "20", 100, 5, 100, ""},
		{"partial last stack", "units", "40", 101, 3, 101, ""},
		{"max units", "units", "20", 400, 20, 400, ""},
		{"mod stacks", "units", "400", 400, 1, 400, ""},
		{"40 tools", "units", "nil", 40, 40, 40, ""},
		{"too many tools", "units", "nil", 41, 1, 0, "ITEM_ENTITY_BUDGET"},
		{"40 small stacks", "units", "10", 400, 40, 400, ""},
		{"stacks retain meaning", "stacks", "20", 3, 3, 60, ""},
		{"stacks exceed units", "stacks", "20", 21, 21, 0, "ITEM_UNIT_BUDGET"},
		{"zero size", "units", "0", 3, 1, 0, "ITEM_STACK_INVALID"},
		{"fractional size", "units", "1.5", 3, 1, 0, "ITEM_STACK_INVALID"},
		{"infinite size", "units", "math.huge", 3, 1, 0, "ITEM_STACK_INVALID"},
		{"nan size", "units", "0/0", 3, 1, 0, "ITEM_STACK_INVALID"},
		{"large mod stack", "units", "9999", 100, 1, 100, ""},
		{"large mod whole stack", "stacks", "9999", 1, 1, 0, "ITEM_UNIT_BUDGET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, err := builtinTemplates()["give_item"].render(map[string]interface{}{"player_id": "KU_TEST123", "prefab": "log", "count": tc.count, "quantity_mode": tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			setup := fmt.Sprintf(`
 local size=%s;local spawned,removed,received,deliveries=0,0,0,0
 local inv={GiveItem=function(_,e) deliveries=deliveries+1;received=received+(e.components.stackable and e.components.stackable:StackSize()or 1);e.valid=false end}
 AllPlayers={{userid="KU_TEST123",components={inventory=inv}}}
 SpawnPrefab=function(name)
  spawned=spawned+1
  local stack=size and {maxsize=size,SetStackSize=function(self,n)self.n=n end,StackSize=function(self)return self.n end}or nil
  return {prefab=name,GUID=spawned,valid=true,IsValid=function(self)return self.valid end,Remove=function(self)removed=removed+1;self.valid=false end,components={inventoryitem={},stackable=stack}}
 end
 function metrics()print("METRICS",spawned,removed,received,deliveries)end
 `, tc.size)
			output, luaErr := auditLuaOutput(t, `local ok,err=pcall(function() `+script+` end);metrics();if not ok then error(err)end`, setup)
			if tc.failure != "" {
				if luaErr == nil || !strings.Contains(luaErr.Error(), tc.failure) || strings.Contains(output, "[DST-ADMIN-GIVE]") || !strings.Contains(output, fmt.Sprintf("METRICS\t%d\t%d\t0\t0", tc.stacks, tc.stacks)) {
					t.Fatal(output, luaErr)
				}
				return
			}
			if luaErr != nil || !strings.Contains(output, fmt.Sprintf("METRICS\t%d\t0\t%d\t%d", tc.stacks, tc.units, tc.stacks)) || !strings.Contains(output, fmt.Sprintf("[DST-ADMIN-GIVE]\tKU_TEST123\tlog\t%s\t%d", tc.mode, tc.units)) {
				t.Fatal(output, luaErr)
			}
			audit := spawnAudit(auditRun("give_item", output))
			if audit.Status != "complete" || len(audit.Records) != tc.stacks || *audit.Total != tc.stacks {
				t.Fatal(audit)
			}
			units := 0
			for _, record := range audit.Records {
				units += record.Quantity
			}
			if units != tc.units {
				t.Fatal(audit)
			}
		})
	}
}

func TestItemGrantStagingFailureCleansUpWithoutDelivery(t *testing.T) {
	for _, failure := range []string{"spawn", "stack", "inventory"} {
		t.Run(failure, func(t *testing.T) {
			script := withSpawnAudit(renderItemGrant("KU_TEST123", "log", 100, "units"))
			setup := fmt.Sprintf(`
 local failure=%q;local spawned,removed,delivered=0,0,0
 AllPlayers={{userid="KU_TEST123",components={inventory={GiveItem=function()delivered=delivered+1 end}}}}
 SpawnPrefab=function(name)
  spawned=spawned+1
  if spawned==2 and failure=="spawn"then error("MOD_SPAWN_ERROR")end
  local stack={maxsize=20,SetStackSize=function(self,n)self.n=n end,StackSize=function(self)return self.n end}
  if spawned==2 and failure=="stack"then stack.SetStackSize=function()error("MOD_STACK_ERROR")end end
  return {prefab=name,GUID=spawned,components={inventoryitem=not(spawned==2 and failure=="inventory")or nil,stackable=stack},Remove=function()removed=removed+1 end}
 end
 function metrics()print("METRICS",spawned,removed,delivered)end
 `, failure)
			output, err := auditLuaOutput(t, `local ok,err=pcall(function() `+script+` end);metrics();if not ok then error(err)end`, setup)
			removed := 2
			if failure == "spawn" {
				removed = 1
			}
			if err == nil || !strings.Contains(output, fmt.Sprintf("METRICS\t2\t%d\t0", removed)) {
				t.Fatal(output, err)
			}
		})
	}
	for _, args := range []map[string]interface{}{
		{"player_id": "KU_TEST123", "prefab": "log", "count": 401},
		{"player_id": "KU_TEST123", "prefab": "log", "count": 41, "quantity_mode": "stacks"},
	} {
		if _, err := builtinTemplates()["give_item"].render(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}
