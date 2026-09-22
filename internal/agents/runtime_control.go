package agents

import (
	"context"
	"errors"
	"strings"

	"dont/internal/requesttiming"
	"dont/shared"
)

func (s *Service) ExecuteRuntime(ctx context.Context, targetID string, request shared.RuntimeOperationRequest, timeoutSeconds int) (RuntimeExecutionResult, error) {
	targetID = strings.TrimSpace(targetID)
	if !strings.HasPrefix(targetID, "agent:") {
		return RuntimeExecutionResult{}, ErrInvalidInput
	}
	agentID := strings.TrimPrefix(targetID, "agent:")
	finishAgent := requesttiming.Start(ctx, "controller.agent_metadata")
	agent, err := s.Agent(agentID)
	finishAgent()
	if err != nil {
		return RuntimeExecutionResult{}, err
	}
	if agent.Status != StatusOnline {
		return RuntimeExecutionResult{}, ErrAgentOffline
	}
	if !containsString(agent.Capabilities, "runtime.driver.v2") || !containsString(agent.Capabilities, runtimeCapability(request.Action)) {
		return RuntimeExecutionResult{}, ErrUnsupportedAction
	}
	if shared.IsGameInstallationAction(request.Action) || request.Action == shared.RuntimeActionLuaJITObserve || (request.Action == shared.RuntimeActionLuaJITInstall || request.Action == shared.RuntimeActionLuaJITDownload) {
		registered := false
		for _, installation := range agent.Installations {
			if installation.ID == request.InstallationID {
				registered = true
				break
			}
		}
		if !registered {
			return RuntimeExecutionResult{}, ErrRuntimeInstallationNotRegistered
		}
	} else {
		finishConfig := requesttiming.Start(ctx, "controller.agent_config")
		config, err := s.runtimeConfigForAgent(agent)
		finishConfig()
		if err != nil {
			return RuntimeExecutionResult{}, err
		}
		if request.InstallationID != "" && request.InstallationID != config.InstallationID {
			return RuntimeExecutionResult{}, ErrInvalidInput
		}
		request.InstallationID = config.InstallationID
	}
	if timeoutSeconds < 5 || timeoutSeconds > runtimeTimeoutLimit(request.Action) || !shared.IsRuntimeAction(request.Action) {
		return RuntimeExecutionResult{}, ErrInvalidInput
	}
	result, err := s.transport.ExecuteRuntime(ctx, agentID, request, timeoutSeconds)
	if err != nil {
		return result, err
	}
	if result.Result.ProtocolVersion != shared.RuntimeOperationProtocolVersion || result.Result.OperationID != request.OperationID ||
		result.Result.InstallationID != request.InstallationID || result.Result.Action != request.Action ||
		result.Result.Cluster != request.Cluster || result.Result.Shard != request.Shard {
		return RuntimeExecutionResult{}, errors.New("Agent 返回的 Runtime 操作结果与请求不一致")
	}
	return result, nil
}

func runtimeCapability(action shared.RuntimeAction) string {
	if shared.IsGameInstallationAction(action) {
		return "runtime.game-install.v1"
	}
	switch action {
	case shared.RuntimeActionConsoleHealth, shared.RuntimeActionConsoleSend:
		return "runtime.console.v2"
	case shared.RuntimeActionReadLogs:
		return "runtime.logs.v1"
	case shared.RuntimeActionChatLogsList, shared.RuntimeActionChatLogsRead:
		return "runtime.chat-history.v1"
	case shared.RuntimeActionReadArtifacts:
		return "runtime.artifacts.v1"
	case shared.RuntimeActionEntityArtwork:
		return "runtime.entity-artwork.v1"
	case shared.RuntimeActionWorldStateRead:
		return "runtime.worldstate.read.v1"
	case shared.RuntimeActionObserveOperation:
		return "runtime.driver.v2"
	case shared.RuntimeActionMigrationExportPrepare, shared.RuntimeActionMigrationExportRead, shared.RuntimeActionMigrationExportRelease,
		shared.RuntimeActionMigrationImportBegin, shared.RuntimeActionMigrationImportWrite, shared.RuntimeActionMigrationImportCommit,
		shared.RuntimeActionMigrationTargetRollback, shared.RuntimeActionMigrationTargetComplete,
		shared.RuntimeActionMigrationSourceFinalize, shared.RuntimeActionMigrationSourceRollback, shared.RuntimeActionMigrationSourceComplete:
		return "runtime.migration.v1"
	case shared.RuntimeActionMigrationPeerGrant, shared.RuntimeActionMigrationFetch:
		return "runtime.migration.peer.v1"
	case shared.RuntimeActionBackupStage, shared.RuntimeActionBackupRead, shared.RuntimeActionBackupRelease,
		shared.RuntimeActionRestoreBegin, shared.RuntimeActionRestoreWrite, shared.RuntimeActionRestorePrepare,
		shared.RuntimeActionRestorePublish, shared.RuntimeActionRestoreRollback, shared.RuntimeActionRestoreComplete:
		return "runtime.backup.v1"
	case shared.RuntimeActionModFetch:
		return "runtime.mods.fetch.v2"
	case shared.RuntimeActionModDownload:
		return "runtime.mods.download.v1"
	case shared.RuntimeActionModLink:
		return "runtime.mods.local-link.v1"
	case shared.RuntimeActionModPeerGrant:
		return "runtime.mods.peer.v1"
	case shared.RuntimeActionModTargetObserve, shared.RuntimeActionModCacheInspect, shared.RuntimeActionModUploadBegin, shared.RuntimeActionModUploadWrite, shared.RuntimeActionModUploadCommit,
		shared.RuntimeActionModReleasePlanBegin, shared.RuntimeActionModReleasePlanWrite, shared.RuntimeActionModReleasePlanCommit,
		shared.RuntimeActionModReleasePrepare, shared.RuntimeActionModReleasePublish, shared.RuntimeActionModReleaseRollback,
		shared.RuntimeActionModReleaseComplete, shared.RuntimeActionModReleaseState, shared.RuntimeActionModOverridesRead:
		return "runtime.mods.v1"
	case shared.RuntimeActionModInstallationState:
		return "runtime.mods.state.v1"
	case shared.RuntimeActionModFilesObserve:
		return "runtime.mods.files.v1"
	case shared.RuntimeActionModFilesInventory:
		return "runtime.mods.inventory.v1"
	case shared.RuntimeActionModSchemaRead:
		return "runtime.mods.schema.v1"
	case shared.RuntimeActionLuaJITObserve, shared.RuntimeActionLuaJITInstall, shared.RuntimeActionLuaJITDownload:
		return "runtime.luajit.v2"
	case shared.RuntimeActionGameVersionObserve, shared.RuntimeActionGameVersionUpdate:
		return "runtime.game-update.v1"
	case shared.RuntimeActionNetworkEgressObserve:
		return "runtime.network.v1"
	case shared.RuntimeActionNetworkEndpointListen, shared.RuntimeActionNetworkEndpointProbe:
		return "runtime.network.endpoints.v1"
	case shared.RuntimeActionCPUPrepare, shared.RuntimeActionCPUApply, shared.RuntimeActionCPUObserve:
		return "runtime.cpu.v1"
	case shared.RuntimeActionConfigurationRead:
		return "runtime.configuration.read.v1"
	case shared.RuntimeActionConfigurationApply:
		return "runtime.configuration.apply.v1"
	case shared.RuntimeActionModConfigurationWrite:
		return "runtime.configuration.mod-write.v1"
	case shared.RuntimeActionClusterTokenReveal:
		return "runtime.configuration.secrets.v1"
	case shared.RuntimeActionConfigurationBegin, shared.RuntimeActionConfigurationWrite, shared.RuntimeActionConfigurationPrepare,
		shared.RuntimeActionConfigurationPublish, shared.RuntimeActionConfigurationRollback, shared.RuntimeActionConfigurationComplete:
		return "runtime.configuration.v1"
	case shared.RuntimeActionRoomRecoveryMove:
		return "runtime.room-recovery.v1"
	case shared.RuntimeActionMapSessions, shared.RuntimeActionMapStatus, shared.RuntimeActionMapSnapshotPrepare, shared.RuntimeActionMapRender,
		shared.RuntimeActionMapRead, shared.RuntimeActionMapRelease:
		return "runtime.maps.v1"
	default:
		return ""
	}
}

func runtimeTimeoutLimit(action shared.RuntimeAction) int {
	if action == shared.RuntimeActionGameInstallationInstall {
		return 1800
	}
	if (action == shared.RuntimeActionLuaJITInstall || action == shared.RuntimeActionLuaJITDownload) || action == shared.RuntimeActionGameVersionUpdate || action == shared.RuntimeActionMigrationFetch || action == shared.RuntimeActionModDownload {
		return 1800
	}
	return 300
}
