package modcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dont/internal/moddistribution"
	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/operationprogress"
	"dont/internal/runtimedriver"
	"dont/shared"

	"github.com/shirou/gopsutil/v3/disk"
)

const localModRuntimeVersion = "1.0.0"

const modRuntimeLeaseRefreshWindow = time.Minute

type ContentSource struct {
	manager              *moddistribution.Manager
	workshopRoot         string
	workshopManifestPath string
	mu                   sync.Mutex
}

func NewContentSource(manager *moddistribution.Manager, workshopRoot string) (*ContentSource, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(workshopRoot))
	if manager == nil || err != nil || strings.TrimSpace(workshopRoot) == "" {
		return nil, ErrInvalidRequest
	}
	root := filepath.Clean(absolute)
	appID := filepath.Base(root)
	manifestPath := ""
	if filepath.Base(filepath.Dir(root)) == "content" {
		manifestPath = filepath.Join(filepath.Dir(filepath.Dir(root)), "appworkshop_"+appID+".acf")
	}
	return &ContentSource{manager: manager, workshopRoot: root, workshopManifestPath: manifestPath}, nil
}

func (s *ContentSource) Resolve(ctx context.Context, requirement modpublication.ModRequirement) (modpublication.ContentArtifact, error) {
	if artifact, ok := proposalArtifact(ctx, requirement); ok {
		return artifact, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	metadata := moddistribution.Metadata{}
	artifact := modpublication.ContentArtifact{}
	if s.workshopManifestPath != "" {
		item, exists, manifestErr := mods.ReadWorkshopManifestItem(s.workshopManifestPath, requirement.WorkshopID)
		if manifestErr != nil {
			return artifact, manifestErr
		}
		if exists {
			metadata.PublishedFileSize = item.Size
			metadata.SteamManifestID = item.ManifestID
			metadata.SteamUpdatedAt = item.UpdatedAt
		}
	}
	manifest, err := s.manager.Import(ctx, requirement.WorkshopID, filepath.Join(s.workshopRoot, requirement.WorkshopID), metadata)
	if err != nil {
		return modpublication.ContentArtifact{}, err
	}
	if requirement.TreeSHA256 != "" && !strings.EqualFold(requirement.TreeSHA256, manifest.TreeSHA256) {
		return modpublication.ContentArtifact{}, modpublication.ErrPlanChanged
	}
	return modpublication.ContentArtifact{
		WorkshopID: requirement.WorkshopID, TreeSHA256: manifest.TreeSHA256,
		ManifestSHA256: manifest.ManifestSHA256, Size: manifest.Size, FileCount: manifest.FileCount,
		SourceRef:       "cache://" + requirement.WorkshopID + "/" + manifest.TreeSHA256,
		SteamManifestID: metadata.SteamManifestID, SteamUpdatedAt: metadata.SteamUpdatedAt,
	}, nil
}

type Runtime struct {
	source          *SnapshotSource
	manager         *moddistribution.Manager
	remote          runtimedriver.ModDriver
	cacheRoot       string
	tempRoot        string
	transferMu      sync.Mutex
	transferLocks   map[string]*sync.Mutex
	convergenceMu   sync.Mutex
	convergenceRuns map[string]*modCacheConvergence
	mutations       runtimedriver.RuntimeMutationObserver
	replicas        *modpublication.ReplicaStore
	artifacts       ControllerArtifactSource
}

func (r *Runtime) DownloadInstallationMods(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation, workshopIDs []string) error {
	if target.TargetID == "local" || len(workshopIDs) == 0 {
		return ErrInvalidRequest
	}
	downloader, ok := any(r.remote).(runtimedriver.ModDownloadDriver)
	if !ok {
		return runtimedriver.ErrCapabilityMissing
	}
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	step, err := session.step(ctx, "download")
	if err != nil {
		return err
	}
	return downloader.DownloadMods(ctx, targetRuntimeTarget(target, operation.TopologyRevision), step, workshopIDs)
}

func (r *Runtime) LinkInstallationMods(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation, workshopIDs []string) error {
	if target.TargetID == "local" {
		return r.manager.LinkLocalMods(ctx, target.InstallationID, workshopIDs)
	}
	linker, ok := any(r.remote).(runtimedriver.ModLocalLinkDriver)
	if !ok {
		return runtimedriver.ErrCapabilityMissing
	}
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	step, err := session.step(ctx, "local-link")
	if err != nil {
		return err
	}
	return linker.LinkMods(ctx, targetRuntimeTarget(target, operation.TopologyRevision), step, workshopIDs)
}

// FetchLatestArtifact asks the selected Runtime to download the current
// Workshop item and returns the exact artifact it received. The Controller
// does not need to keep a second copy for remotely placed rooms.
func (r *Runtime) FetchLatestArtifact(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation, workshopID string, metadata shared.RuntimeModMetadata) (modpublication.ContentArtifact, error) {
	if target.TargetID == "local" || !mods.ValidID(workshopID) {
		return modpublication.ContentArtifact{}, ErrInvalidRequest
	}
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return modpublication.ContentArtifact{}, err
	}
	step, err := session.step(ctx, "latest:"+workshopID)
	if err != nil {
		return modpublication.ContentArtifact{}, err
	}
	result, err := r.remote.FetchModCache(ctx, targetRuntimeTarget(target, operation.TopologyRevision), step, workshopID, "", metadata, nil)
	if err != nil {
		return modpublication.ContentArtifact{}, err
	}
	manifest := result.Manifest
	if manifest.WorkshopID != workshopID || !validModArtifactDigest(manifest.TreeSHA256) ||
		!validModArtifactDigest(manifest.ManifestSHA256) || manifest.Size < 0 || manifest.FileCount < 0 {
		return modpublication.ContentArtifact{}, errors.New("运行机器返回了无效的 Mod 缓存 manifest")
	}
	return modpublication.ContentArtifact{
		WorkshopID: workshopID, TreeSHA256: strings.ToLower(manifest.TreeSHA256),
		ManifestSHA256: strings.ToLower(manifest.ManifestSHA256), Size: manifest.Size, FileCount: manifest.FileCount,
		SourceRef:       "runtime://" + target.TargetID + "/" + target.InstallationID + "/" + workshopID + "/" + strings.ToLower(manifest.TreeSHA256),
		SteamManifestID: manifest.Metadata.SteamManifestID, SteamUpdatedAt: manifest.Metadata.SteamUpdatedAt,
	}, nil
}

func validModArtifactDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

type ControllerArtifactSource interface {
	Issue(context.Context, string, string, string, time.Duration) (shared.RuntimeModFetchLocation, string, error)
	Revoke(string)
}

type modCacheConvergence struct {
	done    chan struct{}
	err     error
	waiters int
}

func NewRuntime(source *SnapshotSource, manager *moddistribution.Manager, remote runtimedriver.ModDriver, cacheRoot, tempRoot string) (*Runtime, error) {
	cacheAbsolute, cacheErr := filepath.Abs(strings.TrimSpace(cacheRoot))
	tempAbsolute, tempErr := filepath.Abs(strings.TrimSpace(tempRoot))
	if source == nil || manager == nil || remote == nil || cacheErr != nil || tempErr != nil || strings.TrimSpace(cacheRoot) == "" || strings.TrimSpace(tempRoot) == "" {
		return nil, ErrInvalidRequest
	}
	if err := os.MkdirAll(tempAbsolute, 0o700); err != nil {
		return nil, err
	}
	return &Runtime{
		source: source, manager: manager, remote: remote, cacheRoot: cacheAbsolute, tempRoot: tempAbsolute,
		transferLocks: make(map[string]*sync.Mutex), convergenceRuns: make(map[string]*modCacheConvergence),
	}, nil
}

func (r *Runtime) ConfigureMutationObserver(observer runtimedriver.RuntimeMutationObserver) error {
	if observer == nil {
		return errors.New("Mod Runtime mutation observer is required")
	}
	r.mutations = observer
	return nil
}

func (r *Runtime) ConfigureReplicaStore(store *modpublication.ReplicaStore) error {
	if store == nil {
		return errors.New("Mod replica store is required")
	}
	r.replicas = store
	return nil
}

func (r *Runtime) ConfigureArtifactSource(source ControllerArtifactSource) error {
	if source == nil {
		return errors.New("Mod artifact source is required")
	}
	r.artifacts = source
	return nil
}

func (r *Runtime) Observe(ctx context.Context, placement modpublication.AppliedPlacement) (modpublication.RuntimeObservation, error) {
	execution, exists, err := r.source.Execution(ctx, placement.TargetID, placement.InstallationID)
	if err != nil || !exists {
		return modpublication.RuntimeObservation{}, errors.Join(err, errors.New("runtime placement is missing from publication snapshot"))
	}
	result := modpublication.RuntimeObservation{
		TargetID: placement.TargetID, NodeID: placement.NodeID, InstallationID: placement.InstallationID,
		Online: true, Capabilities: []string{modpublication.RequiredCapability, modpublication.StateCapability}, Version: localModRuntimeVersion,
	}
	if placement.TargetID == "local" {
		usage, err := disk.Usage(r.cacheRoot)
		if err != nil {
			return modpublication.RuntimeObservation{}, err
		}
		result.AvailableBytes = int64(usage.Free)
		return result, nil
	}
	result.Online = execution.Target.Online
	result.Capabilities = append([]string(nil), execution.Target.Capabilities...)
	available, version, err := r.remote.ObserveModTarget(ctx, runtimeTarget(placement, execution.Revision))
	if err != nil {
		return modpublication.RuntimeObservation{}, err
	}
	result.AvailableBytes, result.Version = available, version
	return result, nil
}

func (r *Runtime) Converged(ctx context.Context, plan modpublication.Plan) (bool, error) {
	if r.replicas != nil {
		if err := r.replicas.SetDesired(plan); err != nil {
			return false, err
		}
	}
	for _, target := range plan.Targets {
		var state *shared.RuntimeModInstallationState
		if target.TargetID == "local" {
			observed, err := r.manager.ObserveState(ctx, target.InstallationID)
			if errors.Is(err, moddistribution.ErrNotFound) || errors.Is(err, moddistribution.ErrConflict) {
				if r.replicas != nil {
					_ = r.replicas.ObserveTarget(target, plan.PlanHash, nil)
				}
				return false, nil
			}
			if err != nil {
				if r.replicas != nil {
					_ = r.replicas.MarkTargetError(target, plan.PlanHash, modpublication.ReplicaStageObserve, err)
				}
				return false, err
			}
			state = runtimeInstallationState(observed)
		} else {
			if !containsRuntimeCapability(target.Capabilities, modpublication.StateCapability) {
				if r.replicas != nil {
					_ = r.replicas.MarkTargetError(target, plan.PlanHash, modpublication.ReplicaStageObserve, errors.New("运行节点不支持 Mod 状态回读"))
				}
				return false, nil
			}
			_, exists, err := r.source.Execution(ctx, target.TargetID, target.InstallationID)
			if err != nil || !exists {
				if r.replicas != nil {
					_ = r.replicas.MarkTargetError(target, plan.PlanHash, modpublication.ReplicaStageObserve, errors.Join(err, errors.New("runtime placement is missing from publication snapshot")))
				}
				return false, errors.Join(err, errors.New("runtime placement is missing from publication snapshot"))
			}
			state, err = r.remote.ModInstallationState(ctx, targetRuntimeTarget(target, plan.TopologyRevision))
			if missingRemoteInstallationState(err) || conflictingRemoteInstallationState(err) {
				if r.replicas != nil {
					_ = r.replicas.ObserveTarget(target, plan.PlanHash, nil)
				}
				return false, nil
			}
			if err != nil {
				if r.replicas != nil {
					_ = r.replicas.MarkTargetError(target, plan.PlanHash, modpublication.ReplicaStageObserve, err)
				}
				return false, err
			}
		}
		if r.replicas != nil {
			if err := r.replicas.ObserveTarget(target, plan.PlanHash, replicaObservation(state)); err != nil {
				return false, err
			}
		}
		if !installationStateMatches(state, target) {
			return false, nil
		}
	}
	return true, nil
}

func (r *Runtime) ObserveRuntimeModFiles(ctx context.Context, request RuntimeFileRequest) (*shared.RuntimeModFilesObservation, error) {
	if len(request.WorkshopIDs) == 0 || request.TargetID == "" || request.InstallationID == "" {
		return nil, ErrInvalidRequest
	}
	worlds := make([]moddistribution.ObserveWorld, 0, len(request.Worlds))
	remoteWorlds := make([]shared.RuntimeModObserveWorld, 0, len(request.Worlds))
	for _, world := range request.Worlds {
		worlds = append(worlds, moddistribution.ObserveWorld{
			RoomID: world.RoomID, RoomDirectory: world.RoomDirectory,
			WorldID: world.WorldID, WorldDirectory: world.WorldDirectory,
		})
		remoteWorlds = append(remoteWorlds, shared.RuntimeModObserveWorld{
			RoomID: world.RoomID, RoomDirectory: world.RoomDirectory,
			WorldID: world.WorldID, WorldDirectory: world.WorldDirectory,
		})
	}
	if request.TargetID == "local" {
		observed, err := r.manager.ObserveFiles(ctx, request.InstallationID, request.WorkshopIDs, worlds)
		if err != nil {
			return nil, err
		}
		return runtimeFilesObservation(observed), nil
	}
	execution, exists, err := r.source.Execution(ctx, request.TargetID, request.InstallationID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("运行位置没有对应的安装实例")
	}
	if !execution.Target.Online {
		return nil, errors.New("运行机器离线")
	}
	if !containsRuntimeCapability(execution.Target.Capabilities, "runtime.mods.files.v1") {
		return nil, errors.New("运行节点版本不支持实际 Mod 文件读取")
	}
	target := runtimedriver.Target{
		TargetID: request.TargetID, InstallationID: request.InstallationID,
		TopologyRevision: request.TopologyRevision,
	}
	if len(request.Worlds) > 0 {
		target.RoomID, target.WorldID = request.Worlds[0].RoomID, request.Worlds[0].WorldID
		target.Cluster, target.Shard = request.Worlds[0].RoomDirectory, request.Worlds[0].WorldDirectory
	}
	return r.remote.ObserveModFiles(ctx, target, request.WorkshopIDs, remoteWorlds)
}

func (r *Runtime) ObserveRuntimeModInventory(ctx context.Context, targetID, installationID string) (*shared.RuntimeModFilesObservation, error) {
	targetID = strings.TrimSpace(targetID)
	installationID = strings.TrimSpace(installationID)
	if targetID == "" || installationID == "" {
		return nil, ErrInvalidRequest
	}
	if targetID == "local" {
		observed, err := r.manager.InventoryFiles(ctx, installationID)
		if err != nil {
			return nil, err
		}
		return runtimeFilesObservation(observed), nil
	}
	return r.remote.InventoryModFiles(ctx, runtimedriver.Target{
		TargetID: targetID, InstallationID: installationID,
		TopologyRevision: "runtime-mod-inventory-v1",
	})
}

func runtimeFilesObservation(value moddistribution.FilesObservation) *shared.RuntimeModFilesObservation {
	result := &shared.RuntimeModFilesObservation{
		InstallationID: value.InstallationID,
		Mods:           make(map[string]shared.RuntimeModFileState, len(value.Mods)),
		Worlds:         make(map[string]shared.RuntimeModWorldFileState, len(value.Worlds)),
		ObservedAt:     value.ObservedAt,
	}
	for workshopID, state := range value.Mods {
		result.Mods[workshopID] = shared.RuntimeModFileState{
			Status: shared.RuntimeModFileStatus(state.Status), Reason: state.Reason,
			Name: state.Name, Version: state.Version, InstalledSize: state.InstalledSize, SteamManifestID: state.SteamManifestID,
			SteamUpdatedAt: cloneRuntimeFileTime(state.SteamUpdatedAt), MetadataReason: state.MetadataReason,
		}
	}
	for key, world := range value.Worlds {
		result.Worlds[key] = shared.RuntimeModWorldFileState{
			RoomID: world.RoomID, WorldID: world.WorldID,
			LoadedModIDs: append([]string(nil), world.LoadedModIDs...), LogObserved: world.LogObserved,
		}
	}
	return result
}

func cloneRuntimeFileTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func replicaObservation(value *shared.RuntimeModInstallationState) *modpublication.InstallationReplicaObservation {
	if value == nil {
		return nil
	}
	result := &modpublication.InstallationReplicaObservation{
		Mods: make(map[string]string, len(value.Mods)), Worlds: make(map[string]modpublication.WorldReplicaObservation, len(value.Shards)),
		ObservedAt: value.UpdatedAt,
	}
	for workshopID, treeSHA := range value.Mods {
		result.Mods[workshopID] = treeSHA
	}
	for key, shard := range value.Shards {
		world := modpublication.WorldReplicaObservation{ConfigSHA256: shard.ConfigSHA256, Mods: make(map[string]string, len(shard.Mods))}
		for _, mod := range shard.Mods {
			world.Mods[mod.WorkshopID] = mod.TreeSHA256
		}
		result.Worlds[key] = world
	}
	return result
}

func missingRemoteInstallationState(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "installations.json") {
		return false
	}
	return strings.Contains(message, "no such file or directory") ||
		strings.Contains(message, "the system cannot find the file specified")
}

func conflictingRemoteInstallationState(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, moddistribution.ErrConflict) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), strings.ToLower(moddistribution.ErrConflict.Error()))
}

func runtimeInstallationState(value moddistribution.InstallationState) *shared.RuntimeModInstallationState {
	result := &shared.RuntimeModInstallationState{
		InstallationID: value.InstallationID, LastOperationID: value.LastOperationID,
		Mods: make(map[string]string, len(value.Mods)), ManagedSetupSHA256: value.ManagedSetupSHA256,
		WorkshopManifestSHA256: value.WorkshopManifestSHA256,
		Shards:                 make(map[string]shared.RuntimeModShardState, len(value.Shards)), UpdatedAt: value.UpdatedAt,
	}
	for id, hash := range value.Mods {
		result.Mods[id] = hash
	}
	for key, shard := range value.Shards {
		converted := shared.RuntimeModShardState{
			RoomID: shard.RoomID, RoomDirectory: shard.RoomDirectory, WorldID: shard.WorldID,
			WorldDirectory: shard.WorldDirectory, ConfigSHA256: shard.ConfigSHA256,
		}
		for _, mod := range shard.Mods {
			converted.Mods = append(converted.Mods, shared.RuntimeModVersion{
				WorkshopID: mod.WorkshopID, TreeSHA256: mod.TreeSHA256,
				Metadata: shared.RuntimeModMetadata{
					Title: mod.Metadata.Title, Version: mod.Metadata.Version,
					PublishedFileSize: mod.Metadata.PublishedFileSize, SteamManifestID: mod.Metadata.SteamManifestID,
					SteamUpdatedAt: mod.Metadata.SteamUpdatedAt,
				},
			})
		}
		result.Shards[key] = converted
	}
	return result
}

func installationStateMatches(state *shared.RuntimeModInstallationState, target modpublication.TargetPlan) bool {
	if state == nil || state.InstallationID != target.InstallationID || len(state.Mods) != len(target.Mods) || len(state.Shards) != len(target.Worlds) {
		return false
	}
	for _, mod := range target.Mods {
		if !strings.EqualFold(state.Mods[mod.WorkshopID], mod.TreeSHA256) {
			return false
		}
	}
	for _, world := range target.Worlds {
		observed, exists := state.Shards[world.RoomID+"/"+world.WorldID]
		if !exists || observed.RoomID != world.RoomID || observed.RoomDirectory != world.RoomDirectory ||
			observed.WorldID != world.WorldID || observed.WorldDirectory != world.WorldDirectory ||
			!strings.EqualFold(observed.ConfigSHA256, shaHex(world.ModOverrides)) || len(observed.Mods) != len(world.Mods) {
			return false
		}
		versions := make(map[string]string, len(observed.Mods))
		for _, mod := range observed.Mods {
			versions[mod.WorkshopID] = mod.TreeSHA256
		}
		for _, mod := range world.Mods {
			if !strings.EqualFold(versions[mod.WorkshopID], mod.TreeSHA256) {
				return false
			}
		}
	}
	return true
}

func containsRuntimeCapability(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func (r *Runtime) EnsureCache(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	if target.TargetID == "local" {
		for _, artifact := range target.Mods {
			if err := r.ensureArtifactDesired(target, artifact, operation.PlanHash); err != nil {
				return err
			}
			started := time.Now()
			_, err := r.manager.Verify(ctx, artifact.WorkshopID, artifact.TreeSHA256)
			attempts := []modpublication.ReplicaFetchAttempt{fetchAttempt(shared.RuntimeModFetchSourceCache, started, 0, err, "CACHE_MISS")}
			if err != nil {
				_ = r.markArtifactFetchAttempts(target, artifact, operation.PlanHash, completeFetchAttempts(attempts, true, false, r.artifacts != nil))
				return err
			}
			attempts[0].Selected = true
			attempts = completeFetchAttempts(attempts, true, false, r.artifacts != nil)
			if err := r.markArtifactCache(target, artifact, operation.PlanHash, shared.RuntimeModFetchSourceCache, attempts); err != nil {
				return err
			}
		}
		operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModCache, Percent: 100, Message: fmt.Sprintf("本机模组缓存已就绪（%d 个）", len(target.Mods)), TotalItems: len(target.Mods)})
		return nil
	}
	driverTarget := targetRuntimeTarget(target, operation.TopologyRevision)
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	totalBytes := int64(0)
	for _, artifact := range target.Mods {
		totalBytes += artifactProgressSize(artifact)
	}
	completedBytes, lastProgressKey := int64(0), ""
	report := func(index int, artifact modpublication.ContentArtifact, transferred, transferSize, bytesPerSecond int64, message string) {
		artifactSize := artifactProgressSize(artifact)
		currentBytes := completedBytes
		if transferSize > 0 && transferred > 0 {
			if transferred > transferSize {
				transferred = transferSize
			}
			currentBytes += artifactSize * transferred / transferSize
		}
		percent := 100
		if totalBytes > 0 {
			percent = int(currentBytes * 100 / totalBytes)
		}
		if strings.TrimSpace(message) == "" {
			message = fmt.Sprintf("正在同步模组 %d/%d · Workshop %s · %d%%", index+1, len(target.Mods), artifact.WorkshopID, percent)
		}
		progressKey := fmt.Sprintf("%d\x00%s\x00%d\x00%d", percent, message, currentBytes, bytesPerSecond)
		if progressKey == lastProgressKey {
			return
		}
		lastProgressKey = progressKey
		operationprogress.Report(ctx, operationprogress.Update{
			Stage: operationprogress.StageModCache, Percent: percent,
			Message:    message,
			WorkshopID: artifact.WorkshopID, CurrentItem: index + 1, TotalItems: len(target.Mods),
			CurrentBytes: currentBytes, TotalBytes: totalBytes, BytesPerSecond: bytesPerSecond,
		})
	}
	remoteProgressContext := func(index int, artifact modpublication.ContentArtifact, fallbackMessage string) context.Context {
		return operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
			message := strings.TrimSpace(update.Message)
			if message == "" {
				message = fallbackMessage
			} else if fallbackMessage != "" && message != fallbackMessage {
				message = fallbackMessage + " · " + message
			}
			report(index, artifact, int64(update.Percent), 100, update.BytesPerSecond, message)
		})
	}
	for index, artifact := range target.Mods {
		report(index, artifact, 0, 0, 0, "")
		key := strings.Join([]string{target.TargetID, target.InstallationID, artifact.WorkshopID, strings.ToLower(artifact.TreeSHA256)}, "\x00")
		err := r.convergeModCache(ctx, key, func() error {
			if err := r.ensureArtifactDesired(target, artifact, operation.PlanHash); err != nil {
				return err
			}
			attempts := make([]modpublication.ReplicaFetchAttempt, 0, 5)
			persistFailure := func() {
				_ = r.markArtifactFetchAttempts(target, artifact, operation.PlanHash, completeFetchAttempts(attempts, containsRuntimeCapability(target.Capabilities, modpublication.FetchCapability), supportsModPeer(r.remote), r.artifacts != nil))
			}
			inspectStarted := time.Now()
			manifest, inspectErr := r.remote.InspectModCache(ctx, driverTarget, artifact.WorkshopID, artifact.TreeSHA256)
			if inspectErr == nil && cacheManifestMatchesArtifact(manifest, artifact) {
				cacheAttempt := fetchAttempt(shared.RuntimeModFetchSourceCache, inspectStarted, 0, nil, "")
				cacheAttempt.Selected = true
				attempts = mergeFetchAttempts(attempts, cacheAttempt)
				attempts = completeFetchAttempts(attempts, true, supportsModPeer(r.remote), r.artifacts != nil)
				return r.markArtifactCache(target, artifact, operation.PlanHash, shared.RuntimeModFetchSourceCache, attempts)
			}
			if inspectErr == nil {
				inspectErr = errors.New("缓存内容与期望版本不一致")
			}
			attempts = mergeFetchAttempts(attempts, fetchAttempt(shared.RuntimeModFetchSourceCache, inspectStarted, 0, inspectErr, "CACHE_MISS"))
			if containsRuntimeCapability(target.Capabilities, modpublication.FetchCapability) {
				step, stepErr := session.step(ctx, "cache:"+artifact.WorkshopID+":fetch")
				if stepErr != nil {
					persistFailure()
					return stepErr
				}
				steamMessage := fmt.Sprintf("正在由运行节点通过 SteamCMD 获取模组 · Workshop %s", artifact.WorkshopID)
				report(index, artifact, 0, 0, 0, steamMessage)
				fetchStarted := time.Now()
				fetched, fetchErr := r.remote.FetchModCache(remoteProgressContext(index, artifact, steamMessage), driverTarget, step, artifact.WorkshopID, artifact.TreeSHA256, runtimeModMetadata(artifact), nil)
				attempts = mergeRuntimeFetchResult(attempts, shared.RuntimeModFetchSourceSteam, fetchStarted, fetched, fetchErr)
				if fetchErr == nil && cacheManifestMatchesArtifact(fetched.Manifest, artifact) {
					source := selectedFetchSource(fetched, shared.RuntimeModFetchSourceSteam)
					attempts = selectFetchAttempt(attempts, source)
					attempts = completeFetchAttempts(attempts, true, supportsModPeer(r.remote), r.artifacts != nil)
					return r.markArtifactCache(target, artifact, operation.PlanHash, source, attempts)
				}
				if fetchErr == nil {
					fetchErr = errors.New("节点 Steam 返回的缓存 manifest 与期望版本不一致")
					attempts = markFetchAttemptFailed(attempts, shared.RuntimeModFetchSourceSteam, "FETCH_MANIFEST_MISMATCH", fetchErr)
				}
				if ctx.Err() != nil {
					persistFailure()
					return ctx.Err()
				}
				fallbackMessage := fmt.Sprintf("节点 SteamCMD 下载失败，正在选择备用来源 · Workshop %s", artifact.WorkshopID)
				if fetchErr != nil {
					fallbackMessage += " · " + fetchErr.Error()
				}
				report(index, artifact, 0, 0, 0, fallbackMessage)
				peerLocations, peerErr := r.modPeerLocations(ctx, target, artifact)
				if len(peerLocations) > 0 {
					step, stepErr = session.step(ctx, "cache:"+artifact.WorkshopID+":fetch-peer")
					if stepErr != nil {
						persistFailure()
						return stepErr
					}
					peerStarted := time.Now()
					peerMessage := fmt.Sprintf("节点 SteamCMD 下载失败，正在从另一运行节点拉取 · Workshop %s", artifact.WorkshopID)
					report(index, artifact, 0, 0, 0, peerMessage)
					peerFetched, peerFetchErr := r.remote.FetchModCache(remoteProgressContext(index, artifact, peerMessage), driverTarget, step, artifact.WorkshopID, artifact.TreeSHA256, runtimeModMetadata(artifact), peerLocations)
					attempts = mergeRuntimeFetchResult(attempts, shared.RuntimeModFetchSourcePeer, peerStarted, peerFetched, peerFetchErr)
					if peerFetchErr == nil && cacheManifestMatchesArtifact(peerFetched.Manifest, artifact) {
						attempts = selectFetchAttempt(attempts, shared.RuntimeModFetchSourcePeer)
						attempts = completeFetchAttempts(attempts, true, true, r.artifacts != nil)
						return r.markArtifactCache(target, artifact, operation.PlanHash, shared.RuntimeModFetchSourcePeer, attempts)
					}
					if peerFetchErr == nil {
						peerFetchErr = errors.New("节点直传返回的缓存 manifest 与期望版本不一致")
						attempts = markFetchAttemptFailed(attempts, shared.RuntimeModFetchSourcePeer, "FETCH_MANIFEST_MISMATCH", peerFetchErr)
					}
					fetchErr = errors.Join(fetchErr, peerFetchErr)
				} else {
					code, message := "PEER_SOURCE_UNAVAILABLE", "当前没有持有该精确版本且可直连的节点"
					if peerErr != nil {
						code, message = "PEER_GRANT_FAILED", peerErr.Error()
					}
					attempts = mergeFetchAttempts(attempts, unavailableFetchAttempt(shared.RuntimeModFetchSourcePeer, code, message))
				}
				if peerErr != nil {
					fetchErr = errors.Join(fetchErr, fmt.Errorf("节点间精确制品: %w", peerErr))
				}
				if r.artifacts != nil {
					controllerPrepareMessage := fmt.Sprintf("节点 SteamCMD 下载失败，正在准备 Controller 模组制品 · Workshop %s", artifact.WorkshopID)
					report(index, artifact, 0, 0, 0, controllerPrepareMessage)
					location, token, sourceErr := r.artifacts.Issue(ctx, target.TargetID, artifact.WorkshopID, artifact.TreeSHA256, 10*time.Minute)
					if sourceErr == nil {
						step, stepErr = session.step(ctx, "cache:"+artifact.WorkshopID+":fetch-controller")
						if stepErr != nil {
							r.artifacts.Revoke(token)
							persistFailure()
							return stepErr
						}
						controllerMessage := fmt.Sprintf("节点 SteamCMD 下载失败，正在改由 Controller 拉取 · Workshop %s", artifact.WorkshopID)
						report(index, artifact, 0, 0, 0, controllerMessage)
						controllerStarted := time.Now()
						controllerFetched, controllerErr := r.remote.FetchModCache(remoteProgressContext(index, artifact, controllerMessage), driverTarget, step, artifact.WorkshopID, artifact.TreeSHA256, runtimeModMetadata(artifact), []shared.RuntimeModFetchLocation{location})
						r.artifacts.Revoke(token)
						attempts = mergeRuntimeFetchResult(attempts, shared.RuntimeModFetchSourceController, controllerStarted, controllerFetched, controllerErr)
						sourceErr = controllerErr
						if sourceErr == nil && cacheManifestMatchesArtifact(controllerFetched.Manifest, artifact) {
							attempts = selectFetchAttempt(attempts, shared.RuntimeModFetchSourceController)
							attempts = completeFetchAttempts(attempts, true, true, true)
							return r.markArtifactCache(target, artifact, operation.PlanHash, shared.RuntimeModFetchSourceController, attempts)
						}
						if sourceErr == nil {
							sourceErr = errors.New("控制器制品返回的缓存 manifest 与期望版本不一致")
							attempts = markFetchAttemptFailed(attempts, shared.RuntimeModFetchSourceController, "FETCH_MANIFEST_MISMATCH", sourceErr)
						}
					}
					if sourceErr != nil {
						if !hasFetchAttempt(attempts, shared.RuntimeModFetchSourceController) {
							attempts = mergeFetchAttempts(attempts, unavailableFetchAttempt(shared.RuntimeModFetchSourceController, "CONTROLLER_SOURCE_UNAVAILABLE", sourceErr.Error()))
						}
						fetchErr = errors.Join(fetchErr, fmt.Errorf("控制器精确制品: %w", sourceErr))
					}
				} else {
					attempts = mergeFetchAttempts(attempts, unavailableFetchAttempt(shared.RuntimeModFetchSourceController, "CONTROLLER_SOURCE_UNAVAILABLE", "控制器没有可签发的精确模组制品"))
				}
				message := fmt.Sprintf("节点无法取得精确版本，正在使用控制器制品 · Workshop %s", artifact.WorkshopID)
				if fetchErr != nil {
					message += " · " + fetchErr.Error()
				}
				report(index, artifact, 0, 0, 0, message)
			} else {
				attempts = mergeFetchAttempts(attempts, unavailableFetchAttempt(shared.RuntimeModFetchSourceSteam, "FETCH_CAPABILITY_MISSING", "运行节点不支持自行获取模组"))
				attempts = mergeFetchAttempts(attempts, unavailableFetchAttempt(shared.RuntimeModFetchSourcePeer, "FETCH_CAPABILITY_MISSING", "运行节点不支持节点间模组直传"))
				attempts = mergeFetchAttempts(attempts, unavailableFetchAttempt(shared.RuntimeModFetchSourceController, "FETCH_CAPABILITY_MISSING", "运行节点不支持控制器制品下载"))
			}
			legacyStarted := time.Now()
			legacyTransferred := int64(0)
			legacyErr := r.uploadBundle(ctx, driverTarget, session, artifact, func(transferred, size int64) {
				legacyTransferred = transferred
				report(index, artifact, transferred, size, 0, "")
			})
			legacyAttempt := fetchAttempt(shared.RuntimeModFetchSourceLegacy, legacyStarted, legacyTransferred, legacyErr, "LEGACY_UPLOAD_FAILED")
			if legacyErr == nil {
				legacyAttempt.Selected = true
			}
			attempts = mergeFetchAttempts(attempts, legacyAttempt)
			if legacyErr == nil {
				attempts = selectFetchAttempt(attempts, shared.RuntimeModFetchSourceLegacy)
			}
			attempts = completeFetchAttempts(attempts, containsRuntimeCapability(target.Capabilities, modpublication.FetchCapability), supportsModPeer(r.remote), r.artifacts != nil)
			if legacyErr != nil {
				_ = r.markArtifactFetchAttempts(target, artifact, operation.PlanHash, attempts)
				return legacyErr
			}
			return r.markArtifactCache(target, artifact, operation.PlanHash, shared.RuntimeModFetchSourceLegacy, attempts)
		})
		if err != nil {
			return err
		}
		completedBytes += artifactProgressSize(artifact)
		report(index, artifact, 0, 0, 0, "")
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModCache, Percent: 100, Message: fmt.Sprintf("模组缓存同步完成（%d 个）", len(target.Mods)), CurrentBytes: totalBytes, TotalBytes: totalBytes, TotalItems: len(target.Mods)})
	return nil
}

func (r *Runtime) ensureArtifactDesired(target modpublication.TargetPlan, artifact modpublication.ContentArtifact, revision string) error {
	if r.replicas == nil {
		return nil
	}
	return r.replicas.EnsureArtifactDesired(target, artifact, revision)
}

func (r *Runtime) markArtifactCache(target modpublication.TargetPlan, artifact modpublication.ContentArtifact, revision string, source shared.RuntimeModFetchSource, attempts []modpublication.ReplicaFetchAttempt) error {
	if r.replicas == nil {
		return nil
	}
	return r.replicas.MarkArtifactCacheWithAttempts(target, artifact, revision, string(source), attempts)
}

func (r *Runtime) markArtifactFetchAttempts(target modpublication.TargetPlan, artifact modpublication.ContentArtifact, revision string, attempts []modpublication.ReplicaFetchAttempt) error {
	if r.replicas == nil || len(attempts) == 0 {
		return nil
	}
	return r.replicas.MarkArtifactFetchAttempts(target, artifact, revision, attempts)
}

func fetchAttempt(source shared.RuntimeModFetchSource, started time.Time, bytes int64, cause error, errorCode string) modpublication.ReplicaFetchAttempt {
	durationMillis := time.Since(started).Milliseconds()
	if durationMillis == 0 && !started.IsZero() {
		durationMillis = 1
	}
	attempt := modpublication.ReplicaFetchAttempt{
		Source: string(source), Feasibility: string(shared.RuntimeModFetchFeasibilityAvailable),
		Status: string(shared.RuntimeModFetchStatusSucceeded), Bytes: maxInt64(bytes, 0),
		DurationMillis: durationMillis, ObservedAt: timePointer(started.UTC()),
	}
	attempt.BytesPerSecond = observedBytesPerSecond(attempt.Bytes, attempt.DurationMillis)
	if cause != nil {
		attempt.Feasibility = string(shared.RuntimeModFetchFeasibilityUnavailable)
		attempt.Status = string(shared.RuntimeModFetchStatusFailed)
		attempt.ErrorCode = errorCode
		attempt.ErrorMessage = limitedFetchError(cause.Error())
	}
	return attempt
}

func unavailableFetchAttempt(source shared.RuntimeModFetchSource, code, message string) modpublication.ReplicaFetchAttempt {
	now := time.Now().UTC()
	return modpublication.ReplicaFetchAttempt{
		Source: string(source), Feasibility: string(shared.RuntimeModFetchFeasibilityUnavailable),
		Status: string(shared.RuntimeModFetchStatusUnavailable), ErrorCode: code,
		ErrorMessage: limitedFetchError(message), ObservedAt: &now,
	}
}

func skippedFetchAttempt(source shared.RuntimeModFetchSource, feasibility shared.RuntimeModFetchFeasibility, code string) modpublication.ReplicaFetchAttempt {
	now := time.Now().UTC()
	return modpublication.ReplicaFetchAttempt{
		Source: string(source), Feasibility: string(feasibility), Status: string(shared.RuntimeModFetchStatusSkipped),
		ErrorCode: code, ObservedAt: &now,
	}
}

func mergeRuntimeFetchResult(attempts []modpublication.ReplicaFetchAttempt, fallback shared.RuntimeModFetchSource, started time.Time, result runtimedriver.ModFetchResult, cause error) []modpublication.ReplicaFetchAttempt {
	if len(result.Attempts) == 0 {
		return mergeFetchAttempts(attempts, fetchAttempt(fallback, started, 0, cause, fetchFailureCode(fallback)))
	}
	for _, observed := range result.Attempts {
		attempts = mergeFetchAttempts(attempts, replicaFetchAttempt(observed))
	}
	if cause != nil {
		if hasFetchAttempt(attempts, fallback) {
			attempts = markFetchAttemptFailed(attempts, fallback, fetchFailureCode(fallback), cause)
		} else {
			attempts = mergeFetchAttempts(attempts, fetchAttempt(fallback, started, 0, cause, fetchFailureCode(fallback)))
		}
	}
	return attempts
}

func replicaFetchAttempt(value shared.RuntimeModFetchAttempt) modpublication.ReplicaFetchAttempt {
	observedAt := value.ObservedAt.UTC()
	var pointer *time.Time
	if !observedAt.IsZero() {
		pointer = &observedAt
	}
	return modpublication.ReplicaFetchAttempt{
		Source: string(value.Source), Feasibility: string(value.Feasibility), Status: string(value.Status),
		Selected: value.Selected, Bytes: maxInt64(value.Bytes, 0), DurationMillis: maxInt64(value.DurationMillis, 0),
		BytesPerSecond: maxInt64(value.BytesPerSecond, 0), ErrorCode: value.ErrorCode,
		ErrorMessage: limitedFetchError(value.ErrorMessage), ObservedAt: pointer,
	}
}

func mergeFetchAttempts(attempts []modpublication.ReplicaFetchAttempt, incoming modpublication.ReplicaFetchAttempt) []modpublication.ReplicaFetchAttempt {
	if !validFetchSource(shared.RuntimeModFetchSource(incoming.Source)) {
		return attempts
	}
	for index := range attempts {
		if attempts[index].Source != incoming.Source {
			continue
		}
		current := attempts[index]
		if incoming.Selected || incoming.Status == string(shared.RuntimeModFetchStatusSucceeded) || current.Status != string(shared.RuntimeModFetchStatusSucceeded) {
			attempts[index] = incoming
		}
		return attempts
	}
	return append(attempts, incoming)
}

func markFetchAttemptFailed(attempts []modpublication.ReplicaFetchAttempt, source shared.RuntimeModFetchSource, code string, cause error) []modpublication.ReplicaFetchAttempt {
	for index := range attempts {
		if attempts[index].Source != string(source) {
			continue
		}
		attempts[index].Feasibility = string(shared.RuntimeModFetchFeasibilityUnavailable)
		attempts[index].Status = string(shared.RuntimeModFetchStatusFailed)
		attempts[index].Selected = false
		attempts[index].ErrorCode = code
		if cause != nil {
			attempts[index].ErrorMessage = limitedFetchError(cause.Error())
		}
		return attempts
	}
	return mergeFetchAttempts(attempts, fetchAttempt(source, time.Now(), 0, cause, code))
}

func selectFetchAttempt(attempts []modpublication.ReplicaFetchAttempt, source shared.RuntimeModFetchSource) []modpublication.ReplicaFetchAttempt {
	for index := range attempts {
		attempts[index].Selected = attempts[index].Source == string(source) && attempts[index].Status == string(shared.RuntimeModFetchStatusSucceeded)
	}
	return attempts
}

func selectedFetchSource(result runtimedriver.ModFetchResult, fallback shared.RuntimeModFetchSource) shared.RuntimeModFetchSource {
	for _, attempt := range result.Attempts {
		if attempt.Selected && validFetchSource(attempt.Source) {
			return attempt.Source
		}
	}
	if validFetchSource(result.Manifest.FetchSource) {
		return result.Manifest.FetchSource
	}
	return fallback
}

func completeFetchAttempts(attempts []modpublication.ReplicaFetchAttempt, fetchSupported, peerSupported, controllerSupported bool) []modpublication.ReplicaFetchAttempt {
	sources := []shared.RuntimeModFetchSource{
		shared.RuntimeModFetchSourceCache, shared.RuntimeModFetchSourceSteam, shared.RuntimeModFetchSourcePeer,
		shared.RuntimeModFetchSourceController, shared.RuntimeModFetchSourceLegacy,
	}
	for _, source := range sources {
		if hasFetchAttempt(attempts, source) {
			continue
		}
		switch source {
		case shared.RuntimeModFetchSourceSteam:
			if !fetchSupported {
				attempts = append(attempts, unavailableFetchAttempt(source, "FETCH_CAPABILITY_MISSING", "运行节点不支持自行获取模组"))
			} else {
				attempts = append(attempts, skippedFetchAttempt(source, shared.RuntimeModFetchFeasibilityUnknown, "SOURCE_NOT_NEEDED"))
			}
		case shared.RuntimeModFetchSourcePeer:
			if !fetchSupported || !peerSupported {
				attempts = append(attempts, unavailableFetchAttempt(source, "PEER_CAPABILITY_MISSING", "当前运行节点组合不支持节点间模组直传"))
			} else {
				attempts = append(attempts, skippedFetchAttempt(source, shared.RuntimeModFetchFeasibilityUnknown, "SOURCE_NOT_NEEDED"))
			}
		case shared.RuntimeModFetchSourceController:
			if !fetchSupported || !controllerSupported {
				attempts = append(attempts, unavailableFetchAttempt(source, "CONTROLLER_SOURCE_UNAVAILABLE", "控制器精确制品下载不可用"))
			} else {
				attempts = append(attempts, skippedFetchAttempt(source, shared.RuntimeModFetchFeasibilityUnknown, "SOURCE_NOT_NEEDED"))
			}
		case shared.RuntimeModFetchSourceLegacy:
			attempts = append(attempts, skippedFetchAttempt(source, shared.RuntimeModFetchFeasibilityAvailable, "SOURCE_NOT_NEEDED"))
		default:
			attempts = append(attempts, skippedFetchAttempt(source, shared.RuntimeModFetchFeasibilityUnknown, "SOURCE_NOT_NEEDED"))
		}
	}
	ordered := make([]modpublication.ReplicaFetchAttempt, 0, len(sources))
	for _, source := range sources {
		for _, attempt := range attempts {
			if attempt.Source == string(source) {
				ordered = append(ordered, attempt)
				break
			}
		}
	}
	return ordered
}

func hasFetchAttempt(attempts []modpublication.ReplicaFetchAttempt, source shared.RuntimeModFetchSource) bool {
	for _, attempt := range attempts {
		if attempt.Source == string(source) {
			return true
		}
	}
	return false
}

func supportsModPeer(driver runtimedriver.ModDriver) bool {
	_, supported := driver.(runtimedriver.ModPeerDriver)
	return supported
}

func validFetchSource(source shared.RuntimeModFetchSource) bool {
	switch source {
	case shared.RuntimeModFetchSourceSteam, shared.RuntimeModFetchSourcePeer, shared.RuntimeModFetchSourceController,
		shared.RuntimeModFetchSourceCache, shared.RuntimeModFetchSourceLegacy:
		return true
	default:
		return false
	}
}

func fetchFailureCode(source shared.RuntimeModFetchSource) string {
	switch source {
	case shared.RuntimeModFetchSourceSteam:
		return "STEAM_FETCH_FAILED"
	case shared.RuntimeModFetchSourcePeer:
		return "PEER_FETCH_FAILED"
	case shared.RuntimeModFetchSourceController:
		return "CONTROLLER_FETCH_FAILED"
	case shared.RuntimeModFetchSourceLegacy:
		return "LEGACY_UPLOAD_FAILED"
	default:
		return "MOD_FETCH_FAILED"
	}
}

func observedBytesPerSecond(bytes, durationMillis int64) int64 {
	if bytes <= 0 || durationMillis <= 0 {
		return 0
	}
	return bytes * 1000 / durationMillis
}

func limitedFetchError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1024 {
		return value[:1024]
	}
	return value
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}

func (r *Runtime) modPeerLocations(ctx context.Context, target modpublication.TargetPlan, artifact modpublication.ContentArtifact) ([]shared.RuntimeModFetchLocation, error) {
	peerDriver, supported := r.remote.(runtimedriver.ModPeerDriver)
	if !supported || r.replicas == nil || r.source == nil {
		return nil, nil
	}
	candidates, err := r.replicas.CachedCandidates(artifact.WorkshopID, artifact.TreeSHA256, target.TargetID)
	if err != nil {
		return nil, err
	}
	locations := make([]shared.RuntimeModFetchLocation, 0, 2)
	var failures []error
	for _, candidate := range candidates {
		if len(locations) == 2 {
			break
		}
		execution, exists, executionErr := r.source.Execution(ctx, candidate.TargetID, candidate.InstallationID)
		if executionErr != nil {
			failures = append(failures, executionErr)
			continue
		}
		if !exists || !execution.Target.Online || !containsRuntimeCapability(execution.Target.Capabilities, "runtime.mods.peer.v1") {
			continue
		}
		sourceTarget := runtimedriver.Target{
			TargetID: candidate.TargetID, InstallationID: candidate.InstallationID,
			TopologyRevision: execution.Revision,
		}
		location, grantErr := peerDriver.GrantModArtifact(ctx, sourceTarget, target.TargetID, artifact.WorkshopID, artifact.TreeSHA256)
		if grantErr != nil {
			failures = append(failures, fmt.Errorf("%s/%s: %w", candidate.TargetID, candidate.InstallationID, grantErr))
			continue
		}
		locations = append(locations, location)
	}
	return locations, errors.Join(failures...)
}

func (r *Runtime) convergeModCache(ctx context.Context, key string, run func() error) error {
	r.convergenceMu.Lock()
	if active := r.convergenceRuns[key]; active != nil {
		active.waiters++
		r.convergenceMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-active.done:
			return active.err
		}
	}
	active := &modCacheConvergence{done: make(chan struct{})}
	r.convergenceRuns[key] = active
	r.convergenceMu.Unlock()

	active.err = run()
	r.convergenceMu.Lock()
	delete(r.convergenceRuns, key)
	close(active.done)
	r.convergenceMu.Unlock()
	return active.err
}

func (r *Runtime) installationTransferLock(target runtimedriver.Target) *sync.Mutex {
	key := target.TargetID + "\x00" + target.InstallationID
	r.transferMu.Lock()
	defer r.transferMu.Unlock()
	if r.transferLocks[key] == nil {
		r.transferLocks[key] = &sync.Mutex{}
	}
	return r.transferLocks[key]
}

func artifactProgressSize(artifact modpublication.ContentArtifact) int64 {
	if artifact.Size > 0 {
		return artifact.Size
	}
	return 1
}

func (r *Runtime) Prepare(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	input := distributionInput(target, operation.PublicationID)
	if target.TargetID == "local" {
		plan, err := r.manager.BuildPlan(ctx, input)
		if err != nil {
			return err
		}
		_, err = r.manager.Prepare(ctx, plan)
		return err
	}
	wire := runtimePlanInput(target, operation.PublicationID)
	encoded, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	driverTarget := targetRuntimeTarget(target, operation.TopologyRevision)
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	descriptor := transferDescriptor("plan", operation.PublicationID, target, encoded)
	if err := uploadBytes(ctx, encoded, descriptor,
		func(offset int64) (int64, error) {
			step, err := session.step(ctx, "plan:begin")
			if err != nil {
				return 0, err
			}
			return r.remote.BeginModReleasePlan(ctx, driverTarget, step, descriptor)
		},
		func(offset int64, data []byte) (int64, error) {
			step, err := session.step(ctx, fmt.Sprintf("plan:write:%d", offset))
			if err != nil {
				return offset, err
			}
			return r.remote.WriteModReleasePlan(ctx, driverTarget, step, descriptor, offset, data)
		},
	); err != nil {
		return err
	}
	step, err := session.step(ctx, "plan:commit")
	if err != nil {
		return err
	}
	if _, err := r.remote.CommitModReleasePlan(ctx, driverTarget, step, descriptor); err != nil {
		return err
	}
	step, err = session.step(ctx, "release:prepare")
	if err != nil {
		return err
	}
	_, err = r.remote.PrepareModRelease(ctx, driverTarget, step, operation.PublicationID)
	return err
}

func (r *Runtime) PrepareInstallationContent(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	input := contentDistributionInput(target, operation.PublicationID)
	if target.TargetID == "local" {
		plan, err := r.manager.BuildContentPlan(ctx, input)
		if err != nil {
			return err
		}
		_, err = r.manager.Prepare(ctx, plan)
		return err
	}
	wire := runtimeContentPlanInput(target, operation.PublicationID)
	encoded, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	driverTarget := targetRuntimeTarget(target, operation.TopologyRevision)
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	descriptor := transferDescriptor("content-plan", operation.PublicationID, target, encoded)
	if err := uploadBytes(ctx, encoded, descriptor,
		func(offset int64) (int64, error) {
			step, err := session.step(ctx, "content-plan:begin")
			if err != nil {
				return 0, err
			}
			return r.remote.BeginModReleasePlan(ctx, driverTarget, step, descriptor)
		},
		func(offset int64, data []byte) (int64, error) {
			step, err := session.step(ctx, fmt.Sprintf("content-plan:write:%d", offset))
			if err != nil {
				return offset, err
			}
			return r.remote.WriteModReleasePlan(ctx, driverTarget, step, descriptor, offset, data)
		},
	); err != nil {
		return err
	}
	step, err := session.step(ctx, "content-plan:commit")
	if err != nil {
		return err
	}
	if _, err := r.remote.CommitModReleasePlan(ctx, driverTarget, step, descriptor); err != nil {
		return err
	}
	step, err = session.step(ctx, "content-release:prepare")
	if err != nil {
		return err
	}
	_, err = r.remote.PrepareModRelease(ctx, driverTarget, step, operation.PublicationID)
	return err
}

func (r *Runtime) Publish(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	defer runtimedriver.NotifyRuntimeTargetsChanged(r.mutations, target.TargetID)
	if target.TargetID == "local" {
		_, err := r.manager.Publish(ctx, operation.PublicationID)
		return err
	}
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	step, err := session.step(ctx, "release:publish")
	if err != nil {
		return err
	}
	_, err = r.remote.PublishModRelease(ctx, targetRuntimeTarget(target, operation.TopologyRevision), step, operation.PublicationID)
	return err
}

func (r *Runtime) Rollback(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	defer runtimedriver.NotifyRuntimeTargetsChanged(r.mutations, target.TargetID)
	if target.TargetID == "local" {
		return r.manager.Rollback(ctx, operation.PublicationID)
	}
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	step, err := session.step(ctx, "release:rollback")
	if err != nil {
		return err
	}
	_, err = r.remote.RollbackModRelease(ctx, targetRuntimeTarget(target, operation.TopologyRevision), step, operation.PublicationID)
	return err
}

func (r *Runtime) Complete(ctx context.Context, target modpublication.TargetPlan, operation modpublication.RuntimeOperation) error {
	defer runtimedriver.NotifyRuntimeTargetsChanged(r.mutations, target.TargetID)
	if target.TargetID == "local" {
		return r.manager.Complete(ctx, operation.PublicationID)
	}
	session, err := newRuntimeOperationSession(target, operation)
	if err != nil {
		return err
	}
	step, err := session.step(ctx, "release:complete")
	if err != nil {
		return err
	}
	_, err = r.remote.CompleteModRelease(ctx, targetRuntimeTarget(target, operation.TopologyRevision), step, operation.PublicationID)
	return err
}

func (r *Runtime) uploadBundle(ctx context.Context, target runtimedriver.Target, session *runtimeOperationSession, artifact modpublication.ContentArtifact, progress func(int64, int64)) error {
	lock := r.installationTransferLock(target)
	lock.Lock()
	defer lock.Unlock()
	file, err := os.CreateTemp(r.tempRoot, ".mod-bundle-*.tar")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	hash := sha256.New()
	if err := r.manager.WriteBundle(ctx, artifact.WorkshopID, artifact.TreeSHA256, io.MultiWriter(file, hash)); err != nil {
		file.Close()
		return err
	}
	info, err := file.Stat()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	descriptor := runtimedriver.ModUploadDescriptor{
		UploadID:   "cache-" + shaHex([]byte(artifact.WorkshopID+"\x00"+artifact.TreeSHA256)),
		WorkshopID: artifact.WorkshopID, ExpectedTreeSHA256: artifact.TreeSHA256,
		Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)),
		Metadata: runtimeModMetadata(artifact),
	}
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()
	step, err := session.step(ctx, "cache:"+artifact.WorkshopID+":begin")
	if err != nil {
		return err
	}
	offset, err := r.remote.BeginModUpload(ctx, target, step, descriptor)
	if err != nil {
		return err
	}
	if offset < 0 || offset > descriptor.Size {
		return errors.New("remote Mod upload returned an invalid resume offset")
	}
	if progress != nil {
		progress(offset, descriptor.Size)
	}
	if _, err := input.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	buffer := make([]byte, shared.MaxChunkBytes)
	for offset < descriptor.Size {
		count, readErr := input.Read(buffer)
		if count > 0 {
			step, stepErr := session.step(ctx, fmt.Sprintf("cache:%s:write:%d", artifact.WorkshopID, offset))
			if stepErr != nil {
				return stepErr
			}
			next, writeErr := r.remote.WriteModUpload(ctx, target, step, descriptor, offset, buffer[:count])
			if writeErr != nil {
				return writeErr
			}
			if next != offset+int64(count) {
				return errors.New("remote Mod upload returned an inconsistent offset")
			}
			offset = next
			if progress != nil {
				progress(offset, descriptor.Size)
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if count == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	step, err = session.step(ctx, "cache:"+artifact.WorkshopID+":commit")
	if err != nil {
		return err
	}
	manifest, err := r.remote.CommitModUpload(ctx, target, step, descriptor)
	if err != nil {
		return err
	}
	if !cacheManifestMatchesArtifact(manifest, artifact) {
		return errors.New("remote Mod cache manifest does not match the controller artifact")
	}
	return nil
}

func cacheManifestMatchesArtifact(manifest shared.RuntimeModCacheManifest, artifact modpublication.ContentArtifact) bool {
	if manifest.WorkshopID != artifact.WorkshopID ||
		!strings.EqualFold(manifest.TreeSHA256, artifact.TreeSHA256) ||
		manifest.Size != artifact.Size || manifest.FileCount != artifact.FileCount {
		return false
	}
	return true
}

func runtimeModMetadata(artifact modpublication.ContentArtifact) shared.RuntimeModMetadata {
	return shared.RuntimeModMetadata{
		PublishedFileSize: artifact.Size,
		SteamManifestID:   artifact.SteamManifestID,
		SteamUpdatedAt:    artifact.SteamUpdatedAt,
	}
}

func distributionInput(target modpublication.TargetPlan, operationID string) moddistribution.PlanInput {
	input := moddistribution.PlanInput{OperationID: operationID, NodeID: target.NodeID}
	for _, world := range target.Worlds {
		shard := moddistribution.ShardRelease{
			InstallationID: target.InstallationID, RoomID: world.RoomID, RoomDirectory: world.RoomDirectory,
			WorldID: world.WorldID, WorldDirectory: world.WorldDirectory, ModOverrides: append([]byte(nil), world.ModOverrides...),
		}
		for _, artifact := range world.Mods {
			shard.Mods = append(shard.Mods, moddistribution.ModVersion{
				WorkshopID: artifact.WorkshopID, TreeSHA256: artifact.TreeSHA256,
				Metadata: moddistribution.Metadata{
					PublishedFileSize: artifact.Size, SteamManifestID: artifact.SteamManifestID,
					SteamUpdatedAt: artifact.SteamUpdatedAt,
				},
			})
		}
		input.Shards = append(input.Shards, shard)
	}
	return input
}

func runtimePlanInput(target modpublication.TargetPlan, operationID string) shared.RuntimeModPlanInput {
	input := distributionInput(target, operationID)
	wire := shared.RuntimeModPlanInput{OperationID: operationID, NodeID: target.NodeID}
	for _, shard := range input.Shards {
		item := shared.RuntimeModShardRelease{
			InstallationID: shard.InstallationID, RoomID: shard.RoomID, RoomDirectory: shard.RoomDirectory,
			WorldID: shard.WorldID, WorldDirectory: shard.WorldDirectory, ModOverrides: append([]byte(nil), shard.ModOverrides...),
		}
		for _, mod := range shard.Mods {
			item.Mods = append(item.Mods, shared.RuntimeModVersion{
				WorkshopID: mod.WorkshopID, TreeSHA256: mod.TreeSHA256,
				Metadata: shared.RuntimeModMetadata{
					Title: mod.Metadata.Title, Version: mod.Metadata.Version,
					PublishedFileSize: mod.Metadata.PublishedFileSize, SteamManifestID: mod.Metadata.SteamManifestID,
					SteamUpdatedAt: mod.Metadata.SteamUpdatedAt,
				},
			})
		}
		wire.Shards = append(wire.Shards, item)
	}
	return wire
}

func contentDistributionInput(target modpublication.TargetPlan, operationID string) moddistribution.ContentPlanInput {
	input := moddistribution.ContentPlanInput{
		OperationID:    operationID,
		NodeID:         target.NodeID,
		InstallationID: target.InstallationID,
	}
	for _, artifact := range target.Mods {
		input.Mods = append(input.Mods, moddistribution.ModVersion{
			WorkshopID: artifact.WorkshopID,
			TreeSHA256: artifact.TreeSHA256,
			Metadata: moddistribution.Metadata{
				PublishedFileSize: artifact.Size,
				SteamManifestID:   artifact.SteamManifestID,
				SteamUpdatedAt:    artifact.SteamUpdatedAt,
			},
		})
	}
	return input
}

func runtimeContentPlanInput(target modpublication.TargetPlan, operationID string) shared.RuntimeModPlanInput {
	input := contentDistributionInput(target, operationID)
	wire := shared.RuntimeModPlanInput{
		OperationID:    input.OperationID,
		NodeID:         input.NodeID,
		Mode:           string(moddistribution.PlanModeContent),
		InstallationID: input.InstallationID,
	}
	for _, mod := range input.Mods {
		wire.Mods = append(wire.Mods, shared.RuntimeModVersion{
			WorkshopID: mod.WorkshopID,
			TreeSHA256: mod.TreeSHA256,
			Metadata: shared.RuntimeModMetadata{
				Title:             mod.Metadata.Title,
				Version:           mod.Metadata.Version,
				PublishedFileSize: mod.Metadata.PublishedFileSize,
				SteamManifestID:   mod.Metadata.SteamManifestID,
				SteamUpdatedAt:    mod.Metadata.SteamUpdatedAt,
			},
		})
	}
	return wire
}

func runtimeOperation(target modpublication.TargetPlan, value modpublication.RuntimeOperation) (runtimedriver.Operation, error) {
	resource := installationResource(target.TargetID, target.InstallationID)
	for _, fence := range value.Fences {
		if fence.RoomID != resource {
			continue
		}
		expires := fence.ExpiresAt
		digest := shaHex([]byte(value.IdempotencyKey))
		return runtimedriver.Operation{
			ID: "modop-" + digest, Key: "modkey-" + digest, LeaseID: fence.LeaseID,
			FencingToken: fence.FencingToken, LeaseExpiresAt: &expires,
		}, nil
	}
	return runtimedriver.Operation{}, errors.New("installation publication fence is missing")
}

func runtimeStepOperation(base runtimedriver.Operation, step string) runtimedriver.Operation {
	digest := shaHex([]byte(base.ID + "\x00" + base.Key + "\x00" + step))
	base.ID = "modop-" + digest
	base.Key = "modkey-" + digest
	return base
}

type runtimeOperationSession struct {
	target    modpublication.TargetPlan
	operation modpublication.RuntimeOperation
}

func newRuntimeOperationSession(target modpublication.TargetPlan, operation modpublication.RuntimeOperation) (*runtimeOperationSession, error) {
	session := &runtimeOperationSession{target: target, operation: operation}
	if _, err := runtimeOperation(target, operation); err != nil {
		return nil, err
	}
	return session, nil
}

func (s *runtimeOperationSession) step(ctx context.Context, name string) (runtimedriver.Operation, error) {
	if s.operation.RenewFences != nil && installationFenceExpiresSoon(s.target, s.operation.Fences, time.Now().UTC().Add(modRuntimeLeaseRefreshWindow)) {
		fences, err := s.operation.RenewFences(ctx, append([]modpublication.Fence(nil), s.operation.Fences...))
		if err != nil {
			return runtimedriver.Operation{}, err
		}
		s.operation.Fences = append([]modpublication.Fence(nil), fences...)
	}
	base, err := runtimeOperation(s.target, s.operation)
	if err != nil {
		return runtimedriver.Operation{}, err
	}
	return runtimeStepOperation(base, name), nil
}

func installationFenceExpiresSoon(target modpublication.TargetPlan, fences []modpublication.Fence, deadline time.Time) bool {
	resource := installationResource(target.TargetID, target.InstallationID)
	for _, fence := range fences {
		if fence.RoomID == resource {
			return fence.ExpiresAt.Before(deadline)
		}
	}
	return true
}

func installationResource(targetID, installationID string) string {
	return "@mod-installation/" + shaHex([]byte(targetID+"\x00"+installationID))
}

func runtimeTarget(placement modpublication.AppliedPlacement, revision string) runtimedriver.Target {
	return runtimedriver.Target{
		TargetID: placement.TargetID, InstallationID: placement.InstallationID,
		RoomID: placement.RoomID, WorldID: placement.WorldID, Cluster: "Mods", Shard: "Installation", TopologyRevision: revision,
	}
}

func targetRuntimeTarget(target modpublication.TargetPlan, revision string) runtimedriver.Target {
	return runtimedriver.Target{TargetID: target.TargetID, InstallationID: target.InstallationID, Cluster: "Mods", Shard: "Installation", TopologyRevision: revision}
}

func transferDescriptor(prefix, operationID string, target modpublication.TargetPlan, data []byte) runtimedriver.ModUploadDescriptor {
	digest := sha256.Sum256(data)
	return runtimedriver.ModUploadDescriptor{
		UploadID:    prefix + "-" + shaHex([]byte(operationID+"\x00"+target.TargetID+"\x00"+target.InstallationID)),
		OperationID: operationID, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
	}
}

func uploadBytes(ctx context.Context, data []byte, descriptor runtimedriver.ModUploadDescriptor, begin func(int64) (int64, error), write func(int64, []byte) (int64, error)) error {
	if descriptor.Size != int64(len(data)) || !strings.EqualFold(descriptor.SHA256, shaHex(data)) {
		return errors.New("remote plan upload descriptor does not match its payload")
	}
	offset, err := begin(0)
	if err != nil {
		return err
	}
	if offset < 0 || offset > int64(len(data)) {
		return errors.New("remote plan upload returned an invalid resume offset")
	}
	for offset < int64(len(data)) {
		end := offset + shared.MaxChunkBytes
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		next, err := write(offset, data[offset:end])
		if err != nil {
			return err
		}
		if next != end {
			return fmt.Errorf("remote plan upload returned offset %d, expected %d", next, end)
		}
		offset = next
	}
	return ctx.Err()
}
