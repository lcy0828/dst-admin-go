package gameupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/internal/topology"
)

type releaseRoomCatalog interface {
	List() ([]rooms.Room, error)
	Worlds(string) ([]rooms.World, error)
}

type releasePlacementReader interface {
	AppliedPlacement(string, string) (topology.ExecutionPlacement, error)
}

type releaseTargetCatalog interface {
	RuntimeTargetInventories(context.Context) ([]agents.RuntimeTargetInventory, error)
}

type TopologyReleaseSnapshot struct {
	rooms      releaseRoomCatalog
	placements releasePlacementReader
	targets    releaseTargetCatalog
}

func NewTopologyReleaseSnapshot(roomCatalog releaseRoomCatalog, placements releasePlacementReader, targets releaseTargetCatalog) (*TopologyReleaseSnapshot, error) {
	if roomCatalog == nil || placements == nil || targets == nil {
		return nil, ErrReleaseInvalid
	}
	return &TopologyReleaseSnapshot{rooms: roomCatalog, placements: placements, targets: targets}, nil
}

func (s *TopologyReleaseSnapshot) Snapshot(ctx context.Context) (ReleasePlacementSnapshot, error) {
	roomValues, err := s.rooms.List()
	if err != nil {
		return ReleasePlacementSnapshot{}, err
	}
	inventories, err := s.targets.RuntimeTargetInventories(ctx)
	if err != nil {
		return ReleasePlacementSnapshot{}, err
	}
	inventoryByEndpoint := make(map[string]agents.RuntimeTargetInventory, len(inventories))
	inventoriesByTarget := make(map[string][]agents.RuntimeTargetInventory)
	for _, inventory := range inventories {
		inventoryByEndpoint[releaseSnapshotEndpointKey(inventory.Target.ID, releaseSnapshotInstallationID(inventory))] = inventory
		targetID := strings.TrimSpace(inventory.Target.ID)
		inventoriesByTarget[targetID] = append(inventoriesByTarget[targetID], inventory)
	}
	sort.Slice(roomValues, func(i, j int) bool { return roomValues[i].ID < roomValues[j].ID })
	result := ReleasePlacementSnapshot{}
	revisions := make([]string, 0)
	for _, room := range roomValues {
		if !room.Managed {
			continue
		}
		if err := ctx.Err(); err != nil {
			return ReleasePlacementSnapshot{}, err
		}
		worlds, worldErr := s.rooms.Worlds(room.ID)
		if worldErr != nil {
			return ReleasePlacementSnapshot{}, worldErr
		}
		sort.Slice(worlds, func(i, j int) bool { return worlds[i].ID < worlds[j].ID })
		roomRevision := ""
		for _, world := range worlds {
			placement, placementErr := s.placements.AppliedPlacement(room.ID, world.ID)
			if placementErr != nil {
				return ReleasePlacementSnapshot{}, placementErr
			}
			if roomRevision == "" {
				roomRevision = placement.Revision
			} else if roomRevision != placement.Revision {
				return ReleasePlacementSnapshot{}, ErrReleaseTopologyChanged
			}
			inventory, installationID, available := releaseSnapshotInventory(
				placement.AppliedTargetID, placement.AppliedInstallationID, inventoryByEndpoint, inventoriesByTarget,
			)
			target := inventory.Target
			if !available {
				target = agents.RuntimeTarget{
					ID: placement.AppliedTargetID, Name: placement.AppliedTargetID,
					Config: agents.RuntimeConfig{InstallationID: installationID},
				}
			}
			result.Shards = append(result.Shards, ReleaseShardSnapshot{
				Room: room, World: world, Target: target, InventoryAvailable: available && inventory.Available,
				InventoryStale: !available || inventory.Stale, InventoryHasShard: available && inventoryContainsReleaseShard(inventory, room.DirectoryName, world.DirectoryName),
				TopologyRevision: placement.Revision,
			})
		}
		if roomRevision != "" {
			revisions = append(revisions, room.ID+"\x00"+roomRevision)
		}
	}
	if len(result.Shards) == 0 {
		return ReleasePlacementSnapshot{}, errors.Join(ErrReleaseInvalid, errors.New("no managed DST shards"))
	}
	sort.Strings(revisions)
	digest := sha256.Sum256([]byte(strings.Join(revisions, "\x00")))
	result.TopologyRevision = hex.EncodeToString(digest[:])
	return result, nil
}

func releaseSnapshotInstallationID(inventory agents.RuntimeTargetInventory) string {
	for _, value := range []string{
		inventory.Target.Config.InstallationID,
		inventory.Inventory.Installation.ID,
		inventory.Target.DefaultInstallationID,
	} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "default"
}

func releaseSnapshotEndpointKey(targetID, installationID string) string {
	return strings.TrimSpace(targetID) + "\x00" + strings.TrimSpace(installationID)
}

func releaseSnapshotInventory(
	targetID, installationID string,
	byEndpoint map[string]agents.RuntimeTargetInventory,
	byTarget map[string][]agents.RuntimeTargetInventory,
) (agents.RuntimeTargetInventory, string, bool) {
	targetID, installationID = strings.TrimSpace(targetID), strings.TrimSpace(installationID)
	if installationID != "" {
		if inventory, ok := byEndpoint[releaseSnapshotEndpointKey(targetID, installationID)]; ok {
			return inventory, releaseSnapshotInstallationID(inventory), true
		}
		if installationID != "default" {
			return agents.RuntimeTargetInventory{}, installationID, false
		}
	}
	candidates := byTarget[targetID]
	if len(candidates) == 0 {
		return agents.RuntimeTargetInventory{}, installationID, false
	}
	selected := candidates[0]
	for _, candidate := range candidates {
		candidateID := releaseSnapshotInstallationID(candidate)
		defaultID := strings.TrimSpace(candidate.Target.DefaultInstallationID)
		if defaultID != "" && candidateID == defaultID {
			selected = candidate
			break
		}
		if candidateID < releaseSnapshotInstallationID(selected) {
			selected = candidate
		}
	}
	return selected, releaseSnapshotInstallationID(selected), true
}

func inventoryContainsReleaseShard(inventory agents.RuntimeTargetInventory, cluster, shard string) bool {
	for _, room := range inventory.Inventory.Rooms {
		if !strings.EqualFold(strings.TrimSpace(room.Directory), strings.TrimSpace(cluster)) {
			continue
		}
		for _, world := range room.Shards {
			if strings.EqualFold(strings.TrimSpace(world.Directory), strings.TrimSpace(shard)) {
				return true
			}
		}
	}
	return false
}
