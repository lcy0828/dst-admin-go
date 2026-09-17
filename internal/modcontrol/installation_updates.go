package modcontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/modpublication"
	"dont/internal/mods"
	"dont/internal/operationprogress"
	"dont/shared"

	"github.com/google/uuid"
)

const installationContentCapability = "runtime.mods.content-publish.v1"

type installationContentCatalog interface {
	UpdateLibrary(context.Context, string, io.Writer) (mods.ActionResult, error)
	DownloadLibraryMods(context.Context, []string, io.Writer) (mods.ActionResult, error)
	Describe(context.Context, []string) (map[string]mods.SteamMod, error)
}

type InstallationUpdater struct {
	catalog installationContentCatalog
	content modpublication.ContentSource
	runtime *Runtime
	leases  modpublication.Lease

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewInstallationUpdater(catalog installationContentCatalog, content modpublication.ContentSource, runtime *Runtime, leases modpublication.Lease) (*InstallationUpdater, error) {
	if catalog == nil || content == nil || runtime == nil || leases == nil {
		return nil, ErrInvalidRequest
	}
	return &InstallationUpdater{catalog: catalog, content: content, runtime: runtime, leases: leases, locks: make(map[string]*sync.Mutex)}, nil
}

func (u *InstallationUpdater) UpdateInstallationMods(ctx context.Context, targetID, installationID string, modIDs []string, output io.Writer) (mods.ActionResult, error) {
	return u.applyInstallationMods(ctx, targetID, installationID, modIDs, output, false)
}

func (u *InstallationUpdater) LinkInstallationMods(ctx context.Context, targetID, installationID string, modIDs []string) error {
	_, err := u.applyInstallationMods(ctx, targetID, installationID, modIDs, io.Discard, true)
	return err
}

func (u *InstallationUpdater) applyInstallationMods(ctx context.Context, targetID, installationID string, modIDs []string, output io.Writer, linkOnly bool) (mods.ActionResult, error) {
	targetID = strings.TrimSpace(targetID)
	installationID = strings.TrimSpace(installationID)
	modIDs = normalizedInstallationModIDs(modIDs)
	if !validInstallationUpdateTarget(targetID, installationID) || len(modIDs) == 0 {
		return mods.ActionResult{}, ErrInvalidRequest
	}
	if output == nil {
		output = io.Discard
	}
	lock := u.installationLock(targetID, installationID)
	lock.Lock()
	defer lock.Unlock()

	operationID := uuid.NewString()
	planHash := installationContentPlanHash(targetID, installationID, modIDs)
	resourceID := installationResource(targetID, installationID)
	fence, err := u.leases.Acquire(ctx, resourceID, "mod.download:"+planHash[:32], 5*time.Minute)
	if err != nil {
		return mods.ActionResult{}, err
	}
	defer u.leases.Release(fence)
	target := installationContentTarget(targetID, installationID, nil)
	operation := modpublication.RuntimeOperation{
		PublicationID: operationID, TopologyRevision: "installation-download-v1", PlanHash: planHash,
		Fences: []modpublication.Fence{fence}, Action: "download-installation-mods", IdempotencyKey: "mod-download:" + operationID,
	}
	if linkOnly {
		if err := u.runtime.LinkInstallationMods(ctx, target, operation, modIDs); err != nil {
			return mods.ActionResult{}, fmt.Errorf("%s/%s 建立本地模组入口: %w", targetID, installationID, err)
		}
		return mods.ActionResult{ModIDs: modIDs, Message: "已使用运行机器上的本地模组"}, nil
	}
	parentCtx := ctx
	var progressItems []shared.ModDownloadProgress
	ctx = operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
		update.TargetID, update.InstallationID = targetID, installationID
		if len(update.Items) == 0 {
			update.Items = progressItems
		}
		update.Items = append([]shared.ModDownloadProgress(nil), update.Items...)
		for index := range update.Items {
			update.Items[index].TargetID, update.Items[index].InstallationID = targetID, installationID
		}
		progressItems = update.Items
		operationprogress.Report(parentCtx, update)
	})
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModCache, Percent: 1, Message: fmt.Sprintf("正在由 %s 直接下载 %d 个模组", targetID, len(modIDs))})
	if targetID == "local" {
		_, err = u.catalog.DownloadLibraryMods(ctx, modIDs, output)
	} else {
		err = u.runtime.DownloadInstallationMods(ctx, target, operation, modIDs)
	}
	if err != nil {
		return mods.ActionResult{}, fmt.Errorf("%s/%s 下载 Workshop 模组: %w", targetID, installationID, err)
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModDone, Percent: 100, Message: "机器模组已直接下载；房间配置和世界进程未改变"})
	return mods.ActionResult{ModIDs: modIDs, Message: fmt.Sprintf("已在 %s 更新 %d 个机器模组；房间配置未改变", targetID, len(modIDs))}, nil
}

// FetchLatestArtifacts prepares the newest Workshop content in the selected
// Runtime cache. A following room publication installs that exact content and
// changes the selected worlds in one user-visible Job.
func (u *InstallationUpdater) FetchLatestArtifacts(ctx context.Context, targetID, installationID string, modIDs []string, output io.Writer) ([]modpublication.ContentArtifact, error) {
	artifacts, _, _, err := u.fetchLatestArtifacts(ctx, targetID, installationID, modIDs, output)
	return artifacts, err
}

func (u *InstallationUpdater) fetchLatestArtifacts(ctx context.Context, targetID, installationID string, modIDs []string, output io.Writer) ([]modpublication.ContentArtifact, modpublication.TargetPlan, modpublication.RuntimeOperation, error) {
	targetID = strings.TrimSpace(targetID)
	installationID = strings.TrimSpace(installationID)
	modIDs = normalizedInstallationModIDs(modIDs)
	if !validInstallationUpdateTarget(targetID, installationID) || len(modIDs) == 0 {
		return nil, modpublication.TargetPlan{}, modpublication.RuntimeOperation{}, ErrInvalidRequest
	}
	if output == nil {
		output = io.Discard
	}
	lock := u.installationLock(targetID, installationID)
	lock.Lock()
	defer lock.Unlock()

	operationID := uuid.NewString()
	seedHash := installationContentPlanHash(targetID, installationID, modIDs)
	resourceID := installationResource(targetID, installationID)
	operationKey := "mod.content:" + seedHash[:32]
	fence, err := u.leases.Acquire(ctx, resourceID, operationKey, 5*time.Minute)
	if err != nil {
		return nil, modpublication.TargetPlan{}, modpublication.RuntimeOperation{}, err
	}
	defer u.leases.Release(fence)
	target := installationContentTarget(targetID, installationID, nil)
	operation := modpublication.RuntimeOperation{
		PublicationID: operationID, TopologyRevision: "installation-content-v1", PlanHash: seedHash,
		Fences: []modpublication.Fence{fence}, Action: "fetch-installation-content", IdempotencyKey: "mod-content:" + operationID,
		RenewFences: func(ctx context.Context, fences []modpublication.Fence) ([]modpublication.Fence, error) {
			updated := append([]modpublication.Fence(nil), fences...)
			for index := range updated {
				renewed, renewErr := u.leases.Renew(ctx, updated[index], 5*time.Minute)
				if renewErr != nil {
					return updated, renewErr
				}
				updated[index] = renewed
			}
			return updated, nil
		},
	}
	metadata, err := u.catalog.Describe(ctx, modIDs)
	if err != nil {
		return nil, modpublication.TargetPlan{}, modpublication.RuntimeOperation{}, fmt.Errorf("读取 Workshop 信息: %w", err)
	}
	artifacts := make([]modpublication.ContentArtifact, 0, len(modIDs))
	for index, modID := range modIDs {
		operationprogress.Report(ctx, operationprogress.Update{
			Stage:      operationprogress.StageModInspect,
			Percent:    index * 100 / len(modIDs),
			Message:    fmt.Sprintf("正在获取最新模组 %d/%d · Workshop %s", index+1, len(modIDs), modID),
			WorkshopID: modID, CurrentItem: index + 1, TotalItems: len(modIDs),
		})
		downloadCtx := withInstallationCatalogProgress(ctx, index, len(modIDs))
		var artifact modpublication.ContentArtifact
		if targetID == "local" {
			if _, err := u.catalog.UpdateLibrary(downloadCtx, modID, output); err != nil {
				return nil, modpublication.TargetPlan{}, modpublication.RuntimeOperation{}, fmt.Errorf("本机下载 Workshop %s: %w", modID, err)
			}
			artifact, err = u.content.Resolve(ctx, modpublication.ModRequirement{WorkshopID: modID})
		} else {
			item := metadata[modID]
			artifact, err = u.runtime.FetchLatestArtifact(downloadCtx, target, operation, modID, shared.RuntimeModMetadata{
				Title: item.Name, Version: item.Version, PublishedFileSize: item.FileSize,
				SteamManifestID: item.SteamManifestID, SteamUpdatedAt: item.UpdatedAt,
			})
		}
		if err != nil {
			return nil, modpublication.TargetPlan{}, modpublication.RuntimeOperation{}, fmt.Errorf("%s/%s 下载 Workshop %s: %w", targetID, installationID, modID, err)
		}
		artifacts = append(artifacts, artifact)
	}
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModInspect, Percent: 100, Message: "最新模组版本已准备完成"})
	sort.Slice(artifacts, func(left, right int) bool { return artifacts[left].WorkshopID < artifacts[right].WorkshopID })
	planHash := installationContentPlanHash(targetID, installationID, artifacts)
	target.Mods = append([]modpublication.ContentArtifact(nil), artifacts...)
	operation.PlanHash = planHash
	operationprogress.Report(ctx, operationprogress.Update{Stage: operationprogress.StageModCache, Percent: 0, Message: "正在把精确版本准备到运行机器"})
	if err := u.runtime.EnsureCache(ctx, target, operation); err != nil {
		return nil, modpublication.TargetPlan{}, modpublication.RuntimeOperation{}, err
	}
	return artifacts, target, operation, nil
}

func withInstallationCatalogProgress(ctx context.Context, itemIndex, totalItems int) context.Context {
	return operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
		if totalItems < 1 {
			return
		}
		percent := installationCatalogProgress(update)
		update.Stage = operationprogress.StageModInspect
		update.Percent = (itemIndex*100 + percent) / totalItems
		operationprogress.Report(ctx, update)
	})
}

func installationCatalogProgress(update operationprogress.Update) int {
	percent := update.Percent
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	switch update.Stage {
	case operationprogress.StageModInspect:
		return percent / 10
	case operationprogress.StageModCache:
		return 10 + percent*85/100
	case operationprogress.StageModDone:
		return 100
	default:
		return percent
	}
}

func (u *InstallationUpdater) installationLock(targetID, installationID string) *sync.Mutex {
	key := targetID + "\x00" + installationID
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.locks[key] == nil {
		u.locks[key] = &sync.Mutex{}
	}
	return u.locks[key]
}

func normalizedInstallationModIDs(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !mods.ValidID(value) || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func validInstallationUpdateTarget(targetID, installationID string) bool {
	if installationID == "" || len(installationID) > 128 || strings.ContainsAny(installationID, "\x00\r\n") {
		return false
	}
	return targetID == "local" || strings.HasPrefix(targetID, "agent:") && len(strings.TrimPrefix(targetID, "agent:")) > 0
}

func installationContentTarget(targetID, installationID string, artifacts []modpublication.ContentArtifact) modpublication.TargetPlan {
	nodeID := targetID
	if strings.HasPrefix(targetID, "agent:") {
		nodeID = strings.TrimPrefix(targetID, "agent:")
	}
	return modpublication.TargetPlan{
		TargetID:       targetID,
		NodeID:         nodeID,
		InstallationID: installationID,
		Online:         true,
		Capabilities:   []string{modpublication.RequiredCapability, modpublication.StateCapability, modpublication.FetchCapability, installationContentCapability},
		Mods:           append([]modpublication.ContentArtifact(nil), artifacts...),
	}
}

func installationContentPlanHash(targetID, installationID string, artifacts any) string {
	encoded, _ := json.Marshal(struct {
		TargetID       string
		InstallationID string
		Payload        any
	}{targetID, installationID, artifacts})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
