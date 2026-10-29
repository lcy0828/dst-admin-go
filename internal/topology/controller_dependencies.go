package topology

import "dont/internal/rooms"

// RemoteDeploymentRooms reads persisted placement and catalog state without
// probing Agents, changing topology, or treating offline worlds as removed.
// Applied and desired placements both matter when disabling the Controller.
func (s *Service) RemoteDeploymentRooms() ([]rooms.Room, error) {
	items, err := s.rooms.List()
	if err != nil {
		return nil, err
	}
	records, err := s.store.Records()
	if err != nil {
		return nil, err
	}
	byRoom := make(map[string]map[string]storedPlacement, len(records))
	for _, record := range records {
		worlds := make(map[string]storedPlacement, len(record.Placements))
		for _, placement := range record.Placements {
			worlds[placement.WorldID] = placement
		}
		byRoom[record.RoomID] = worlds
	}
	dependent := make([]rooms.Room, 0)
	for _, room := range items {
		worlds, err := s.rooms.Worlds(room.ID)
		if err != nil {
			return nil, err
		}
		remote := len(worlds) == 0 && hasRemoteTarget(room.TargetIDs)
		for _, world := range worlds {
			if placement, exists := byRoom[room.ID][world.ID]; exists {
				remote = hasRemoteTarget([]string{placement.AppliedTargetID, placement.DesiredTargetID})
			} else {
				remote = hasRemoteTarget(world.TargetIDs)
			}
			if remote {
				break
			}
		}
		if remote {
			dependent = append(dependent, room)
		}
	}
	return dependent, nil
}

func hasRemoteTarget(ids []string) bool {
	for _, id := range ids {
		if id != "" && id != localTargetID {
			return true
		}
	}
	return false
}
