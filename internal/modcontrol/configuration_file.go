package modcontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/operationprogress"
	"dont/internal/rooms"
	"dont/shared"
)

func (s *Service) PreviewConfigurationFile(ctx context.Context, roomID, worldID string, request mods.ConfigurationFilePreviewRequest) (mods.ConfigurationFilePreview, error) {
	roomID = strings.TrimSpace(roomID)
	worldID = strings.TrimSpace(worldID)
	if roomID == "" || worldID == "" {
		return mods.ConfigurationFilePreview{}, ErrInvalidRequest
	}
	room, err := s.source.rooms.Room(roomID)
	if err != nil {
		return mods.ConfigurationFilePreview{}, err
	}
	if !room.Managed {
		return mods.ConfigurationFilePreview{}, mods.ErrRoomNotManaged
	}
	roomWorlds, err := s.source.rooms.Worlds(roomID)
	if err != nil {
		return mods.ConfigurationFilePreview{}, err
	}
	worldByID := make(map[string]rooms.World, len(roomWorlds))
	for _, w := range roomWorlds {
		worldByID[w.ID] = w
	}
	if _, ok := worldByID[worldID]; !ok {
		return mods.ConfigurationFilePreview{}, rooms.ErrWorldNotFound
	}

	selectedIDs, selectedMap, err := resolveSelectedWorldIDs(worldID, request.WorldIDs, worldByID)
	if err != nil {
		return mods.ConfigurationFilePreview{}, err
	}

	normalizedBytes, afterSnap, notices, err := mods.NormalizeAndInspectModOverrides(request.Content, request.Format)
	if err != nil {
		return mods.ConfigurationFilePreview{}, err
	}

	prepared := proposal{roomID: roomID, roomOnly: true, action: "reconcile", worldIDs: map[string]bool{}, readOnly: true}
	snapshotCtx := withPublicationSnapshot(ctx, prepared)
	contents, err := s.source.Contents(snapshotCtx, roomID)
	if err != nil {
		return mods.ConfigurationFilePreview{}, err
	}
	placements, err := s.source.AppliedPlacements(snapshotCtx)
	if err != nil {
		return mods.ConfigurationFilePreview{}, err
	}
	if err := s.checkTopologyRevision(snapshotCtx, roomID, request.ExpectedTopologyRevision, placements.TopologyRevision); err != nil {
		return mods.ConfigurationFilePreview{}, err
	}

	worldRevisions := make(map[string]string, len(contents))
	beforeByWorld := make(map[string]mods.OverrideSnapshot, len(contents))
	for wid, raw := range contents {
		snap, inspectErr := mods.InspectModOverride(raw)
		if inspectErr != nil {
			return mods.ConfigurationFilePreview{}, inspectErr
		}
		worldRevisions[wid] = snap.Revision
		beforeByWorld[wid] = snap
	}

	if err := checkConfigurationFileRevisions(selectedIDs, worldRevisions, request.ExpectedRevision, request.ExpectedRevisions); err != nil {
		return mods.ConfigurationFilePreview{}, err
	}

	changes := mods.DiffModOverridesAcrossWorlds(beforeByWorld, selectedIDs, afterSnap)

	// Inspect Runtime mod file readiness across selected worlds' placements
	missingWorkshop, missingLocal, localNames := s.inspectConfigurationFileMissingMods(snapshotCtx, room, worldByID, selectedMap, placements, afterSnap)

	// Enrich change items with local or cached Steam metadata (never blocking on slow network)
	cachedMeta := s.cachedModMetadata(ctx, collectModIDsForMetadata(changes, missingWorkshop))
	missingSet := make(map[string]bool, len(missingWorkshop)+len(missingLocal))
	for _, id := range missingWorkshop {
		missingSet[id] = true
	}
	for _, id := range missingLocal {
		missingSet[id] = true
	}
	for i := range changes {
		id := changes[i].ModID
		if meta, ok := cachedMeta[id]; ok {
			if strings.TrimSpace(meta.Name) != "" {
				changes[i].Name = strings.TrimSpace(meta.Name)
			}
			changes[i].PreviewURL = strings.TrimSpace(meta.PreviewURL)
		}
		if changes[i].Name == "" && localNames[id] != "" {
			changes[i].Name = localNames[id]
		}
		if missingSet[id] {
			changes[i].Missing = true
		}
	}

	return mods.ConfigurationFilePreview{
		Revision:           worldRevisions[worldID],
		NextRevision:       afterSnap.Revision,
		WorldRevisions:     worldRevisions,
		TopologyRevision:   placements.TopologyRevision,
		NormalizedContent:  string(normalizedBytes),
		Changes:            changes,
		MissingWorkshopIDs: missingWorkshop,
		MissingLocalModIDs: missingLocal,
		Notices:            notices,
	}, nil
}

func (s *Service) ApplyConfigurationFile(ctx context.Context, roomID, worldID string, request mods.ConfigurationFileApplyRequest) (mods.ConfigApplyResult, error) {
	return s.applyConfigurationFileInternal(ctx, roomID, worldID, request, false)
}

func (s *Service) applyConfigurationFileInternal(ctx context.Context, roomID, worldID string, request mods.ConfigurationFileApplyRequest, allowUnchanged bool) (mods.ConfigApplyResult, error) {
	roomID = strings.TrimSpace(roomID)
	worldID = strings.TrimSpace(worldID)
	if roomID == "" || worldID == "" {
		return mods.ConfigApplyResult{}, ErrInvalidRequest
	}
	room, err := s.source.rooms.Room(roomID)
	if err != nil {
		return mods.ConfigApplyResult{}, err
	}
	if !room.Managed {
		return mods.ConfigApplyResult{}, mods.ErrRoomNotManaged
	}
	roomWorlds, err := s.source.rooms.Worlds(roomID)
	if err != nil {
		return mods.ConfigApplyResult{}, err
	}
	worldByID := make(map[string]rooms.World, len(roomWorlds))
	for _, w := range roomWorlds {
		worldByID[w.ID] = w
	}
	if _, ok := worldByID[worldID]; !ok {
		return mods.ConfigApplyResult{}, rooms.ErrWorldNotFound
	}

	selectedIDs, selectedMap, err := resolveSelectedWorldIDs(worldID, request.WorldIDs, worldByID)
	if err != nil {
		return mods.ConfigApplyResult{}, err
	}
	if err := requireConfigurationFileExpectedRevisions(selectedIDs, request.ExpectedRevision, request.ExpectedRevisions); err != nil {
		return mods.ConfigApplyResult{}, err
	}

	normalizedBytes, _, _, err := mods.NormalizeAndInspectModOverrides(request.Content, request.Format)
	if err != nil {
		return mods.ConfigApplyResult{}, err
	}

	overridesMap := make(map[string][]byte, len(selectedIDs))
	for _, wid := range selectedIDs {
		overridesMap[roomID+"\x00"+wid] = append([]byte(nil), normalizedBytes...)
	}

	prepared := proposal{
		roomID:    roomID,
		roomOnly:  true,
		action:    mods.OverrideActionConfigure,
		worldIDs:  selectedMap,
		overrides: overridesMap,
		revision:  strings.TrimSpace(request.ExpectedRevision),
		revisions: cloneStringMap(request.ExpectedRevisions),
	}

	result, err := s.applyPreparedOverrides(ctx, prepared, request.ExpectedTopologyRevision)
	if err != nil {
		return result, err
	}
	if result.PublishedTargets == 0 && !allowUnchanged {
		return result, mods.ErrNoChanges
	}

	s.syncConfigurationModesAfterFileApply(ctx, roomID, len(selectedIDs) == len(roomWorlds), len(roomWorlds) > 1)
	return result, nil
}

func (s *Service) DownloadAndApplyConfigurationFile(ctx context.Context, _ string, roomID, worldID string, request mods.ConfigurationFileApplyRequest, output io.Writer) (mods.ActionResult, error) {
	if s.installer == nil {
		return mods.ActionResult{}, errors.New("房间模组安装器未配置")
	}
	if output == nil {
		output = io.Discard
	}
	preview, err := s.PreviewConfigurationFile(ctx, roomID, worldID, mods.ConfigurationFilePreviewRequest{
		Content:                  request.Content,
		WorldIDs:                 request.WorldIDs,
		ExpectedRevision:         request.ExpectedRevision,
		ExpectedRevisions:        request.ExpectedRevisions,
		ExpectedTopologyRevision: request.ExpectedTopologyRevision,
		Format:                   request.Format,
	})
	if err != nil {
		return mods.ActionResult{}, err
	}

	roomWorlds, err := s.source.rooms.Worlds(roomID)
	if err != nil {
		return mods.ActionResult{}, err
	}
	worldByID := make(map[string]rooms.World, len(roomWorlds))
	for _, w := range roomWorlds {
		worldByID[w.ID] = w
	}
	_, selectedMap, err := resolveSelectedWorldIDs(worldID, request.WorldIDs, worldByID)
	if err != nil {
		return mods.ActionResult{}, err
	}

	downloadedTargets, readyTargets := 0, 0
	if len(preview.MissingWorkshopIDs) > 0 {
		operationprogress.Report(ctx, operationprogress.Update{
			Stage:   operationprogress.StageModInspect,
			Percent: 5,
			Message: "正在确认房间运行节点与缺失模组",
		})
		targets, targetErr := s.selectedInstallationPlacements(ctx, roomID, selectedMap)
		if targetErr != nil {
			return mods.ActionResult{}, targetErr
		}
		downloadedTargets, readyTargets, err = s.downloadRoomInstallations(ctx, targets, preview.MissingWorkshopIDs, output)
		if err != nil {
			return mods.ActionResult{}, err
		}
		operationprogress.Report(ctx, operationprogress.Update{
			Stage:   operationprogress.StageModPrepare,
			Percent: 60,
			Message: "缺失模组已就绪，正在原子写入 modoverrides.lua",
		})
		currentTargets, targetErr := s.selectedInstallationPlacements(ctx, roomID, selectedMap)
		if targetErr != nil {
			return mods.ActionResult{}, targetErr
		}
		if len(currentTargets) != len(targets) {
			return mods.ActionResult{}, ErrTopologyChanged
		}
		for key := range currentTargets {
			if _, ok := targets[key]; !ok {
				return mods.ActionResult{}, ErrTopologyChanged
			}
		}
	}

	if _, err := s.applyConfigurationFileInternal(ctx, roomID, worldID, request, len(preview.MissingWorkshopIDs) > 0); err != nil {
		return mods.ActionResult{}, err
	}

	operationprogress.Report(ctx, operationprogress.Update{
		Stage:   operationprogress.StageModDone,
		Percent: 100,
		Message: "模组下载与配置文件写入完成",
	})

	message := fmt.Sprintf("已在 %d 台运行机器补齐 %d 个缺失模组并保存 modoverrides.lua；运行中的世界将在下次重启后生效", downloadedTargets+readyTargets, len(preview.MissingWorkshopIDs))
	if len(preview.MissingWorkshopIDs) == 0 {
		message = "已保存 modoverrides.lua；运行中的世界将在下次重启后生效"
	}
	return mods.ActionResult{
		ModIDs:  append([]string(nil), preview.MissingWorkshopIDs...),
		Message: message,
	}, nil
}

func resolveSelectedWorldIDs(defaultWorldID string, requested []string, worldByID map[string]rooms.World) ([]string, map[string]bool, error) {
	raw := requested
	if len(raw) == 0 {
		raw = []string{defaultWorldID}
	}
	selectedMap := make(map[string]bool, len(raw))
	selectedList := make([]string, 0, len(raw))
	for _, wid := range raw {
		trimmed := strings.TrimSpace(wid)
		if trimmed == "" {
			return nil, nil, ErrInvalidRequest
		}
		if _, exists := worldByID[trimmed]; !exists {
			return nil, nil, rooms.ErrWorldNotFound
		}
		if !selectedMap[trimmed] {
			selectedMap[trimmed] = true
			selectedList = append(selectedList, trimmed)
		}
	}
	sort.Strings(selectedList)
	return selectedList, selectedMap, nil
}

func requireConfigurationFileExpectedRevisions(selectedWorldIDs []string, expectedRevision string, expectedRevisions map[string]string) error {
	if len(selectedWorldIDs) == 1 && len(expectedRevisions) == 0 {
		if strings.TrimSpace(expectedRevision) == "" {
			return &mods.FieldError{Fields: map[string]string{"expectedRevision": "必须提供当前配置的 expectedRevision"}}
		}
		return nil
	}
	if len(expectedRevisions) == 0 {
		return &mods.FieldError{Fields: map[string]string{"expectedRevisions": "同步多个世界时必须提供各世界的 expectedRevisions"}}
	}
	for _, wid := range selectedWorldIDs {
		if strings.TrimSpace(expectedRevisions[wid]) == "" {
			return &mods.FieldError{Fields: map[string]string{"expectedRevisions." + wid: "必须提供该世界的当前 revision"}}
		}
	}
	return nil
}

func checkConfigurationFileRevisions(selectedWorldIDs []string, actual map[string]string, expectedRevision string, expectedRevisions map[string]string) error {
	if len(expectedRevisions) > 0 {
		for _, wid := range selectedWorldIDs {
			exp := strings.TrimSpace(expectedRevisions[wid])
			if exp != "" && exp != actual[wid] {
				return &mods.RevisionConflictError{CurrentRevision: actual[wid]}
			}
		}
		return nil
	}
	if exp := strings.TrimSpace(expectedRevision); exp != "" && len(selectedWorldIDs) == 1 {
		wid := selectedWorldIDs[0]
		if exp != actual[wid] {
			return &mods.RevisionConflictError{CurrentRevision: actual[wid]}
		}
	}
	return nil
}

func (s *Service) inspectConfigurationFileMissingMods(
	ctx context.Context,
	room rooms.Room,
	worldByID map[string]rooms.World,
	selectedWorlds map[string]bool,
	placements modpublication.PlacementSnapshot,
	afterSnap mods.OverrideSnapshot,
) ([]string, []string, map[string]string) {
	localNames := make(map[string]string)
	if s.runtimeFiles == nil || len(afterSnap.Mods) == 0 {
		return []string{}, []string{}, localNames
	}

	modIDs := make([]string, 0, len(afterSnap.Mods))
	for _, m := range afterSnap.Mods {
		modIDs = append(modIDs, m.ModID)
	}
	sort.Strings(modIDs)

	groups := make(map[string]*runtimeFileGroup)
	for _, placement := range placements.Placements {
		if placement.RoomID != room.ID || !selectedWorlds[placement.WorldID] {
			continue
		}
		world, ok := worldByID[placement.WorldID]
		if !ok {
			continue
		}
		key := placement.TargetID + "\x00" + placement.InstallationID
		group := groups[key]
		if group == nil {
			group = &runtimeFileGroup{
				request: RuntimeFileRequest{
					TargetID:         placement.TargetID,
					InstallationID:   placement.InstallationID,
					TopologyRevision: placements.TopologyRevision,
					SkipLoadedLogs:   true,
					WorkshopIDs:      append([]string(nil), modIDs...),
				},
				worldIDs: make(map[string]bool),
			}
			groups[key] = group
		}
		group.worldIDs[world.ID] = true
		group.request.Worlds = append(group.request.Worlds, RuntimeFileWorld{
			RoomID:         room.ID,
			RoomDirectory:  room.DirectoryName,
			WorldID:        world.ID,
			WorldDirectory: world.DirectoryName,
		})
	}
	if len(groups) == 0 {
		return []string{}, []string{}, localNames
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var wg sync.WaitGroup
	for _, k := range keys {
		group := groups[k]
		wg.Add(1)
		go func() {
			defer wg.Done()
			group.observation, group.err = s.runtimeFiles.ObserveRuntimeModFiles(ctx, group.request)
		}()
	}
	wg.Wait()

	missingWorkshopSet := make(map[string]bool)
	missingLocalSet := make(map[string]bool)
	for _, m := range afterSnap.Mods {
		ref, _ := mods.ParseReference(m.ModID)
		missingOnAnyTarget := false
		for _, k := range keys {
			group := groups[k]
			if group.err != nil || group.observation == nil {
				continue
			}
			state, exists := group.observation.Mods[m.ModID]
			if exists && strings.TrimSpace(state.Name) != "" && localNames[m.ModID] == "" {
				localNames[m.ModID] = strings.TrimSpace(state.Name)
			}
			if !exists || state.Status != shared.RuntimeModFileReady || state.Reason != "" {
				missingOnAnyTarget = true
			}
		}
		if missingOnAnyTarget {
			if ref.Source == "workshop" && mods.ValidID(ref.ID) {
				missingWorkshopSet[ref.ID] = true
			} else if ref.Source == "local" {
				missingLocalSet[ref.ID] = true
			}
		}
	}

	missingWorkshop := make([]string, 0, len(missingWorkshopSet))
	for id := range missingWorkshopSet {
		missingWorkshop = append(missingWorkshop, id)
	}
	sort.Strings(missingWorkshop)

	missingLocal := make([]string, 0, len(missingLocalSet))
	for id := range missingLocalSet {
		missingLocal = append(missingLocal, id)
	}
	sort.Strings(missingLocal)

	return missingWorkshop, missingLocal, localNames
}

func collectModIDsForMetadata(changes []mods.ConfigurationFileModChange, missingWorkshop []string) []string {
	seen := make(map[string]bool, len(changes)+len(missingWorkshop))
	ids := make([]string, 0, len(changes)+len(missingWorkshop))
	for _, ch := range changes {
		if mods.ValidID(ch.ModID) && !seen[ch.ModID] {
			seen[ch.ModID] = true
			ids = append(ids, ch.ModID)
		}
	}
	for _, id := range missingWorkshop {
		if mods.ValidID(id) && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Service) cachedModMetadata(ctx context.Context, ids []string) map[string]mods.SteamMod {
	result := make(map[string]mods.SteamMod)
	if len(ids) == 0 || s.mods == nil {
		return result
	}
	reader, ok := s.mods.(interface {
		CachedMetadata(context.Context, []string) (map[string]mods.SteamMod, error)
	})
	if !ok {
		return result
	}
	for start := 0; start < len(ids); start += 100 {
		end := start + 100
		if end > len(ids) {
			end = len(ids)
		}
		batch, err := reader.CachedMetadata(ctx, ids[start:end])
		if err != nil {
			continue
		}
		for id, meta := range batch {
			result[id] = meta
		}
	}
	return result
}

func (s *Service) syncConfigurationModesAfterFileApply(ctx context.Context, roomID string, appliedAllWorlds, multiWorldRoom bool) {
	if s.configurationModes == nil || !multiWorldRoom {
		return
	}
	if appliedAllWorlds {
		_ = s.configurationModes.db.Table(s.configurationModes.table).Where("room_id = ?", roomID).Delete(&configurationModeRecord{}).Error
		return
	}
	profile, err := s.RoomProfile(ctx, roomID)
	if err != nil {
		return
	}
	for _, item := range profile.Items {
		if len(item.ExceptionWorldIDs) > 0 {
			_ = s.SetConfigurationMode(ctx, roomID, item.ModID, "separate")
		}
	}
}
