package modcontrol

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/rooms"

	"github.com/google/uuid"
)

type Service struct {
	source      *SnapshotSource
	mods        ModCatalog
	coordinator PublicationCoordinator
	now         Clock
}

func NewService(source *SnapshotSource, modCatalog ModCatalog, coordinator PublicationCoordinator) (*Service, error) {
	if source == nil || modCatalog == nil || coordinator == nil {
		return nil, ErrInvalidRequest
	}
	return &Service{source: source, mods: modCatalog, coordinator: coordinator, now: time.Now}, nil
}

func (s *Service) Preview(ctx context.Context, roomID string, request Request) (modpublication.Plan, error) {
	prepared, err := s.prepare(ctx, roomID, request)
	if err != nil {
		return modpublication.Plan{}, err
	}
	snapshotCtx := withPublicationSnapshot(ctx, prepared)
	plan, err := s.coordinator.Preview(snapshotCtx, roomID)
	if err != nil {
		return modpublication.Plan{}, err
	}
	if err := s.checkTopologyRevision(snapshotCtx, roomID, request.ExpectedTopologyRevision, plan.TopologyRevision); err != nil {
		return modpublication.Plan{}, err
	}
	return plan, nil
}

func (s *Service) Publish(ctx context.Context, sourceJobID, roomID string, request Request) (modpublication.Publication, error) {
	prepared, err := s.prepare(ctx, roomID, request)
	if err != nil {
		return modpublication.Publication{}, err
	}
	snapshotCtx := withPublicationSnapshot(ctx, prepared)
	plan, err := s.coordinator.Preview(snapshotCtx, roomID)
	if err != nil {
		return modpublication.Publication{}, err
	}
	if err := s.checkTopologyRevision(snapshotCtx, roomID, request.ExpectedTopologyRevision, plan.TopologyRevision); err != nil {
		return modpublication.Publication{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(request.PlanHash), plan.PlanHash) {
		return modpublication.Publication{}, modpublication.ErrPlanChanged
	}
	if request.Confirmation != plan.PlanHash {
		return modpublication.Publication{}, ErrConfirmation
	}
	if !plan.Ready {
		return modpublication.Publication{}, modpublication.ErrPreviewBlocked
	}
	return s.coordinator.Publish(withPublicationSnapshot(ctx, prepared), modpublication.PublishRequest{
		ID: uuid.NewString(), SourceJobID: strings.TrimSpace(sourceJobID), Plan: plan, Activation: request.Activation,
	})
}

func (s *Service) Activate(ctx context.Context, sourceJobID, publicationID string, policy modpublication.ActivationPolicy) (modpublication.Publication, error) {
	return s.coordinator.Activate(ctx, strings.TrimSpace(publicationID), strings.TrimSpace(sourceJobID), policy)
}

func (s *Service) checkTopologyRevision(ctx context.Context, roomID, expected, planRevision string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" || expected == planRevision {
		return nil
	}
	roomRevision, exists, err := s.source.RoomTopologyRevision(ctx, roomID)
	if err != nil {
		return err
	}
	if !exists {
		return rooms.ErrRoomNotFound
	}
	if expected != roomRevision {
		return ErrTopologyChanged
	}
	return nil
}

func (s *Service) Retry(ctx context.Context, sourceJobID, publicationID string) (modpublication.Publication, error) {
	current, err := s.coordinator.Get(publicationID)
	if err != nil {
		return modpublication.Publication{}, err
	}
	switch current.Status {
	case modpublication.StatusPreviewed, modpublication.StatusPreparing, modpublication.StatusPrepared,
		modpublication.StatusPublishing, modpublication.StatusCommitted, modpublication.StatusCompleting,
		modpublication.StatusRecoveryRequired:
		return s.coordinator.RecoverOne(ctx, publicationID)
	case modpublication.StatusFailed, modpublication.StatusRolledBack:
		// A terminal failed attempt may be retried below, but it must still
		// describe the exact plan the operator originally confirmed.
	default:
		return current, ErrPublicationState
	}
	overrides := make(map[string][]byte)
	for _, target := range current.Plan.Targets {
		for _, world := range target.Worlds {
			overrides[world.RoomID+"\x00"+world.WorldID] = append([]byte(nil), world.ModOverrides...)
		}
	}
	prepared := proposal{roomID: current.RoomID, overrides: overrides, worldIDs: map[string]bool{}}
	plan, err := s.coordinator.Preview(withPublicationSnapshot(ctx, prepared), current.RoomID)
	if err != nil {
		return modpublication.Publication{}, err
	}
	if plan.TopologyRevision != current.Plan.TopologyRevision {
		return modpublication.Publication{}, ErrTopologyChanged
	}
	if plan.PlanHash != current.Plan.PlanHash {
		return modpublication.Publication{}, modpublication.ErrPlanChanged
	}
	if !plan.Ready {
		return modpublication.Publication{}, modpublication.ErrPreviewBlocked
	}
	return s.coordinator.Publish(withPublicationSnapshot(ctx, prepared), modpublication.PublishRequest{
		ID: uuid.NewString(), SourceJobID: strings.TrimSpace(sourceJobID), Plan: plan, Activation: current.Activation.Policy,
	})
}

func (s *Service) Get(publicationID string) (modpublication.Publication, error) {
	return s.coordinator.Get(publicationID)
}

func (s *Service) List(roomID string, limit, offset int) (ListResult, error) {
	items, total, err := s.coordinator.List(roomID, limit, offset)
	return ListResult{Items: items, Total: total, Limit: limit, Offset: offset}, err
}

func (s *Service) Recover(ctx context.Context) ([]modpublication.Publication, error) {
	return s.coordinator.Recover(ctx)
}

func (s *Service) RunRecovery(ctx context.Context, interval time.Duration) {
	if interval < 30*time.Second {
		interval = time.Minute
	}
	_, _ = s.Recover(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.Recover(ctx)
		}
	}
}

func (s *Service) RoomList(ctx context.Context, roomID string) (mods.ModList, error) {
	if _, err := s.source.rooms.Room(roomID); err != nil {
		return mods.ModList{}, err
	}
	prepared := proposal{roomID: roomID, roomOnly: true, action: "reconcile", worldIDs: map[string]bool{}}
	contents, err := s.source.Contents(withPublicationSnapshot(ctx, prepared), roomID)
	if err != nil {
		return mods.ModList{}, err
	}
	return s.mods.ListFromOverrides(ctx, roomID, contents)
}

func (s *Service) ConfigurationFile(ctx context.Context, roomID, worldID string) (mods.ConfigurationFile, error) {
	content, err := s.worldContent(ctx, roomID, worldID)
	if err != nil {
		return mods.ConfigurationFile{}, err
	}
	snapshot, err := mods.InspectModOverride(content)
	if err != nil {
		return mods.ConfigurationFile{}, err
	}
	return mods.ConfigurationFile{
		RoomID: roomID, WorldID: worldID, FileName: "modoverrides.lua", Content: string(content),
		Exists: len(content) > 0, Revision: snapshot.Revision, ReadAt: s.now().UTC(),
	}, nil
}

func (s *Service) Configuration(ctx context.Context, roomID, worldID, modID string) (mods.ModConfiguration, error) {
	content, err := s.worldContent(ctx, roomID, worldID)
	if err != nil {
		return mods.ModConfiguration{}, err
	}
	return s.mods.ConfigurationFromContent(ctx, roomID, worldID, modID, content)
}

func (s *Service) PreviewConfiguration(ctx context.Context, roomID, worldID, modID string, request mods.ConfigUpdateRequest) (mods.ConfigPreview, error) {
	content, err := s.worldContent(ctx, roomID, worldID)
	if err != nil {
		return mods.ConfigPreview{}, err
	}
	configuration, err := s.mods.ConfigurationFromContent(ctx, roomID, worldID, modID, content)
	if err != nil {
		return mods.ConfigPreview{}, err
	}
	result, err := mods.MutateModOverride(content, mods.OverrideMutation{
		Action: mods.OverrideActionConfigure, ModID: modID, Enabled: request.Enabled,
		ExpectedRevision: request.ExpectedRevision, Patch: clonePatch(request.Patch), Fields: configuration.Fields,
	})
	if err != nil {
		return mods.ConfigPreview{}, err
	}
	return mods.ConfigPreview{
		Revision: result.Revision, NextRevision: result.NextRevision, Changes: result.Changes,
		Warnings: append([]string(nil), configuration.Warnings...), RawPreserved: true,
	}, nil
}

func (s *Service) worldContent(ctx context.Context, roomID, worldID string) ([]byte, error) {
	prepared := proposal{roomID: roomID, roomOnly: true, action: "reconcile", worldIDs: map[string]bool{}}
	contents, err := s.source.Contents(withPublicationSnapshot(ctx, prepared), roomID)
	if err != nil {
		return nil, err
	}
	content, ok := contents[worldID]
	if !ok {
		return nil, rooms.ErrWorldNotFound
	}
	return content, nil
}

func (s *Service) prepare(ctx context.Context, roomID string, request Request) (proposal, error) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return proposal{}, ErrInvalidRequest
	}
	action := strings.ToLower(strings.TrimSpace(request.Action))
	if action == "" {
		action = "reconcile"
	}
	prepared := proposal{
		roomID: roomID, action: mods.OverrideAction(action), modID: strings.TrimSpace(request.ModID),
		enabled: request.Enabled, revision: strings.TrimSpace(request.ExpectedConfigurationRevision),
		patch: clonePatch(request.Patch), worldIDs: make(map[string]bool),
	}
	for _, worldID := range request.WorldIDs {
		worldID = strings.TrimSpace(worldID)
		if worldID == "" || prepared.worldIDs[worldID] {
			return proposal{}, ErrInvalidRequest
		}
		prepared.worldIDs[worldID] = true
	}
	switch prepared.action {
	case "reconcile":
		seen := make(map[string]bool, len(request.ModIDs))
		for _, modID := range request.ModIDs {
			modID = strings.TrimSpace(modID)
			if !mods.ValidID(modID) || seen[modID] {
				return proposal{}, ErrInvalidRequest
			}
			seen[modID] = true
			prepared.modIDs = append(prepared.modIDs, modID)
		}
		sort.Strings(prepared.modIDs)
		return prepared, nil
	case mods.OverrideActionAdd:
		if !mods.ValidID(prepared.modID) || len(prepared.worldIDs) == 0 {
			return proposal{}, ErrInvalidRequest
		}
		ids, err := s.mods.ResolveDependencies(ctx, prepared.modID, request.IncludeDependencies)
		if err != nil {
			return proposal{}, err
		}
		prepared.modIDs = ids
	case mods.OverrideActionEnable, mods.OverrideActionRemove:
		if !mods.ValidID(prepared.modID) || len(prepared.worldIDs) == 0 {
			return proposal{}, ErrInvalidRequest
		}
	case mods.OverrideActionConfigure:
		if !mods.ValidID(prepared.modID) || len(prepared.worldIDs) != 1 || prepared.revision == "" {
			return proposal{}, ErrInvalidRequest
		}
	default:
		return proposal{}, ErrInvalidRequest
	}
	return prepared, nil
}

func clonePatch(values map[string]json.RawMessage) map[string]json.RawMessage {
	if values == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		result[key] = append(json.RawMessage(nil), value...)
	}
	return result
}
