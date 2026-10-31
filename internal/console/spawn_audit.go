package console

import (
	"math"
	"strconv"
	"strings"
)

const spawnAuditPrefix = "[DST-ADMIN-SPAWN-V1] "
const maxSpawnRecords = 40

// WorldPosition is an observed game coordinate, not a saved position or a
// promise that an entity remains there. Zero is a valid coordinate.
type WorldPosition struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type SpawnRecord struct {
	Prefab      string         `json:"prefab"`
	Quantity    int            `json:"quantity"`
	Destination string         `json:"destination"`
	EntityGUID  string         `json:"entityGuid,omitempty"`
	Position    *WorldPosition `json:"position,omitempty"`
}

// SpawnAudit is derived exclusively from this command's persisted print
// receipt. It never reads player snapshots or attempts to find entities later.
// GUIDs identify a live instance only; they are not stable across world loads.
type SpawnAudit struct {
	Status   string         `json:"status"`
	PlayerID string         `json:"playerId"`
	Origin   *WorldPosition `json:"origin,omitempty"`
	Records  []SpawnRecord  `json:"records"`
	Total    *int           `json:"total,omitempty"`
	Omitted  int            `json:"omitted"`
}

// Instrument only our own templates. Do not replace SpawnPrefab/print globally
// or rewrite administrator/mod code. Recording failure must not fail an action.
// Bounded output uses the existing receipt transport, without polling or a new
// Runtime version. Full/merged inventories must not invent a ground position.
const spawnAuditLua = `local __dst_n=0;local function __dst_record(e,p,d,q) __dst_n=__dst_n+1;if __dst_n>40 then return end;pcall(function() local function xyz(v) if v and v.Transform then local x,y,z=v.Transform:GetWorldPosition();return tostring(x),tostring(y),tostring(z) end;return "?","?","?" end;if __dst_n==1 then local x,y,z=xyz(p);print("[DST-ADMIN-SPAWN-V1] ORIGIN\t"..x.."\t"..y.."\t"..z) end;local x,y,z="?","?","?";if d=="inventory" then if e.IsValid and not e:IsValid() then d="unresolved" elseif e.components and e.components.inventoryitem and e.components.inventoryitem.owner then d="inventory" elseif e.HasTag and e:HasTag("INLIMBO") then d="unresolved" else d="world" end end;if d=="world" then x,y,z=xyz(e);if x=="?" then d="unresolved" end end;print("[DST-ADMIN-SPAWN-V1] ITEM\t"..tostring(e.prefab or "?").."\t"..tostring(e.GUID or "").."\t"..d.."\t"..tostring(q or 1).."\t"..x.."\t"..y.."\t"..z) end) end;`

func withSpawnAudit(script string) string {
	return spawnAuditLua + script + `;pcall(function() print("[DST-ADMIN-SPAWN-V1] END\t"..__dst_n) end)`
}

func spawnAudit(run Run) *SpawnAudit {
	if run.Mode != "builtin" || run.Status == RunSending {
		return nil
	}
	switch run.CommandID {
	case "spawn_entity", "spawn_player_entity", "spawn_domesticated_beefalo", "give_item", "give_blueprint", "give_recipe_materials":
	default:
		return nil
	}
	playerID, _ := run.Arguments["player_id"].(string)
	audit := &SpawnAudit{Status: "unavailable", PlayerID: playerID, Records: []SpawnRecord{}}
	if run.Output == nil || !strings.HasPrefix(run.Script, spawnAuditLua) {
		return audit
	}
	text := run.Output.Text
	partial := run.Output.Truncated
	if len(text) > 16<<10 {
		text, partial = text[:16<<10], true
	}
	seen, originSeen := 0, false
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, spawnAuditPrefix) {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(line, spawnAuditPrefix), "\t")
		switch parts[0] {
		case "ORIGIN":
			if originSeen || audit.Total != nil || len(parts) != 4 {
				partial = true
				continue
			}
			originSeen = true
			audit.Origin = parsePosition(parts[1:])
		case "ITEM":
			seen++
			if audit.Total != nil || len(audit.Records) >= maxSpawnRecords {
				partial = true
				continue
			}
			record, ok := parseSpawnRecord(parts)
			if !ok {
				partial = true
				continue
			}
			audit.Records = append(audit.Records, record)
		case "END":
			if len(parts) != 2 || audit.Total != nil {
				partial = true
				continue
			}
			total, err := strconv.Atoi(parts[1])
			if err != nil || total < 0 || total > 1_000_000 {
				partial = true
				continue
			}
			audit.Total = &total
		default:
			partial = true
		}
	}
	if audit.Total != nil {
		if *audit.Total < seen || seen != min(*audit.Total, maxSpawnRecords) {
			partial = true
		}
		audit.Omitted = max(0, *audit.Total-len(audit.Records))
		partial = partial || audit.Omitted > 0
	}
	if audit.Total != nil && !partial {
		audit.Status = "complete"
	} else if originSeen || seen > 0 || audit.Total != nil {
		audit.Status = "partial"
	}
	return audit
}

func parseSpawnRecord(parts []string) (SpawnRecord, bool) {
	if len(parts) != 8 || !prefabPattern.MatchString(parts[1]) {
		return SpawnRecord{}, false
	}
	quantity, err := strconv.Atoi(parts[4])
	if err != nil || quantity < 1 || quantity > 1_000_000 {
		return SpawnRecord{}, false
	}
	record := SpawnRecord{Prefab: parts[1], Destination: parts[3], Quantity: quantity}
	if parts[2] != "" {
		if _, err := strconv.ParseUint(parts[2], 10, 53); err != nil {
			return SpawnRecord{}, false
		}
		record.EntityGUID = parts[2]
	}
	switch record.Destination {
	case "world":
		record.Position = parsePosition(parts[5:])
		if record.Position == nil {
			return SpawnRecord{}, false
		}
	case "inventory", "unresolved":
		// Inventory transfers and consumed/merged source entities have no
		// independently confirmed ground position.
	default:
		return SpawnRecord{}, false
	}
	return record, true
}

func parsePosition(parts []string) *WorldPosition {
	if len(parts) != 3 {
		return nil
	}
	var values [3]float64
	for index, part := range parts {
		value, err := strconv.ParseFloat(part, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 1e9 {
			return nil
		}
		values[index] = value
	}
	return &WorldPosition{X: values[0], Y: values[1], Z: values[2]}
}
