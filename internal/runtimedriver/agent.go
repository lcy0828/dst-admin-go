package runtimedriver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dont/internal/agents"
	"dont/shared"
)

type AgentExecutor interface {
	ExecuteShard(context.Context, string, shared.ShardOperationRequest, int) (agents.ShardExecutionResult, error)
	ExecuteRuntime(context.Context, string, shared.RuntimeOperationRequest, int) (agents.RuntimeExecutionResult, error)
}

type Agent struct {
	executor AgentExecutor
}

var (
	_ Driver            = (*Agent)(nil)
	_ ModDriver         = (*Agent)(nil)
	_ GameVersionDriver = (*Agent)(nil)
	_ CPUDriver         = (*Agent)(nil)
)

func NewAgent(executor AgentExecutor) (*Agent, error) {
	if executor == nil {
		return nil, ErrInvalidTarget
	}
	return &Agent{executor: executor}, nil
}

func (d *Agent) Kind() Kind { return KindNative }

func (d *Agent) Capabilities() []Capability {
	return []Capability{
		CapabilityLifecycle, CapabilityConsoleInput, CapabilityConsoleHealth, CapabilityRawConsole,
		CapabilityOperationProof, CapabilityLogContinuation, CapabilityArtifacts,
		CapabilitySnapshotBarrier, CapabilityBackupStage, CapabilityBackupRestore,
		CapabilityModPrepare, CapabilityModPublish, CapabilityGameUpdate,
		CapabilityExclusiveCPU,
	}
}

func (d *Agent) PrepareCPU(ctx context.Context, target Target, operation Operation, cpu shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	return d.executeCPU(ctx, target, operation, shared.RuntimeActionCPUPrepare, cpu)
}

func (d *Agent) ApplyCPU(ctx context.Context, target Target, operation Operation, cpu shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	return d.executeCPU(ctx, target, operation, shared.RuntimeActionCPUApply, cpu)
}

func (d *Agent) ObserveCPU(ctx context.Context, target Target, cpu shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	return d.executeCPU(ctx, target, Operation{ID: newOperationID()}, shared.RuntimeActionCPUObserve, cpu)
}

func (d *Agent) executeCPU(ctx context.Context, target Target, operation Operation, action shared.RuntimeAction, cpu shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	request := runtimeRequest(target, operation, action)
	request.CPU = &cpu
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	if result.Result.CPU == nil {
		if err != nil {
			return shared.RuntimeCPUResult{}, err
		}
		return shared.RuntimeCPUResult{}, errors.New("Agent 未返回有效的 CPU 执行结果")
	}
	return *result.Result.CPU, err
}

func (d *Agent) ObserveGameVersion(ctx context.Context, target Target) (shared.RuntimeGameVersionResult, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionGameVersionObserve)
	request.GameVersion = &shared.RuntimeGameVersionRequest{}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	return checkedGameVersionResult(result.Result, err)
}

func (d *Agent) UpdateGameVersion(ctx context.Context, target Target, operation Operation, expectedVersion string, cleanCache bool) (shared.RuntimeGameVersionResult, error) {
	request := runtimeRequest(target, operation, shared.RuntimeActionGameVersionUpdate)
	request.GameVersion = &shared.RuntimeGameVersionRequest{ExpectedVersion: expectedVersion, CleanCache: cleanCache}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 1800)
	return checkedGameVersionResult(result.Result, err)
}

func checkedGameVersionResult(result shared.RuntimeOperationResult, err error) (shared.RuntimeGameVersionResult, error) {
	if result.GameVersion == nil {
		if err != nil {
			return shared.RuntimeGameVersionResult{}, err
		}
		return shared.RuntimeGameVersionResult{}, errors.New("Agent 未返回有效的游戏版本结果")
	}
	return *result.GameVersion, err
}

func (d *Agent) Status(ctx context.Context, target Target) (shared.ShardRuntimeStatus, error) {
	result, err := d.ExecuteShard(ctx, target, Operation{ID: newOperationID()}, shared.ShardActionStatus, 30*time.Second)
	return result.Status, err
}

func (d *Agent) ExecuteShard(ctx context.Context, target Target, operation Operation, action shared.ShardAction, timeout time.Duration) (shared.ShardOperationResult, error) {
	request := shared.ShardOperationRequest{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: operation.ID, OperationKey: operation.Key,
		InstallationID: target.InstallationID, Action: action, Cluster: target.Cluster, Shard: target.Shard,
		TopologyRevision: target.TopologyRevision, LeaseID: operation.LeaseID, FencingToken: operation.FencingToken, LeaseExpiresAt: operation.LeaseExpiresAt,
	}
	result, err := d.executor.ExecuteShard(ctx, target.TargetID, request, timeoutSeconds(timeout))
	return result.Result, err
}

func (d *Agent) SendConsole(ctx context.Context, target Target, operation Operation, console shared.RuntimeConsoleRequest, timeout time.Duration) (shared.RuntimeOperationResult, error) {
	request := runtimeRequest(target, operation, shared.RuntimeActionConsoleSend)
	request.Console = &console
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, timeoutSeconds(timeout))
	return result.Result, err
}

func (d *Agent) ConsoleHealth(ctx context.Context, target Target) (shared.RuntimeConsoleHealth, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionConsoleHealth)
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	if result.Result.ConsoleHealth == nil {
		return shared.RuntimeConsoleHealth{}, err
	}
	return *result.Result.ConsoleHealth, err
}

func (d *Agent) ObserveOperation(ctx context.Context, target Target, operationID, operationKey string) (shared.RuntimeOperationEvidence, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionObserveOperation)
	request.Observation = &shared.RuntimeObservationRequest{ObservedOperationID: operationID, ObservedOperationKey: operationKey}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	if result.Result.Evidence == nil {
		return shared.RuntimeOperationEvidence{}, err
	}
	return *result.Result.Evidence, err
}

func (d *Agent) ReadLogs(ctx context.Context, target Target, logs shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionReadLogs)
	request.Logs = &logs
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	if result.Result.Logs == nil {
		return shared.RuntimeLogChunk{}, err
	}
	return *result.Result.Logs, err
}

func (d *Agent) ReadArtifacts(ctx context.Context, target Target, kind shared.ArtifactKind) (shared.RuntimeArtifactBundle, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionReadArtifacts)
	request.Artifacts = &shared.RuntimeArtifactRequest{Kind: kind}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	if result.Result.Artifacts == nil {
		return shared.RuntimeArtifactBundle{}, err
	}
	return *result.Result.Artifacts, err
}

func (d *Agent) ObserveModTarget(ctx context.Context, target Target) (int64, string, error) {
	request := modRuntimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionModTargetObserve)
	request.Mod = &shared.RuntimeModRequest{}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	value, err := checkedModResult(result.Result, err)
	if err != nil {
		return 0, "", err
	}
	if value.AvailableBytes < 0 || value.RuntimeVersion == "" {
		return 0, "", errors.New("Agent 返回了无效的 Mod 运行目标状态")
	}
	return value.AvailableBytes, value.RuntimeVersion, nil
}

func (d *Agent) InspectModCache(ctx context.Context, target Target, workshopID, treeSHA string) (shared.RuntimeModCacheManifest, error) {
	request := modRuntimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionModCacheInspect)
	request.Mod = &shared.RuntimeModRequest{WorkshopID: workshopID, ExpectedTreeSHA256: treeSHA}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	value, err := checkedModResult(result.Result, err)
	if err != nil {
		return shared.RuntimeModCacheManifest{}, err
	}
	if value.CacheManifest == nil {
		return shared.RuntimeModCacheManifest{}, errors.New("Agent 未返回 Mod 缓存 manifest")
	}
	return *value.CacheManifest, nil
}

func (d *Agent) BeginModUpload(ctx context.Context, target Target, operation Operation, descriptor ModUploadDescriptor) (int64, error) {
	descriptor.Kind = shared.RuntimeModUploadCacheBundle
	return d.beginModTransfer(ctx, target, operation, shared.RuntimeActionModUploadBegin, descriptor)
}

func (d *Agent) WriteModUpload(ctx context.Context, target Target, operation Operation, descriptor ModUploadDescriptor, offset int64, data []byte) (int64, error) {
	descriptor.Kind = shared.RuntimeModUploadCacheBundle
	return d.writeModTransfer(ctx, target, operation, shared.RuntimeActionModUploadWrite, descriptor, offset, data)
}

func (d *Agent) CommitModUpload(ctx context.Context, target Target, operation Operation, descriptor ModUploadDescriptor) (shared.RuntimeModCacheManifest, error) {
	descriptor.Kind = shared.RuntimeModUploadCacheBundle
	result, err := d.executeMod(ctx, target, operation, shared.RuntimeActionModUploadCommit, modUploadRequest(descriptor), 5*time.Minute)
	value, err := checkedModResult(result, err)
	if err != nil {
		return shared.RuntimeModCacheManifest{}, err
	}
	if value.CacheManifest == nil || !value.Complete {
		return shared.RuntimeModCacheManifest{}, errors.New("Agent 未确认 Mod 缓存提交")
	}
	return *value.CacheManifest, nil
}

func (d *Agent) BeginModReleasePlan(ctx context.Context, target Target, operation Operation, descriptor ModUploadDescriptor) (int64, error) {
	descriptor.Kind = shared.RuntimeModUploadReleasePlan
	return d.beginModTransfer(ctx, target, operation, shared.RuntimeActionModReleasePlanBegin, descriptor)
}

func (d *Agent) WriteModReleasePlan(ctx context.Context, target Target, operation Operation, descriptor ModUploadDescriptor, offset int64, data []byte) (int64, error) {
	descriptor.Kind = shared.RuntimeModUploadReleasePlan
	return d.writeModTransfer(ctx, target, operation, shared.RuntimeActionModReleasePlanWrite, descriptor, offset, data)
}

func (d *Agent) CommitModReleasePlan(ctx context.Context, target Target, operation Operation, descriptor ModUploadDescriptor) (shared.RuntimeModReleaseState, error) {
	descriptor.Kind = shared.RuntimeModUploadReleasePlan
	result, err := d.executeMod(ctx, target, operation, shared.RuntimeActionModReleasePlanCommit, modUploadRequest(descriptor), 5*time.Minute)
	return checkedModRelease(result, descriptor.OperationID, err)
}

func (d *Agent) PrepareModRelease(ctx context.Context, target Target, operation Operation, operationID string) (shared.RuntimeModReleaseState, error) {
	return d.executeModRelease(ctx, target, operation, shared.RuntimeActionModReleasePrepare, operationID)
}

func (d *Agent) PublishModRelease(ctx context.Context, target Target, operation Operation, operationID string) (shared.RuntimeModReleaseState, error) {
	return d.executeModRelease(ctx, target, operation, shared.RuntimeActionModReleasePublish, operationID)
}

func (d *Agent) RollbackModRelease(ctx context.Context, target Target, operation Operation, operationID string) (shared.RuntimeModReleaseState, error) {
	return d.executeModRelease(ctx, target, operation, shared.RuntimeActionModReleaseRollback, operationID)
}

func (d *Agent) CompleteModRelease(ctx context.Context, target Target, operation Operation, operationID string) (shared.RuntimeModReleaseState, error) {
	return d.executeModRelease(ctx, target, operation, shared.RuntimeActionModReleaseComplete, operationID)
}

func (d *Agent) ModReleaseState(ctx context.Context, target Target, operationID string) (shared.RuntimeModReleaseState, error) {
	request := modRuntimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionModReleaseState)
	request.Mod = &shared.RuntimeModRequest{OperationID: operationID}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	return checkedModRelease(result.Result, operationID, err)
}

func (d *Agent) ReadModOverrides(ctx context.Context, target Target, roomDirectory, worldDirectory string, offset int64) (shared.RuntimeModOverridesChunk, error) {
	request := modRuntimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionModOverridesRead)
	request.Mod = &shared.RuntimeModRequest{RoomDirectory: roomDirectory, WorldDirectory: worldDirectory, Offset: offset}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	value, err := checkedModResult(result.Result, err)
	if err != nil {
		return shared.RuntimeModOverridesChunk{}, err
	}
	if value.Overrides == nil {
		return shared.RuntimeModOverridesChunk{}, errors.New("Agent 未返回 modoverrides.lua 数据块")
	}
	return *value.Overrides, nil
}

func (d *Agent) beginModTransfer(ctx context.Context, target Target, operation Operation, action shared.RuntimeAction, descriptor ModUploadDescriptor) (int64, error) {
	result, err := d.executeMod(ctx, target, operation, action, modUploadRequest(descriptor), time.Minute)
	value, err := checkedModResult(result, err)
	if err != nil {
		return 0, err
	}
	return value.NextOffset, nil
}

func (d *Agent) writeModTransfer(ctx context.Context, target Target, operation Operation, action shared.RuntimeAction, descriptor ModUploadDescriptor, offset int64, data []byte) (int64, error) {
	request := modUploadRequest(descriptor)
	request.Offset, request.Data = offset, data
	result, err := d.executeMod(ctx, target, operation, action, request, time.Minute)
	value, err := checkedModResult(result, err)
	if err != nil {
		return offset, err
	}
	return value.NextOffset, nil
}

func (d *Agent) executeModRelease(ctx context.Context, target Target, operation Operation, action shared.RuntimeAction, operationID string) (shared.RuntimeModReleaseState, error) {
	result, err := d.executeMod(ctx, target, operation, action, shared.RuntimeModRequest{OperationID: operationID}, 5*time.Minute)
	return checkedModRelease(result, operationID, err)
}

func (d *Agent) executeMod(ctx context.Context, target Target, operation Operation, action shared.RuntimeAction, mod shared.RuntimeModRequest, timeout time.Duration) (shared.RuntimeOperationResult, error) {
	request := modRuntimeRequest(target, operation, action)
	request.Mod = &mod
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, timeoutSeconds(timeout))
	return result.Result, err
}

func modUploadRequest(value ModUploadDescriptor) shared.RuntimeModRequest {
	return shared.RuntimeModRequest{
		Kind: value.Kind, UploadID: value.UploadID, OperationID: value.OperationID,
		WorkshopID: value.WorkshopID, ExpectedTreeSHA256: value.ExpectedTreeSHA256,
		Size: value.Size, SHA256: value.SHA256, Metadata: value.Metadata,
	}
}

func checkedModResult(result shared.RuntimeOperationResult, err error) (*shared.RuntimeModResult, error) {
	if err != nil {
		return nil, err
	}
	if result.Mod == nil {
		return nil, errors.New("Agent 未返回有效的 Mod Runtime 结果")
	}
	return result.Mod, nil
}

func checkedModRelease(result shared.RuntimeOperationResult, operationID string, err error) (shared.RuntimeModReleaseState, error) {
	value, err := checkedModResult(result, err)
	if err != nil {
		return shared.RuntimeModReleaseState{}, err
	}
	if value.Release == nil || value.Release.OperationID != operationID {
		return shared.RuntimeModReleaseState{}, errors.New("Agent 未返回匹配的 Mod 发布状态")
	}
	return *value.Release, nil
}

func modRuntimeRequest(target Target, operation Operation, action shared.RuntimeAction) shared.RuntimeOperationRequest {
	// Mod cache and release state are installation-scoped, so every room on
	// the installation shares one fencing and idempotency domain.
	target.Cluster = "Mods"
	target.Shard = "Installation"
	if target.TopologyRevision == "" {
		target.TopologyRevision = "runtime-mods-v1"
	}
	return runtimeRequest(target, operation, action)
}

func (d *Agent) PrepareMigrationExport(ctx context.Context, target Target, operation Operation, migrationID string) (MigrationDescriptor, error) {
	result, err := d.executeMigration(ctx, target, operation, shared.RuntimeActionMigrationExportPrepare, shared.RuntimeMigrationRequest{MigrationID: migrationID}, 5*time.Minute)
	value, err := checkedMigrationResult(result, migrationID, err)
	if err != nil {
		return MigrationDescriptor{}, err
	}
	if value.Size < 1 || len(value.SHA256) != 64 || !value.Complete {
		return MigrationDescriptor{}, errors.New("Agent 返回了无效的迁移导出描述")
	}
	return migrationDescriptor(result), nil
}

func (d *Agent) ReadMigrationExport(ctx context.Context, target Target, migrationID string, offset int64) (MigrationChunk, error) {
	result, err := d.executeMigration(ctx, target, Operation{ID: newOperationID()}, shared.RuntimeActionMigrationExportRead, shared.RuntimeMigrationRequest{MigrationID: migrationID, Offset: offset}, time.Minute)
	value, err := checkedMigrationResult(result, migrationID, err)
	if err != nil {
		return MigrationChunk{}, err
	}
	return MigrationChunk{Offset: value.Offset, NextOffset: value.NextOffset, Size: value.Size, SHA256: value.SHA256, Data: value.Data, Complete: value.Complete}, err
}

func (d *Agent) ReleaseMigrationExport(ctx context.Context, target Target, operation Operation, migrationID string) error {
	result, err := d.executeMigration(ctx, target, operation, shared.RuntimeActionMigrationExportRelease, shared.RuntimeMigrationRequest{MigrationID: migrationID}, time.Minute)
	return checkedCompletedMigration(result, migrationID, err)
}

func (d *Agent) BeginMigrationImport(ctx context.Context, target Target, operation Operation, descriptor MigrationDescriptor) error {
	result, err := d.executeMigration(ctx, target, operation, shared.RuntimeActionMigrationImportBegin, shared.RuntimeMigrationRequest{MigrationID: descriptor.MigrationID, Size: descriptor.Size, SHA256: descriptor.SHA256}, time.Minute)
	value, err := checkedMigrationResult(result, descriptor.MigrationID, err)
	if err != nil {
		return err
	}
	if value.Size != descriptor.Size || value.SHA256 != descriptor.SHA256 {
		return errors.New("Agent 未确认迁移导入描述")
	}
	return nil
}

func (d *Agent) WriteMigrationImport(ctx context.Context, target Target, operation Operation, descriptor MigrationDescriptor, offset int64, data []byte) (int64, error) {
	result, err := d.executeMigration(ctx, target, operation, shared.RuntimeActionMigrationImportWrite, shared.RuntimeMigrationRequest{MigrationID: descriptor.MigrationID, Offset: offset, Size: descriptor.Size, SHA256: descriptor.SHA256, Data: data}, time.Minute)
	value, err := checkedMigrationResult(result, descriptor.MigrationID, err)
	if err != nil {
		return offset, err
	}
	return value.NextOffset, nil
}

func (d *Agent) CommitMigrationImport(ctx context.Context, target Target, operation Operation, migrationID string) error {
	result, err := d.executeMigration(ctx, target, operation, shared.RuntimeActionMigrationImportCommit, shared.RuntimeMigrationRequest{MigrationID: migrationID}, 5*time.Minute)
	return checkedCompletedMigration(result, migrationID, err)
}

func (d *Agent) RollbackMigrationTarget(ctx context.Context, target Target, operation Operation, migrationID string) error {
	result, err := d.executeMigration(ctx, target, operation, shared.RuntimeActionMigrationTargetRollback, shared.RuntimeMigrationRequest{MigrationID: migrationID}, 5*time.Minute)
	return checkedCompletedMigration(result, migrationID, err)
}

func (d *Agent) CompleteMigrationTarget(ctx context.Context, target Target, operation Operation, migrationID string) error {
	result, err := d.executeMigration(ctx, target, operation, shared.RuntimeActionMigrationTargetComplete, shared.RuntimeMigrationRequest{MigrationID: migrationID}, time.Minute)
	return checkedCompletedMigration(result, migrationID, err)
}

func (d *Agent) FinalizeMigrationSource(ctx context.Context, target Target, operation Operation, migrationID string) (string, error) {
	result, err := d.executeMigration(ctx, target, operation, shared.RuntimeActionMigrationSourceFinalize, shared.RuntimeMigrationRequest{MigrationID: migrationID}, 5*time.Minute)
	value, err := checkedMigrationResult(result, migrationID, err)
	if err != nil {
		return "", err
	}
	if !value.Complete || value.RecoveryRef == "" {
		return "", errors.New("Agent 未返回源迁移恢复位置")
	}
	return value.RecoveryRef, nil
}

func (d *Agent) RollbackMigrationSource(ctx context.Context, target Target, operation Operation, migrationID string) error {
	result, err := d.executeMigration(ctx, target, operation, shared.RuntimeActionMigrationSourceRollback, shared.RuntimeMigrationRequest{MigrationID: migrationID}, 5*time.Minute)
	return checkedCompletedMigration(result, migrationID, err)
}

func (d *Agent) CompleteMigrationSource(ctx context.Context, target Target, operation Operation, migrationID string) (string, error) {
	result, err := d.executeMigration(ctx, target, operation, shared.RuntimeActionMigrationSourceComplete, shared.RuntimeMigrationRequest{MigrationID: migrationID}, time.Minute)
	value, err := checkedMigrationResult(result, migrationID, err)
	if err != nil {
		return "", err
	}
	if !value.Complete || value.RecoveryRef == "" {
		return "", errors.New("Agent 未确认源迁移恢复记录")
	}
	return value.RecoveryRef, nil
}

func (d *Agent) StageBackup(ctx context.Context, target Target, operation Operation, backupID string) (BackupDescriptor, error) {
	result, err := d.executeBackup(ctx, target, operation, shared.RuntimeActionBackupStage, shared.RuntimeBackupRequest{BackupID: backupID}, 5*time.Minute)
	value, err := checkedBackupResult(result, backupID, err)
	if err != nil {
		return BackupDescriptor{}, err
	}
	if !value.Complete || value.Size < 1 || value.ContentSize < 1 || value.FileCount < 2 || len(value.SHA256) != 64 || len(value.SharedSHA256) != 64 {
		return BackupDescriptor{}, errors.New("Agent 返回了无效的备份描述")
	}
	return backupDescriptor(result), nil
}

func (d *Agent) ReadBackup(ctx context.Context, target Target, backupID string, offset int64) (BackupChunk, error) {
	result, err := d.executeBackup(ctx, target, Operation{ID: newOperationID()}, shared.RuntimeActionBackupRead, shared.RuntimeBackupRequest{BackupID: backupID, Offset: offset}, time.Minute)
	value, err := checkedBackupResult(result, backupID, err)
	if err != nil {
		return BackupChunk{}, err
	}
	return BackupChunk{Offset: value.Offset, NextOffset: value.NextOffset, Size: value.Size, SHA256: value.SHA256, Data: value.Data, Complete: value.Complete}, nil
}

func (d *Agent) ReleaseBackup(ctx context.Context, target Target, operation Operation, backupID string) error {
	result, err := d.executeBackup(ctx, target, operation, shared.RuntimeActionBackupRelease, shared.RuntimeBackupRequest{BackupID: backupID}, time.Minute)
	return checkedCompletedBackup(result, backupID, err)
}

func (d *Agent) BeginRestore(ctx context.Context, target Target, operation Operation, descriptor BackupDescriptor) error {
	result, err := d.executeBackup(ctx, target, operation, shared.RuntimeActionRestoreBegin, backupRequest(descriptor), time.Minute)
	value, err := checkedBackupResult(result, descriptor.BackupID, err)
	if err != nil {
		return err
	}
	if !value.Complete || value.Size != descriptor.Size || value.SHA256 != descriptor.SHA256 || value.SharedSHA256 != descriptor.SharedSHA256 {
		return errors.New("Agent 未确认恢复描述")
	}
	return nil
}

func (d *Agent) WriteRestore(ctx context.Context, target Target, operation Operation, descriptor BackupDescriptor, offset int64, data []byte) (int64, error) {
	request := shared.RuntimeBackupRequest{BackupID: descriptor.BackupID, Offset: offset, Size: descriptor.Size, SHA256: descriptor.SHA256, Data: data}
	result, err := d.executeBackup(ctx, target, operation, shared.RuntimeActionRestoreWrite, request, time.Minute)
	value, err := checkedBackupResult(result, descriptor.BackupID, err)
	if err != nil {
		return offset, err
	}
	return value.NextOffset, nil
}

func (d *Agent) PrepareRestore(ctx context.Context, target Target, operation Operation, backupID string) error {
	result, err := d.executeBackup(ctx, target, operation, shared.RuntimeActionRestorePrepare, shared.RuntimeBackupRequest{BackupID: backupID}, 5*time.Minute)
	return checkedCompletedBackup(result, backupID, err)
}

func (d *Agent) PublishRestore(ctx context.Context, target Target, operation Operation, backupID string, publishShared bool) (string, error) {
	result, err := d.executeBackup(ctx, target, operation, shared.RuntimeActionRestorePublish, shared.RuntimeBackupRequest{BackupID: backupID, PublishShared: publishShared}, 5*time.Minute)
	value, err := checkedBackupResult(result, backupID, err)
	if err != nil {
		return "", err
	}
	if !value.Complete || value.RecoveryRef == "" {
		return "", errors.New("Agent 未确认恢复发布")
	}
	return value.RecoveryRef, nil
}

func (d *Agent) RollbackRestore(ctx context.Context, target Target, operation Operation, backupID string) error {
	result, err := d.executeBackup(ctx, target, operation, shared.RuntimeActionRestoreRollback, shared.RuntimeBackupRequest{BackupID: backupID}, 5*time.Minute)
	return checkedCompletedBackup(result, backupID, err)
}

func (d *Agent) CompleteRestore(ctx context.Context, target Target, operation Operation, backupID string) (string, error) {
	result, err := d.executeBackup(ctx, target, operation, shared.RuntimeActionRestoreComplete, shared.RuntimeBackupRequest{BackupID: backupID}, 5*time.Minute)
	value, err := checkedBackupResult(result, backupID, err)
	if err != nil {
		return "", err
	}
	if !value.Complete || value.RecoveryRef == "" {
		return "", errors.New("Agent 未确认恢复清理")
	}
	return value.RecoveryRef, nil
}

func (d *Agent) executeBackup(ctx context.Context, target Target, operation Operation, action shared.RuntimeAction, backup shared.RuntimeBackupRequest, timeout time.Duration) (shared.RuntimeOperationResult, error) {
	request := runtimeRequest(target, operation, action)
	request.Backup = &backup
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, timeoutSeconds(timeout))
	return result.Result, err
}

func backupRequest(value BackupDescriptor) shared.RuntimeBackupRequest {
	return shared.RuntimeBackupRequest{
		BackupID: value.BackupID, Size: value.Size, ContentSize: value.ContentSize, FileCount: value.FileCount,
		SHA256: value.SHA256, SharedSHA256: value.SharedSHA256,
	}
}

func backupDescriptor(result shared.RuntimeOperationResult) BackupDescriptor {
	if result.Backup == nil {
		return BackupDescriptor{}
	}
	return BackupDescriptor{
		BackupID: result.Backup.BackupID, Size: result.Backup.Size, ContentSize: result.Backup.ContentSize,
		FileCount: result.Backup.FileCount, SHA256: result.Backup.SHA256, SharedSHA256: result.Backup.SharedSHA256,
	}
}

func checkedBackupResult(result shared.RuntimeOperationResult, backupID string, err error) (*shared.RuntimeBackupResult, error) {
	if err != nil {
		return nil, err
	}
	if result.Backup == nil || result.Backup.BackupID != backupID {
		return nil, fmt.Errorf("Agent 未返回备份 %s 的有效结果", backupID)
	}
	return result.Backup, nil
}

func checkedCompletedBackup(result shared.RuntimeOperationResult, backupID string, err error) error {
	value, err := checkedBackupResult(result, backupID, err)
	if err != nil {
		return err
	}
	if !value.Complete {
		return fmt.Errorf("Agent 未确认备份步骤 %s 已完成", backupID)
	}
	return nil
}

func (d *Agent) executeMigration(ctx context.Context, target Target, operation Operation, action shared.RuntimeAction, migration shared.RuntimeMigrationRequest, timeout time.Duration) (shared.RuntimeOperationResult, error) {
	request := runtimeRequest(target, operation, action)
	request.Migration = &migration
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, timeoutSeconds(timeout))
	return result.Result, err
}

func migrationDescriptor(result shared.RuntimeOperationResult) MigrationDescriptor {
	if result.Migration == nil {
		return MigrationDescriptor{}
	}
	return MigrationDescriptor{
		MigrationID: result.Migration.MigrationID, Size: result.Migration.Size,
		SHA256: result.Migration.SHA256, RecoveryRef: result.Migration.RecoveryRef,
	}
}

func checkedMigrationResult(result shared.RuntimeOperationResult, migrationID string, err error) (*shared.RuntimeMigrationResult, error) {
	if err != nil {
		return nil, err
	}
	if result.Migration == nil || result.Migration.MigrationID != migrationID {
		return nil, fmt.Errorf("Agent 未返回迁移 %s 的有效结果", migrationID)
	}
	return result.Migration, nil
}

func checkedCompletedMigration(result shared.RuntimeOperationResult, migrationID string, err error) error {
	value, err := checkedMigrationResult(result, migrationID, err)
	if err != nil {
		return err
	}
	if !value.Complete {
		return fmt.Errorf("Agent 未确认迁移步骤 %s 已完成", migrationID)
	}
	return nil
}

func runtimeRequest(target Target, operation Operation, action shared.RuntimeAction) shared.RuntimeOperationRequest {
	return shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: operation.ID, OperationKey: operation.Key,
		InstallationID: target.InstallationID, Action: action, Cluster: target.Cluster, Shard: target.Shard,
		TopologyRevision: target.TopologyRevision, LeaseID: operation.LeaseID, FencingToken: operation.FencingToken, LeaseExpiresAt: operation.LeaseExpiresAt,
	}
}

func timeoutSeconds(value time.Duration) int {
	seconds := int(value.Seconds())
	if seconds < 5 {
		return 5
	}
	if seconds > 300 {
		return 300
	}
	return seconds
}
