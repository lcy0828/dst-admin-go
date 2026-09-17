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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dont/internal/dstserver"
	"dont/internal/moddistribution"
	"dont/internal/mods"
	"dont/internal/operationprogress"
	"dont/internal/roomops"
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
	modWorkshopID   = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	modReleaseID    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)
	modContentRange = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)
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

type modFetchCommandRunner interface {
	Download(context.Context, RuntimeInstallation, string, string, bool) error
}

type steamModFetchCommandRunner struct{}

func (steamModFetchCommandRunner) Download(ctx context.Context, installation RuntimeInstallation, downloadRoot, workshopID string, validate bool) error {
	runner := mods.NewSteamCMDRunner(installation.SteamCMDPath, downloadRoot, "322330")
	return runner.Download(ctx, []string{workshopID}, validate, io.Discard)
}

type installationModDownloadRunner interface {
	Download(context.Context, RuntimeInstallation, []string) error
	Close() error
}

type steamInstallationModDownloadRunner struct {
	mu      sync.Mutex
	runners map[string]*mods.SteamCMDRunner
}

func newSteamInstallationModDownloadRunner() *steamInstallationModDownloadRunner {
	return &steamInstallationModDownloadRunner{runners: make(map[string]*mods.SteamCMDRunner)}
}

func (r *steamInstallationModDownloadRunner) Download(ctx context.Context, installation RuntimeInstallation, workshopIDs []string) error {
	downloadRoot, err := runtimeWorkshopDownloadRoot(installation.WorkshopContentPath, "322330")
	if err != nil {
		return err
	}
	key := downloadRoot
	r.mu.Lock()
	runner := r.runners[key]
	if runner == nil {
		runner = mods.NewSteamCMDRunner(installation.SteamCMDPath, downloadRoot, "322330")
		r.runners[key] = runner
	}
	r.mu.Unlock()
	return runner.DownloadSession(ctx, workshopIDs, false, io.Discard)
}

func (r *steamInstallationModDownloadRunner) Close() error {
	r.mu.Lock()
	runners := make([]*mods.SteamCMDRunner, 0, len(r.runners))
	for _, runner := range r.runners {
		runners = append(runners, runner)
	}
	r.runners = make(map[string]*mods.SteamCMDRunner)
	r.mu.Unlock()
	var result error
	for _, runner := range runners {
		result = errors.Join(result, runner.Close())
	}
	return result
}

func runtimeWorkshopDownloadRoot(contentPath, appID string) (string, error) {
	appRoot := filepath.Clean(strings.TrimSpace(contentPath))
	contentRoot := filepath.Dir(appRoot)
	workshopRoot := filepath.Dir(contentRoot)
	steamAppsRoot := filepath.Dir(workshopRoot)
	downloadRoot := filepath.Dir(steamAppsRoot)
	if !filepath.IsAbs(appRoot) || filepath.Base(appRoot) != appID || filepath.Base(contentRoot) != "content" ||
		filepath.Base(workshopRoot) != "workshop" || filepath.Base(steamAppsRoot) != "steamapps" || downloadRoot == steamAppsRoot {
		return "", errors.New("Workshop 内容目录必须使用 <下载根目录>/steamapps/workshop/content/322330")
	}
	return downloadRoot, nil
}

func (a *Agent) executeModAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(request, shared.RuntimeOutcomeConfirmed, "Mod Runtime 步骤已完成")
	response := &shared.RuntimeModResult{}
	result.Mod = response
	var err error
	if request.Action == shared.RuntimeActionModDownload || request.Action == shared.RuntimeActionModLink {
		var release func()
		ctx, release, err = roomops.Acquire(ctx, "workshop:"+installation.WorkshopContentPath)
		if err != nil {
			result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
			return result, err
		}
		defer release()
	}
	switch request.Action {
	case shared.RuntimeActionModFetch:
		*response, err = a.fetchModCache(ctx, installation, *request.Mod)
	case shared.RuntimeActionModDownload:
		err = a.modDownloadRunner.Download(ctx, installation, request.Mod.WorkshopIDs)
		if err == nil {
			err = moddistribution.LinkWorkshopMods(ctx, runtimeModServerPath(installation), installation.WorkshopContentPath, request.Mod.WorkshopIDs)
		}
		response.Complete = err == nil
	case shared.RuntimeActionModLink:
		err = moddistribution.LinkWorkshopMods(ctx, runtimeModServerPath(installation), installation.WorkshopContentPath, request.Mod.WorkshopIDs)
		response.Complete = err == nil
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
	if request.Action == shared.RuntimeActionModSchemaRead {
		parsed, err := mods.ParseInstalledModInfo(ctx, mods.NewDualParser("", ""), installation.WorkshopContentPath, runtimeModServerPath(installation), request.Mod.WorkshopID)
		if err == nil {
			response.Schema = &shared.RuntimeModSchema{
				WorkshopID: request.Mod.WorkshopID, Options: parsed.Values["configuration_options"],
				Parser: parsed.Parser, FallbackUsed: parsed.FallbackUsed, FallbackReason: parsed.FallbackReason, Warnings: parsed.Warnings,
			}
			if encoded, encodeErr := json.Marshal(response.Schema); encodeErr != nil || len(encoded) > int(shared.MaximumSecureMessageBytes/2) {
				err = errors.New("Mod 配置声明超过读取大小限制或无法编码")
				response.Schema = nil
			}
		}
		response.Complete = err == nil
		if err != nil {
			result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		}
		return result, err
	}
	if request.Action == shared.RuntimeActionModFilesObserve || request.Action == shared.RuntimeActionModFilesInventory {
		worlds := make([]moddistribution.ObserveWorld, 0, len(request.Mod.Worlds))
		for _, world := range request.Mod.Worlds {
			worlds = append(worlds, moddistribution.ObserveWorld{
				RoomID: world.RoomID, RoomDirectory: world.RoomDirectory,
				WorldID: world.WorldID, WorldDirectory: world.WorldDirectory,
			})
		}
		trusted := moddistribution.TrustedInstallation{
			ID: installation.ID, ServerPath: runtimeModServerPath(installation), SavePath: installation.SavePath,
			WorkshopContentPath: runtimeWorkshopContentPath(installation),
		}
		var observation moddistribution.FilesObservation
		var err error
		if request.Action == shared.RuntimeActionModFilesInventory {
			observation, err = moddistribution.InventoryInstallationFiles(ctx, trusted)
		} else {
			observation, err = moddistribution.ObserveInstallationFiles(ctx, trusted, request.Mod.WorkshopIDs, worlds)
		}
		if err == nil {
			response.Files = runtimeModFilesObservation(observation)
			response.Complete = true
			return result, nil
		}
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
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
	case shared.RuntimeActionModPeerGrant:
		location, grantErr := a.issueModPeerGrant(ctx, installation, request.Mod.PeerSubject, request.Mod.WorkshopID, request.Mod.ExpectedTreeSHA256)
		if grantErr == nil {
			response.FetchLocation = &location
			response.Complete = true
		}
		err = grantErr
	case shared.RuntimeActionModReleaseState:
		*response, err = a.readModReleaseState(installation, manager, request.Mod.OperationID)
	case shared.RuntimeActionModInstallationState:
		state, stateErr := manager.ObserveState(ctx, installation.ID)
		if stateErr == nil {
			response.Installation = runtimeModInstallationState(state)
			response.Complete = true
		} else if errors.Is(stateErr, moddistribution.ErrNotFound) {
			response.Complete = true
		} else {
			err = stateErr
		}
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
	if mod == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.GameVersion != nil {
		return errors.New("Mod Runtime 请求负载无效")
	}
	validUpload := operationIdentity.MatchString(mod.UploadID)
	validOperation := modReleaseID.MatchString(mod.OperationID)
	validMetadata := validRuntimeModMetadata(mod.Metadata)
	emptyMetadata := mod.Metadata == (shared.RuntimeModMetadata{})
	emptyTransferCore := mod.Kind == "" && mod.UploadID == "" && mod.Size == 0 && mod.SHA256 == "" && len(mod.Data) == 0
	emptyTransfer := emptyTransferCore && mod.PeerSubject == ""
	emptyFetch := len(mod.FetchSources) == 0 && len(mod.FetchLocations) == 0 && !mod.Validate
	if request.Action != shared.RuntimeActionModFilesObserve && request.Action != shared.RuntimeActionModDownload && request.Action != shared.RuntimeActionModLink && (len(mod.WorkshopIDs) != 0 || len(mod.Worlds) != 0) {
		return errors.New("Mod 文件观察参数不能用于当前动作")
	}
	switch request.Action {
	case shared.RuntimeActionModTargetObserve:
		if !emptyTransfer || mod.OperationID != "" || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" || mod.Offset != 0 ||
			!emptyMetadata || !emptyFetch || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 运行目标观察请求无效")
		}
	case shared.RuntimeActionModInstallationState:
		if !emptyTransfer || mod.OperationID != "" || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" || mod.Offset != 0 ||
			!emptyMetadata || !emptyFetch || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 安装状态观察请求无效")
		}
	case shared.RuntimeActionModFilesObserve:
		if !emptyTransfer || mod.OperationID != "" || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" || mod.Offset != 0 ||
			!emptyMetadata || !emptyFetch || mod.RoomDirectory != "" || mod.WorldDirectory != "" || !validModFilesObservation(*mod) {
			return errors.New("Mod 文件状态观察请求无效")
		}
	case shared.RuntimeActionModFilesInventory:
		if !emptyTransfer || mod.OperationID != "" || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" || mod.Offset != 0 ||
			!emptyMetadata || !emptyFetch || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 安装目录清单请求无效")
		}
	case shared.RuntimeActionModCacheInspect:
		if !emptyTransfer || mod.OperationID != "" || !modWorkshopID.MatchString(mod.WorkshopID) || !validRuntimeDigest(mod.ExpectedTreeSHA256) ||
			mod.Offset != 0 || !emptyMetadata || !emptyFetch || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 缓存检查请求无效")
		}
	case shared.RuntimeActionModSchemaRead:
		if !emptyTransfer || mod.OperationID != "" || !modWorkshopID.MatchString(mod.WorkshopID) || mod.ExpectedTreeSHA256 != "" ||
			mod.Offset != 0 || !emptyMetadata || !emptyFetch || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 配置声明读取请求无效")
		}
	case shared.RuntimeActionModPeerGrant:
		if !emptyTransferCore || mod.OperationID != "" || !modWorkshopID.MatchString(mod.WorkshopID) || !validRuntimeDigest(mod.ExpectedTreeSHA256) ||
			mod.Offset != 0 || !emptyMetadata || !emptyFetch || !validModPeerSubject(mod.PeerSubject) || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod Peer 授权请求无效")
		}
	case shared.RuntimeActionModFetch:
		latestFromSteam := mod.ExpectedTreeSHA256 == "" && len(mod.FetchSources) == 1 && mod.FetchSources[0] == shared.RuntimeModFetchSourceSteam && len(mod.FetchLocations) == 0
		if !emptyTransfer || mod.OperationID != "" || !modWorkshopID.MatchString(mod.WorkshopID) || (!latestFromSteam && !validRuntimeDigest(mod.ExpectedTreeSHA256)) ||
			mod.Offset != 0 || !validMetadata || !validModFetchSources(*mod) || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 节点本地获取请求无效")
		}
	case shared.RuntimeActionModDownload, shared.RuntimeActionModLink:
		if !emptyTransfer || mod.OperationID != "" || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" || mod.Offset != 0 ||
			!emptyMetadata || !emptyFetch || mod.RoomDirectory != "" || mod.WorldDirectory != "" || len(mod.Worlds) != 0 || !validModDownloadIDs(mod.WorkshopIDs) {
			return errors.New("Mod 直接下载请求无效")
		}
	case shared.RuntimeActionModUploadBegin, shared.RuntimeActionModUploadWrite, shared.RuntimeActionModUploadCommit:
		if mod.Kind != shared.RuntimeModUploadCacheBundle || !validUpload || mod.OperationID != "" || !modWorkshopID.MatchString(mod.WorkshopID) ||
			!validRuntimeDigest(mod.ExpectedTreeSHA256) || mod.Size < 1 || mod.Size > modMaximumBundleBytes || !validRuntimeDigest(mod.SHA256) ||
			!validMetadata || !emptyFetch || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 缓存上传描述无效")
		}
		if err := validateModChunkAction(request.Action, mod.Offset, mod.Size, mod.Data); err != nil {
			return err
		}
	case shared.RuntimeActionModReleasePlanBegin, shared.RuntimeActionModReleasePlanWrite, shared.RuntimeActionModReleasePlanCommit:
		if mod.Kind != shared.RuntimeModUploadReleasePlan || !validUpload || !validOperation || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" ||
			mod.Size < 1 || mod.Size > modMaximumPlanBytes || !validRuntimeDigest(mod.SHA256) || !emptyMetadata ||
			!emptyFetch || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 发布计划上传描述无效")
		}
		if err := validateModChunkAction(request.Action, mod.Offset, mod.Size, mod.Data); err != nil {
			return err
		}
	case shared.RuntimeActionModReleasePrepare, shared.RuntimeActionModReleasePublish, shared.RuntimeActionModReleaseRollback,
		shared.RuntimeActionModReleaseComplete, shared.RuntimeActionModReleaseState:
		if !validOperation || !emptyTransfer || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" || mod.Offset != 0 ||
			!emptyMetadata || !emptyFetch || mod.RoomDirectory != "" || mod.WorldDirectory != "" {
			return errors.New("Mod 发布状态请求无效")
		}
	case shared.RuntimeActionModOverridesRead:
		if mod.OperationID != "" || !emptyTransfer || mod.WorkshopID != "" || mod.ExpectedTreeSHA256 != "" || mod.Offset < 0 ||
			!emptyMetadata || !emptyFetch || !safeModPathComponent(mod.RoomDirectory) || !safeModPathComponent(mod.WorldDirectory) {
			return errors.New("modoverrides.lua 分块读取请求无效")
		}
	default:
		return errors.New("Mod Runtime 动作无效")
	}
	return nil
}

func validModDownloadIDs(values []string) bool {
	if len(values) < 1 || len(values) > 4096 {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !modWorkshopID.MatchString(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func validModFilesObservation(mod shared.RuntimeModRequest) bool {
	if len(mod.WorkshopIDs) < 1 || len(mod.WorkshopIDs) > 4096 || len(mod.Worlds) > 256 {
		return false
	}
	ids := make(map[string]bool, len(mod.WorkshopIDs))
	for _, workshopID := range mod.WorkshopIDs {
		if !modWorkshopID.MatchString(workshopID) || ids[workshopID] {
			return false
		}
		ids[workshopID] = true
	}
	worlds := make(map[string]bool, len(mod.Worlds))
	for _, world := range mod.Worlds {
		key := world.RoomID + "\x00" + world.WorldID
		if !operationIdentity.MatchString(world.RoomID) || !operationIdentity.MatchString(world.WorldID) ||
			!safeModPathComponent(world.RoomDirectory) || !safeModPathComponent(world.WorldDirectory) || worlds[key] {
			return false
		}
		worlds[key] = true
	}
	return true
}

func validModFetchSources(mod shared.RuntimeModRequest) bool {
	values, locations := mod.FetchSources, mod.FetchLocations
	if len(values) < 1 || len(values) > 3 {
		return false
	}
	if len(values) == 1 && values[0] == shared.RuntimeModFetchSourceSteam {
		return len(locations) == 0
	}
	if len(values) != len(locations) {
		return false
	}
	for index, location := range locations {
		if location.Source != values[index] || !validModFetchLocation(mod, location) {
			return false
		}
	}
	return true
}

func validModFetchLocation(mod shared.RuntimeModRequest, location shared.RuntimeModFetchLocation) bool {
	if !validModDownloadToken(location.DownloadToken) || location.Size < 1 || location.Size > modMaximumBundleBytes ||
		!validRuntimeDigest(location.SHA256) || strings.ContainsAny(location.DownloadURL+location.DownloadPath+location.DownloadToken, "\x00\r\n") {
		return false
	}
	expectedSuffix := "/" + mod.WorkshopID + "/" + strings.ToLower(mod.ExpectedTreeSHA256)
	switch location.Source {
	case shared.RuntimeModFetchSourceController:
		return location.DownloadURL == "" && location.DownloadPath == "/mod-artifacts"+expectedSuffix
	case shared.RuntimeModFetchSourcePeer:
		parts := strings.Split(strings.Trim(location.DownloadPath, "/"), "/")
		if len(parts) != 4 || parts[0] != "mod-peer" || parts[1] == "" ||
			parts[2] != mod.WorkshopID || !strings.EqualFold(parts[3], mod.ExpectedTreeSHA256) {
			return false
		}
		parsed, err := url.Parse(location.DownloadURL)
		return err == nil && parsed.User == nil && parsed.Host != "" &&
			(parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Path == location.DownloadPath &&
			parsed.RawQuery == "" && parsed.Fragment == ""
	default:
		return false
	}
}

func validModPeerSubject(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func validModDownloadToken(value string) bool {
	if len(value) < 32 || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
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
		!strings.ContainsAny(value.Title+value.Version, "\x00\r\n") &&
		(value.SteamManifestID == "" || modWorkshopID.MatchString(value.SteamManifestID))
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
			ServerPath: runtimeModServerPath(installation), SavePath: installation.SavePath,
			WorkshopContentPath: runtimeWorkshopContentPath(installation),
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

func runtimeModServerPath(installation RuntimeInstallation) string {
	if installation.Driver != "container" {
		if layout, ok := dstserver.Resolve(installation.ServerPath, installation.ServerMode); ok {
			return layout.ContentRoot
		}
	}
	return installation.ServerPath
}

func runtimeWorkshopContentPath(installation RuntimeInstallation) string {
	if strings.TrimSpace(installation.UGCPath) == "" {
		return ""
	}
	return installation.WorkshopContentPath
}

func (a *Agent) fetchModCache(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeModRequest) (shared.RuntimeModResult, error) {
	manager, err := a.modManager(installation)
	if err != nil {
		return shared.RuntimeModResult{}, err
	}
	if request.ExpectedTreeSHA256 != "" {
		if manifest, verifyErr := manager.Verify(ctx, request.WorkshopID, request.ExpectedTreeSHA256); verifyErr == nil {
			result := shared.RuntimeModResult{FetchSource: request.FetchSources[0], CacheManifest: runtimeModManifest(manifest), Complete: true}
			result.CacheManifest.FetchSource = shared.RuntimeModFetchSourceCache
			result.FetchAttempts = []shared.RuntimeModFetchAttempt{successfulModFetchAttempt(shared.RuntimeModFetchSourceCache, time.Now().UTC(), 0, 0)}
			return result, nil
		}
	}
	switch request.FetchSources[0] {
	case shared.RuntimeModFetchSourceSteam:
		return a.fetchModCacheFromSteam(ctx, installation, manager, request)
	case shared.RuntimeModFetchSourcePeer, shared.RuntimeModFetchSourceController:
		var failures []error
		var attempts []shared.RuntimeModFetchAttempt
		for _, location := range request.FetchLocations {
			result, fetchErr := a.fetchModCacheFromHTTP(ctx, installation, manager, request, location)
			attempts = append(attempts, result.FetchAttempts...)
			if fetchErr == nil {
				result.FetchAttempts = attempts
				return result, nil
			}
			failures = append(failures, fmt.Errorf("%s: %w", location.Source, fetchErr))
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
		}
		return shared.RuntimeModResult{FetchSource: request.FetchSources[0], FetchAttempts: attempts}, errors.Join(failures...)
	default:
		return shared.RuntimeModResult{}, errors.New("Agent 不支持指定的 Mod 获取来源")
	}
}

func (a *Agent) fetchModCacheFromSteam(ctx context.Context, installation RuntimeInstallation, manager *moddistribution.Manager, request shared.RuntimeModRequest) (shared.RuntimeModResult, error) {
	result := shared.RuntimeModResult{FetchSource: shared.RuntimeModFetchSourceSteam}
	if a.modFetchRunner == nil {
		return result, errors.New("Agent 未配置 Mod 节点下载器")
	}
	fetchRoot := filepath.Join(installation.ModStatePath, "fetches")
	if err := ensureModDirectory(fetchRoot); err != nil {
		return result, err
	}
	staging, err := os.MkdirTemp(fetchRoot, ".steam-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(staging)
	started := time.Now()
	if err := a.modFetchRunner.Download(ctx, installation, staging, request.WorkshopID, request.Validate); err != nil {
		result.FetchAttempts = []shared.RuntimeModFetchAttempt{failedModFetchAttempt(shared.RuntimeModFetchSourceSteam, started, directoryRegularBytes(staging), "STEAM_FETCH_FAILED", err)}
		return result, fmt.Errorf("节点 Steam 下载 Workshop %s: %w", request.WorkshopID, err)
	}
	source := filepath.Join(staging, "steamapps", "workshop", "content", "322330", request.WorkshopID)
	downloadedBytes := directoryRegularBytes(source)
	duration := time.Since(started)
	manifest, err := manager.Import(ctx, request.WorkshopID, source, moddistribution.Metadata{
		Title: request.Metadata.Title, Version: request.Metadata.Version,
		PublishedFileSize: request.Metadata.PublishedFileSize, SteamManifestID: request.Metadata.SteamManifestID,
		SteamUpdatedAt: request.Metadata.SteamUpdatedAt,
	})
	if err != nil {
		result.FetchAttempts = []shared.RuntimeModFetchAttempt{failedModFetchAttemptWithDuration(shared.RuntimeModFetchSourceSteam, started, downloadedBytes, duration, "MOD_IMPORT_FAILED", err)}
		return result, fmt.Errorf("导入节点 Steam 模组 %s: %w", request.WorkshopID, err)
	}
	result.CacheManifest = runtimeModManifest(manifest)
	result.CacheManifest.FetchSource = shared.RuntimeModFetchSourceSteam
	if request.ExpectedTreeSHA256 != "" && !strings.EqualFold(manifest.TreeSHA256, request.ExpectedTreeSHA256) {
		mismatch := fmt.Errorf("节点 Steam 当前内容与期望版本不一致: Workshop %s 期望 %s，实际 %s", request.WorkshopID, request.ExpectedTreeSHA256, manifest.TreeSHA256)
		result.FetchAttempts = []shared.RuntimeModFetchAttempt{failedModFetchAttemptWithDuration(shared.RuntimeModFetchSourceSteam, started, downloadedBytes, duration, "STEAM_VERSION_MISMATCH", mismatch)}
		return result, mismatch
	}
	result.FetchAttempts = []shared.RuntimeModFetchAttempt{successfulModFetchAttempt(shared.RuntimeModFetchSourceSteam, started.UTC(), downloadedBytes, duration)}
	result.Complete = true
	return result, nil
}

func (a *Agent) fetchModCacheFromHTTP(ctx context.Context, installation RuntimeInstallation, manager *moddistribution.Manager, request shared.RuntimeModRequest, location shared.RuntimeModFetchLocation) (shared.RuntimeModResult, error) {
	result := shared.RuntimeModResult{FetchSource: location.Source}
	fetchRoot := filepath.Join(installation.ModStatePath, "fetches", "http")
	if err := ensureModDirectory(fetchRoot); err != nil {
		return result, err
	}
	path, attempt, err := downloadModArtifact(ctx, a.Config.ServerURL, request.WorkshopID, location, fetchRoot)
	result.FetchAttempts = []shared.RuntimeModFetchAttempt{attempt}
	if err != nil {
		return result, fmt.Errorf("下载 Mod 制品: %w", err)
	}
	operationprogress.Report(ctx, operationprogress.Update{
		Stage: operationprogress.StageModCache, Percent: 90,
		Message:    modArtifactProgressMessage(location.Source, request.WorkshopID, "校验完成，正在导入"),
		WorkshopID: request.WorkshopID, CurrentBytes: location.Size, TotalBytes: location.Size,
	})
	state := modUploadState{
		Version: modUploadStateVersion, UploadID: "fetch-" + location.SHA256[:32], Kind: shared.RuntimeModUploadCacheBundle,
		WorkshopID: request.WorkshopID, ExpectedTreeSHA256: strings.ToLower(request.ExpectedTreeSHA256),
		Size: location.Size, SHA256: strings.ToLower(location.SHA256), Metadata: request.Metadata, Offset: location.Size,
	}
	result, err = importModBundle(ctx, installation, manager, state, path)
	result.FetchSource = location.Source
	result.FetchAttempts = []shared.RuntimeModFetchAttempt{attempt}
	if result.CacheManifest != nil {
		result.CacheManifest.FetchSource = location.Source
	}
	if err != nil {
		result.FetchAttempts[0].Status = shared.RuntimeModFetchStatusFailed
		result.FetchAttempts[0].Feasibility = shared.RuntimeModFetchFeasibilityUnavailable
		result.FetchAttempts[0].Selected = false
		result.FetchAttempts[0].ErrorCode = "MOD_IMPORT_FAILED"
		result.FetchAttempts[0].ErrorMessage = err.Error()
		return result, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return result, err
	}
	result.FetchAttempts[0].Selected = true
	operationprogress.Report(ctx, operationprogress.Update{
		Stage: operationprogress.StageModCache, Percent: 100,
		Message:    modArtifactProgressMessage(location.Source, request.WorkshopID, "已导入"),
		WorkshopID: request.WorkshopID, CurrentBytes: location.Size, TotalBytes: location.Size,
	})
	return result, nil
}

func downloadModArtifact(ctx context.Context, serverURL, workshopID string, location shared.RuntimeModFetchLocation, directory string) (downloadedPath string, attempt shared.RuntimeModFetchAttempt, err error) {
	started := time.Now()
	attempt = shared.RuntimeModFetchAttempt{
		Source: location.Source, Feasibility: shared.RuntimeModFetchFeasibilityAvailable,
		Status: shared.RuntimeModFetchStatusSucceeded, ObservedAt: started.UTC(),
	}
	defer func() {
		attempt.DurationMillis = elapsedMilliseconds(time.Since(started))
		if attempt.Bytes > 0 {
			attempt.BytesPerSecond = bytesPerSecond(attempt.Bytes, attempt.DurationMillis)
		}
		if err != nil {
			attempt.Feasibility = shared.RuntimeModFetchFeasibilityUnavailable
			attempt.Status = shared.RuntimeModFetchStatusFailed
			attempt.ErrorCode = fetchSourceErrorCode(location.Source)
			attempt.ErrorMessage = err.Error()
		}
	}()
	downloadURL, err := resolveModArtifactDownloadURL(serverURL, location)
	if err != nil {
		return "", attempt, err
	}
	path := filepath.Join(directory, ".artifact-"+strings.ToLower(location.SHA256)+".part")
	if !pathWithinRoot(path, directory) {
		return "", attempt, errors.New("Mod 制品暂存路径越界")
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > location.Size {
			return "", attempt, errors.New("Mod 制品断点文件无效")
		}
	} else if !os.IsNotExist(statErr) {
		return "", attempt, statErr
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", attempt, err
	}
	offset, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		file.Close()
		return "", attempt, err
	}
	if offset < location.Size {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
		if err != nil {
			file.Close()
			return "", attempt, err
		}
		request.Header.Set("Authorization", "Bearer "+location.DownloadToken)
		if offset > 0 {
			request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		client := &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
				IdleConnTimeout: 30 * time.Second,
			},
			CheckRedirect: func(next *http.Request, previous []*http.Request) error {
				if len(previous) == 0 || !strings.EqualFold(next.URL.Scheme, previous[0].URL.Scheme) || !strings.EqualFold(next.URL.Host, previous[0].URL.Host) {
					return errors.New("Mod 制品下载不允许跨主机重定向")
				}
				return nil
			},
		}
		response, requestErr := client.Do(request)
		if requestErr != nil {
			file.Close()
			return "", attempt, requestErr
		}
		defer response.Body.Close()
		if offset == 0 && response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent ||
			offset > 0 && response.StatusCode != http.StatusPartialContent {
			file.Close()
			return "", attempt, fmt.Errorf("Mod 制品下载 HTTP %d", response.StatusCode)
		}
		if response.StatusCode == http.StatusPartialContent && !validModRange(response.Header.Get("Content-Range"), offset, location.Size) {
			file.Close()
			return "", attempt, errors.New("Mod 制品 Content-Range 无效")
		}
		remaining := location.Size - offset
		if response.ContentLength >= 0 && response.ContentLength != remaining {
			file.Close()
			return "", attempt, errors.New("Mod 制品响应长度与断点不一致")
		}
		progressWriter := &modArtifactProgressWriter{
			ctx: ctx, writer: file, source: location.Source, workshopID: workshopID,
			offset: offset, total: location.Size, startedAt: time.Now(),
		}
		progressWriter.report(progressWriter.startedAt, true)
		written, copyErr := io.Copy(progressWriter, io.LimitReader(response.Body, remaining+1))
		attempt.Bytes += written
		syncErr := file.Sync()
		if err := errors.Join(copyErr, syncErr); err != nil {
			file.Close()
			return "", attempt, err
		}
		if written != remaining {
			file.Close()
			if written > remaining {
				_ = os.Remove(path)
			}
			return "", attempt, io.ErrUnexpectedEOF
		}
	}
	if err := file.Close(); err != nil {
		return "", attempt, err
	}
	operationprogress.Report(ctx, operationprogress.Update{
		Stage: operationprogress.StageModCache, Percent: 85,
		Message:    modArtifactProgressMessage(location.Source, workshopID, "下载完成，正在校验"),
		WorkshopID: workshopID, CurrentBytes: location.Size, TotalBytes: location.Size,
	})
	digest, err := hashModFile(ctx, path)
	if err != nil {
		return "", attempt, err
	}
	if !strings.EqualFold(digest, location.SHA256) {
		_ = os.Remove(path)
		return "", attempt, errors.New("Mod 制品 SHA256 校验失败")
	}
	attempt.Selected = true
	return path, attempt, nil
}

type modArtifactProgressWriter struct {
	ctx        context.Context
	writer     io.Writer
	source     shared.RuntimeModFetchSource
	workshopID string
	offset     int64
	total      int64
	written    int64
	startedAt  time.Time
	lastAt     time.Time
}

func (w *modArtifactProgressWriter) Write(value []byte) (int, error) {
	written, err := w.writer.Write(value)
	w.written += int64(written)
	w.report(time.Now(), w.offset+w.written >= w.total)
	return written, err
}

func (w *modArtifactProgressWriter) report(now time.Time, force bool) {
	if !force && !w.lastAt.IsZero() && now.Sub(w.lastAt) < 250*time.Millisecond {
		return
	}
	w.lastAt = now
	current := w.offset + w.written
	if current > w.total {
		current = w.total
	}
	percent := 0
	if w.total > 0 {
		percent = int(current * 80 / w.total)
	}
	rate := int64(0)
	if elapsed := now.Sub(w.startedAt); w.written > 0 && elapsed > 0 {
		rate = int64(float64(w.written) / elapsed.Seconds())
	}
	operationprogress.Report(w.ctx, operationprogress.Update{
		Stage: operationprogress.StageModCache, Percent: percent,
		Message:    modArtifactProgressMessage(w.source, w.workshopID, "正在拉取"),
		WorkshopID: w.workshopID, CurrentBytes: current, TotalBytes: w.total, BytesPerSecond: rate,
	})
}

func modArtifactProgressMessage(source shared.RuntimeModFetchSource, workshopID, action string) string {
	label := "运行节点"
	if source == shared.RuntimeModFetchSourceController {
		label = "Controller"
	}
	return fmt.Sprintf("%s%s模组制品 · Workshop %s", label, action, workshopID)
}

func successfulModFetchAttempt(source shared.RuntimeModFetchSource, observedAt time.Time, bytes int64, duration time.Duration) shared.RuntimeModFetchAttempt {
	millis := elapsedMilliseconds(duration)
	return shared.RuntimeModFetchAttempt{
		Source: source, Feasibility: shared.RuntimeModFetchFeasibilityAvailable,
		Status: shared.RuntimeModFetchStatusSucceeded, Selected: true, Bytes: bytes,
		DurationMillis: millis, BytesPerSecond: bytesPerSecond(bytes, millis), ObservedAt: observedAt.UTC(),
	}
}

func failedModFetchAttempt(source shared.RuntimeModFetchSource, started time.Time, bytes int64, code string, cause error) shared.RuntimeModFetchAttempt {
	return failedModFetchAttemptWithDuration(source, started, bytes, time.Since(started), code, cause)
}

func failedModFetchAttemptWithDuration(source shared.RuntimeModFetchSource, started time.Time, bytes int64, duration time.Duration, code string, cause error) shared.RuntimeModFetchAttempt {
	millis := elapsedMilliseconds(duration)
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	return shared.RuntimeModFetchAttempt{
		Source: source, Feasibility: shared.RuntimeModFetchFeasibilityUnavailable,
		Status: shared.RuntimeModFetchStatusFailed, Bytes: bytes, DurationMillis: millis,
		BytesPerSecond: bytesPerSecond(bytes, millis), ErrorCode: code, ErrorMessage: message, ObservedAt: started.UTC(),
	}
}

func elapsedMilliseconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	millis := duration.Milliseconds()
	if millis == 0 {
		return 1
	}
	return millis
}

func bytesPerSecond(bytes, durationMillis int64) int64 {
	if bytes <= 0 || durationMillis <= 0 {
		return 0
	}
	return bytes * 1000 / durationMillis
}

func directoryRegularBytes(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func fetchSourceErrorCode(source shared.RuntimeModFetchSource) string {
	switch source {
	case shared.RuntimeModFetchSourcePeer:
		return "PEER_FETCH_FAILED"
	case shared.RuntimeModFetchSourceController:
		return "CONTROLLER_FETCH_FAILED"
	default:
		return "MOD_FETCH_FAILED"
	}
}

func resolveModArtifactDownloadURL(serverURL string, location shared.RuntimeModFetchLocation) (string, error) {
	if location.Source == shared.RuntimeModFetchSourcePeer {
		parsed, err := url.Parse(location.DownloadURL)
		if err != nil || parsed.User != nil || parsed.Host == "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Path != location.DownloadPath ||
			parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", errors.New("Mod Peer 下载地址无效")
		}
		return parsed.String(), nil
	}
	if location.Source != shared.RuntimeModFetchSourceController || location.DownloadURL != "" {
		return "", errors.New("Mod 制品下载来源无效")
	}
	return resolveControllerDownloadURL(serverURL, location.DownloadPath, "/mod-artifacts/")
}

func validModRange(value string, expectedOffset, expectedSize int64) bool {
	parts := modContentRange.FindStringSubmatch(strings.TrimSpace(value))
	if len(parts) != 4 {
		return false
	}
	start, startErr := strconv.ParseInt(parts[1], 10, 64)
	end, endErr := strconv.ParseInt(parts[2], 10, 64)
	size, sizeErr := strconv.ParseInt(parts[3], 10, 64)
	return startErr == nil && endErr == nil && sizeErr == nil && start == expectedOffset && end == expectedSize-1 && size == expectedSize
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
		SteamManifestID: state.Metadata.SteamManifestID, SteamUpdatedAt: state.Metadata.SteamUpdatedAt,
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
	plan, err := buildRuntimeModPlan(ctx, manager, wire, installation, a.Config.AgentID, state.OperationID)
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

func buildRuntimeModPlan(ctx context.Context, manager *moddistribution.Manager, value shared.RuntimeModPlanInput, installation RuntimeInstallation, nodeID, operationID string) (moddistribution.Plan, error) {
	if value.Mode == string(moddistribution.PlanModeContent) {
		input, err := runtimeModContentPlanInput(value, installation, nodeID, operationID)
		if err != nil {
			return moddistribution.Plan{}, err
		}
		return manager.BuildContentPlan(ctx, input)
	}
	if value.Mode != "" {
		return moddistribution.Plan{}, errors.New("Mod 发布计划模式不受支持")
	}
	input, err := runtimeModPlanInput(value, installation, nodeID, operationID)
	if err != nil {
		return moddistribution.Plan{}, err
	}
	return manager.BuildPlan(ctx, input)
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
	if value.OperationID != operationID || value.NodeID != nodeID || value.Mode != "" || value.InstallationID != "" || len(value.Mods) != 0 || len(value.Shards) == 0 {
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
			converted.Mods = append(converted.Mods, moddistribution.ModVersion{
				WorkshopID: mod.WorkshopID, TreeSHA256: mod.TreeSHA256,
				Metadata: moddistribution.Metadata{
					Title: mod.Metadata.Title, Version: mod.Metadata.Version,
					PublishedFileSize: mod.Metadata.PublishedFileSize, SteamManifestID: mod.Metadata.SteamManifestID,
					SteamUpdatedAt: mod.Metadata.SteamUpdatedAt,
				},
			})
		}
		result.Shards = append(result.Shards, converted)
	}
	return result, nil
}

func runtimeModContentPlanInput(value shared.RuntimeModPlanInput, installation RuntimeInstallation, nodeID, operationID string) (moddistribution.ContentPlanInput, error) {
	if value.OperationID != operationID || value.NodeID != nodeID || value.Mode != string(moddistribution.PlanModeContent) ||
		value.InstallationID != installation.ID || len(value.Mods) == 0 || len(value.Shards) != 0 {
		return moddistribution.ContentPlanInput{}, errors.New("Mod 安装内容发布计划无效")
	}
	result := moddistribution.ContentPlanInput{
		OperationID:    value.OperationID,
		NodeID:         value.NodeID,
		InstallationID: value.InstallationID,
		Mods:           make([]moddistribution.ModVersion, 0, len(value.Mods)),
	}
	for _, mod := range value.Mods {
		result.Mods = append(result.Mods, moddistribution.ModVersion{
			WorkshopID: mod.WorkshopID,
			TreeSHA256: mod.TreeSHA256,
			Metadata: moddistribution.Metadata{
				Title:             mod.Metadata.Title,
				Version:           mod.Metadata.Version,
				PublishedFileSize: mod.Metadata.PublishedFileSize,
				SteamManifestID:   mod.Metadata.SteamManifestID,
				SteamUpdatedAt:    mod.Metadata.SteamUpdatedAt,
			},
		})
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
			mode := os.FileMode(0o444)
			if header.Mode&0o111 != 0 {
				mode = 0o555
			}
			if err := os.Chmod(path, mode); err != nil {
				return err
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
			SteamManifestID: value.Metadata.SteamManifestID, SteamUpdatedAt: value.Metadata.SteamUpdatedAt,
		},
	}
}

func runtimeModInstallationState(value moddistribution.InstallationState) *shared.RuntimeModInstallationState {
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
			Mods: make([]shared.RuntimeModVersion, 0, len(shard.Mods)),
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

func runtimeModFilesObservation(value moddistribution.FilesObservation) *shared.RuntimeModFilesObservation {
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
			SteamUpdatedAt: cloneObservedTime(state.SteamUpdatedAt), MetadataReason: state.MetadataReason,
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

func cloneObservedTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
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
