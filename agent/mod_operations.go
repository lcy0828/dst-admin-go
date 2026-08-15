package agent

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"dont/internal/moddistribution"
	"dont/shared"
	"github.com/shirou/gopsutil/v3/disk"
)

const (
	modRuntimeVersion      = "1.0.0"
	modUploadStateVersion  = 1
	modMaximumBundleBytes  = int64(64 << 30)
	modMaximumPlanBytes    = int64(32 << 20)
	modMaximumArchiveFiles = 100000
	modMaximumOverrides    = int64(8 << 20)
	modDiskReserveBytes    = int64(16 << 20)
)

var (
	modWorkshopID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	modReleaseID  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)
)

type modUploadState struct {
	Version            int                         `json:"version"`
	UploadID           string                      `json:"upload_id"`
	Kind               shared.RuntimeModUploadKind `json:"kind"`
	OperationID        string                      `json:"operation_id,omitempty"`
	WorkshopID         string                      `json:"workshop_id,omitempty"`
	ExpectedTreeSHA256 string                      `json:"expected_tree_sha256,omitempty"`
	Size               int64                       `json:"size"`
	SHA256             string                      `json:"sha256"`
	Metadata           shared.RuntimeModMetadata   `json:"metadata,omitempty"`
	Offset             int64                       `json:"offset"`
	Committed          bool                        `json:"committed"`
	CreatedAt          time.Time                   `json:"created_at"`
	UpdatedAt          time.Time                   `json:"updated_at"`
}

type persistedModReleaseState struct {
	Version     int       `json:"version"`
	OperationID string    `json:"operation_id"`
	Phase       string    `json:"phase"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (a *Agent) executeModAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(request, shared.RuntimeOutcomeConfirmed, "Mod Runtime 步骤已完成")
	response := &shared.RuntimeModResult{}
	result.Mod = response
	var err error
	switch request.Action {
	case shared.RuntimeActionModUploadBegin, shared.RuntimeActionModReleasePlanBegin:
		*response, err = a.beginModUpload(installation, *request.Mod)
	case shared.RuntimeActionModUploadWrite, shared.RuntimeActionModReleasePlanWrite:
		*response, err = a.writeModUpload(installation, *request.Mod)
	case shared.RuntimeActionModUploadCommit, shared.RuntimeActionModReleasePlanCommit:
		*response, err = a.commitModUpload(ctx, installation, *request.Mod)
	case shared.RuntimeActionModReleasePrepare:
		*response, err = a.prepareModRelease(ctx, installation, request.Mod.OperationID)
	case shared.RuntimeActionModReleasePublish:
		*response, err = a.publishModRelease(ctx, installation, request.Mod.OperationID)
	case shared.RuntimeActionModReleaseRollback:
		*response, err = a.rollbackModRelease(ctx, installation, request.Mod.OperationID)
	case shared.RuntimeActionModReleaseComplete:
		*response, err = a.completeModRelease(ctx, installation, request.Mod.OperationID)
	default:
		err = errors.New("Mod Runtime 动作不受支持")
	}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func (a *Agent) observeModAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "Mod Runtime 状态已读取")
	response := &shared.RuntimeModResult{}
	result.Mod = response
	manager, err := a.modManager(installation)
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	switch request.Action {
	case shared.RuntimeActionModTargetObserve:
		usage, usageErr := disk.Usage(installation.ModCachePath)
		if usageErr == nil {
			available := int64(usage.Free)
			if available > modDiskReserveBytes {
				available -= modDiskReserveBytes
			} else {
				available = 0
			}
			response.AvailableBytes = available
			response.RuntimeVersion = modRuntimeVersion
			response.Complete = true
		}
		err = usageErr
	case shared.RuntimeActionModCacheInspect:
		manifest, inspectErr := manager.Verify(ctx, request.Mod.WorkshopID, request.Mod.ExpectedTreeSHA256)
		if inspectErr == nil {
			response.CacheManifest = runtimeModManifest(manifest)
			response.Complete = true
		}
		err = inspectErr
	case shared.RuntimeActionModReleaseState:
		*response, err = a.readModReleaseState(installation, manager, request.Mod.OperationID)
	case shared.RuntimeActionModOverridesRead:
		response.Overrides, err = readModOverrides(ctx, installation, *request.Mod)
		response.Complete = err == nil && response.Overrides.Complete
	default:
		err = errors.New("Mod Runtime 读取动作不受支持")
	}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func validateModOperationPayload(request shared.RuntimeOperationRequest) error {
	mod := request.Mod
	if mod == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil {
		return errors.New("Mod Runtime 请求负载无效")
	}
	validUpload := operationIdentity.MatchString(mod.UploadID)
	validOperation := modReleaseID.MatchString(mod.OperationID)
	validMetadata := validRuntimeModMetadata(mod.Metadata)
	emptyMetadata := mod.Metadata == (shared.RuntimeModMetadata{})
	emptyTransfer := mod.Kind == "" && mod.UploadID == "" && mod.Size == 0 && mod.SHA256 == "" && len(mod.Data) == 0
	switch request.Action {
	case shared.RuntimeActionModTargetObserve:
		if !emptyTransfer || mod.OperationID != "" || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" || mod.Offset != 0 ||
			!emptyMetadata || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 运行目标观察请求无效")
		}
	case shared.RuntimeActionModCacheInspect:
		if !emptyTransfer || mod.OperationID != "" || !modWorkshopID.MatchString(mod.WorkshopID) || !validRuntimeDigest(mod.ExpectedTreeSHA256) ||
			mod.Offset != 0 || !emptyMetadata || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 缓存检查请求无效")
		}
	case shared.RuntimeActionModUploadBegin, shared.RuntimeActionModUploadWrite, shared.RuntimeActionModUploadCommit:
		if mod.Kind != shared.RuntimeModUploadCacheBundle || !validUpload || mod.OperationID != "" || !modWorkshopID.MatchString(mod.WorkshopID) ||
			!validRuntimeDigest(mod.ExpectedTreeSHA256) || mod.Size < 1 || mod.Size > modMaximumBundleBytes || !validRuntimeDigest(mod.SHA256) ||
			!validMetadata || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 缓存上传描述无效")
		}
		if err := validateModChunkAction(request.Action, mod.Offset, mod.Size, mod.Data); err != nil {
			return err
		}
	case shared.RuntimeActionModReleasePlanBegin, shared.RuntimeActionModReleasePlanWrite, shared.RuntimeActionModReleasePlanCommit:
		if mod.Kind != shared.RuntimeModUploadReleasePlan || !validUpload || !validOperation || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" ||
			mod.Size < 1 || mod.Size > modMaximumPlanBytes || !validRuntimeDigest(mod.SHA256) || !emptyMetadata ||
			mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 发布计划上传描述无效")
		}
		if err := validateModChunkAction(request.Action, mod.Offset, mod.Size, mod.Data); err != nil {
			return err
		}
	case shared.RuntimeActionModReleasePrepare, shared.RuntimeActionModReleasePublish, shared.RuntimeActionModReleaseRollback,
		shared.RuntimeActionModReleaseComplete, shared.RuntimeActionModReleaseState:
		if !validOperation || !emptyTransfer || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" || mod.Offset != 0 ||
			!emptyMetadata || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 发布状态请求无效")
		}
	case shared.RuntimeActionModOverridesRead:
		if mod.OperationID != "" || !emptyTransfer || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" || mod.Offset < 0 ||
			!emptyMetadata || !safeModPathComponent(mod.RoomDirectory) || !safeModPathComponent(mod.WorldDirectory) {
			return errors.New("modoverrides.lua 分块读取请求无效")
		}
	default:
		return errors.New("Mod Runtime 动作无效")
	}
	return nil
}

func safeModPathComponent(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && value != "." && value != ".." && len(value) <= 255 &&
		utf8.ValidString(value) && filepath.Base(value) == value && !strings.ContainsAny(value, "/\\\x00\r\n")
}

func validateModChunkAction(action shared.RuntimeAction, offset, size int64, data []byte) error {
	switch action {
	case shared.RuntimeActionModUploadBegin, shared.RuntimeActionModReleasePlanBegin,
		shared.RuntimeActionModUploadCommit, shared.RuntimeActionModReleasePlanCommit:
		if offset != 0 || len(data) != 0 {
			return errors.New("Mod 上传开始或提交请求包含数据块")
		}
	case shared.RuntimeActionModUploadWrite, shared.RuntimeActionModReleasePlanWrite:
		if offset < 0 || offset > size || len(data) < 1 || len(data) > shared.MaxChunkBytes || int64(len(data)) > size-offset {
			return errors.New("Mod 上传数据块无效")
		}
	default:
		return errors.New("Mod 上传动作无效")
	}
	return nil
}

func validRuntimeModMetadata(value shared.RuntimeModMetadata) bool {
	return value.PublishedFileSize >= 0 && value.PublishedFileSize <= modMaximumBundleBytes &&
		len(value.Title) <= 1024 && len(value.Version) <= 256 && utf8.ValidString(value.Title) && utf8.ValidString(value.Version) &&
		!strings.ContainsAny(value.Title+value.Version, "\x00\r\n")
}

func (a *Agent) modManager(installation RuntimeInstallation) (*moddistribution.Manager, error) {
	a.modDistributionMu.Lock()
	defer a.modDistributionMu.Unlock()
	if existing := a.modDistributions[installation.ID]; existing != nil {
		return existing, nil
	}
	created, err := moddistribution.New(moddistribution.Config{
		CacheRoot: installation.ModCachePath,
		StateRoot: installation.ModStatePath,
		NodeID:    a.Config.AgentID,
		Installations: []moddistribution.TrustedInstallation{{
			ID: installation.ID, NodeID: a.Config.AgentID,
			ServerPath: installation.ServerPath, SavePath: installation.SavePath,
		}},
		ReserveBytes: modDiskReserveBytes,
	})
	if err != nil {
		return nil, err
	}
	if err := reconcileModReleaseStates(installation, created); err != nil {
		return nil, err
	}
	a.modDistributions[installation.ID] = created
	return created, nil
}

func (a *Agent) beginModUpload(installation RuntimeInstallation, request shared.RuntimeModRequest) (shared.RuntimeModResult, error) {
	if _, err := a.modManager(installation); err != nil {
		return shared.RuntimeModResult{}, err
	}
	directory, err := modUploadDirectory(installation)
	if err != nil {
		return shared.RuntimeModResult{}, err
	}
	statePath, dataPath := modUploadPaths(directory, request.UploadID)
	if state, err := loadModUploadState(statePath, dataPath); err == nil {
		if !sameModUploadDescriptor(state, request) {
			return modUploadResult(state), errors.New("Mod 上传 ID 已用于另一份内容")
		}
		return modUploadResult(state), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return shared.RuntimeModResult{}, err
	}
	if _, err := os.Lstat(dataPath); err == nil {
		return shared.RuntimeModResult{}, errors.New("Mod 上传暂存文件与状态冲突")
	} else if !os.IsNotExist(err) {
		return shared.RuntimeModResult{}, err
	}
	if err := ensureModDiskSpace(directory, request.Size); err != nil {
		return shared.RuntimeModResult{}, err
	}
	file, err := os.OpenFile(dataPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return shared.RuntimeModResult{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return shared.RuntimeModResult{}, err
	}
	if err := file.Close(); err != nil {
		return shared.RuntimeModResult{}, err
	}
	now := time.Now().UTC()
	state := modUploadState{
		Version: modUploadStateVersion, UploadID: request.UploadID, Kind: request.Kind,
		OperationID: request.OperationID, WorkshopID: request.WorkshopID,
		ExpectedTreeSHA256: strings.ToLower(request.ExpectedTreeSHA256), Size: request.Size,
		SHA256: strings.ToLower(request.SHA256), Metadata: request.Metadata, CreatedAt: now, UpdatedAt: now,
	}
	if err := writeModJSONAtomic(statePath, state); err != nil {
		return shared.RuntimeModResult{}, err
	}
	return modUploadResult(state), nil
}

func (a *Agent) writeModUpload(installation RuntimeInstallation, request shared.RuntimeModRequest) (shared.RuntimeModResult, error) {
	directory, err := modUploadDirectory(installation)
	if err != nil {
		return shared.RuntimeModResult{}, err
	}
	statePath, dataPath := modUploadPaths(directory, request.UploadID)
	state, err := loadModUploadState(statePath, dataPath)
	if err != nil {
		return shared.RuntimeModResult{}, err
	}
	response := modUploadResult(state)
	if !sameModUploadDescriptor(state, request) || state.Committed {
		return response, errors.New("Mod 上传状态与数据块描述不一致")
	}
	if request.Offset != state.Offset {
		return response, fmt.Errorf("Mod 上传 offset 冲突: 当前为 %d", state.Offset)
	}
	if len(request.Data) == 0 || len(request.Data) > shared.MaxChunkBytes || request.Offset > state.Size || int64(len(request.Data)) > state.Size-request.Offset {
		return response, errors.New("Mod 上传数据块无效")
	}
	file, err := os.OpenFile(dataPath, os.O_WRONLY, 0)
	if err != nil {
		return response, err
	}
	written, writeErr := file.WriteAt(request.Data, request.Offset)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return response, err
	}
	if written != len(request.Data) {
		return response, io.ErrShortWrite
	}
	state.Offset += int64(written)
	state.UpdatedAt = time.Now().UTC()
	if err := writeModJSONAtomic(statePath, state); err != nil {
		return modUploadResult(state), err
	}
	return modUploadResult(state), nil
}

func (a *Agent) commitModUpload(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeModRequest) (shared.RuntimeModResult, error) {
	manager, err := a.modManager(installation)
	if err != nil {
		return shared.RuntimeModResult{}, err
	}
	directory, err := modUploadDirectory(installation)
	if err != nil {
		return shared.RuntimeModResult{}, err
	}
	statePath, dataPath := modUploadPaths(directory, request.UploadID)
	state, err := loadModUploadState(statePath, dataPath)
	if err != nil {
		return shared.RuntimeModResult{}, err
	}
	if !sameModUploadDescriptor(state, request) {
		return modUploadResult(state), errors.New("Mod 上传提交描述不一致")
	}
	if state.Committed {
		return a.committedModUploadResult(ctx, installation, manager, state)
	}
	if state.Offset != state.Size {
		return modUploadResult(state), fmt.Errorf("Mod 上传尚未完成: %d/%d", state.Offset, state.Size)
	}
	digest, err := hashModFile(ctx, dataPath)
	if err != nil {
		return modUploadResult(state), err
	}
	if digest != state.SHA256 {
		return modUploadResult(state), errors.New("Mod 上传 SHA256 校验失败")
	}
	var response shared.RuntimeModResult
	if state.Kind == shared.RuntimeModUploadCacheBundle {
		response, err = importModBundle(ctx, installation, manager, state, dataPath)
	} else {
		response, err = a.importModReleasePlan(ctx, installation, manager, state, dataPath)
	}
	if err != nil {
		return response, err
	}
	state.Committed, state.UpdatedAt = true, time.Now().UTC()
	if err := writeModJSONAtomic(statePath, state); err != nil {
		return response, err
	}
	if err := os.Remove(dataPath); err != nil && !os.IsNotExist(err) {
		return response, err
	}
	return response, nil
}

func (a *Agent) committedModUploadResult(ctx context.Context, installation RuntimeInstallation, manager *moddistribution.Manager, state modUploadState) (shared.RuntimeModResult, error) {
	if state.Kind == shared.RuntimeModUploadCacheBundle {
		manifest, err := manager.Verify(ctx, state.WorkshopID, state.ExpectedTreeSHA256)
		result := modUploadResult(state)
		if err == nil {
			result.CacheManifest, result.Complete = runtimeModManifest(manifest), true
		}
		return result, err
	}
	result, err := a.readModReleaseState(installation, manager, state.OperationID)
	result.Kind, result.UploadID, result.Offset, result.NextOffset, result.Size, result.SHA256 = state.Kind, state.UploadID, state.Offset, state.Offset, state.Size, state.SHA256
	return result, err
}

func importModBundle(ctx context.Context, installation RuntimeInstallation, manager *moddistribution.Manager, state modUploadState, dataPath string) (shared.RuntimeModResult, error) {
	importsRoot := filepath.Join(installation.ModStatePath, "imports")
	if err := ensureModDirectory(importsRoot); err != nil {
		return modUploadResult(state), err
	}
	source := filepath.Join(importsRoot, state.UploadID)
	if err := removeModImportDirectory(source, importsRoot); err != nil {
		return modUploadResult(state), err
	}
	if err := os.Mkdir(source, 0o700); err != nil {
		return modUploadResult(state), err
	}
	defer os.RemoveAll(source)
	if err := extractModArchive(ctx, dataPath, source); err != nil {
		return modUploadResult(state), err
	}
	manifest, err := manager.Import(ctx, state.WorkshopID, source, moddistribution.Metadata{
		Title: state.Metadata.Title, Version: state.Metadata.Version, PublishedFileSize: state.Metadata.PublishedFileSize,
		SteamUpdatedAt: state.Metadata.SteamUpdatedAt,
	})
	if err != nil {
		return modUploadResult(state), err
	}
	if !strings.EqualFold(manifest.TreeSHA256, state.ExpectedTreeSHA256) {
		return modUploadResult(state), errors.New("Mod 缓存包的预期 tree SHA256 与导入 manifest 不一致")
	}
	result := modUploadResult(state)
	result.CacheManifest, result.Complete = runtimeModManifest(manifest), true
	return result, nil
}

func (a *Agent) importModReleasePlan(ctx context.Context, installation RuntimeInstallation, manager *moddistribution.Manager, state modUploadState, dataPath string) (shared.RuntimeModResult, error) {
	file, err := os.Open(dataPath)
	if err != nil {
		return modUploadResult(state), err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, modMaximumPlanBytes+1))
	if err != nil || int64(len(data)) != state.Size || len(data) > int(modMaximumPlanBytes) || !utf8.Valid(data) {
		return modUploadResult(state), errors.Join(errors.New("Mod 发布计划必须是有效 UTF-8 JSON"), err)
	}
	var wire shared.RuntimeModPlanInput
	if err := decodeSingleModJSON(data, &wire); err != nil {
		return modUploadResult(state), err
	}
	input, err := runtimeModPlanInput(wire, installation, a.Config.AgentID, state.OperationID)
	if err != nil {
		return modUploadResult(state), err
	}
	plan, err := manager.BuildPlan(ctx, input)
	if err != nil {
		return modUploadResult(state), err
	}
	if err := writeModJSONAtomic(modPlanPath(installation, state.OperationID), plan); err != nil {
		return modUploadResult(state), err
	}
	release, err := writeModReleaseState(installation, state.OperationID, "planned")
	if err != nil {
		return modUploadResult(state), err
	}
	result := modUploadResult(state)
	result.Complete, result.OperationID, result.Release = true, state.OperationID, release
	return result, nil
}

func (a *Agent) prepareModRelease(ctx context.Context, installation RuntimeInstallation, operationID string) (shared.RuntimeModResult, error) {
	manager, plan, err := a.loadModPlan(installation, operationID)
	if err != nil {
		return shared.RuntimeModResult{OperationID: operationID}, err
	}
	journal, err := manager.Prepare(ctx, plan)
	if err != nil {
		return shared.RuntimeModResult{OperationID: operationID}, err
	}
	release, err := writeModReleaseState(installation, operationID, string(journal.Phase))
	return shared.RuntimeModResult{OperationID: operationID, Complete: err == nil, Release: release}, err
}

func (a *Agent) publishModRelease(ctx context.Context, installation RuntimeInstallation, operationID string) (shared.RuntimeModResult, error) {
	manager, err := a.modManager(installation)
	if err != nil {
		return shared.RuntimeModResult{OperationID: operationID}, err
	}
	journal, err := manager.Publish(ctx, operationID)
	if err != nil {
		return shared.RuntimeModResult{OperationID: operationID}, err
	}
	release, err := writeModReleaseState(installation, operationID, string(journal.Phase))
	return shared.RuntimeModResult{OperationID: operationID, Complete: err == nil, Release: release}, err
}

func (a *Agent) rollbackModRelease(ctx context.Context, installation RuntimeInstallation, operationID string) (shared.RuntimeModResult, error) {
	manager, err := a.modManager(installation)
	if err == nil {
		err = manager.Rollback(ctx, operationID)
	}
	if err != nil {
		return shared.RuntimeModResult{OperationID: operationID}, err
	}
	release, err := writeModReleaseState(installation, operationID, "rolled_back")
	return shared.RuntimeModResult{OperationID: operationID, Complete: err == nil, Release: release}, err
}

func (a *Agent) completeModRelease(ctx context.Context, installation RuntimeInstallation, operationID string) (shared.RuntimeModResult, error) {
	manager, err := a.modManager(installation)
	if err == nil {
		err = manager.Complete(ctx, operationID)
	}
	if err != nil {
		return shared.RuntimeModResult{OperationID: operationID}, err
	}
	release, err := writeModReleaseState(installation, operationID, "committed")
	if err != nil {
		return shared.RuntimeModResult{OperationID: operationID}, err
	}
	state, err := manager.State(installation.ID)
	if err == nil {
		release.State = runtimeModInstallationState(state)
	}
	return shared.RuntimeModResult{OperationID: operationID, Complete: err == nil, Release: release}, err
}

func (a *Agent) loadModPlan(installation RuntimeInstallation, operationID string) (*moddistribution.Manager, moddistribution.Plan, error) {
	manager, err := a.modManager(installation)
	if err != nil {
		return nil, moddistribution.Plan{}, err
	}
	var plan moddistribution.Plan
	if err := readModJSON(modPlanPath(installation, operationID), modMaximumPlanBytes, &plan); err != nil {
		return nil, moddistribution.Plan{}, err
	}
	if plan.OperationID != operationID || plan.NodeID != a.Config.AgentID || len(plan.Installations) != 1 || plan.Installations[0].InstallationID != installation.ID {
		return nil, moddistribution.Plan{}, errors.New("Mod 发布计划身份不匹配")
	}
	return manager, plan, nil
}

func (a *Agent) readModReleaseState(installation RuntimeInstallation, manager *moddistribution.Manager, operationID string) (shared.RuntimeModResult, error) {
	var persisted persistedModReleaseState
	if err := readModJSON(modReleaseStatePath(installation, operationID), 1<<20, &persisted); err != nil {
		return shared.RuntimeModResult{OperationID: operationID}, err
	}
	if persisted.Version != 1 || persisted.OperationID != operationID || persisted.Phase == "" {
		return shared.RuntimeModResult{OperationID: operationID}, errors.New("Mod 发布状态无效")
	}
	release := &shared.RuntimeModReleaseState{OperationID: operationID, Phase: persisted.Phase, UpdatedAt: persisted.UpdatedAt}
	if state, err := manager.State(installation.ID); err == nil && state.LastOperationID == operationID {
		release.State = runtimeModInstallationState(state)
	}
	return shared.RuntimeModResult{OperationID: operationID, Complete: true, Release: release}, nil
}

func writeModReleaseState(installation RuntimeInstallation, operationID, phase string) (*shared.RuntimeModReleaseState, error) {
	value := persistedModReleaseState{Version: 1, OperationID: operationID, Phase: phase, UpdatedAt: time.Now().UTC()}
	if err := writeModJSONAtomic(modReleaseStatePath(installation, operationID), value); err != nil {
		return nil, err
	}
	return &shared.RuntimeModReleaseState{OperationID: operationID, Phase: phase, UpdatedAt: value.UpdatedAt}, nil
}

func reconcileModReleaseStates(installation RuntimeInstallation, manager *moddistribution.Manager) error {
	directory := filepath.Join(installation.ModStatePath, "releases")
	items, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".state.json") {
			continue
		}
		path := filepath.Join(directory, item.Name())
		var persisted persistedModReleaseState
		if err := readModJSON(path, 1<<20, &persisted); err != nil {
			return err
		}
		switch persisted.Phase {
		case "preparing", "prepared", "publishing", "published", "completing", "rolling_back":
			phase := "rolled_back"
			if state, stateErr := manager.State(installation.ID); stateErr == nil && state.LastOperationID == persisted.OperationID {
				phase = "committed"
			}
			if _, err := writeModReleaseState(installation, persisted.OperationID, phase); err != nil {
				return err
			}
		}
	}
	return nil
}

func runtimeModPlanInput(value shared.RuntimeModPlanInput, installation RuntimeInstallation, nodeID, operationID string) (moddistribution.PlanInput, error) {
	if value.OperationID != operationID || value.NodeID != nodeID || len(value.Shards) == 0 {
		return moddistribution.PlanInput{}, errors.New("Mod 发布计划身份或分片列表无效")
	}
	result := moddistribution.PlanInput{OperationID: value.OperationID, NodeID: value.NodeID, Shards: make([]moddistribution.ShardRelease, 0, len(value.Shards))}
	for _, shard := range value.Shards {
		if shard.InstallationID != installation.ID {
			return moddistribution.PlanInput{}, errors.New("Mod 发布计划包含其他 DST 安装")
		}
		if !utf8.Valid(shard.ModOverrides) || strings.IndexByte(string(shard.ModOverrides), 0) >= 0 {
			return moddistribution.PlanInput{}, errors.New("Mod 发布计划包含非 UTF-8 modoverrides.lua")
		}
		converted := moddistribution.ShardRelease{
			InstallationID: shard.InstallationID, RoomID: shard.RoomID, RoomDirectory: shard.RoomDirectory,
			WorldID: shard.WorldID, WorldDirectory: shard.WorldDirectory,
			ModOverrides: append([]byte(nil), shard.ModOverrides...), Mods: make([]moddistribution.ModVersion, 0, len(shard.Mods)),
		}
		for _, mod := range shard.Mods {
			converted.Mods = append(converted.Mods, moddistribution.ModVersion{WorkshopID: mod.WorkshopID, TreeSHA256: mod.TreeSHA256})
		}
		result.Shards = append(result.Shards, converted)
	}
	return result, nil
}

func readModOverrides(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeModRequest) (*shared.RuntimeModOverridesChunk, error) {
	path := filepath.Join(installation.SavePath, request.RoomDirectory, request.WorldDirectory, "modoverrides.lua")
	if !pathWithinRoot(path, installation.SavePath) {
		return nil, errors.New("modoverrides.lua 位于受信存档目录之外")
	}
	if err := rejectModSymlinkComponents(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > modMaximumOverrides {
		return nil, errors.New("modoverrides.lua 不是安全的普通文件")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return nil, errors.Join(errors.New("modoverrides.lua 在读取期间发生变化"), err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, modMaximumOverrides+1))
	if err != nil || int64(len(data)) != info.Size() {
		return nil, errors.Join(errors.New("modoverrides.lua 读取不完整"), err)
	}
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return nil, errors.New("modoverrides.lua 不是有效 UTF-8 文本")
	}
	if request.Offset > int64(len(data)) {
		return nil, fmt.Errorf("modoverrides.lua offset 超出文件大小: %d", len(data))
	}
	digest := sha256.Sum256(data)
	end := request.Offset + shared.MaxChunkBytes
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return &shared.RuntimeModOverridesChunk{
		RoomDirectory: request.RoomDirectory, WorldDirectory: request.WorldDirectory,
		Offset: request.Offset, NextOffset: end, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		Data: append([]byte(nil), data[request.Offset:end]...), Complete: end == int64(len(data)),
	}, nil
}

func extractModArchive(ctx context.Context, archivePath, target string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := io.Reader(bufio.NewReader(file))
	buffered := reader.(*bufio.Reader)
	magic, _ := buffered.Peek(2)
	var gzipReader *gzip.Reader
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gzipReader, err = gzip.NewReader(buffered)
		if err != nil {
			return err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	tarReader := tar.NewReader(reader)
	seen := make(map[string]bool)
	var total int64
	fileCount := 0
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		canonical := strings.TrimSuffix(header.Name, "/")
		if count >= modMaximumArchiveFiles || !safeModArchivePath(header.Name) || seen[canonical] {
			return errors.New("Mod 缓存归档包含无效或重复路径")
		}
		seen[canonical] = true
		path := filepath.Join(target, filepath.FromSlash(canonical))
		if !pathWithinRoot(path, target) {
			return errors.New("Mod 缓存归档路径越界")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			fileCount++
			if header.Size < 0 || total > modMaximumBundleBytes-header.Size {
				return errors.New("Mod 缓存归档展开后过大")
			}
			total += header.Size
			if err := ensureModDiskSpace(target, header.Size); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			written, copyErr := io.CopyN(output, tarReader, header.Size)
			syncErr := output.Sync()
			closeErr := output.Close()
			if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
				return err
			}
			if written != header.Size {
				return io.ErrUnexpectedEOF
			}
		default:
			return errors.New("Mod 缓存归档包含链接或特殊文件")
		}
	}
	if len(seen) == 0 || fileCount == 0 {
		return errors.New("Mod 缓存归档为空")
	}
	return nil
}

func safeModArchivePath(value string) bool {
	if value == "" || value == "." || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.ContainsRune(value, 0) {
		return false
	}
	comparison := strings.TrimSuffix(value, "/")
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(comparison)))
	return comparison != "" && !strings.HasSuffix(comparison, "/") && cleaned == comparison && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func modUploadDirectory(installation RuntimeInstallation) (string, error) {
	directory := filepath.Join(installation.ModStatePath, "uploads")
	if !pathWithinRoot(directory, installation.ModStatePath) {
		return "", errors.New("Mod 上传目录越界")
	}
	return directory, ensureModDirectory(directory)
}

func modUploadPaths(directory, uploadID string) (string, string) {
	return filepath.Join(directory, uploadID+".json"), filepath.Join(directory, uploadID+".part")
}

func modPlanPath(installation RuntimeInstallation, operationID string) string {
	return filepath.Join(installation.ModStatePath, "releases", operationID+".plan.json")
}

func modReleaseStatePath(installation RuntimeInstallation, operationID string) string {
	return filepath.Join(installation.ModStatePath, "releases", operationID+".state.json")
}

func loadModUploadState(statePath, dataPath string) (modUploadState, error) {
	var state modUploadState
	if err := readModJSON(statePath, 1<<20, &state); err != nil {
		return modUploadState{}, err
	}
	if state.Version != modUploadStateVersion || !operationIdentity.MatchString(state.UploadID) ||
		(state.Kind != shared.RuntimeModUploadCacheBundle && state.Kind != shared.RuntimeModUploadReleasePlan) ||
		state.Size < 1 || state.Offset < 0 || state.Offset > state.Size || !validRuntimeDigest(state.SHA256) {
		return modUploadState{}, errors.New("Mod 上传状态无效")
	}
	info, err := os.Lstat(dataPath)
	if state.Committed && os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return modUploadState{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < state.Offset || info.Size() > state.Size {
		return modUploadState{}, errors.New("Mod 上传暂存文件无效")
	}
	if info.Size() != state.Offset {
		state.Offset, state.UpdatedAt = info.Size(), time.Now().UTC()
		if err := writeModJSONAtomic(statePath, state); err != nil {
			return modUploadState{}, err
		}
	}
	return state, nil
}

func sameModUploadDescriptor(state modUploadState, request shared.RuntimeModRequest) bool {
	return state.UploadID == request.UploadID && state.Kind == request.Kind && state.OperationID == request.OperationID &&
		state.WorkshopID == request.WorkshopID && strings.EqualFold(state.ExpectedTreeSHA256, request.ExpectedTreeSHA256) &&
		state.Size == request.Size && strings.EqualFold(state.SHA256, request.SHA256) && state.Metadata == request.Metadata
}

func modUploadResult(state modUploadState) shared.RuntimeModResult {
	return shared.RuntimeModResult{
		Kind: state.Kind, UploadID: state.UploadID, OperationID: state.OperationID,
		Offset: state.Offset, NextOffset: state.Offset, Size: state.Size, SHA256: state.SHA256,
		Complete: state.Committed,
	}
}

func runtimeModManifest(value moddistribution.Manifest) *shared.RuntimeModCacheManifest {
	return &shared.RuntimeModCacheManifest{
		Version: value.Version, WorkshopID: value.WorkshopID, TreeSHA256: value.TreeSHA256,
		ManifestSHA256: value.ManifestSHA256, Size: value.Size, FileCount: value.FileCount, CreatedAt: value.CreatedAt,
		Metadata: shared.RuntimeModMetadata{
			Title: value.Metadata.Title, Version: value.Metadata.Version, PublishedFileSize: value.Metadata.PublishedFileSize,
			SteamUpdatedAt: value.Metadata.SteamUpdatedAt,
		},
	}
}

func runtimeModInstallationState(value moddistribution.InstallationState) *shared.RuntimeModInstallationState {
	result := &shared.RuntimeModInstallationState{
		InstallationID: value.InstallationID, LastOperationID: value.LastOperationID,
		Mods: make(map[string]string, len(value.Mods)), ManagedSetupSHA256: value.ManagedSetupSHA256,
		Shards: make(map[string]shared.RuntimeModShardState, len(value.Shards)), UpdatedAt: value.UpdatedAt,
	}
	for id, hash := range value.Mods {
		result.Mods[id] = hash
	}
	for key, shard := range value.Shards {
		converted := shared.RuntimeModShardState{
			RoomID: shard.RoomID, RoomDirectory: shard.RoomDirectory, WorldID: shard.WorldID,
			WorldDirectory: shard.WorldDirectory, ConfigSHA256: shard.ConfigSHA256,
			Mods: make([]shared.RuntimeModVersion, 0, len(shard.Mods)),
		}
		for _, mod := range shard.Mods {
			converted.Mods = append(converted.Mods, shared.RuntimeModVersion{WorkshopID: mod.WorkshopID, TreeSHA256: mod.TreeSHA256})
		}
		result.Shards[key] = converted
	}
	return result
}

func decodeSingleModJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("解析 Mod 发布计划: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("Mod 发布计划包含多个 JSON 值")
		}
		return fmt.Errorf("解析 Mod 发布计划尾部: %w", err)
	}
	return nil
}

func readModJSON(path string, limit int64, target any) error {
	if err := rejectModSymlinkComponents(path); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return errors.Join(errors.New("Mod 状态文件无效"), err)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit || !utf8.Valid(data) {
		return errors.Join(errors.New("Mod 状态 JSON 无效"), err)
	}
	return decodeSingleModJSON(data, target)
}

func writeModJSONAtomic(path string, value any) error {
	if err := ensureModDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".mod-json-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	keep = true
	return syncModDirectory(filepath.Dir(path))
}

func ensureModDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return rejectModSymlinkComponents(path)
}

func rejectModSymlinkComponents(path string) error {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Mod 路径包含符号链接")
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func removeModImportDirectory(path, root string) error {
	if !pathWithinRoot(path, root) || path == root {
		return errors.New("Mod 导入目录越界")
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Mod 导入暂存目标不是安全目录")
	}
	return os.RemoveAll(path)
}

func ensureModDiskSpace(path string, required int64) error {
	if required < 0 {
		return errors.New("Mod 磁盘空间请求无效")
	}
	usage, err := disk.Usage(path)
	if err != nil {
		return err
	}
	if required > int64(usage.Free) || modDiskReserveBytes > int64(usage.Free)-required {
		return moddistribution.ErrInsufficientSpace
	}
	return nil
}

func hashModFile(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, shared.MaxChunkBytes)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncModDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
