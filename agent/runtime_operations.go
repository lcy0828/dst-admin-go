package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"dont/internal/configpublication"
	"dont/internal/consoledispatch"
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
	operationContext, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	if !shared.RuntimeActionMutates(request.Action) {
		if isCPUAction(request.Action) {
			return a.executeCPUAction(operationContext, installation, *request)
		}
		if isModAction(request.Action) {
			return a.observeModAction(operationContext, installation, *request)
		}
		if request.Action == shared.RuntimeActionGameVersionObserve {
			return a.observeGameVersion(operationContext, installation, *request)
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
	} else if request.Action == shared.RuntimeActionGameVersionUpdate {
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
	if isModAction(request.Action) {
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
			result, operationErr = executeConsoleSend(operationContext, control, *request)
		}
	} else if isMigrationAction(request.Action) {
		result, operationErr = a.executeMigrationAction(operationContext, installation, *request)
	} else if isConfigurationAction(request.Action) {
		result, operationErr = a.executeConfigurationAction(operationContext, installation, *request)
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
	if shared.RuntimeActionMutates(request.Action) {
		if !operationIdentity.MatchString(request.OperationKey) || !operationIdentity.MatchString(request.LeaseID) ||
			request.FencingToken == 0 || request.LeaseExpiresAt == nil || request.LeaseExpiresAt.Before(now.Add(-30*time.Second)) ||
			request.LeaseExpiresAt.After(now.Add(10*time.Minute)) {
			return errors.New("Runtime 操作租约无效或已过期")
		}
	}
	if !isCPUAction(request.Action) && request.CPU != nil {
		return errors.New("Runtime 操作包含无关 CPU 负载")
	}
	if isConfigurationAction(request.Action) {
		return validateConfigurationOperationPayload(request)
	}
	if request.Configuration != nil {
		return errors.New("Runtime 操作包含无关配置发布负载")
	}
	switch request.Action {
	case shared.RuntimeActionConsoleHealth:
		if request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil {
			return errors.New("控制台健康请求包含无关负载")
		}
	case shared.RuntimeActionConsoleSend:
		if request.Console == nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil {
			return errors.New("控制台请求负载无效")
		}
		console := request.Console
		if console.Mode != shared.ConsoleModeManaged && console.Mode != shared.ConsoleModeProbe && console.Mode != shared.ConsoleModeRaw ||
			strings.TrimSpace(console.Command) == "" || len(console.Command) > 4096 || !utf8.ValidString(console.Command) ||
			strings.ContainsAny(console.Command, "\x00\r\n") || len(console.CoalesceKey) > 80 || strings.ContainsAny(console.CoalesceKey, "\x00\r\n") ||
			console.Mode != shared.ConsoleModeProbe && strings.TrimSpace(console.CoalesceKey) != "" {
			return errors.New("控制台请求内容无效")
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
		shared.RuntimeActionMigrationImportBegin, shared.RuntimeActionMigrationImportWrite, shared.RuntimeActionMigrationImportCommit,
		shared.RuntimeActionMigrationTargetRollback, shared.RuntimeActionMigrationTargetComplete,
		shared.RuntimeActionMigrationSourceFinalize, shared.RuntimeActionMigrationSourceRollback, shared.RuntimeActionMigrationSourceComplete:
		if request.Migration == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil ||
			!operationIdentity.MatchString(request.Migration.MigrationID) || request.Migration.Offset < 0 || request.Migration.Size < 0 ||
			len(request.Migration.Data) > shardtransfer.MaxChunkBytes || len(request.Migration.SHA256) > 64 {
			return errors.New("分片迁移请求无效")
		}
		if request.Action == shared.RuntimeActionMigrationImportWrite && len(request.Migration.Data) == 0 {
			return errors.New("分片迁移块为空")
		}
	case shared.RuntimeActionModTargetObserve, shared.RuntimeActionModCacheInspect, shared.RuntimeActionModUploadBegin, shared.RuntimeActionModUploadWrite, shared.RuntimeActionModUploadCommit,
		shared.RuntimeActionModReleasePlanBegin, shared.RuntimeActionModReleasePlanWrite, shared.RuntimeActionModReleasePlanCommit,
		shared.RuntimeActionModReleasePrepare, shared.RuntimeActionModReleasePublish, shared.RuntimeActionModReleaseRollback,
		shared.RuntimeActionModReleaseComplete, shared.RuntimeActionModReleaseState, shared.RuntimeActionModOverridesRead:
		if err := validateModOperationPayload(request); err != nil {
			return err
		}
	case shared.RuntimeActionGameVersionObserve, shared.RuntimeActionGameVersionUpdate:
		if err := validateGameVersionPayload(request); err != nil {
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
	if value == nil || request.Console != nil || request.Logs != nil || request.Artifacts != nil || request.Observation != nil ||
		request.Migration != nil || request.Backup != nil || request.Mod != nil || request.GameVersion != nil || request.CPU != nil ||
		!operationIdentity.MatchString(value.PublicationID) || value.Scope != string(configpublication.ScopeShared) && value.Scope != string(configpublication.ScopeWorld) ||
		value.Offset < 0 || value.Size < 0 || len(value.SHA256) > 64 || len(value.Data) > configpublication.MaxChunkBytes {
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
	case shared.RuntimeActionReadArtifacts:
		bundle, err := runtimefiles.ReadArtifacts(ctx, installation.SavePath, request.Cluster, request.Shard, request.Artifacts.Kind)
		result.Artifacts = &bundle
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
		response.Complete = err == nil
	case shared.RuntimeActionMigrationImportBegin:
		descriptor, stepErr := transfer.BeginImport(migration.MigrationID, migration.Size, migration.SHA256)
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

func isMigrationAction(action shared.RuntimeAction) bool {
	switch action {
	case shared.RuntimeActionMigrationExportPrepare, shared.RuntimeActionMigrationExportRead, shared.RuntimeActionMigrationExportRelease,
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
	case shared.RuntimeActionModTargetObserve, shared.RuntimeActionModCacheInspect, shared.RuntimeActionModUploadBegin, shared.RuntimeActionModUploadWrite, shared.RuntimeActionModUploadCommit,
		shared.RuntimeActionModReleasePlanBegin, shared.RuntimeActionModReleasePlanWrite, shared.RuntimeActionModReleasePlanCommit,
		shared.RuntimeActionModReleasePrepare, shared.RuntimeActionModReleasePublish, shared.RuntimeActionModReleaseRollback,
		shared.RuntimeActionModReleaseComplete, shared.RuntimeActionModReleaseState, shared.RuntimeActionModOverridesRead:
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
	case shared.RuntimeActionConfigurationBegin, shared.RuntimeActionConfigurationWrite, shared.RuntimeActionConfigurationPrepare,
		shared.RuntimeActionConfigurationPublish, shared.RuntimeActionConfigurationRollback, shared.RuntimeActionConfigurationComplete:
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
	switch action {
	case shared.RuntimeActionMigrationImportBegin, shared.RuntimeActionMigrationImportWrite, shared.RuntimeActionMigrationImportCommit,
		shared.RuntimeActionMigrationTargetRollback, shared.RuntimeActionMigrationTargetComplete,
		shared.RuntimeActionMigrationExportRelease, shared.RuntimeActionMigrationSourceRollback, shared.RuntimeActionMigrationSourceComplete,
		shared.RuntimeActionBackupRead, shared.RuntimeActionBackupRelease,
		shared.RuntimeActionRestoreBegin, shared.RuntimeActionRestoreWrite, shared.RuntimeActionRestorePrepare,
		shared.RuntimeActionRestorePublish, shared.RuntimeActionRestoreRollback, shared.RuntimeActionRestoreComplete,
		shared.RuntimeActionModTargetObserve, shared.RuntimeActionModCacheInspect, shared.RuntimeActionModUploadBegin, shared.RuntimeActionModUploadWrite,
		shared.RuntimeActionModUploadCommit, shared.RuntimeActionModReleasePlanBegin, shared.RuntimeActionModReleasePlanWrite,
		shared.RuntimeActionModReleasePlanCommit, shared.RuntimeActionModReleasePrepare, shared.RuntimeActionModReleasePublish,
		shared.RuntimeActionModReleaseRollback, shared.RuntimeActionModReleaseComplete, shared.RuntimeActionModReleaseState,
		shared.RuntimeActionModOverridesRead:
		return false
	case shared.RuntimeActionGameVersionObserve, shared.RuntimeActionGameVersionUpdate:
		return false
	default:
		return true
	}
}

func runtimeOperationTimeoutLimit(action shared.RuntimeAction) int {
	if action == shared.RuntimeActionGameVersionUpdate {
		return 1800
	}
	return 300
}

func executeConsoleSend(ctx context.Context, control shardRuntimeControl, request shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	console := *request.Console
	var err error
	if console.Mode == shared.ConsoleModeProbe && strings.TrimSpace(console.CoalesceKey) != "" {
		if background, ok := control.(backgroundConsoleRuntime); ok {
			err = background.SendBackground(ctx, request.Cluster, request.Shard, console.CoalesceKey, console.Command)
		} else {
			err = control.Send(ctx, request.Cluster, request.Shard, console.Command)
		}
	} else {
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
	return shared.ShardRuntimeStatus{State: string(status.State), Code: status.Code, Message: status.Message, SessionExists: status.SessionExists}
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
