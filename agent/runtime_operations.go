package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"dont/internal/configpublication"
	"dont/internal/consoledispatch"
	"dont/internal/networkprobe"
	"dont/internal/roomops"
	"dont/internal/runtimefiles"
	"dont/internal/shards"
	"dont/internal/shardtransfer"
	"dont/shared"
)

type rememberedRuntimeOperation struct {
	OperationID  string                        `json:"operation_id"`
	Action       shared.RuntimeAction          `json:"action"`
	Cluster      string                        `json:"cluster"`
	Shard        string                        `json:"shard"`
	Fingerprint  string                        `json:"fingerprint"`
	Completed    bool                          `json:"completed"`
	ErrorMessage string                        `json:"error_message,omitempty"`
	AcceptedAt   time.Time                     `json:"accepted_at"`
	Result       shared.RuntimeOperationResult `json:"result"`
}

type backgroundConsoleRuntime interface {
	SendBackground(context.Context, string, string, string, string) error
}

type consoleHealthRuntime interface {
	ConsoleHealth(string, string) consoledispatch.Health
}

func (a *Agent) executeRuntimeOperation(commandType string, request *shared.RuntimeOperationRequest, timeout int) (shared.RuntimeOperationResult, error) {
	return a.executeRuntimeOperationContext(context.Background(), commandType, request, timeout)
}

func (a *Agent) executeRuntimeOperationContext(parent context.Context, commandType string, request *shared.RuntimeOperationRequest, timeout int) (shared.RuntimeOperationResult, error) {
	if request == nil {
		return shared.RuntimeOperationResult{}, errors.New("Runtime 操作负载缺失")
	}
	now := a.now().UTC()
	if err := validateRuntimeOperationRequest(commandType, *request, timeout, now); err != nil {
		return shared.RuntimeOperationResult{}, err
	}
	installation, exists := a.runtimeInstallation(request.InstallationID)
	if !exists {
		return shared.RuntimeOperationResult{}, errors.New("Agent 未登记该 DST 安装")
	}
	if runtimeActionRequiresExistingShard(request.Action) {
		if err := validateShardOwnership(installation, request.Cluster, request.Shard); err != nil {
			return shared.RuntimeOperationResult{}, err
		}
	}
	if parent == nil {
		parent = context.Background()
	}
	operationContext, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Second)
	defer cancel()

	if !shared.RuntimeOperationRequiresLease(*request) {
		if request.Action == shared.RuntimeActionConsoleSend {
			control, err := a.runtimeControl(installation)
			if err != nil {
				return shared.RuntimeOperationResult{}, err
			}
			return executeConsoleSend(operationContext, control, installation, *request)
		}
		if isMapAction(request.Action) {
			return a.observeMapAction(operationContext, installation, *request)
		}
		if isCPUAction(request.Action) {
			return a.executeCPUAction(operationContext, installation, *request)
		}
		if isModAction(request.Action) {
			return a.observeModAction(operationContext, installation, *request)
		}
		if request.Action == shared.RuntimeActionMigrationPeerGrant {
			return a.observeMigrationPeerGrant(installation, *request)
		}
		if shared.IsGameInstallationAction(request.Action) {
			return a.executeGameInstallation(operationContext, installation, *request)
		}
		if request.Action == shared.RuntimeActionLuaJITObserve {
			return a.executeLuaJITAction(operationContext, installation, *request)
		}
		if request.Action == shared.RuntimeActionGameVersionObserve {
			return a.observeGameVersion(operationContext, installation, *request)
		}
		if request.Action == shared.RuntimeActionNetworkEgressObserve {
			return observeNetworkEgress(operationContext, *request)
		}
		if request.Action == shared.RuntimeActionNetworkEndpointListen {
			return listenNetworkEndpoint(operationContext, *request)
		}
		if request.Action == shared.RuntimeActionNetworkEndpointProbe {
			return probeNetworkEndpoints(operationContext, *request)
		}
		if request.Action == shared.RuntimeActionConfigurationRead {
			return readConfigurationAction(operationContext, installation, *request)
		}
		if request.Action == shared.RuntimeActionClusterTokenReveal {
			return revealClusterTokenAction(operationContext, installation, *request)
		}
		control, err := a.runtimeControl(installation)
		if err != nil {
			return shared.RuntimeOperationResult{}, err
		}
		return a.observeRuntimeAction(operationContext, control, installation, *request)
	}
	lockKey := request.InstallationID + "\x00" + request.Cluster
	if isModAction(request.Action) {
		lockKey = request.InstallationID + "\x00mods"
	} else if request.Action == shared.RuntimeActionLuaJITInstall || request.Action == shared.RuntimeActionLuaJITDownload {
		lockKey = request.InstallationID + "\x00game-version"
	} else if shared.IsGameInstallationAction(request.Action) || request.Action == shared.RuntimeActionGameVersionUpdate {
		lockKey = request.InstallationID + "\x00game-version"
	}
	operationContext, release, err := roomops.Acquire(operationContext, lockKey)
	if err != nil {
		return shared.RuntimeOperationResult{}, err
	}
	defer release()
	if cached, beginErr := a.shardState.beginRuntime(*request, now); cached != nil {
		return *cached, beginErr
	} else if beginErr != nil {
		return shared.RuntimeOperationResult{}, beginErr
	}
	var result shared.RuntimeOperationResult
	var operationErr error
	if shared.IsGameInstallationAction(request.Action) {
		result, operationErr = a.executeGameInstallation(operationContext, installation, *request)
	} else if request.Action == shared.RuntimeActionLuaJITInstall || request.Action == shared.RuntimeActionLuaJITDownload {
		result, operationErr = a.executeLuaJITAction(operationContext, installation, *request)
	} else if isModAction(request.Action) {
		result, operationErr = a.executeModAction(operationContext, installation, *request)
	} else if request.Action == shared.RuntimeActionGameVersionUpdate {
		result, operationErr = a.updateGameVersion(operationContext, installation, *request)
	} else if isCPUAction(request.Action) {
		result, operationErr = a.executeCPUAction(operationContext, installation, *request)
	} else if request.Action == shared.RuntimeActionConsoleSend {
		control, controlErr := a.runtimeControl(installation)
		if controlErr != nil {
			result, operationErr = runtimeResult(*request, shared.RuntimeOutcomeFailed, controlErr.Error()), controlErr
		} else {
			result, operationErr = executeConsoleSend(operationContext, control, installation, *request)
		}
	} else if isMigrationAction(request.Action) {
		result, operationErr = a.executeMigrationAction(operationContext, installation, *request)
	} else if isConfigurationAction(request.Action) {
		result, operationErr = a.executeConfigurationAction(operationContext, installation, *request)
	} else if request.Action == shared.RuntimeActionRoomRecoveryMove {
		result, operationErr = moveRoomToRecovery(installation, *request)
	} else if isMapAction(request.Action) {
		result, operationErr = a.executeMapAction(operationContext, installation, *request)
	} else {
		result, operationErr = a.executeBackupAction(operationContext, installation, *request)
	}
	if finishErr := a.shardState.finishRuntime(*request, result, operationErr); finishErr != nil {
		return shared.RuntimeOperationResult{}, fmt.Errorf("保存 Agent Runtime 操作结果: %w", finishErr)
	}
	return result, operationErr
}

func validateRuntimeOperationRequest(commandType string, request shared.RuntimeOperationRequest, timeout int, now time.Time) error {
	if request.ProtocolVersion != shared.RuntimeOperationProtocolVersion || !shared.IsRuntimeAction(request.Action) ||
		commandType != string(request.Action) || timeout < 5 || timeout > runtimeOperationTimeoutLimit(request.Action) ||
		!runtimeInstallationID.MatchString(request.InstallationID) || !shardResourceName.MatchString(request.Cluster) ||
		!shardResourceName.MatchString(request.Shard) || !operationIdentity.MatchString(request.OperationID) ||
		len(request.TopologyRevision) < 1 || len(request.TopologyRevision) > 128 || strings.ContainsAny(request.TopologyRevision, "\x00\r\n") {
		return errors.New("Runtime 操作请求无效")
	}
	if shared.RuntimeOperationRequiresLease(request) {
		if !operationIdentity.MatchString(request.OperationKey) || !operationIdentity.MatchString(request.LeaseID) ||
			request.FencingToken == 0 || request.LeaseExpiresAt == nil || request.LeaseExpiresAt.Before(now.Add(-30*time.Second)) ||
			request.LeaseExpiresAt.After(now.Add(10*time.Minute)) {
			return errors.New("Runtime 操作租约无效或已过期")
		}
	}
	if shared.IsGameInstallationAction(request.Action) {
		return validateGameInstallationPayload(request)
	}
	if request.GameInstallation != nil {
		return errors.New("Runtime 操作包含无关游戏安装负载")
	}
	if (request.Action == shared.RuntimeActionLuaJITInstall || request.Action == shared.RuntimeActionLuaJITDownload) || request.Action == shared.RuntimeActionLuaJITObserve {
		return validateLuaJITPayload(request)
	}
	if request.LuaJIT != nil {
		return errors.New("Runtime 操作包含无关 LuaJIT 负载")
	}
	if !isCPUAction(request.Action) && request.CPU != nil {
		return errors.New("Runtime 操作包含无关 CPU 负载")
	}
	if request.Action == shared.RuntimeActionClusterTokenReveal {
		if request.Console != nil || request.Logs != nil || request.ChatLogs != nil || request.Artifacts != nil || request.Observation != nil ||
			request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil || request.Network != nil || request.CPU != nil ||
			request.Configuration != nil || request.Map != nil {
			return errors.New("Cluster Token 读取请求包含无关负载")
		}
		return nil
	}
	if isMapAction(request.Action) {
		return validateMapOperationPayload(request)
	}
	if request.Map != nil {
		return errors.New("Runtime 操作包含无关地图负载")
	}
	if isConfigurationAction(request.Action) {
		return validateConfigurationOperationPayload(request)
	}
	if request.Configuration != nil {
		return errors.New("Runtime 操作包含无关配置发布负载")
	}
	if request.Action == shared.RuntimeActionRoomRecoveryMove {
		if request.Console != nil || request.Logs != nil || request.ChatLogs != nil || request.Artifacts != nil || request.Observation != nil ||
			request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil || request.Network != nil || request.CPU != nil || request.Map != nil {
			return errors.New("房间回收请求包含无关负载")
		}
		return nil
	}
	if !isNetworkAction(request.Action) && request.Network != nil {
		return errors.New("Runtime 操作包含无关网络探测负载")
	}
	if request.Action != shared.RuntimeActionChatLogsList && request.Action != shared.RuntimeActionChatLogsRead && request.ChatLogs != nil {
		return errors.New("Runtime 操作包含无关聊天历史负载")
	}
	switch request.Action {
	case shared.RuntimeActionConsoleHealth, shared.RuntimeActionWorldStateRead:
		if request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil {
			return errors.New("控制台健康请求包含无关负载")
		}
	case shared.RuntimeActionConsoleSend:
		if request.Console == nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil {
			return errors.New("控制台请求负载无效")
		}
		console := request.Console
		if console.Mode != shared.ConsoleModeManaged && console.Mode != shared.ConsoleModeProbe && console.Mode != shared.ConsoleModeRaw ||
			strings.TrimSpace(console.Command) == "" || len(console.Command) > shared.MaximumRuntimeConsoleCommandBytes || !utf8.ValidString(console.Command) ||
			strings.ContainsAny(console.Command, "\x00\r\n") || len(console.CoalesceKey) > 80 || strings.ContainsAny(console.CoalesceKey, "\x00\r\n") ||
			console.Mode != shared.ConsoleModeProbe && strings.TrimSpace(console.CoalesceKey) != "" ||
			console.CommandDocument != nil && (console.Mode != shared.ConsoleModeManaged || strings.TrimSpace(console.CoalesceKey) != "") {
			return errors.New("控制台请求内容无效")
		}
		if console.CommandDocument != nil {
			if err := runtimefiles.ValidateCommandDocument(*console.CommandDocument); err != nil {
				return err
			}
		}
	case shared.RuntimeActionReadLogs:
		if request.Logs == nil || request.Console != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil ||
			!shared.IsRuntimeLogSource(request.Logs.Source) ||
			request.Logs.Cursor < -1 || request.Logs.MaxBytes < 1 || request.Logs.MaxBytes > runtimefiles.MaximumLogBytes ||
			(request.Logs.Raw && request.Logs.MaxLines != 0 || !request.Logs.Raw && (request.Logs.MaxLines < 1 || request.Logs.MaxLines > 2000)) ||
			request.Logs.Raw && strings.TrimSpace(request.Logs.Query) != "" || len([]rune(request.Logs.Query)) > 256 ||
			len(request.Logs.FileID) > 128 || strings.ContainsAny(request.Logs.FileID, "\x00\r\n") {
			return errors.New("日志读取请求无效")
		}
	case shared.RuntimeActionChatLogsList:
		if request.ChatLogs == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil ||
			request.ChatLogs.GenerationID != "" || request.ChatLogs.Cursor != 0 || request.ChatLogs.MaxBytes != 0 || request.ChatLogs.MaxLines != 0 {
			return errors.New("聊天历史代次请求无效")
		}
	case shared.RuntimeActionChatLogsRead:
		if request.ChatLogs == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil ||
			request.ChatLogs.GenerationID == "" || len(request.ChatLogs.GenerationID) > 128 || strings.ContainsAny(request.ChatLogs.GenerationID, "\x00\r\n") ||
			request.ChatLogs.Cursor < 0 || request.ChatLogs.MaxBytes < 1 || request.ChatLogs.MaxBytes > runtimefiles.MaximumLogBytes || request.ChatLogs.MaxLines < 1 || request.ChatLogs.MaxLines > 2000 {
			return errors.New("聊天历史读取请求无效")
		}
	case shared.RuntimeActionReadArtifacts:
		if request.Artifacts == nil || request.Console != nil || request.Logs != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil || !runtimefiles.IsArtifactKind(request.Artifacts.Kind) {
			return errors.New("Runtime 制品读取请求无效")
		}
	case shared.RuntimeActionObserveOperation:
		if request.Observation == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil ||
			!operationIdentity.MatchString(request.Observation.ObservedOperationID) ||
			request.Observation.ObservedOperationKey != "" && !operationIdentity.MatchString(request.Observation.ObservedOperationKey) {
			return errors.New("Runtime 操作观察请求无效")
		}
	case shared.RuntimeActionMigrationExportPrepare, shared.RuntimeActionMigrationExportRead, shared.RuntimeActionMigrationExportRelease,
		shared.RuntimeActionMigrationPeerGrant, shared.RuntimeActionMigrationFetch,
		shared.RuntimeActionMigrationImportBegin, shared.RuntimeActionMigrationImportWrite, shared.RuntimeActionMigrationImportCommit,
		shared.RuntimeActionMigrationTargetRollback, shared.RuntimeActionMigrationTargetComplete,
		shared.RuntimeActionMigrationSourceFinalize, shared.RuntimeActionMigrationSourceRollback, shared.RuntimeActionMigrationSourceComplete:
		return validateMigrationOperationPayload(request, now)
	case shared.RuntimeActionModTargetObserve, shared.RuntimeActionModCacheInspect, shared.RuntimeActionModPeerGrant, shared.RuntimeActionModFetch, shared.RuntimeActionModDownload, shared.RuntimeActionModLink, shared.RuntimeActionModUploadBegin, shared.RuntimeActionModUploadWrite, shared.RuntimeActionModUploadCommit,
		shared.RuntimeActionModReleasePlanBegin, shared.RuntimeActionModReleasePlanWrite, shared.RuntimeActionModReleasePlanCommit,
		shared.RuntimeActionModReleasePrepare, shared.RuntimeActionModReleasePublish, shared.RuntimeActionModReleaseRollback,
		shared.RuntimeActionModReleaseComplete, shared.RuntimeActionModReleaseState, shared.RuntimeActionModInstallationState, shared.RuntimeActionModFilesObserve, shared.RuntimeActionModFilesInventory, shared.RuntimeActionModSchemaRead, shared.RuntimeActionModOverridesRead:
		if err := validateModOperationPayload(request); err != nil {
			return err
		}
	case shared.RuntimeActionGameVersionObserve, shared.RuntimeActionGameVersionUpdate:
		if err := validateGameVersionPayload(request); err != nil {
			return err
		}
	case shared.RuntimeActionNetworkEgressObserve:
		if request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil {
			return errors.New("公网出口探测请求包含无关负载")
		}
		if request.Network == nil || !shared.IsRuntimeNetworkRegion(request.Network.Region) {
			return errors.New("公网出口探测区域无效")
		}
		if request.Network.BindAddress != "" || request.Network.Port != 0 || len(request.Network.Tokens) != 0 ||
			len(request.Network.Endpoints) != 0 || request.Network.TimeoutMillis != 0 {
			return errors.New("公网出口探测请求包含无关端点参数")
		}
	case shared.RuntimeActionNetworkEndpointListen:
		if err := validateNetworkEndpointListen(request); err != nil {
			return err
		}
	case shared.RuntimeActionNetworkEndpointProbe:
		if err := validateNetworkEndpointProbe(request); err != nil {
			return err
		}
	case shared.RuntimeActionCPUPrepare, shared.RuntimeActionCPUApply, shared.RuntimeActionCPUObserve:
		if err := validateCPUOperationPayload(request); err != nil {
			return err
		}
	default:
		if err := validateBackupOperationPayload(request); err != nil {
			return err
		}
	}
	return nil
}

func validateCPUOperationPayload(request shared.RuntimeOperationRequest) error {
	if request.CPU == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil || !shared.IsRuntimeCPUPolicy(request.CPU.Policy) {
		return errors.New("CPU Runtime 请求无效")
	}
	ids := request.CPU.LogicalCPUIds
	if request.CPU.Policy == shared.RuntimeCPUPolicyNone && len(ids) != 0 || request.CPU.Policy != shared.RuntimeCPUPolicyNone && (len(ids) == 0 || len(ids) > 4096) {
		return errors.New("CPU Runtime 策略与逻辑 CPU 选择不一致")
	}
	seen := make(map[int]bool, len(ids))
	for _, id := range ids {
		if id < 0 || id > 1048575 || seen[id] {
			return errors.New("CPU Runtime 逻辑 CPU 列表无效")
		}
		seen[id] = true
	}
	return nil
}

func validateConfigurationOperationPayload(request shared.RuntimeOperationRequest) error {
	value := request.Configuration
	if request.Action == shared.RuntimeActionConfigurationApply {
		if value == nil || value.ExpectedSHA256 != "" || value.Offset != 0 ||
			request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil ||
			request.Backup != nil || request.Mod != nil || request.GameVersion != nil || request.Network != nil || request.CPU != nil || request.Map != nil {
			return errors.New("配置保存 Runtime 请求无效")
		}
		return configpublication.ValidateApply(configpublication.Descriptor{
			PublicationID: value.PublicationID, Cluster: request.Cluster, Shard: request.Shard,
			Scope: configpublication.Scope(value.Scope), Size: value.Size, SHA256: value.SHA256,
		}, value.Data, value.ExpectedFiles)
	}
	if value != nil && len(value.ExpectedFiles) != 0 {
		return errors.New("配置 Runtime 请求包含无关文件 revision")
	}
	if request.Action == shared.RuntimeActionModConfigurationWrite {
		if value == nil || value.Scope != runtimefiles.ConfigurationScopeMod || value.PublicationID != "" || value.Offset != 0 || value.Size != 0 || value.SHA256 != "" ||
			len(value.Data) == 0 || len(value.Data) > shared.MaximumRuntimeModOverridesBytes ||
			request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil ||
			request.Backup != nil || request.Mod != nil || request.GameVersion != nil || request.Network != nil || request.CPU != nil || request.Map != nil {
			return errors.New("Mod 配置写入请求无效")
		}
		digest, err := hex.DecodeString(value.ExpectedSHA256)
		if err != nil || len(digest) != sha256.Size {
			return errors.New("Mod 配置写入缺少有效的文件 revision")
		}
		return nil
	}
	if value == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil ||
		request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil || request.CPU != nil || request.Map != nil ||
		value.ExpectedSHA256 != "" || value.Offset < 0 || value.Size < 0 || len(value.SHA256) > 64 || len(value.Data) > configpublication.MaxChunkBytes {
		return errors.New("配置发布 Runtime 请求无效")
	}
	if request.Action == shared.RuntimeActionConfigurationRead {
		if value.Scope != runtimefiles.ConfigurationScopeShared && value.Scope != runtimefiles.ConfigurationScopeWorld && value.Scope != runtimefiles.ConfigurationScopeMod && value.Scope != runtimefiles.ConfigurationScopeTokenStatus {
			return errors.New("配置读取 Runtime 范围无效")
		}
		if value.PublicationID != "" || value.Offset != 0 || value.Size != 0 || value.SHA256 != "" || len(value.Data) != 0 {
			return errors.New("配置读取 Runtime 请求无效")
		}
		return nil
	}
	if value.Scope != string(configpublication.ScopeShared) && value.Scope != string(configpublication.ScopeWorld) && value.Scope != string(configpublication.ScopeMod) {
		return errors.New("配置发布 Runtime 范围无效")
	}
	if !operationIdentity.MatchString(value.PublicationID) {
		return errors.New("配置发布 Runtime 请求无效")
	}
	if request.Action == shared.RuntimeActionConfigurationBegin && (value.Size < 1 || len(value.SHA256) != 64 || len(value.Data) != 0) {
		return errors.New("配置发布开始请求无效")
	}
	if request.Action == shared.RuntimeActionConfigurationWrite && (len(value.Data) == 0 || value.Size < 1) {
		return errors.New("配置发布块为空")
	}
	if request.Action != shared.RuntimeActionConfigurationBegin && request.Action != shared.RuntimeActionConfigurationWrite &&
		(value.Offset != 0 || value.Size != 0 || value.SHA256 != "" || len(value.Data) != 0) {
		return errors.New("配置发布步骤包含无关负载")
	}
	return nil
}

func readConfigurationAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	configuration, err := runtimefiles.ReadConfiguration(ctx, installation.SavePath, request.Cluster, request.Shard, request.Configuration.Scope)
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "Runtime 配置已读取")
	result.Configuration = &configuration
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func revealClusterTokenAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	token, err := runtimefiles.ReadClusterToken(ctx, installation.SavePath, request.Cluster, request.Shard)
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "Cluster Token 已读取")
	result.ClusterToken = &token
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func validateMapOperationPayload(request shared.RuntimeOperationRequest) error {
	value := request.Map
	if value == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil ||
		request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil || request.CPU != nil || request.Configuration != nil ||
		value.Offset < 0 || len(value.Layers) > 4 {
		return errors.New("地图 Runtime 请求无效")
	}
	validTransfer := operationIdentity.MatchString(value.TransferID)
	validSession := safeRuntimeMapComponent(value.SessionID) && safeRuntimeMapComponent(value.FileName)
	switch request.Action {
	case shared.RuntimeActionMapSessions, shared.RuntimeActionMapStatus:
		if value.TransferID != "" || value.SessionID != "" || value.FileName != "" || value.Offset != 0 || len(value.Layers) != 0 {
			return errors.New("地图 Session 请求包含无关负载")
		}
	case shared.RuntimeActionMapSnapshotPrepare:
		if !validTransfer || !validSession || value.Offset != 0 || len(value.Layers) != 0 {
			return errors.New("地图 Session 准备请求无效")
		}
	case shared.RuntimeActionMapRender:
		if !validTransfer || !validSession || value.Offset != 0 || !validRuntimeMapLayers(value.Layers) {
			return errors.New("地图渲染请求无效")
		}
	case shared.RuntimeActionMapRead:
		if !validTransfer || value.SessionID != "" || value.FileName != "" || len(value.Layers) != 0 {
			return errors.New("地图读取请求无效")
		}
	case shared.RuntimeActionMapRelease:
		if !validTransfer || value.SessionID != "" || value.FileName != "" || value.Offset != 0 || len(value.Layers) != 0 {
			return errors.New("地图释放请求无效")
		}
	default:
		return errors.New("地图 Runtime 动作无效")
	}
	return nil
}

func safeRuntimeMapComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !strings.ContainsAny(value, "/\\\x00\r\n") && len(value) <= 255
}

func validRuntimeMapLayers(values []string) bool {
	if len(values) == 0 {
		return true
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "terrain" && value != "features" && value != "worldState" || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func (a *Agent) observeRuntimeAction(ctx context.Context, control shardRuntimeControl, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "Runtime 状态已读取")
	switch request.Action {
	case shared.RuntimeActionConsoleHealth:
		status, err := control.Status(ctx, request.Cluster, request.Shard)
		health := shared.RuntimeConsoleHealth{Available: true, Accepting: status.State == shards.RuntimeRunning, Runtime: sharedRuntimeStatus(status)}
		if provider, ok := control.(consoleHealthRuntime); ok {
			current := provider.ConsoleHealth(request.Cluster, request.Shard)
			health.Accepting, health.Busy, health.Pending = current.Accepting, current.Busy, current.Pending
			health.Status, health.PendingLimit, health.InstanceID = current.Status, current.PendingLimit, current.InstanceID
			health.Class, health.CoalesceKey, health.StartedAt = string(current.Class), current.CoalesceKey, current.StartedAt
			health.Maintenance, health.MaintenanceOwner, health.MaintenanceStartedAt = current.Maintenance, current.MaintenanceOwner, current.MaintenanceStartedAt
			health.InputDirty, health.ExternalWriter = current.InputDirty, current.ExternalWriter
		}
		result.ConsoleHealth = &health
		return result, err
	case shared.RuntimeActionReadLogs:
		chunk, err := runtimefiles.ReadLogs(ctx, installation.SavePath, request.Cluster, request.Shard, *request.Logs)
		result.Logs = &chunk
		return result, err
	case shared.RuntimeActionChatLogsList:
		generations, err := runtimefiles.ListChatLogGenerations(ctx, installation.SavePath, request.Cluster, request.Shard)
		result.ChatLogs = &shared.RuntimeChatLogResult{Generations: generations}
		return result, err
	case shared.RuntimeActionChatLogsRead:
		chatLogs, err := runtimefiles.ReadChatLogGeneration(ctx, installation.SavePath, request.Cluster, request.Shard, *request.ChatLogs)
		result.ChatLogs = &chatLogs
		return result, err
	case shared.RuntimeActionReadArtifacts:
		bundle, err := runtimefiles.ReadArtifacts(ctx, installation.SavePath, request.Cluster, request.Shard, request.Artifacts.Kind)
		result.Artifacts = &bundle
		return result, err
	case shared.RuntimeActionWorldStateRead:
		status, err := control.Status(ctx, request.Cluster, request.Shard)
		if err != nil {
			return result, err
		}
		value, err := runtimefiles.ReadWorldState(ctx, installation.SavePath, request.Cluster, request.Shard, sharedRuntimeStatus(status))
		result.WorldState = &value
		return result, err
	case shared.RuntimeActionObserveOperation:
		evidence, err := a.shardState.observeRuntime(request.InstallationID, request.Cluster, *request.Observation)
		result.Evidence = &evidence
		return result, err
	case shared.RuntimeActionMigrationExportRead:
		transfer, err := a.transferManager(installation)
		if err != nil {
			return result, err
		}
		chunk, err := transfer.ReadExport(ctx, request.Migration.MigrationID, request.Migration.Offset)
		result.Migration = &shared.RuntimeMigrationResult{
			MigrationID: request.Migration.MigrationID, Offset: chunk.Offset, NextOffset: chunk.NextOffset,
			Size: chunk.Size, SHA256: chunk.SHA256, Data: chunk.Data, Complete: chunk.Complete,
		}
		return result, err
	case shared.RuntimeActionBackupRead:
		transfer, err := a.transferManager(installation)
		if err != nil {
			return result, err
		}
		chunk, err := transfer.ReadBackup(ctx, request.Backup.BackupID, request.Backup.Offset)
		result.Backup = &shared.RuntimeBackupResult{
			BackupID: request.Backup.BackupID, Offset: chunk.Offset, NextOffset: chunk.NextOffset,
			Size: chunk.Size, SHA256: chunk.SHA256, Data: chunk.Data, Complete: chunk.Complete,
		}
		return result, err
	default:
		return result, errors.New("Runtime 操作不受支持")
	}
}

func (a *Agent) executeMigrationAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	transfer, err := a.transferManager(installation)
	result := runtimeResult(request, shared.RuntimeOutcomeConfirmed, "分片迁移步骤已完成")
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	migration := *request.Migration
	response := &shared.RuntimeMigrationResult{MigrationID: migration.MigrationID}
	result.Migration = response
	switch request.Action {
	case shared.RuntimeActionMigrationExportPrepare:
		descriptor, stepErr := transfer.PrepareExport(ctx, migration.MigrationID, request.Cluster, request.Shard)
		response.Size, response.SHA256, response.Complete = descriptor.Size, descriptor.SHA256, stepErr == nil
		err = stepErr
	case shared.RuntimeActionMigrationExportRelease:
		err = transfer.ReleaseExport(migration.MigrationID)
		if err == nil {
			a.revokeMigrationPeerGrants(installation.ID, migration.MigrationID)
		}
		response.Complete = err == nil
	case shared.RuntimeActionMigrationFetch:
		response.NextOffset, err = a.fetchMigrationImport(ctx, installation, migration)
		response.Offset, response.Size, response.SHA256 = migration.Offset, migration.Size, migration.SHA256
		response.Complete = err == nil && response.NextOffset == migration.Size
	case shared.RuntimeActionMigrationImportBegin:
		descriptor, stepErr := transfer.BeginImportWithShardEndpoint(
			migration.MigrationID, migration.Size, migration.SHA256,
			migration.ShardBindAll, migration.ShardMasterAddress, migration.ShardMasterPort,
		)
		response.Size, response.SHA256 = descriptor.Size, descriptor.SHA256
		err = stepErr
	case shared.RuntimeActionMigrationImportWrite:
		next, stepErr := transfer.WriteImport(migration.MigrationID, migration.Offset, migration.Data)
		response.Offset, response.NextOffset, response.Size = migration.Offset, next, migration.Size
		response.Complete, err = migration.Size > 0 && next == migration.Size, stepErr
	case shared.RuntimeActionMigrationImportCommit:
		descriptor, stepErr := transfer.CommitImport(ctx, migration.MigrationID, request.Cluster, request.Shard)
		response.Size, response.SHA256, response.Complete = descriptor.Size, descriptor.SHA256, stepErr == nil
		err = stepErr
	case shared.RuntimeActionMigrationTargetRollback:
		err = transfer.RollbackTarget(migration.MigrationID)
		response.Complete = err == nil
	case shared.RuntimeActionMigrationTargetComplete:
		err = transfer.CompleteTarget(migration.MigrationID)
		response.Complete = err == nil
	case shared.RuntimeActionMigrationSourceFinalize:
		response.RecoveryRef, err = transfer.FinalizeSource(migration.MigrationID, request.Cluster, request.Shard)
		response.Complete = err == nil
	case shared.RuntimeActionMigrationSourceRollback:
		err = transfer.RollbackSource(migration.MigrationID)
		response.Complete = err == nil
	case shared.RuntimeActionMigrationSourceComplete:
		response.RecoveryRef, err = transfer.CompleteSource(migration.MigrationID)
		response.Complete = err == nil
	default:
		err = errors.New("分片迁移动作不受支持")
	}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func (a *Agent) executeConfigurationAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	if request.Action == shared.RuntimeActionModConfigurationWrite {
		value := request.Configuration
		revision, err := runtimefiles.PublishModOverrides(ctx, installation.SavePath, request.Cluster, request.Shard, value.ExpectedSHA256, value.Data)
		result := runtimeResult(request, shared.RuntimeOutcomeConfirmed, "Mod 配置已保存")
		result.Configuration = &shared.RuntimeConfigurationResult{SHA256: revision, Complete: err == nil}
		if err != nil {
			var conflict *runtimefiles.ConfigurationConflictError
			result.Configuration.RevisionConflict = errors.As(err, &conflict)
			result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		}
		return result, err
	}
	manager, err := configpublication.New(installation.SavePath, filepath.Join(a.Config.OperationStateFile+".configurations", installation.ID))
	result := runtimeResult(request, shared.RuntimeOutcomeConfirmed, "配置发布步骤已完成")
	value := *request.Configuration
	response := &shared.RuntimeConfigurationResult{PublicationID: value.PublicationID, Size: value.Size, SHA256: value.SHA256}
	result.Configuration = response
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	switch request.Action {
	case shared.RuntimeActionConfigurationApply:
		response.Warnings, err = manager.Apply(ctx, configpublication.Descriptor{
			PublicationID: value.PublicationID, Cluster: request.Cluster, Shard: request.Shard,
			Scope: configpublication.Scope(value.Scope), Size: value.Size, SHA256: value.SHA256,
		}, value.Data, value.ExpectedFiles)
		response.Complete = err == nil
		response.RevisionConflict = errors.Is(err, configpublication.ErrRevisionConflict)
	case shared.RuntimeActionConfigurationBegin:
		response.NextOffset, err = manager.Begin(configpublication.Descriptor{
			PublicationID: value.PublicationID, Cluster: request.Cluster, Shard: request.Shard,
			Scope: configpublication.Scope(value.Scope), Size: value.Size, SHA256: value.SHA256,
		})
	case shared.RuntimeActionConfigurationWrite:
		response.Offset = value.Offset
		response.NextOffset, err = manager.Write(value.PublicationID, value.Offset, value.Data)
		response.Complete = err == nil && response.NextOffset == value.Size
	case shared.RuntimeActionConfigurationPrepare:
		err = manager.Prepare(ctx, value.PublicationID)
		response.Complete = err == nil
	case shared.RuntimeActionConfigurationPublish:
		err = manager.Publish(value.PublicationID)
		response.Complete = err == nil
	case shared.RuntimeActionConfigurationRollback:
		err = manager.Rollback(value.PublicationID)
		response.Complete = err == nil
	case shared.RuntimeActionConfigurationComplete:
		err = manager.Complete(value.PublicationID)
		response.Complete = err == nil
	default:
		err = errors.New("配置发布动作不受支持")
	}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func (a *Agent) executeBackupAction(ctx context.Context, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	transfer, err := a.transferManager(installation)
	result := runtimeResult(request, shared.RuntimeOutcomeConfirmed, "备份 Runtime 步骤已完成")
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	backup := *request.Backup
	response := &shared.RuntimeBackupResult{BackupID: backup.BackupID}
	result.Backup = response
	switch request.Action {
	case shared.RuntimeActionBackupStage:
		descriptor, stepErr := transfer.PrepareBackup(ctx, backup.BackupID, request.Cluster, request.Shard)
		response.Size, response.ContentSize, response.FileCount = descriptor.Size, descriptor.ContentSize, descriptor.FileCount
		response.SHA256, response.SharedSHA256, response.Complete = descriptor.SHA256, descriptor.SharedSHA256, stepErr == nil
		err = stepErr
	case shared.RuntimeActionBackupRelease:
		err = transfer.ReleaseBackup(backup.BackupID)
		response.Complete = err == nil
	case shared.RuntimeActionRestoreBegin:
		descriptor, stepErr := transfer.BeginRestore(backupDescriptor(request))
		response.Size, response.ContentSize, response.FileCount = descriptor.Size, descriptor.ContentSize, descriptor.FileCount
		response.SHA256, response.SharedSHA256, response.Complete = descriptor.SHA256, descriptor.SharedSHA256, stepErr == nil
		err = stepErr
	case shared.RuntimeActionRestoreWrite:
		response.NextOffset, err = transfer.WriteRestore(backup.BackupID, backup.Offset, backup.Data)
		response.Offset, response.Size, response.SHA256 = backup.Offset, backup.Size, backup.SHA256
		response.Complete = err == nil && response.NextOffset == backup.Size
	case shared.RuntimeActionRestorePrepare:
		descriptor, stepErr := transfer.PrepareRestore(ctx, backup.BackupID)
		response.Size, response.ContentSize, response.FileCount = descriptor.Size, descriptor.ContentSize, descriptor.FileCount
		response.SHA256, response.SharedSHA256, response.Complete = descriptor.SHA256, descriptor.SharedSHA256, stepErr == nil
		err = stepErr
	case shared.RuntimeActionRestorePublish:
		response.RecoveryRef, err = transfer.PublishRestore(backup.BackupID, backup.PublishShared)
		response.Complete = err == nil
	case shared.RuntimeActionRestoreRollback:
		err = transfer.RollbackRestore(backup.BackupID)
		response.Complete = err == nil
	case shared.RuntimeActionRestoreComplete:
		response.RecoveryRef, err = transfer.CompleteRestore(backup.BackupID)
		response.Complete = err == nil
	default:
		err = errors.New("备份 Runtime 动作不受支持")
	}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func backupDescriptor(request shared.RuntimeOperationRequest) shardtransfer.BackupDescriptor {
	backup := request.Backup
	return shardtransfer.BackupDescriptor{
		BackupID: backup.BackupID, Cluster: request.Cluster, Shard: request.Shard,
		Size: backup.Size, ContentSize: backup.ContentSize, FileCount: backup.FileCount,
		SHA256: backup.SHA256, SharedSHA256: backup.SharedSHA256,
	}
}

func validateBackupOperationPayload(request shared.RuntimeOperationRequest) error {
	backup := request.Backup
	if backup == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Mod != nil || request.GameVersion != nil ||
		!operationIdentity.MatchString(backup.BackupID) || backup.Offset < 0 || backup.Size < 0 || backup.ContentSize < 0 || backup.FileCount < 0 ||
		len(backup.Data) > shardtransfer.MaxChunkBytes || len(backup.SHA256) > 64 || len(backup.SharedSHA256) > 64 {
		return errors.New("备份 Runtime 请求无效")
	}
	emptyDescriptor := func() bool {
		return backup.Size == 0 && backup.ContentSize == 0 && backup.FileCount == 0 && backup.SHA256 == "" && backup.SharedSHA256 == "" && len(backup.Data) == 0
	}
	switch request.Action {
	case shared.RuntimeActionBackupStage, shared.RuntimeActionBackupRelease, shared.RuntimeActionRestorePrepare,
		shared.RuntimeActionRestoreRollback, shared.RuntimeActionRestoreComplete:
		if backup.Offset != 0 || backup.PublishShared || !emptyDescriptor() {
			return errors.New("备份 Runtime 请求包含无关负载")
		}
	case shared.RuntimeActionBackupRead:
		if backup.PublishShared || !emptyDescriptor() {
			return errors.New("备份读取请求包含无关负载")
		}
	case shared.RuntimeActionRestoreBegin:
		if backup.Offset != 0 || backup.PublishShared || len(backup.Data) != 0 || backup.Size < 1 || backup.Size > shardtransfer.MaximumTransferBytes ||
			backup.ContentSize < 1 || backup.ContentSize > shardtransfer.MaximumTransferBytes || backup.FileCount < 2 || backup.FileCount > shardtransfer.MaximumTransferEntries ||
			!validRuntimeDigest(backup.SHA256) || !validRuntimeDigest(backup.SharedSHA256) {
			return errors.New("恢复描述无效")
		}
	case shared.RuntimeActionRestoreWrite:
		if backup.PublishShared || len(backup.Data) < 1 || backup.Size < 1 || backup.Size > shardtransfer.MaximumTransferBytes ||
			backup.Offset > backup.Size || int64(len(backup.Data)) > backup.Size-backup.Offset || !validRuntimeDigest(backup.SHA256) ||
			backup.ContentSize != 0 || backup.FileCount != 0 || backup.SharedSHA256 != "" {
			return errors.New("恢复数据块无效")
		}
	case shared.RuntimeActionRestorePublish:
		if backup.Offset != 0 || !emptyDescriptor() {
			return errors.New("恢复发布请求包含无关负载")
		}
	default:
		return errors.New("备份 Runtime 动作无效")
	}
	return nil
}

func validRuntimeDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validateMigrationOperationPayload(request shared.RuntimeOperationRequest, now time.Time) error {
	migration := request.Migration
	if migration == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil ||
		request.Backup != nil || request.Mod != nil || request.GameVersion != nil || !operationIdentity.MatchString(migration.MigrationID) ||
		migration.Offset < 0 || migration.Size < 0 || len(migration.Data) > shardtransfer.MaxChunkBytes || len(migration.SHA256) > 64 {
		return errors.New("分片迁移请求无效")
	}
	emptyData := migration.Offset == 0 && migration.Size == 0 && migration.SHA256 == "" && len(migration.Data) == 0
	emptyPeer := migration.PeerSubject == "" && len(migration.FetchLocations) == 0
	emptyRouting := !migration.ShardBindAll && migration.ShardMasterAddress == "" && migration.ShardMasterPort == 0
	switch request.Action {
	case shared.RuntimeActionMigrationExportPrepare, shared.RuntimeActionMigrationExportRelease,
		shared.RuntimeActionMigrationImportCommit, shared.RuntimeActionMigrationTargetRollback, shared.RuntimeActionMigrationTargetComplete,
		shared.RuntimeActionMigrationSourceFinalize, shared.RuntimeActionMigrationSourceRollback, shared.RuntimeActionMigrationSourceComplete:
		if !emptyData || !emptyPeer || !emptyRouting {
			return errors.New("分片迁移步骤包含无关负载")
		}
	case shared.RuntimeActionMigrationExportRead:
		if migration.Size != 0 || migration.SHA256 != "" || len(migration.Data) != 0 || !emptyPeer || !emptyRouting {
			return errors.New("分片迁移读取请求无效")
		}
	case shared.RuntimeActionMigrationPeerGrant:
		if migration.Offset != 0 || migration.Size < 1 || migration.Size > shardtransfer.MaximumTransferBytes ||
			!validRuntimeDigest(migration.SHA256) || len(migration.Data) != 0 || !validModPeerSubject(migration.PeerSubject) ||
			len(migration.FetchLocations) != 0 || !emptyRouting {
			return errors.New("分片迁移 Peer 授权请求无效")
		}
	case shared.RuntimeActionMigrationFetch:
		if migration.Offset != 0 || migration.Size < 1 || migration.Size > shardtransfer.MaximumTransferBytes ||
			!validRuntimeDigest(migration.SHA256) || len(migration.Data) != 0 || migration.PeerSubject != "" ||
			len(migration.FetchLocations) < 1 || len(migration.FetchLocations) > 2 || !emptyRouting {
			return errors.New("分片迁移 Peer 获取请求无效")
		}
		for _, location := range migration.FetchLocations {
			if !validMigrationFetchLocation(location, *migration, now) {
				return errors.New("分片迁移 Peer 获取位置无效")
			}
		}
	case shared.RuntimeActionMigrationImportBegin:
		address := strings.TrimSpace(strings.Trim(migration.ShardMasterAddress, "[]"))
		if migration.Offset != 0 || migration.Size < 1 || migration.Size > shardtransfer.MaximumTransferBytes ||
			!validRuntimeDigest(migration.SHA256) || len(migration.Data) != 0 || !emptyPeer ||
			address != "" && (!validNetworkEndpointAddress(address) || migration.ShardMasterPort < 1 || migration.ShardMasterPort > 65535) ||
			address == "" && migration.ShardMasterPort != 0 {
			return errors.New("分片迁移目标导入描述无效")
		}
	case shared.RuntimeActionMigrationImportWrite:
		if migration.Size < 1 || migration.Size > shardtransfer.MaximumTransferBytes || !validRuntimeDigest(migration.SHA256) ||
			len(migration.Data) < 1 || migration.Offset > migration.Size || int64(len(migration.Data)) > migration.Size-migration.Offset ||
			!emptyPeer || !emptyRouting {
			return errors.New("分片迁移数据块无效")
		}
	default:
		return errors.New("分片迁移动作无效")
	}
	return nil
}

func isMigrationAction(action shared.RuntimeAction) bool {
	switch action {
	case shared.RuntimeActionMigrationExportPrepare, shared.RuntimeActionMigrationExportRead, shared.RuntimeActionMigrationExportRelease,
		shared.RuntimeActionMigrationPeerGrant, shared.RuntimeActionMigrationFetch,
		shared.RuntimeActionMigrationImportBegin, shared.RuntimeActionMigrationImportWrite, shared.RuntimeActionMigrationImportCommit,
		shared.RuntimeActionMigrationTargetRollback, shared.RuntimeActionMigrationTargetComplete,
		shared.RuntimeActionMigrationSourceFinalize, shared.RuntimeActionMigrationSourceRollback, shared.RuntimeActionMigrationSourceComplete:
		return true
	default:
		return false
	}
}

func isModAction(action shared.RuntimeAction) bool {
	switch action {
	case shared.RuntimeActionModTargetObserve, shared.RuntimeActionModCacheInspect, shared.RuntimeActionModPeerGrant, shared.RuntimeActionModFetch, shared.RuntimeActionModDownload, shared.RuntimeActionModLink, shared.RuntimeActionModUploadBegin, shared.RuntimeActionModUploadWrite, shared.RuntimeActionModUploadCommit,
		shared.RuntimeActionModReleasePlanBegin, shared.RuntimeActionModReleasePlanWrite, shared.RuntimeActionModReleasePlanCommit,
		shared.RuntimeActionModReleasePrepare, shared.RuntimeActionModReleasePublish, shared.RuntimeActionModReleaseRollback,
		shared.RuntimeActionModReleaseComplete, shared.RuntimeActionModReleaseState, shared.RuntimeActionModInstallationState, shared.RuntimeActionModFilesObserve, shared.RuntimeActionModFilesInventory, shared.RuntimeActionModSchemaRead, shared.RuntimeActionModOverridesRead:
		return true
	default:
		return false
	}
}

func isCPUAction(action shared.RuntimeAction) bool {
	return action == shared.RuntimeActionCPUPrepare || action == shared.RuntimeActionCPUApply || action == shared.RuntimeActionCPUObserve
}

func isConfigurationAction(action shared.RuntimeAction) bool {
	switch action {
	case shared.RuntimeActionConfigurationRead, shared.RuntimeActionConfigurationApply, shared.RuntimeActionModConfigurationWrite, shared.RuntimeActionConfigurationBegin, shared.RuntimeActionConfigurationWrite, shared.RuntimeActionConfigurationPrepare,
		shared.RuntimeActionConfigurationPublish, shared.RuntimeActionConfigurationRollback, shared.RuntimeActionConfigurationComplete:
		return true
	default:
		return false
	}
}

func isMapAction(action shared.RuntimeAction) bool {
	switch action {
	case shared.RuntimeActionMapSessions, shared.RuntimeActionMapStatus, shared.RuntimeActionMapSnapshotPrepare, shared.RuntimeActionMapRender,
		shared.RuntimeActionMapRead, shared.RuntimeActionMapRelease:
		return true
	default:
		return false
	}
}

func (a *Agent) transferManager(installation RuntimeInstallation) (*shardtransfer.Manager, error) {
	a.shardTransferMu.Lock()
	defer a.shardTransferMu.Unlock()
	if existing := a.shardTransfers[installation.ID]; existing != nil {
		return existing, nil
	}
	created, err := shardtransfer.New(installation.SavePath, filepath.Join(a.Config.OperationStateFile+".transfers", installation.ID))
	if err != nil {
		return nil, err
	}
	a.shardTransfers[installation.ID] = created
	return created, nil
}

func runtimeActionRequiresExistingShard(action shared.RuntimeAction) bool {
	if shared.IsGameInstallationAction(action) {
		return false
	}
	switch action {
	case shared.RuntimeActionMigrationImportBegin, shared.RuntimeActionMigrationImportWrite, shared.RuntimeActionMigrationImportCommit,
		shared.RuntimeActionMigrationFetch,
		shared.RuntimeActionMigrationTargetRollback, shared.RuntimeActionMigrationTargetComplete,
		shared.RuntimeActionMigrationExportRelease, shared.RuntimeActionMigrationSourceRollback, shared.RuntimeActionMigrationSourceComplete,
		shared.RuntimeActionBackupRead, shared.RuntimeActionBackupRelease,
		shared.RuntimeActionRestoreBegin, shared.RuntimeActionRestoreWrite, shared.RuntimeActionRestorePrepare,
		shared.RuntimeActionRestorePublish, shared.RuntimeActionRestoreRollback, shared.RuntimeActionRestoreComplete,
		shared.RuntimeActionModTargetObserve, shared.RuntimeActionModCacheInspect, shared.RuntimeActionModPeerGrant, shared.RuntimeActionModFetch, shared.RuntimeActionModUploadBegin, shared.RuntimeActionModUploadWrite,
		shared.RuntimeActionModUploadCommit, shared.RuntimeActionModDownload, shared.RuntimeActionModLink, shared.RuntimeActionModReleasePlanBegin, shared.RuntimeActionModReleasePlanWrite,
		shared.RuntimeActionModReleasePlanCommit, shared.RuntimeActionModReleasePrepare, shared.RuntimeActionModReleasePublish,
		shared.RuntimeActionModReleaseRollback, shared.RuntimeActionModReleaseComplete, shared.RuntimeActionModReleaseState, shared.RuntimeActionModInstallationState, shared.RuntimeActionModFilesObserve, shared.RuntimeActionModFilesInventory,
		shared.RuntimeActionModSchemaRead, shared.RuntimeActionModOverridesRead:
		return false
	case shared.RuntimeActionMapRead, shared.RuntimeActionMapRelease:
		return false
	case shared.RuntimeActionLuaJITObserve, shared.RuntimeActionLuaJITInstall, shared.RuntimeActionLuaJITDownload, shared.RuntimeActionGameVersionObserve, shared.RuntimeActionGameVersionUpdate:
		return false
	case shared.RuntimeActionNetworkEgressObserve, shared.RuntimeActionNetworkEndpointListen, shared.RuntimeActionNetworkEndpointProbe:
		return false
	case shared.RuntimeActionRoomRecoveryMove:
		return false
	default:
		return true
	}
}

func moveRoomToRecovery(installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(request, shared.RuntimeOutcomeConfirmed, "房间已移入运行节点回收目录")
	source := filepath.Join(installation.SavePath, request.Cluster)
	if !pathWithinRoot(source, installation.SavePath) {
		return runtimeResult(request, shared.RuntimeOutcomeFailed, "房间目录越出受信存档根目录"), errors.New("房间目录越出受信存档根目录")
	}
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("房间目录不是受信普通目录")
		}
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, "房间目录不存在或不受信"
		return result, err
	}
	trashRoot := filepath.Join(installation.SavePath, ".dst-admin-trash")
	if err := os.MkdirAll(trashRoot, 0o750); err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	trashInfo, err := os.Lstat(trashRoot)
	if err != nil || !trashInfo.IsDir() || trashInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("房间回收根目录不受信")
		}
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, "房间回收根目录不可用"
		return result, err
	}
	trashName := fmt.Sprintf("%d-%s", time.Now().UTC().UnixNano(), request.Cluster)
	target := filepath.Join(trashRoot, trashName)
	if err := os.Rename(source, target); err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	result.RoomRecovery = &shared.RuntimeRoomRecoveryResult{RecoveryRef: filepath.ToSlash(filepath.Join(".dst-admin-trash", trashName))}
	return result, nil
}

func observeNetworkEgress(ctx context.Context, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	address, err := networkprobe.Detect(ctx, request.Network.Region)
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "已探测运行节点的公网出口地址")
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	result.Network = &shared.RuntimeNetworkResult{Address: address.String(), Region: request.Network.Region, ObservedAt: time.Now().UTC()}
	return result, nil
}

func listenNetworkEndpoint(ctx context.Context, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	timeout := time.Duration(request.Network.TimeoutMillis) * time.Millisecond
	received, err := networkprobe.ListenEndpoint(ctx, request.Network.BindAddress, request.Network.Port, request.Network.Tokens, timeout)
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "端点监听探测已完成")
	result.Network = &shared.RuntimeNetworkResult{ReceivedTokens: received, ObservedAt: time.Now().UTC()}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	return result, nil
}

func probeNetworkEndpoints(ctx context.Context, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	timeout := time.Duration(request.Network.TimeoutMillis) * time.Millisecond
	probes := networkprobe.ProbeEndpoints(ctx, request.Network.Endpoints, timeout)
	result := runtimeResult(request, shared.RuntimeOutcomeObserved, "候选端点探测已完成")
	result.Network = &shared.RuntimeNetworkResult{EndpointProbes: probes, ObservedAt: time.Now().UTC()}
	return result, nil
}

func isNetworkAction(action shared.RuntimeAction) bool {
	return action == shared.RuntimeActionNetworkEgressObserve || action == shared.RuntimeActionNetworkEndpointListen ||
		action == shared.RuntimeActionNetworkEndpointProbe
}

func validateNetworkEndpointListen(request shared.RuntimeOperationRequest) error {
	if request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil ||
		request.Backup != nil || request.Mod != nil || request.GameVersion != nil || request.Network == nil {
		return errors.New("端点监听探测请求包含无关负载")
	}
	value := request.Network
	if value.Region != "" || net.ParseIP(strings.TrimSpace(value.BindAddress)) == nil || value.Port < 1 || value.Port > 65535 ||
		len(value.Tokens) < 1 || len(value.Tokens) > 64 || len(value.Endpoints) != 0 || value.TimeoutMillis < 500 || value.TimeoutMillis > 15_000 {
		return errors.New("端点监听探测参数无效")
	}
	seen := make(map[string]bool, len(value.Tokens))
	for _, token := range value.Tokens {
		if len(token) < 16 || len(token) > 128 || !operationIdentity.MatchString(token) || seen[token] {
			return errors.New("端点监听探测令牌无效")
		}
		seen[token] = true
	}
	return nil
}

func validateNetworkEndpointProbe(request shared.RuntimeOperationRequest) error {
	if request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil ||
		request.Backup != nil || request.Mod != nil || request.GameVersion != nil || request.Network == nil {
		return errors.New("端点连通探测请求包含无关负载")
	}
	value := request.Network
	if value.Region != "" || value.BindAddress != "" || value.Port != 0 || len(value.Tokens) != 0 ||
		len(value.Endpoints) < 1 || len(value.Endpoints) > 64 || value.TimeoutMillis < 500 || value.TimeoutMillis > 15_000 {
		return errors.New("端点连通探测参数无效")
	}
	for _, endpoint := range value.Endpoints {
		if !validNetworkEndpointAddress(endpoint.Address) || endpoint.Port < 1 || endpoint.Port > 65535 ||
			len(endpoint.Token) < 16 || len(endpoint.Token) > 128 || !operationIdentity.MatchString(endpoint.Token) {
			return errors.New("端点连通探测候选无效")
		}
	}
	return nil
}

func validNetworkEndpointAddress(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 253 || strings.ContainsAny(value, "\x00\r\n \t/\\") {
		return false
	}
	if net.ParseIP(strings.Trim(value, "[]")) != nil {
		return true
	}
	for _, label := range strings.Split(strings.TrimSuffix(value, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for index := range len(label) {
			character := label[index]
			if character != '-' && (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
				return false
			}
		}
	}
	return true
}

func runtimeOperationTimeoutLimit(action shared.RuntimeAction) int {
	if action == shared.RuntimeActionGameInstallationInstall {
		return 1800
	}
	if (action == shared.RuntimeActionLuaJITInstall || action == shared.RuntimeActionLuaJITDownload) || action == shared.RuntimeActionGameVersionUpdate || action == shared.RuntimeActionMigrationFetch {
		return 1800
	}
	return 300
}

func executeConsoleSend(ctx context.Context, control shardRuntimeControl, installation RuntimeInstallation, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	console := *request.Console
	var err error
	if console.CommandDocument != nil {
		err = runtimefiles.PublishCommandDocument(ctx, installation.SavePath, request.Cluster, request.Shard, *console.CommandDocument)
	}
	if err == nil && console.Mode == shared.ConsoleModeProbe && strings.TrimSpace(console.CoalesceKey) != "" {
		if background, ok := control.(backgroundConsoleRuntime); ok {
			err = background.SendBackground(ctx, request.Cluster, request.Shard, console.CoalesceKey, console.Command)
		} else {
			err = control.Send(ctx, request.Cluster, request.Shard, console.Command)
		}
	} else if err == nil {
		err = control.Send(ctx, request.Cluster, request.Shard, console.Command)
	}
	result := runtimeResult(request, shared.RuntimeOutcomeSent, "控制台命令已发送；是否执行成功需要对应回执或状态证据")
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	return result, err
}

func runtimeResult(request shared.RuntimeOperationRequest, outcome shared.RuntimeOutcome, message string) shared.RuntimeOperationResult {
	return shared.RuntimeOperationResult{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: request.OperationID, OperationKey: request.OperationKey,
		InstallationID: request.InstallationID, Action: request.Action, Cluster: request.Cluster, Shard: request.Shard,
		FencingToken: request.FencingToken, Outcome: outcome, Message: message, ObservedAt: time.Now().UTC(),
	}
}

func sharedRuntimeStatus(status shards.RuntimeStatus) shared.ShardRuntimeStatus {
	return shared.ShardRuntimeStatus{State: string(status.State), Code: status.Code, Message: status.Message, SessionExists: status.SessionExists, Paused: status.Paused}
}

func (state *shardOperationState) beginRuntime(request shared.RuntimeOperationRequest, now time.Time) (*shared.RuntimeOperationResult, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	roomKey := runtimeOperationRoomKey(request)
	originalRoom, roomExisted := state.Rooms[roomKey]
	room := cloneShardRoomState(originalRoom)
	if room.RuntimeOperations == nil {
		room.RuntimeOperations = make(map[string]rememberedRuntimeOperation)
	}
	fingerprint, err := runtimeOperationFingerprint(request)
	if err != nil {
		return nil, err
	}
	if remembered, exists := room.RuntimeOperations[request.OperationKey]; exists {
		if remembered.Fingerprint != fingerprint {
			return nil, errors.New("幂等键已被另一项 Runtime 操作使用")
		}
		if !remembered.Completed {
			return nil, errOperationOutcomeUnknown
		}
		result := remembered.Result
		result.Idempotent = true
		if remembered.ErrorMessage != "" {
			return &result, errors.New(remembered.ErrorMessage)
		}
		return &result, nil
	}
	if request.FencingToken < room.FencingToken {
		return nil, fmt.Errorf("fencing token 已过期: %d < %d", request.FencingToken, room.FencingToken)
	}
	if request.FencingToken == room.FencingToken && room.FencingToken > 0 && room.LeaseID != request.LeaseID {
		return nil, errors.New("fencing token 已由另一租约占用")
	}
	room.FencingToken, room.LeaseID = request.FencingToken, request.LeaseID
	room.RuntimeOperations[request.OperationKey] = rememberedRuntimeOperation{
		OperationID: request.OperationID, Action: request.Action, Cluster: request.Cluster, Shard: request.Shard, Fingerprint: fingerprint, AcceptedAt: now.UTC(),
	}
	state.Rooms[roomKey] = room
	if err := state.persistLocked(); err != nil {
		if roomExisted {
			state.Rooms[roomKey] = originalRoom
		} else {
			delete(state.Rooms, roomKey)
		}
		return nil, err
	}
	return nil, nil
}

func (state *shardOperationState) finishRuntime(request shared.RuntimeOperationRequest, result shared.RuntimeOperationResult, operationErr error) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	roomKey := runtimeOperationRoomKey(request)
	room := state.Rooms[roomKey]
	remembered, exists := room.RuntimeOperations[request.OperationKey]
	if !exists {
		return errors.New("Agent Runtime 操作状态缺失")
	}
	remembered.Completed, remembered.Result = true, result
	if operationErr != nil {
		remembered.ErrorMessage = operationErr.Error()
	}
	room.RuntimeOperations[request.OperationKey] = remembered
	trimRememberedRuntimeOperations(room.RuntimeOperations)
	state.Rooms[roomKey] = room
	return state.persistLocked()
}

func runtimeOperationRoomKey(request shared.RuntimeOperationRequest) string {
	if isModAction(request.Action) {
		return request.InstallationID + "\x00mods"
	}
	return request.InstallationID + "\x00" + strings.ToLower(request.Cluster)
}

func trimRememberedRuntimeOperations(values map[string]rememberedRuntimeOperation) {
	if len(values) <= maximumRememberedOperationsPerRoom {
		return
	}
	type operationTime struct {
		key string
		at  time.Time
	}
	completed := make([]operationTime, 0, len(values))
	for key, value := range values {
		if value.Completed {
			completed = append(completed, operationTime{key: key, at: value.Result.ObservedAt})
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].at.Before(completed[j].at) })
	for _, item := range completed {
		if len(values) <= maximumRememberedOperationsPerRoom {
			break
		}
		delete(values, item.key)
	}
}

func (state *shardOperationState) observeRuntime(installationID, cluster string, request shared.RuntimeObservationRequest) (shared.RuntimeOperationEvidence, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	room := state.Rooms[installationID+"\x00"+strings.ToLower(cluster)]
	for key, operation := range room.RuntimeOperations {
		if operation.OperationID != request.ObservedOperationID && key != request.ObservedOperationKey {
			continue
		}
		outcome := operation.Result.Outcome
		if !operation.Completed {
			outcome = shared.RuntimeOutcomeUnknown
		}
		return shared.RuntimeOperationEvidence{OperationID: request.ObservedOperationID, Action: string(operation.Action), Completed: operation.Completed, Outcome: outcome, Message: operation.Result.Message, ObservedAt: operation.Result.ObservedAt}, nil
	}
	for key, operation := range room.Operations {
		if operation.Result.OperationID != request.ObservedOperationID && (operation.Result.OperationID != "" || key != request.ObservedOperationKey) {
			continue
		}
		outcome := shared.RuntimeOutcomeConfirmed
		if !operation.Completed {
			outcome = shared.RuntimeOutcomeUnknown
		} else if operation.ErrorMessage != "" {
			outcome = shared.RuntimeOutcomeFailed
		}
		return shared.RuntimeOperationEvidence{OperationID: request.ObservedOperationID, Action: string(operation.Action), Completed: operation.Completed, Outcome: outcome, Message: operation.Result.Message, ObservedAt: operation.Result.ObservedAt}, nil
	}
	return shared.RuntimeOperationEvidence{}, errors.New("未找到需要观察的 Runtime 操作")
}

func runtimeOperationFingerprint(request shared.RuntimeOperationRequest) (string, error) {
	copy := request
	copy.LeaseExpiresAt = nil
	data, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
