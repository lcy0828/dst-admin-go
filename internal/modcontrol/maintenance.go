package modcontrol

import "context"

// AffectedRoomIDs includes rooms sharing the installations whose Mod files are
// about to change. It is called only for an enabled maintenance operation.
func (s *Service) AffectedRoomIDs(ctx context.Context, roomID string) ([]string, error) {
	executions, err := s.source.topology.ResolveRoomExecutions(ctx, roomID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	roomIDs := []string{roomID}
	for _, execution := range executions {
		placement := publicationPlacement(execution, roomID, execution.World.ID)
		key := placement.TargetID + "\x00" + placement.InstallationID
		if seen[key] {
			continue
		}
		seen[key] = true
		values, err := s.InstallationRoomIDs(ctx, placement.TargetID, placement.InstallationID)
		if err != nil {
			return nil, err
		}
		roomIDs = append(roomIDs, values...)
	}
	return uniqueSortedStrings(roomIDs), nil
}
