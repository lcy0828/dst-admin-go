package agents

import (
	"context"
	"errors"
	"strings"

	"dont/shared"
)

func (s *Service) ExecuteRuntime(ctx context.Context, targetID string, request shared.RuntimeOperationRequest, timeoutSeconds int) (RuntimeExecutionResult, error) {
	targetID = strings.TrimSpace(targetID)
	if !strings.HasPrefix(targetID, "agent:") {
		return RuntimeExecutionResult{}, ErrInvalidInput
	}
	agentID := strings.TrimPrefix(targetID, "agent:")
	agent, err := s.Agent(agentID)
	if err != nil {
		return RuntimeExecutionResult{}, err
	}
	if agent.Status != StatusOnline {
		return RuntimeExecutionResult{}, ErrAgentOffline
	}
	if !containsString(agent.Capabilities, "runtime.driver.v1") || !containsString(agent.Capabilities, runtimeCapability(request.Action)) {
		return RuntimeExecutionResult{}, ErrUnsupportedAction
	}
	config, err := s.store.RuntimeConfig(agentID)
	if err != nil {
		return RuntimeExecutionResult{}, err
	}
	if request.InstallationID != "" && request.InstallationID != config.InstallationID {
		return RuntimeExecutionResult{}, ErrInvalidInput
	}
	request.InstallationID = config.InstallationID
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
	switch action {
	case shared.RuntimeActionConsoleHealth, shared.RuntimeActionConsoleSend:
		return "runtime.console.v1"
	case shared.RuntimeActionReadLogs:
		return "runtime.logs.v1"
	case shared.RuntimeActionReadArtifacts:
		return "runtime.artifacts.v1"
	case shared.RuntimeActionObserveOperation:
		return "runtime.driver.v1"
	case shared.RuntimeActionMigrationExportPrepare, shared.RuntimeActionMigrationExportRead, shared.RuntimeActionMigrationExportRelease,
		shared.RuntimeActionMigrationImportBegin, shared.RuntimeActionMigrationImportWrite, shared.RuntimeActionMigrationImportCommit,
		shared.RuntimeActionMigrationTargetRollback, shared.RuntimeActionMigrationTargetComplete,
		shared.RuntimeActionMigrationSourceFinalize, shared.RuntimeActionMigrationSourceRollback, shared.RuntimeActionMigrationSourceComplete:
		return "runtime.migration.v1"
	case shared.RuntimeActionBackupStage, shared.RuntimeActionBackupRead, shared.RuntimeActionBackupRelease,
		shared.RuntimeActionRestoreBegin, shared.RuntimeActionRestoreWrite, shared.RuntimeActionRestorePrepare,
		shared.RuntimeActionRestorePublish, shared.RuntimeActionRestoreRollback, shared.RuntimeActionRestoreComplete:
		return "runtime.backup.v1"
	case shared.RuntimeActionModTargetObserve, shared.RuntimeActionModCacheInspect, shared.RuntimeActionModUploadBegin, shared.RuntimeActionModUploadWrite, shared.RuntimeActionModUploadCommit,
		shared.RuntimeActionModReleasePlanBegin, shared.RuntimeActionModReleasePlanWrite, shared.RuntimeActionModReleasePlanCommit,
		shared.RuntimeActionModReleasePrepare, shared.RuntimeActionModReleasePublish, shared.RuntimeActionModReleaseRollback,
		shared.RuntimeActionModReleaseComplete, shared.RuntimeActionModReleaseState, shared.RuntimeActionModOverridesRead:
		return "runtime.mods.v1"
	case shared.RuntimeActionGameVersionObserve, shared.RuntimeActionGameVersionUpdate:
		return "runtime.game-update.v1"
	case shared.RuntimeActionCPUPrepare, shared.RuntimeActionCPUApply, shared.RuntimeActionCPUObserve:
		return "runtime.cpu.v1"
	default:
		return ""
	}
}

func runtimeTimeoutLimit(action shared.RuntimeAction) int {
	if action == shared.RuntimeActionGameVersionUpdate {
		return 1800
	}
	return 300
}
