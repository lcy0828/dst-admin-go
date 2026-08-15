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
	}
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
