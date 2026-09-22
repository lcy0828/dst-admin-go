package topology

import (
	"sort"

	"dont/internal/agents"
)

func machineObservations(plans map[string]roomPlan, inventories []agents.RuntimeTargetInventory) []MachineObservation {
	result := make([]MachineObservation, 0, len(inventories))
	for _, inventory := range inventories {
		target := inventory.Target
		item := MachineObservation{TargetID: target.ID, InstallationID: target.Config.InstallationID,
			Available: inventory.Available, Stale: inventory.Stale || !target.Online, StaleReason: inventory.StaleReason,
			ObservedAt: inventory.ObservedAt, Capacity: inventory.Capacity, Memory: inventory.Inventory.Memory, Worlds: []MachineWorld{}}
		for roomID, plan := range plans {
			placements := placementsByWorld(plan.record.Placements)
			for _, world := range plan.worlds {
				placement, ok := placements[world.ID]
				if !ok || !sameEndpoint(placement.AppliedTargetID, placement.AppliedInstallationID, target.ID, target.Config.InstallationID) {
					continue
				}
				value := MachineWorld{RoomID: roomID, RoomName: plan.room.Name, WorldID: world.ID, WorldName: world.Name, Role: string(world.Role), ServerPort: world.ServerPort}
				found := false
				for _, room := range inventory.Inventory.Rooms {
					if identityFor(room.Directory, "").cluster != identityFor(plan.room.DirectoryName, "").cluster {
						continue
					}
					for _, shard := range room.Shards {
						if identityFor(room.Directory, shard.Directory) == identityFor(plan.room.DirectoryName, world.DirectoryName) {
							found = true
							value.ServerPort = shard.ServerPort
						}
					}
				}
				value.Known = inventory.Available && !item.Stale && found
				// Preserve the last observation even while offline; Known gates its use.
				for _, process := range inventory.Inventory.Processes {
					if identityFor(process.Cluster, process.Shard) == identityFor(plan.room.DirectoryName, world.DirectoryName) {
						value.Running = true
						break
					}
				}
				item.Worlds = append(item.Worlds, value)
			}
		}
		sort.Slice(item.Worlds, func(i, j int) bool {
			a, b := item.Worlds[i], item.Worlds[j]
			if a.RoomID != b.RoomID {
				return a.RoomID < b.RoomID
			}
			return a.WorldID < b.WorldID
		})
		result = append(result, item)
	}
	return result
}
