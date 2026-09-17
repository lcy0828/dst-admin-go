package modcontrol

import (
	"context"
	"errors"
	"io"
	"sort"

	"dont/internal/mods"
	"dont/internal/rooms"
)

// UpdateRoomMods uses the same node-owned download operation as a single Mod
// update. Each installation gets only Mods enabled in its assigned worlds.
// No Controller content, publication plan or configuration write is involved.
func (s *Service) UpdateRoomMods(ctx context.Context, roomID string, modIDs []string, output io.Writer) (mods.ActionResult, error) {
	if s.installer == nil {
		return mods.ActionResult{}, errors.New("房间模组安装器未配置")
	}
	if output == nil {
		output = io.Discard
	}
	list, err := s.RoomFacts(ctx, roomID)
	if err != nil {
		return mods.ActionResult{}, err
	}
	requested := make(map[string]bool, len(modIDs))
	for _, id := range modIDs {
		requested[id] = true
	}
	byWorld := make(map[string][]string)
	for _, item := range list.Items {
		if !requested[item.ID] || !item.Enabled {
			continue
		}
		for _, worldID := range item.EnabledWorlds {
			byWorld[worldID] = append(byWorld[worldID], item.ID)
		}
	}
	if len(byWorld) == 0 {
		return mods.ActionResult{ModIDs: []string{}}, nil
	}
	executions, err := s.source.topology.ResolveRoomExecutions(ctx, roomID)
	if err != nil {
		return mods.ActionResult{}, err
	}
	groups := make(map[string]installationModDownload)
	seenWorlds := make(map[string]bool)
	for _, execution := range executions {
		ids := byWorld[execution.World.ID]
		if len(ids) == 0 {
			continue
		}
		placement := publicationPlacement(execution, roomID, execution.World.ID)
		key := placement.TargetID + "\x00" + placement.InstallationID
		group := groups[key]
		group.placement = placement
		group.modIDs = normalizedInstallationModIDs(append(group.modIDs, ids...))
		groups[key] = group
		seenWorlds[execution.World.ID] = true
	}
	if len(seenWorlds) != len(byWorld) {
		return mods.ActionResult{}, rooms.ErrWorldNotFound
	}
	_, _, err = s.downloadInstallationGroups(ctx, groups, output)
	if err != nil {
		return mods.ActionResult{}, err
	}
	updated := make(map[string]bool)
	for _, group := range groups {
		for _, id := range group.modIDs {
			updated[id] = true
		}
	}
	ids := make([]string, 0, len(updated))
	for id := range updated {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return mods.ActionResult{ModIDs: ids, Message: "房间运行机器上的模组已更新，配置未改变"}, nil
}
