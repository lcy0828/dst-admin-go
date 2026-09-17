package modcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/operationlease"
	"dont/internal/operationprogress"
	"dont/internal/requesttiming"
	"dont/internal/rooms"
	"dont/internal/runtimefiles"
	"dont/internal/topology"
	"dont/shared"

	"github.com/google/uuid"
)

type Service struct {
	source             *SnapshotSource
	mods               ModCatalog
	coordinator        PublicationCoordinator
	convergence        PlanConvergence
	runtimeFiles       RuntimeFileObserver
	replicas           ReplicaReader
	installer          InstallationContentFetcher
	configuration      ModConfigurationPublisher
	configurationModes *ConfigurationModeStore
	now                Clock
}

func (s *Service) ConfigureConfigurationPublisher(publisher ModConfigurationPublisher) error {
	if publisher == nil {
		return ErrInvalidRequest
	}
	s.configuration = publisher
	return nil
}

func (s *Service) ConfigureInstallationContentFetcher(fetcher InstallationContentFetcher) error {
	if fetcher == nil {
		return ErrInvalidRequest
	}
	s.installer = fetcher
	return nil
}

func (s *Service) ConfigureRuntimeFileObserver(observer RuntimeFileObserver) error {
	if observer == nil {
		return ErrInvalidRequest
	}
	s.runtimeFiles = observer
	return nil
}

func (s *Service) ConfigureConvergence(convergence PlanConvergence) error {
	if convergence == nil {
		return ErrInvalidRequest
	}
	s.convergence = convergence
	return nil
}

func (s *Service) ConfigureReplicaReader(reader ReplicaReader) error {
	if reader == nil {
		return ErrInvalidRequest
	}
	s.replicas = reader
	return nil
}

func (s *Service) RoomReplicas(roomID string) (modpublication.RoomReplicaState, error) {
	if s.replicas == nil {
		return modpublication.RoomReplicaState{}, ErrInvalidRequest
	}
	if _, err := s.source.rooms.Room(roomID); err != nil {
		return modpublication.RoomReplicaState{}, err
	}
	return s.replicas.Room(roomID)
}

func (s *Service) ReconcilePlacementMigration(ctx context.Context, roomID string, worldIDs []string, lease operationlease.Lease) error {
	if lease.RoomID != roomID || lease.LeaseID == "" || lease.OperationKey == "" || lease.FencingToken == 0 {
		return ErrInvalidRequest
	}
	return s.reconcileRuntimeMods(ctx, roomID, worldIDs, []modpublication.Fence{publicationFence(lease)})
}

func (s *Service) PreparePlacementMigration(ctx context.Context, migration topology.MigrationPlacement, lease operationlease.Lease) (modpublication.PreparedTransaction, error) {
	roomID := strings.TrimSpace(migration.Room.ID)
	worldID := strings.TrimSpace(migration.World.ID)
	targetID := strings.TrimSpace(migration.TargetTargetID)
	installationID := strings.TrimSpace(migration.TargetInstallationID)
	if roomID == "" || worldID == "" || targetID == "" || installationID == "" ||
		lease.RoomID != roomID || lease.LeaseID == "" || lease.OperationKey == "" || lease.FencingToken == 0 {
		return nil, ErrInvalidRequest
	}
	targetExecution := topology.ExecutionPlacement{
		Room: migration.Room, World: migration.World, Revision: migration.Revision,
		DesiredTargetID: targetID, AppliedTargetID: targetID,
		DesiredInstallationID: installationID, AppliedInstallationID: installationID,
		Target: migration.Target, Inventory: migration.TargetInventory,
	}
	targetExecution.Target.Config.InstallationID = installationID
	placement := publicationPlacement(targetExecution, roomID, worldID)
	installationKey := placement.TargetID + "\x00" + placement.InstallationID
	prepared := proposal{
		roomID: roomID, action: "reconcile", worldIDs: map[string]bool{},
		placements:       map[string]modpublication.AppliedPlacement{roomID + "\x00" + worldID: placement},
		executions:       map[string]topology.ExecutionPlacement{installationKey: targetExecution},
		installationOnly: map[string]bool{installationKey: true},
	}
	snapshotCtx := withPublicationSnapshot(ctx, prepared)
	plan, err := s.coordinator.Preview(snapshotCtx, roomID)
	if err != nil {
		return nil, err
	}
	if !plan.Ready {
		return nil, modpublication.ErrPreviewBlocked
	}
	return s.coordinator.BeginTransaction(snapshotCtx, modpublication.PublishRequest{
		ID: uuid.NewString(), Plan: plan, BorrowedFences: []modpublication.Fence{publicationFence(lease)},
		SkipProtectionBackup: true,
	})
}

func (s *Service) reconcileRuntimeMods(ctx context.Context, roomID string, worldIDs []string, borrowed []modpublication.Fence) error {
	if s.convergence == nil {
		return errors.New("Mod 运行状态协调器未配置")
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModInspect, Message: "正在读取模组与运行位置"})
	prepared, err := s.prepare(ctx, roomID, Request{Action: "reconcile"})
	if err != nil {
		return err
	}
	snapshotCtx := withPublicationSnapshot(ctx, prepared)
	plan, err := s.coordinator.Preview(snapshotCtx, roomID)
	if err != nil {
		return err
	}
	if !selectedWorldsHaveMods(plan, roomID, worldIDs) {
		operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModDone, Percent: 100, Message: "当前世界没有需要同步的模组"})
		return nil
	}
	if !plan.Ready {
		return modpublication.ErrPreviewBlocked
	}
	converged, err := s.convergence.Converged(snapshotCtx, plan)
	if err != nil {
		return fmt.Errorf("读取 Mod 安装状态: %w", err)
	}
	if converged {
		operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModDone, Percent: 100, Message: "运行机器上的模组已同步"})
		return nil
	}
	publication, err := s.coordinator.Publish(snapshotCtx, modpublication.PublishRequest{
		ID: uuid.NewString(), Plan: plan, BorrowedFences: append([]modpublication.Fence(nil), borrowed...),
		SkipProtectionBackup: true,
	})
	if err != nil {
		return fmt.Errorf("同步跨机器 Mod: %w", err)
	}
	if publication.Status != modpublication.StatusSucceeded {
		return fmt.Errorf("同步跨机器 Mod 未完成: %s", publication.Status)
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModDone, Percent: 100, Message: "模组与世界配置已同步"})
	return nil
}

func selectedWorldsHaveMods(plan modpublication.Plan, roomID string, worldIDs []string) bool {
	selected := make(map[string]bool, len(worldIDs))
	for _, worldID := range worldIDs {
		selected[strings.TrimSpace(worldID)] = true
	}
	for _, target := range plan.Targets {
		for _, world := range target.Worlds {
			if world.RoomID == roomID && (len(selected) == 0 || selected[world.WorldID]) && len(world.Mods) > 0 {
				return true
			}
		}
	}
	return false
}

func NewService(source *SnapshotSource, modCatalog ModCatalog, coordinator PublicationCoordinator) (*Service, error) {
	if source == nil || modCatalog == nil || coordinator == nil {
		return nil, ErrInvalidRequest
	}
	return &Service{source: source, mods: modCatalog, coordinator: coordinator, now: time.Now}, nil
}

// Install downloads on each selected Runtime and directly updates only the
// selected worlds' modoverrides.lua files.
func (s *Service) Install(ctx context.Context, _ string, roomID string, request mods.InstallRequest, output io.Writer) (mods.ActionResult, error) {
	if s.installer == nil {
		return mods.ActionResult{}, errors.New("房间模组安装器未配置")
	}
	if output == nil {
		output = io.Discard
	}
	prepared, err := s.prepare(ctx, roomID, Request{
		Action: "add", ModID: request.ModID, WorldIDs: request.WorldIDs,
		Enabled: request.Enabled, IncludeDependencies: request.IncludeDependencies,
	})
	if err != nil {
		return mods.ActionResult{}, err
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModInspect, Percent: 5, Message: "正在确认房间运行位置"})
	targets, err := s.selectedInstallationPlacements(ctx, roomID, prepared.worldIDs)
	if err != nil {
		return mods.ActionResult{}, err
	}
	downloadedTargets, readyTargets, err := s.downloadRoomInstallations(ctx, targets, prepared.modIDs, output)
	if err != nil {
		return mods.ActionResult{}, err
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModPrepare, Percent: 50, Message: "下载完成，正在写入房间模组配置"})
	currentTargets, err := s.selectedInstallationPlacements(ctx, roomID, prepared.worldIDs)
	if err != nil {
		return mods.ActionResult{}, err
	}
	if len(currentTargets) != len(targets) {
		return mods.ActionResult{}, ErrTopologyChanged
	}
	for key := range currentTargets {
		if _, downloaded := targets[key]; !downloaded {
			return mods.ActionResult{}, ErrTopologyChanged
		}
	}
	prepared.roomOnly = true
	if _, err := s.applyPreparedOverrides(ctx, prepared, ""); err != nil {
		return mods.ActionResult{}, err
	}
	message := fmt.Sprintf("已在 %d 台运行机器下载模组并添加到所选世界；运行中的世界将在下次重启后生效", downloadedTargets)
	progressMessage := "模组已下载并添加到房间"
	if downloadedTargets == 0 {
		message = fmt.Sprintf("%d 台运行机器已有最新模组，已直接添加到所选世界；运行中的世界将在下次重启后生效", readyTargets)
		progressMessage = "运行机器已有最新模组，已直接添加到房间"
	} else if readyTargets > 0 {
		message = fmt.Sprintf("已在 %d 台运行机器下载模组，另有 %d 台无需重复下载，并添加到所选世界；运行中的世界将在下次重启后生效", downloadedTargets, readyTargets)
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModDone, Percent: 100, Message: progressMessage})
	return mods.ActionResult{
		ModIDs:  append([]string(nil), prepared.modIDs...),
		Message: message,
	}, nil
}

func (s *Service) installationModsCurrent(ctx context.Context, targetID, installationID string, modIDs []string) (bool, error) {
	if s.runtimeFiles == nil {
		return false, nil
	}
	observed, err := s.runtimeFiles.ObserveRuntimeModInventory(ctx, targetID, installationID)
	if err != nil {
		return false, err
	}
	if observed == nil || observed.InstallationID != installationID {
		return false, errors.New("运行机器返回的 Mod 安装目录清单无效")
	}
	for _, modID := range modIDs {
		state, exists := observed.Mods[modID]
		if !exists || state.Status != shared.RuntimeModFileReady {
			return false, nil
		}
	}
	metadata, err := s.mods.Describe(ctx, modIDs)
	if err != nil {
		return false, err
	}
	for _, modID := range modIDs {
		state := observed.Mods[modID]
		latest, exists := metadata[modID]
		if !exists || runtimeModVersionStatus(state, latest.SteamManifestID, latest.Version, latest.UpdatedAt) != "current" {
			return false, nil
		}
	}
	return true, nil
}

func (s *Service) selectedInstallationPlacements(ctx context.Context, roomID string, worldIDs map[string]bool) (map[string]modpublication.AppliedPlacement, error) {
	executions, err := s.source.topology.ResolveRoomExecutions(ctx, roomID)
	if err != nil {
		return nil, err
	}
	targets := make(map[string]modpublication.AppliedPlacement)
	selectedWorlds := make(map[string]bool, len(worldIDs))
	for _, execution := range executions {
		if !worldIDs[execution.World.ID] {
			continue
		}
		placement := publicationPlacement(execution, roomID, execution.World.ID)
		targets[placement.TargetID+"\x00"+placement.InstallationID] = placement
		selectedWorlds[execution.World.ID] = true
	}
	if len(targets) == 0 || len(selectedWorlds) != len(worldIDs) {
		return nil, rooms.ErrWorldNotFound
	}
	return targets, nil
}

// SetEnabled changes only the enabled field and requires the revision that the
// user saw, so an external edit cannot be overwritten by a stale page.
func (s *Service) SetEnabled(ctx context.Context, roomID, modID string, request mods.EnableRequest) (mods.ConfigApplyResult, error) {
	prepared, err := s.prepare(ctx, roomID, Request{
		Action: string(mods.OverrideActionEnable), ModID: modID, WorldIDs: request.WorldIDs, Enabled: request.Enabled,
		ExpectedConfigurationRevision:  request.ExpectedRevision,
		ExpectedConfigurationRevisions: request.ExpectedRevisions,
		ExpectedTopologyRevision:       request.ExpectedTopologyRevision,
	})
	if err != nil {
		return mods.ConfigApplyResult{}, err
	}
	if err := requireExpectedRevisions(prepared); err != nil {
		return mods.ConfigApplyResult{}, err
	}
	prepared.roomOnly = true
	return s.applyPreparedOverrides(ctx, prepared, request.ExpectedTopologyRevision)
}

func requireExpectedRevisions(prepared proposal) error {
	if len(prepared.worldIDs) == 1 && len(prepared.revisions) == 0 {
		if prepared.revision == "" {
			return ErrInvalidRequest
		}
		return nil
	}
	if len(prepared.revisions) != len(prepared.worldIDs) {
		return ErrInvalidRequest
	}
	for worldID, revision := range prepared.revisions {
		if !prepared.worldIDs[worldID] || strings.TrimSpace(revision) == "" {
			return ErrInvalidRequest
		}
	}
	return nil
}

func (s *Service) applyPreparedOverrides(ctx context.Context, prepared proposal, expectedTopologyRevision string) (mods.ConfigApplyResult, error) {
	started := time.Now()
	if s.configuration == nil {
		return mods.ConfigApplyResult{}, errors.New("Mod 配置发布器未配置")
	}
	snapshotCtx := withPublicationSnapshot(ctx, prepared)
	snapshot, err := s.source.load(snapshotCtx)
	if err != nil {
		return mods.ConfigApplyResult{}, err
	}
	if err := s.checkTopologyRevision(snapshotCtx, prepared.roomID, expectedTopologyRevision, snapshot.placements.TopologyRevision); err != nil {
		return mods.ConfigApplyResult{}, err
	}
	worldIDs := make([]string, 0, len(prepared.worldIDs))
	for worldID := range prepared.worldIDs {
		worldIDs = append(worldIDs, worldID)
	}
	sort.Strings(worldIDs)
	result := mods.ConfigApplyResult{WorldIDs: append([]string(nil), worldIDs...), Revisions: make(map[string]string, len(worldIDs))}
	preparedAt := time.Now()
	result.PublishedTargets, err = s.configuration.PublishModOverrides(ctx, prepared.roomID, snapshot.overrides)
	log.Printf("[ModConfig] room_id=%s mod_id=%s action=%s worlds=%d prepare_ms=%d publish_ms=%d total_ms=%d error=%v", prepared.roomID, prepared.modID, prepared.action, len(worldIDs), preparedAt.Sub(started).Milliseconds(), time.Since(preparedAt).Milliseconds(), time.Since(started).Milliseconds(), err)
	if err != nil {
		var conflict *runtimefiles.ConfigurationConflictError
		if errors.As(err, &conflict) {
			return result, fmt.Errorf("%w: %v", &mods.RevisionConflictError{CurrentRevision: conflict.CurrentSHA256}, err)
		}
		return result, err
	}
	for _, worldID := range worldIDs {
		content, exists := snapshot.contents[prepared.roomID+"\x00"+worldID]
		if !exists {
			return mods.ConfigApplyResult{}, rooms.ErrWorldNotFound
		}
		state, inspectErr := mods.InspectModOverride(content)
		if inspectErr != nil {
			return mods.ConfigApplyResult{}, inspectErr
		}
		result.Revisions[worldID] = state.Revision
		if len(worldIDs) == 1 {
			result.Revision = state.Revision
		}
	}
	return result, nil
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

// PrepareCache resolves the room's current desired Mod plan and downloads its
// exact content to every shared installation without publishing configuration.
func (s *Service) PrepareCache(ctx context.Context, roomID string) (modpublication.CachePreparation, error) {
	prepared, err := s.prepare(ctx, roomID, Request{Action: "reconcile"})
	if err != nil {
		return modpublication.CachePreparation{}, err
	}
	snapshotCtx := withPublicationSnapshot(ctx, prepared)
	plan, err := s.coordinator.Preview(snapshotCtx, roomID)
	if err != nil {
		return modpublication.CachePreparation{}, err
	}
	if !plan.Ready {
		return modpublication.CachePreparation{}, modpublication.ErrPreviewBlocked
	}
	cache, ok := s.coordinator.(CachePreparer)
	if !ok {
		return modpublication.CachePreparation{}, errors.New("Mod cache preparation is unavailable")
	}
	return cache.EnsureCache(snapshotCtx, plan)
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
		SkipProtectionBackup: true,
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
		return s.coordinator.RecoverOne(ctx, publicationID, strings.TrimSpace(sourceJobID))
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
		SkipProtectionBackup: true,
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

func (s *Service) RoomList(ctx context.Context, roomID string) (mods.ModList, error) {
	return s.roomList(ctx, roomID, true)
}

func (s *Service) RoomFacts(ctx context.Context, roomID string) (mods.ModList, error) {
	return s.roomList(ctx, roomID, false)
}

func (s *Service) roomList(ctx context.Context, roomID string, includeMetadata bool) (mods.ModList, error) {
	finishCatalog := requesttiming.Start(ctx, "mods.catalog")
	room, err := s.source.rooms.Room(roomID)
	if err != nil {
		finishCatalog()
		return mods.ModList{}, err
	}
	worlds, err := s.source.rooms.Worlds(roomID)
	finishCatalog()
	if err != nil {
		return mods.ModList{}, err
	}
	prepared := proposal{roomID: roomID, roomOnly: true, action: "reconcile", worldIDs: map[string]bool{}, readOnly: true}
	snapshotCtx := withPublicationSnapshot(ctx, prepared)
	finishOverrides := requesttiming.Start(ctx, "mods.placement_and_overrides")
	contents, err := s.source.Contents(snapshotCtx, roomID)
	if err != nil {
		finishOverrides()
		return mods.ModList{}, err
	}
	placements, err := s.source.AppliedPlacements(snapshotCtx)
	finishOverrides()
	if err != nil {
		return mods.ModList{}, err
	}
	finishList := requesttiming.Start(ctx, "mods.list_build")
	result, err := s.mods.ListFromOverrides(ctx, roomID, contents)
	finishList()
	if err != nil {
		return mods.ModList{}, err
	}
	if includeMetadata {
		ids := make([]string, 0, len(result.Items))
		for _, item := range result.Items {
			ids = append(ids, item.ID)
		}
		metadata, metadataErr := s.mods.Describe(ctx, ids)
		if metadataErr != nil {
			result.MetadataWarning = metadataErr.Error()
		}
		for index := range result.Items {
			item := &result.Items[index]
			item.SteamMod = metadata[item.ID]
			item.ID = ids[index]
			item.LatestVersion = item.Version
		}
	}
	if len(worlds) > 0 {
		finishProfile := requesttiming.Start(ctx, "mods.configuration_profile")
		profile, profileErr := roomProfileFromContents(roomID, worlds, contents)
		if profileErr != nil {
			finishProfile()
			return mods.ModList{}, profileErr
		}
		result.Profile = &profile
		if err := s.applyConfigurationModes(result.Profile); err != nil {
			finishProfile()
			return mods.ModList{}, err
		}
		finishProfile()
	}
	finishFiles := requesttiming.Start(ctx, "mods.runtime_files")
	s.observeRoomModFiles(snapshotCtx, room, worlds, placements, &result)
	finishFiles()
	result.Healthy, result.Attention = 0, 0
	for _, item := range result.Items {
		if item.Health == mods.HealthHealthy || item.Health == mods.HealthDisabled {
			result.Healthy++
		} else {
			result.Attention++
		}
	}
	return result, nil
}

// CheckUpdates compares Workshop metadata with the installations currently
// assigned to the room, not with the Controller's own content library.
func (s *Service) CheckUpdates(ctx context.Context, roomID string) (mods.ActionResult, error) {
	list, err := s.RoomFacts(ctx, roomID)
	if err != nil {
		return mods.ActionResult{}, err
	}
	ids := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		if item.Enabled {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) == 0 {
		return mods.ActionResult{ModIDs: []string{}, Message: "房间没有已启用的模组"}, nil
	}
	// Always fetch version evidence. Display metadata may be old or unavailable
	// without affecting this comparison, and missing evidence is never current.
	metadata, metadataErr := s.mods.Describe(ctx, ids)
	failures := []error{metadataErr}
	updates := make([]string, 0)
	for _, item := range list.Items {
		if !item.Enabled {
			continue
		}
		latest := metadata[item.ID]
		if latest.SteamManifestID == "" && latest.Version == "" && latest.UpdatedAt.IsZero() {
			// A failed batch request already explains why its missing items
			// cannot be compared. Do not repeat it as a failure on every node.
			if metadataErr == nil {
				failures = append(failures, fmt.Errorf("Workshop %s 未返回可比较的版本信息", item.ID))
			}
			continue
		}
		outdated := false
		if len(item.RuntimeVersions) == 0 {
			failures = append(failures, fmt.Errorf("Workshop %s 未能读取运行机器上的版本", item.ID))
		}
		for _, target := range item.RuntimeVersions {
			status := target.Status
			if status == "unknown" || status == "current" || status == "outdated" {
				status = runtimeModVersionStatus(shared.RuntimeModFileState{
					Status: shared.RuntimeModFileReady, Version: target.Version,
					SteamManifestID: target.SteamManifestID, SteamUpdatedAt: target.SteamUpdatedAt,
				}, latest.SteamManifestID, latest.Version, latest.UpdatedAt)
			}
			switch status {
			case "outdated":
				outdated = true
			case "current":
			default:
				failures = append(failures, fmt.Errorf("Workshop %s 在 %s/%s 的版本未确认：%s", item.ID, target.TargetID, target.InstallationID, status))
			}
		}
		if outdated {
			updates = append(updates, item.ID)
		}
	}
	return mods.ActionResult{ModIDs: updates, Message: fmt.Sprintf("检测到 %d 个 Mod 可更新", len(updates))}, errors.Join(failures...)
}

func (s *Service) InstallationModInventory(ctx context.Context, targetID, installationID string) (mods.RuntimeInstallationModInventory, error) {
	return s.installationModInventory(ctx, targetID, installationID, false)
}

// InstallationModFacts reads node-owned files without Workshop or a fleet-wide
// room-reference scan. The room catalog already knows its configured worlds.
func (s *Service) InstallationModFacts(ctx context.Context, targetID, installationID string) (mods.RuntimeInstallationModInventory, error) {
	return s.installationModInventory(ctx, targetID, installationID, true)
}

func (s *Service) installationModInventory(ctx context.Context, targetID, installationID string, factsOnly bool) (mods.RuntimeInstallationModInventory, error) {
	targetID = strings.TrimSpace(targetID)
	installationID = strings.TrimSpace(installationID)
	result := mods.RuntimeInstallationModInventory{TargetID: targetID, InstallationID: installationID, Items: []mods.RuntimeInstallationMod{}}
	if targetID == "" || installationID == "" || strings.ContainsAny(targetID+installationID, "\x00\r\n") || s.runtimeFiles == nil {
		return result, ErrInvalidRequest
	}
	observed, err := s.runtimeFiles.ObserveRuntimeModInventory(ctx, targetID, installationID)
	if err != nil {
		return result, err
	}
	if observed == nil || observed.InstallationID != installationID {
		return result, errors.New("运行机器返回的 Mod 安装目录清单无效")
	}
	result.ObservedAt = observed.ObservedAt.UTC()
	ids := make([]string, 0, len(observed.Mods))
	for workshopID := range observed.Mods {
		ids = append(ids, workshopID)
	}
	sort.Strings(ids)
	var metadata map[string]mods.SteamMod
	var references map[string][]mods.RuntimeModRoomReference
	if !factsOnly {
		var metadataErr, referencesErr error
		metadata, metadataErr = s.mods.Describe(ctx, ids)
		if metadataErr != nil {
			result.MetadataWarning = metadataErr.Error()
		}
		references, referencesErr = s.installationModReferences(ctx, targetID, installationID)
		if referencesErr != nil {
			result.ReferencesWarning = referencesErr.Error()
		}
	}
	for _, workshopID := range ids {
		state := observed.Mods[workshopID]
		item := mods.RuntimeInstallationMod{
			SteamMod: metadata[workshopID], CurrentVersion: strings.TrimSpace(state.Version),
			LatestVersion: strings.TrimSpace(metadata[workshopID].Version), LatestSteamManifestID: strings.TrimSpace(metadata[workshopID].SteamManifestID),
			FileStatus: string(state.Status),
			FileReason: state.Reason, InstalledSize: state.InstalledSize,
			SteamManifestID: state.SteamManifestID, SteamUpdatedAt: cloneRuntimeVersionTime(state.SteamUpdatedAt),
			MetadataReason: state.MetadataReason, RoomReferences: references[workshopID],
		}
		item.ID = workshopID
		if strings.TrimSpace(state.Name) != "" {
			item.Name = strings.TrimSpace(state.Name)
		}
		if strings.TrimSpace(item.Name) == "" {
			item.Name = "Workshop " + workshopID
		}
		if state.Status == shared.RuntimeModFileReady {
			item.VersionStatus = runtimeModVersionStatus(state, item.LatestSteamManifestID, item.LatestVersion, item.UpdatedAt)
			switch item.VersionStatus {
			case "current":
				result.Current++
			case "outdated":
				result.Outdated++
			default:
				result.Unknown++
			}
		} else {
			item.VersionStatus = string(state.Status)
			result.Invalid++
		}
		result.Items = append(result.Items, item)
	}
	sort.SliceStable(result.Items, func(left, right int) bool {
		leftName := strings.ToLower(result.Items[left].Name)
		rightName := strings.ToLower(result.Items[right].Name)
		if leftName == rightName {
			return result.Items[left].ID < result.Items[right].ID
		}
		return leftName < rightName
	})
	result.Total = len(result.Items)
	return result, nil
}

func (s *Service) installationModReferences(ctx context.Context, targetID, installationID string) (map[string][]mods.RuntimeModRoomReference, error) {
	key := targetID + "\x00" + installationID
	snapshotCtx := withPublicationSnapshot(ctx, proposal{
		action: "reconcile", worldIDs: map[string]bool{}, installationOnly: map[string]bool{key: true}, readOnly: true,
	})
	managed, err := s.source.ManagedWorlds(snapshotCtx)
	if err != nil {
		return map[string][]mods.RuntimeModRoomReference{}, err
	}
	roomValues, err := s.source.rooms.List()
	if err != nil {
		return map[string][]mods.RuntimeModRoomReference{}, err
	}
	roomNames := make(map[string]string, len(roomValues))
	worldNames := make(map[string]string)
	for _, room := range roomValues {
		roomNames[room.ID] = room.Name
		worlds, worldsErr := s.source.rooms.Worlds(room.ID)
		if worldsErr != nil {
			continue
		}
		for _, world := range worlds {
			worldNames[room.ID+"\x00"+world.ID] = world.Name
		}
	}
	result := make(map[string][]mods.RuntimeModRoomReference)
	seen := make(map[string]bool)
	for _, world := range managed {
		for _, requirement := range world.Mods {
			identity := requirement.WorkshopID + "\x00" + world.RoomID + "\x00" + world.WorldID
			if seen[identity] {
				continue
			}
			seen[identity] = true
			result[requirement.WorkshopID] = append(result[requirement.WorkshopID], mods.RuntimeModRoomReference{
				RoomID: world.RoomID, RoomName: roomNames[world.RoomID],
				WorldID: world.WorldID, WorldName: worldNames[world.RoomID+"\x00"+world.WorldID],
			})
		}
	}
	for workshopID := range result {
		sort.Slice(result[workshopID], func(left, right int) bool {
			leftRef, rightRef := result[workshopID][left], result[workshopID][right]
			if leftRef.RoomName == rightRef.RoomName {
				return leftRef.WorldName < rightRef.WorldName
			}
			return leftRef.RoomName < rightRef.RoomName
		})
	}
	return result, nil
}

func (s *Service) InstallationRoomIDs(ctx context.Context, targetID, installationID string) ([]string, error) {
	targetID = strings.TrimSpace(targetID)
	installationID = strings.TrimSpace(installationID)
	if targetID == "" || installationID == "" || len(targetID) > 128 || len(installationID) > 128 ||
		strings.ContainsAny(targetID+installationID, "\x00\r\n") {
		return nil, ErrInvalidRequest
	}
	roomValues, err := s.source.rooms.List()
	if err != nil {
		return nil, err
	}
	roomIDs := make([]string, 0)
	for _, room := range roomValues {
		if !room.Managed {
			continue
		}
		executions, err := s.source.topology.ResolveCachedRoomExecutions(ctx, room.ID)
		if err != nil {
			return nil, err
		}
		for _, execution := range executions {
			placement := publicationPlacement(execution, room.ID, execution.World.ID)
			if placement.TargetID == targetID && placement.InstallationID == installationID {
				roomIDs = append(roomIDs, room.ID)
				break
			}
		}
	}
	sort.Strings(roomIDs)
	return roomIDs, nil
}

type runtimeFileGroup struct {
	request     RuntimeFileRequest
	worldIDs    map[string]bool
	workshopIDs map[string]bool
	observation *shared.RuntimeModFilesObservation
	err         error
}

func (s *Service) observeRoomModFiles(ctx context.Context, room rooms.Room, worlds []rooms.World, placements modpublication.PlacementSnapshot, result *mods.ModList) {
	if len(result.Items) == 0 {
		return
	}
	worldByID := make(map[string]rooms.World, len(worlds))
	for _, world := range worlds {
		worldByID[world.ID] = world
	}
	modsByWorld := make(map[string][]string, len(worlds))
	for _, item := range result.Items {
		for _, worldID := range item.EnabledWorlds {
			modsByWorld[worldID] = append(modsByWorld[worldID], item.ID)
		}
	}
	groups := make(map[string]*runtimeFileGroup)
	for _, placement := range placements.Placements {
		if placement.RoomID != room.ID || len(modsByWorld[placement.WorldID]) == 0 {
			continue
		}
		world, exists := worldByID[placement.WorldID]
		if !exists {
			continue
		}
		key := placement.TargetID + "\x00" + placement.InstallationID
		group := groups[key]
		if group == nil {
			group = &runtimeFileGroup{
				request:  RuntimeFileRequest{TargetID: placement.TargetID, InstallationID: placement.InstallationID, TopologyRevision: placements.TopologyRevision},
				worldIDs: make(map[string]bool), workshopIDs: make(map[string]bool),
			}
			groups[key] = group
		}
		group.worldIDs[world.ID] = true
		group.request.Worlds = append(group.request.Worlds, RuntimeFileWorld{
			RoomID: room.ID, RoomDirectory: room.DirectoryName,
			WorldID: world.ID, WorldDirectory: world.DirectoryName,
		})
		for _, workshopID := range modsByWorld[world.ID] {
			group.workshopIDs[workshopID] = true
		}
	}
	groupKeys := make([]string, 0, len(groups))
	for key := range groups {
		groupKeys = append(groupKeys, key)
	}
	sort.Strings(groupKeys)
	var wait sync.WaitGroup
	limit := make(chan struct{}, 4)
	for _, key := range groupKeys {
		group := groups[key]
		for workshopID := range group.workshopIDs {
			group.request.WorkshopIDs = append(group.request.WorkshopIDs, workshopID)
		}
		sort.Strings(group.request.WorkshopIDs)
		if s.runtimeFiles == nil {
			group.err = errors.New("Mod 文件观察器未配置")
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				group.err = ctx.Err()
				return
			}
			group.observation, group.err = s.runtimeFiles.ObserveRuntimeModFiles(ctx, group.request)
		}()
	}
	wait.Wait()
	for index := range result.Items {
		item := &result.Items[index]
		item.RuntimeObserved = false
		item.RuntimeFileStatus = ""
		item.RuntimeReadyTargets = 0
		item.RuntimePendingTargets = 0
		item.RuntimeUnavailableTargets = 0
		item.RuntimeTotalTargets = 0
		item.RuntimeVersion = ""
		item.RuntimeVersionStatus = ""
		item.RuntimeCurrentTargets = 0
		item.RuntimeOutdatedTargets = 0
		item.RuntimeUnknownTargets = 0
		item.RuntimeVersions = []mods.RuntimeModVersionTarget{}
		enabledWorlds := make(map[string]bool, len(item.EnabledWorlds))
		for _, worldID := range item.EnabledWorlds {
			enabledWorlds[worldID] = true
		}
		targets := make(map[string]*runtimeFileGroup)
		for key, group := range groups {
			for worldID := range group.worldIDs {
				if enabledWorlds[worldID] {
					targets[key] = group
					break
				}
			}
		}
		item.RuntimeTotalTargets = len(targets)
		item.InstalledWorlds = []string{}
		item.LoadedWorlds = []string{}
		targetKeys := make([]string, 0, len(targets))
		for key := range targets {
			targetKeys = append(targetKeys, key)
		}
		sort.Strings(targetKeys)
		versions := make(map[string]bool)
		runtimeName := ""
		for _, targetKey := range targetKeys {
			group := targets[targetKey]
			fact := mods.RuntimeModVersionTarget{
				TargetID: group.request.TargetID, InstallationID: group.request.InstallationID, Status: "unavailable",
			}
			if group.err != nil || group.observation == nil {
				item.RuntimeUnavailableTargets++
				item.RuntimeVersions = append(item.RuntimeVersions, fact)
				continue
			}
			state, exists := group.observation.Mods[item.ID]
			if !exists {
				item.RuntimeUnavailableTargets++
				item.RuntimeVersions = append(item.RuntimeVersions, fact)
				continue
			}
			fact.Version = strings.TrimSpace(state.Version)
			fact.SteamManifestID = state.SteamManifestID
			fact.SteamUpdatedAt = cloneRuntimeVersionTime(state.SteamUpdatedAt)
			fact.MetadataReason = state.MetadataReason
			if state.Status == shared.RuntimeModFileReady && state.Reason == "" {
				item.RuntimeReadyTargets++
				fact.Status = runtimeModVersionStatus(state, item.SteamManifestID, item.LatestVersion, item.UpdatedAt)
				switch fact.Status {
				case "current":
					item.RuntimeCurrentTargets++
				case "outdated":
					item.RuntimeOutdatedTargets++
				default:
					item.RuntimeUnknownTargets++
				}
				if runtimeName == "" && strings.TrimSpace(state.Name) != "" {
					runtimeName = strings.TrimSpace(state.Name)
				}
				if fact.Version != "" {
					versions[fact.Version] = true
				}
				for worldID := range group.worldIDs {
					if enabledWorlds[worldID] {
						item.InstalledWorlds = append(item.InstalledWorlds, worldID)
					}
				}
			} else {
				item.RuntimePendingTargets++
				fact.Status = string(state.Status)
				if state.Status == shared.RuntimeModFileReady && state.Reason != "" {
					fact.Status, fact.MetadataReason = "not_installed", state.Reason
				}
			}
			item.RuntimeVersions = append(item.RuntimeVersions, fact)
			for key, worldState := range group.observation.Worlds {
				_ = key
				if !enabledWorlds[worldState.WorldID] || !containsStringValue(worldState.LoadedModIDs, item.ID) {
					continue
				}
				item.LoadedWorlds = append(item.LoadedWorlds, worldState.WorldID)
			}
		}
		if runtimeName != "" {
			item.Name = runtimeName
		}
		sort.Strings(item.InstalledWorlds)
		item.InstalledWorlds = uniqueSortedStrings(item.InstalledWorlds)
		sort.Strings(item.LoadedWorlds)
		item.LoadedWorlds = uniqueSortedStrings(item.LoadedWorlds)
		item.RuntimeObserved = item.RuntimeTotalTargets > 0 && item.RuntimeUnavailableTargets == 0
		item.Installed = item.RuntimeTotalTargets > 0 && item.RuntimeReadyTargets == item.RuntimeTotalTargets
		item.Loaded = len(item.LoadedWorlds) > 0
		if len(versions) == 1 {
			for version := range versions {
				item.RuntimeVersion = version
			}
		}
		switch {
		case len(versions) > 1:
			item.RuntimeVersionStatus = "mixed"
		case item.RuntimeOutdatedTargets > 0:
			item.RuntimeVersionStatus = "outdated"
		case item.RuntimeReadyTargets > 0 && item.RuntimeCurrentTargets == item.RuntimeReadyTargets:
			item.RuntimeVersionStatus = "current"
		case item.RuntimeReadyTargets > 0:
			item.RuntimeVersionStatus = "unknown"
		case item.RuntimeUnavailableTargets > 0:
			item.RuntimeVersionStatus = "unavailable"
		default:
			item.RuntimeVersionStatus = "missing"
		}
		switch {
		case !item.Enabled:
			item.RuntimeFileStatus = "disabled"
		case item.RuntimeTotalTargets == 0:
			item.RuntimeFileStatus = "unavailable"
		case item.RuntimeUnavailableTargets > 0:
			item.RuntimeFileStatus = "unavailable"
		case item.RuntimePendingTargets > 0:
			item.RuntimeFileStatus = "pending"
		default:
			item.RuntimeFileStatus = "ready"
		}
		applyRuntimeModHealth(item)
	}
}

func runtimeModVersionStatus(state shared.RuntimeModFileState, latestManifestID, latestVersion string, latestUpdatedAt time.Time) string {
	if state.Status != shared.RuntimeModFileReady {
		return string(state.Status)
	}
	currentManifestID := strings.TrimSpace(state.SteamManifestID)
	latestManifestID = strings.TrimSpace(latestManifestID)
	if currentManifestID != "" && latestManifestID != "" {
		if currentManifestID == latestManifestID {
			return "current"
		}
		return "outdated"
	}
	if state.SteamUpdatedAt != nil && !latestUpdatedAt.IsZero() {
		if latestUpdatedAt.After(state.SteamUpdatedAt.Add(time.Second)) {
			return "outdated"
		}
		return "current"
	}
	currentVersion := strings.TrimSpace(state.Version)
	latestVersion = strings.TrimSpace(latestVersion)
	if currentVersion != "" && latestVersion != "" {
		if currentVersion == latestVersion {
			return "current"
		}
		return "outdated"
	}
	return "unknown"
}

func cloneRuntimeVersionTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func applyRuntimeModHealth(item *mods.ModState) {
	switch item.RuntimeFileStatus {
	case "disabled":
		item.Health, item.HealthMessage, item.RepairAction = mods.HealthDisabled, "所有已配置分片均已禁用", ""
	case "pending":
		item.Health, item.HealthMessage, item.RepairAction = mods.HealthNotDownloaded, "运行机器缺少模组文件", "repair"
		for _, target := range item.RuntimeVersions {
			if target.MetadataReason == "local_mod_entry_missing" || target.MetadataReason == "local_mod_entry_invalid" {
				item.Health, item.HealthMessage = mods.HealthNotInstalled, "模组文件已下载，但本地加载入口未就绪"
				break
			}
		}
	case "unavailable":
		item.Health, item.HealthMessage, item.RepairAction = mods.HealthNotInstalled, "运行机器模组状态不可用", ""
	case "ready":
		if item.RuntimeOutdatedTargets > 0 {
			item.Health, item.HealthMessage, item.RepairAction = mods.HealthUpdateAvailable, "运行机器上的模组版本落后于 Steam Workshop", "update"
		} else {
			item.Health, item.HealthMessage, item.RepairAction = mods.HealthHealthy, "运行机器模组文件正常", ""
		}
	}
}

func containsStringValue(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func uniqueSortedStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func (s *Service) RoomProfile(ctx context.Context, roomID string) (mods.RoomModProfile, error) {
	if _, err := s.source.rooms.Room(roomID); err != nil {
		return mods.RoomModProfile{}, err
	}
	worlds, err := s.source.rooms.Worlds(roomID)
	if err != nil {
		return mods.RoomModProfile{}, err
	}
	prepared := proposal{roomID: roomID, roomOnly: true, action: "reconcile", worldIDs: map[string]bool{}, readOnly: true}
	contents, err := s.source.Contents(withPublicationSnapshot(ctx, prepared), roomID)
	if err != nil {
		return mods.RoomModProfile{}, err
	}
	profile, err := roomProfileFromContents(roomID, worlds, contents)
	if err != nil {
		return mods.RoomModProfile{}, err
	}
	return profile, s.applyConfigurationModes(&profile)
}

func roomProfileFromContents(roomID string, worlds []rooms.World, contents map[string][]byte) (mods.RoomModProfile, error) {
	inputs := make([]mods.RoomProfileWorldInput, 0, len(worlds))
	for _, world := range worlds {
		content, exists := contents[world.ID]
		if !exists {
			return mods.RoomModProfile{}, rooms.ErrWorldNotFound
		}
		inputs = append(inputs, mods.RoomProfileWorldInput{
			WorldID: world.ID, Name: world.Name, Master: world.IsMaster, Content: content,
		})
	}
	return mods.DeriveRoomModProfile(roomID, inputs)
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
	configuration, _, err := s.source.readConfiguration(ctx, roomID, worldID, modID)
	return configuration, err
}

func (s *Service) PreviewConfiguration(ctx context.Context, roomID, worldID, modID string, request mods.ConfigUpdateRequest) (mods.ConfigPreview, error) {
	configuration, content, err := s.source.readConfiguration(ctx, roomID, worldID, modID)
	if err != nil {
		return mods.ConfigPreview{}, err
	}
	result, err := mods.MutateModOverride(content, mods.OverrideMutation{
		Action: mods.OverrideActionConfigure, ModID: modID, Enabled: request.Enabled, PreserveEnabled: request.PreserveEnabled,
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

func (s *Service) ApplyConfiguration(ctx context.Context, roomID, worldID, modID string, request mods.ConfigUpdateRequest) (mods.ConfigApplyResult, error) {
	worldIDs := append([]string(nil), request.WorldIDs...)
	if len(worldIDs) == 0 {
		worldIDs = []string{worldID}
	}
	prepared, err := s.prepare(ctx, roomID, Request{
		Action: "configure", ModID: modID, WorldIDs: worldIDs, Enabled: request.Enabled, PreserveEnabled: request.PreserveEnabled,
		SourceWorldID:                  request.SourceWorldID,
		ExpectedConfigurationRevision:  request.ExpectedRevision,
		ExpectedConfigurationRevisions: request.ExpectedRevisions,
		ExpectedTopologyRevision:       request.ExpectedTopologyRevision,
		Patch:                          request.Patch,
	})
	if err != nil {
		return mods.ConfigApplyResult{}, err
	}
	prepared.roomOnly = true
	return s.applyPreparedOverrides(ctx, prepared, request.ExpectedTopologyRevision)
}

func (s *Service) worldContent(ctx context.Context, roomID, worldID string) ([]byte, error) {
	prepared := proposal{roomID: roomID, roomOnly: true, action: "reconcile", worldIDs: map[string]bool{}, readOnly: true}
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
		configurationSourceWorldID: strings.TrimSpace(request.SourceWorldID),
		enabled:                    request.Enabled, preserveEnabled: request.PreserveEnabled, revision: strings.TrimSpace(request.ExpectedConfigurationRevision),
		revisions: cloneStringMap(request.ExpectedConfigurationRevisions),
		patch:     clonePatch(request.Patch), worldIDs: make(map[string]bool),
	}
	for _, worldID := range request.WorldIDs {
		worldID = strings.TrimSpace(worldID)
		if worldID == "" || prepared.worldIDs[worldID] {
			return proposal{}, ErrInvalidRequest
		}
		prepared.worldIDs[worldID] = true
	}
	if prepared.configurationSourceWorldID != "" && (prepared.action != mods.OverrideActionConfigure || !prepared.worldIDs[prepared.configurationSourceWorldID]) {
		return proposal{}, ErrInvalidRequest
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
		if !mods.ValidID(prepared.modID) || len(prepared.worldIDs) == 0 {
			return proposal{}, ErrInvalidRequest
		}
		if len(prepared.worldIDs) == 1 && len(prepared.revisions) == 0 {
			if prepared.revision == "" {
				return proposal{}, ErrInvalidRequest
			}
			break
		}
		if len(prepared.revisions) != len(prepared.worldIDs) {
			return proposal{}, ErrInvalidRequest
		}
		for worldID, revision := range prepared.revisions {
			if !prepared.worldIDs[worldID] || strings.TrimSpace(revision) == "" {
				return proposal{}, ErrInvalidRequest
			}
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

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return result
}
