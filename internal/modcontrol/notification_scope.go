package modcontrol

import (
	"context"
	"sort"

	"dont/internal/mods"
)

// InstallationEnabledModRooms reads the applied configuration once. Disabled
// entries and worlds on another installation are never notified as affected.
func (s *Service) InstallationEnabledModRooms(ctx context.Context, targetID, installationID string, ids []string) (map[string][]string, error) {
	key := targetID + "\x00" + installationID
	ctx = withPublicationSnapshot(ctx, proposal{action: "reconcile", worldIDs: map[string]bool{}, installationOnly: map[string]bool{key: true}, readOnly: true})
	worlds, err := s.source.ManagedWorlds(ctx)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	seen := map[string]map[string]bool{}
	for _, world := range worlds {
		configuration, err := mods.InspectModOverride(world.ModOverrides)
		if err != nil {
			return nil, err
		}
		for _, mod := range configuration.Mods {
			if !mod.Enabled || !wanted[mod.ModID] {
				continue
			}
			if seen[world.RoomID] == nil {
				seen[world.RoomID] = map[string]bool{}
			}
			seen[world.RoomID][mod.ModID] = true
		}
	}
	result := map[string][]string{}
	for room, matches := range seen {
		for id := range matches {
			result[room] = append(result[room], id)
		}
		sort.Strings(result[room])
	}
	return result, nil
}
