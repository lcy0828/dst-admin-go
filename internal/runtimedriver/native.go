package runtimedriver

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dont/internal/configpublication"
	"dont/internal/consoledispatch"
	"dont/internal/runtimefiles"
	"dont/internal/shards"
	"dont/internal/shardtransfer"
	"dont/shared"
)

type NativeControl interface {
	Status(context.Context, string, string) (shards.RuntimeStatus, error)
	Start(context.Context, string, string) error
	Stop(context.Context, string, string) error
	Send(context.Context, string, string, string) error
}

type nativeBackgroundSender interface {
	SendBackground(context.Context, string, string, string, string) error
}

type nativeConsoleHealth interface {
	ConsoleHealth(string, string) consoledispatch.Health
}

type Native struct {
	saveRoot string
	control  NativeControl
	transfer *shardtransfer.Manager
	configs  *configpublication.Manager
	cpu      NativeCPUBackend

	mu       sync.Mutex
	evidence map[string]shared.RuntimeOperationEvidence
}

type NativeCPUBackend interface {
	Prepare(context.Context, string, string, string, shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error)
	Apply(context.Context, string, string, string, shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error)
	Observe(context.Context, string, string, string, shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error)
}

func NewNative(saveRoot string, control NativeControl) (*Native, error) {
	if strings.TrimSpace(saveRoot) == "" || control == nil {
		return nil, ErrInvalidTarget
	}
	transfer, err := shardtransfer.New(saveRoot, filepath.Join(saveRoot, ".dst-admin-transfers"))
	if err != nil {
		return nil, err
	}
	configs, err := configpublication.New(saveRoot, filepath.Join(saveRoot, ".dst-admin-config-publications"))
	if err != nil {
		return nil, err
	}
	return &Native{saveRoot: saveRoot, control: control, transfer: transfer, configs: configs, evidence: make(map[string]shared.RuntimeOperationEvidence)}, nil
}

func (d *Native) BeginConfiguration(_ context.Context, target Target, _ Operation, descriptor ConfigurationDescriptor) (int64, error) {
	return d.configs.Begin(configpublication.Descriptor{
		PublicationID: descriptor.PublicationID, Cluster: target.Cluster, Shard: target.Shard,
		Scope: configpublication.Scope(descriptor.Scope), Size: descriptor.Size, SHA256: descriptor.SHA256,
	})
}

func (d *Native) WriteConfiguration(_ context.Context, _ Target, _ Operation, descriptor ConfigurationDescriptor, offset int64, data []byte) (int64, error) {
	return d.configs.Write(descriptor.PublicationID, offset, data)
}

func (d *Native) PrepareConfiguration(ctx context.Context, _ Target, _ Operation, publicationID, _ string) error {
	return d.configs.Prepare(ctx, publicationID)
}

func (d *Native) PublishConfiguration(_ context.Context, _ Target, _ Operation, publicationID, _ string) error {
	return d.configs.Publish(publicationID)
}

func (d *Native) RollbackConfiguration(_ context.Context, _ Target, _ Operation, publicationID, _ string) error {
	return d.configs.Rollback(publicationID)
}

func (d *Native) CompleteConfiguration(_ context.Context, _ Target, _ Operation, publicationID, _ string) error {
	return d.configs.Complete(publicationID)
}

func (d *Native) ConfigureCPU(backend NativeCPUBackend) error {
	if backend == nil {
		return ErrInvalidTarget
	}
	d.cpu = backend
	return nil
}

func (d *Native) PrepareCPU(ctx context.Context, target Target, _ Operation, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	if d.cpu == nil {
		return shared.RuntimeCPUResult{}, ErrCapabilityMissing
	}
	return d.cpu.Prepare(ctx, target.InstallationID, target.Cluster, target.Shard, request)
}

func (d *Native) ApplyCPU(ctx context.Context, target Target, _ Operation, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	if d.cpu == nil {
		return shared.RuntimeCPUResult{}, ErrCapabilityMissing
	}
	return d.cpu.Apply(ctx, target.InstallationID, target.Cluster, target.Shard, request)
}

func (d *Native) ObserveCPU(ctx context.Context, target Target, request shared.RuntimeCPURequest) (shared.RuntimeCPUResult, error) {
	if d.cpu == nil {
		return shared.RuntimeCPUResult{}, ErrCapabilityMissing
	}
	return d.cpu.Observe(ctx, target.InstallationID, target.Cluster, target.Shard, request)
}

func (d *Native) PrepareMigrationExport(ctx context.Context, target Target, _ Operation, migrationID string) (MigrationDescriptor, error) {
	value, err := d.transfer.PrepareExport(ctx, migrationID, target.Cluster, target.Shard)
	return MigrationDescriptor{MigrationID: value.MigrationID, Size: value.Size, SHA256: value.SHA256}, err
}

func (d *Native) ReadMigrationExport(ctx context.Context, _ Target, migrationID string, offset int64) (MigrationChunk, error) {
	value, err := d.transfer.ReadExport(ctx, migrationID, offset)
	return MigrationChunk{Offset: value.Offset, NextOffset: value.NextOffset, Size: value.Size, SHA256: value.SHA256, Data: value.Data, Complete: value.Complete}, err
}

func (d *Native) ReleaseMigrationExport(_ context.Context, _ Target, _ Operation, migrationID string) error {
	return d.transfer.ReleaseExport(migrationID)
}

func (d *Native) BeginMigrationImport(_ context.Context, _ Target, _ Operation, descriptor MigrationDescriptor) error {
	_, err := d.transfer.BeginImport(descriptor.MigrationID, descriptor.Size, descriptor.SHA256)
	return err
}

func (d *Native) WriteMigrationImport(_ context.Context, _ Target, _ Operation, descriptor MigrationDescriptor, offset int64, data []byte) (int64, error) {
	return d.transfer.WriteImport(descriptor.MigrationID, offset, data)
}

func (d *Native) CommitMigrationImport(ctx context.Context, target Target, _ Operation, migrationID string) error {
	_, err := d.transfer.CommitImport(ctx, migrationID, target.Cluster, target.Shard)
	return err
}

func (d *Native) RollbackMigrationTarget(_ context.Context, _ Target, _ Operation, migrationID string) error {
	return d.transfer.RollbackTarget(migrationID)
}

func (d *Native) CompleteMigrationTarget(_ context.Context, _ Target, _ Operation, migrationID string) error {
	return d.transfer.CompleteTarget(migrationID)
}

func (d *Native) FinalizeMigrationSource(_ context.Context, target Target, _ Operation, migrationID string) (string, error) {
	return d.transfer.FinalizeSource(migrationID, target.Cluster, target.Shard)
}

func (d *Native) RollbackMigrationSource(_ context.Context, _ Target, _ Operation, migrationID string) error {
	return d.transfer.RollbackSource(migrationID)
}

func (d *Native) CompleteMigrationSource(_ context.Context, _ Target, _ Operation, migrationID string) (string, error) {
	return d.transfer.CompleteSource(migrationID)
}

func (d *Native) StageBackup(ctx context.Context, target Target, _ Operation, backupID string) (BackupDescriptor, error) {
	value, err := d.transfer.PrepareBackup(ctx, backupID, target.Cluster, target.Shard)
	return backupDescriptorFromTransfer(value), err
}

func (d *Native) ReadBackup(ctx context.Context, _ Target, backupID string, offset int64) (BackupChunk, error) {
	value, err := d.transfer.ReadBackup(ctx, backupID, offset)
	return BackupChunk{Offset: value.Offset, NextOffset: value.NextOffset, Size: value.Size, SHA256: value.SHA256, Data: value.Data, Complete: value.Complete}, err
}

func (d *Native) ReleaseBackup(_ context.Context, _ Target, _ Operation, backupID string) error {
	return d.transfer.ReleaseBackup(backupID)
}

func (d *Native) BeginRestore(_ context.Context, target Target, _ Operation, descriptor BackupDescriptor) error {
	_, err := d.transfer.BeginRestore(backupDescriptorToTransfer(target, descriptor))
	return err
}

func (d *Native) WriteRestore(_ context.Context, _ Target, _ Operation, descriptor BackupDescriptor, offset int64, data []byte) (int64, error) {
	return d.transfer.WriteRestore(descriptor.BackupID, offset, data)
}

func (d *Native) PrepareRestore(ctx context.Context, _ Target, _ Operation, backupID string) error {
	_, err := d.transfer.PrepareRestore(ctx, backupID)
	return err
}

func (d *Native) PublishRestore(_ context.Context, _ Target, _ Operation, backupID string, publishShared bool) (string, error) {
	return d.transfer.PublishRestore(backupID, publishShared)
}

func (d *Native) RollbackRestore(_ context.Context, _ Target, _ Operation, backupID string) error {
	return d.transfer.RollbackRestore(backupID)
}

func (d *Native) CompleteRestore(_ context.Context, _ Target, _ Operation, backupID string) (string, error) {
	return d.transfer.CompleteRestore(backupID)
}

func backupDescriptorFromTransfer(value shardtransfer.BackupDescriptor) BackupDescriptor {
	return BackupDescriptor{
		BackupID: value.BackupID, Size: value.Size, ContentSize: value.ContentSize, FileCount: value.FileCount,
		SHA256: value.SHA256, SharedSHA256: value.SharedSHA256,
	}
}

func backupDescriptorToTransfer(target Target, value BackupDescriptor) shardtransfer.BackupDescriptor {
	return shardtransfer.BackupDescriptor{
		BackupID: value.BackupID, Cluster: target.Cluster, Shard: target.Shard,
		Size: value.Size, ContentSize: value.ContentSize, FileCount: value.FileCount,
		SHA256: value.SHA256, SharedSHA256: value.SharedSHA256,
	}
}

func (d *Native) Kind() Kind { return KindNative }

func (d *Native) Capabilities() []Capability {
	result := []Capability{
		CapabilityLifecycle, CapabilityConsoleInput, CapabilityConsoleHealth, CapabilityRawConsole,
		CapabilityOperationProof, CapabilityLogContinuation, CapabilityArtifacts,
		CapabilitySnapshotBarrier, CapabilityBackupStage, CapabilityBackupRestore, CapabilityConfigPublish,
	}
	if d.cpu != nil {
		result = append(result, CapabilityExclusiveCPU)
	}
	return result
}

func (d *Native) Status(ctx context.Context, target Target) (shared.ShardRuntimeStatus, error) {
	if err := validateTarget(target); err != nil {
		return shared.ShardRuntimeStatus{}, err
	}
	status, err := d.control.Status(ctx, target.Cluster, target.Shard)
	return nativeStatus(status), err
}

func (d *Native) ExecuteShard(ctx context.Context, target Target, operation Operation, action shared.ShardAction, timeout time.Duration) (shared.ShardOperationResult, error) {
	if err := validateTarget(target); err != nil || !shared.IsShardAction(action) {
		return shared.ShardOperationResult{}, ErrInvalidTarget
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	operationContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	status, err := d.control.Status(operationContext, target.Cluster, target.Shard)
	observed := status
	if err == nil {
		switch action {
		case shared.ShardActionStart:
			if status.State != shards.RuntimeRunning && status.State != shards.RuntimeStarting {
				err = d.control.Start(operationContext, target.Cluster, target.Shard)
			}
			if err == nil {
				observed, err = waitForNativeShardState(operationContext, d.control, target.Cluster, target.Shard, true)
			}
		case shared.ShardActionStop:
			if status.SessionExists {
				err = d.control.Stop(operationContext, target.Cluster, target.Shard)
			}
			if err == nil {
				observed, err = waitForNativeShardState(operationContext, d.control, target.Cluster, target.Shard, false)
			}
		case shared.ShardActionRestart:
			if status.SessionExists {
				err = d.control.Stop(operationContext, target.Cluster, target.Shard)
			}
			if err == nil {
				observed, err = waitForNativeShardState(operationContext, d.control, target.Cluster, target.Shard, false)
			}
			if err == nil {
				err = d.control.Start(operationContext, target.Cluster, target.Shard)
			}
			if err == nil {
				observed, err = waitForNativeShardState(operationContext, d.control, target.Cluster, target.Shard, true)
			}
		case shared.ShardActionSave:
			if status.State != shards.RuntimeRunning {
				err = errors.New("分片未运行，无法保存")
			} else {
				err = d.control.Send(operationContext, target.Cluster, target.Shard, "c_save()")
			}
			if err == nil {
				observed, err = d.control.Status(operationContext, target.Cluster, target.Shard)
			}
		}
	}
	result := shared.ShardOperationResult{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: operation.ID, OperationKey: operation.Key,
		InstallationID: target.InstallationID, Action: action, Cluster: target.Cluster, Shard: target.Shard,
		FencingToken: operation.FencingToken, Status: nativeStatus(observed), ObservedAt: time.Now().UTC(),
	}
	if err != nil {
		result.Message = err.Error()
	}
	return result, err
}

func waitForNativeShardState(ctx context.Context, control NativeControl, cluster, shard string, running bool) (shards.RuntimeStatus, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := control.Status(ctx, cluster, shard)
		if err != nil {
			return status, err
		}
		if running && status.State == shards.RuntimeRunning {
			return status, nil
		}
		if running && status.State == shards.RuntimeFailed {
			message := strings.TrimSpace(status.Message)
			if message == "" {
				message = "DST 分片启动失败"
			}
			return status, errors.New(message)
		}
		if !running && status.State == shards.RuntimeStopped && !status.SessionExists {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (d *Native) SendConsole(ctx context.Context, target Target, operation Operation, request shared.RuntimeConsoleRequest, timeout time.Duration) (shared.RuntimeOperationResult, error) {
	if err := validateTarget(target); err != nil {
		return shared.RuntimeOperationResult{}, err
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	sendContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var err error
	if request.Mode == shared.ConsoleModeProbe && strings.TrimSpace(request.CoalesceKey) != "" {
		if background, ok := d.control.(nativeBackgroundSender); ok {
			err = background.SendBackground(sendContext, target.Cluster, target.Shard, request.CoalesceKey, request.Command)
		} else {
			err = d.control.Send(sendContext, target.Cluster, target.Shard, request.Command)
		}
	} else {
		err = d.control.Send(sendContext, target.Cluster, target.Shard, request.Command)
	}
	result := shared.RuntimeOperationResult{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: operation.ID, OperationKey: operation.Key,
		InstallationID: target.InstallationID, Action: shared.RuntimeActionConsoleSend, Cluster: target.Cluster, Shard: target.Shard,
		FencingToken: operation.FencingToken, Outcome: shared.RuntimeOutcomeSent,
		Message: "控制台命令已发送；是否执行成功需要对应回执或状态证据", ObservedAt: time.Now().UTC(),
	}
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
	}
	d.remember(result)
	return result, err
}

func (d *Native) ConsoleHealth(ctx context.Context, target Target) (shared.RuntimeConsoleHealth, error) {
	status, err := d.Status(ctx, target)
	health := shared.RuntimeConsoleHealth{Available: true, Accepting: status.State == string(shards.RuntimeRunning), Runtime: status}
	if provider, ok := d.control.(nativeConsoleHealth); ok {
		current := provider.ConsoleHealth(target.Cluster, target.Shard)
		health.Accepting, health.Busy, health.Pending = current.Accepting, current.Busy, current.Pending
		health.Status, health.PendingLimit, health.InstanceID = current.Status, current.PendingLimit, current.InstanceID
		health.Class, health.CoalesceKey, health.StartedAt = string(current.Class), current.CoalesceKey, current.StartedAt
		health.Maintenance, health.MaintenanceOwner, health.MaintenanceStartedAt = current.Maintenance, current.MaintenanceOwner, current.MaintenanceStartedAt
		health.InputDirty, health.ExternalWriter = current.InputDirty, current.ExternalWriter
		health.Available = current.Status == "ready" || current.Status == "maintenance"
	}
	return health, err
}

func (d *Native) ObserveOperation(_ context.Context, _ Target, operationID, _ string) (shared.RuntimeOperationEvidence, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	evidence, exists := d.evidence[operationID]
	if !exists {
		return shared.RuntimeOperationEvidence{}, errors.New("未找到需要观察的本机 Runtime 操作")
	}
	return evidence, nil
}

func (d *Native) ReadLogs(ctx context.Context, target Target, request shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error) {
	if err := validateTarget(target); err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	return runtimefiles.ReadLogs(ctx, d.saveRoot, target.Cluster, target.Shard, request)
}

func (d *Native) ReadArtifacts(ctx context.Context, target Target, kind shared.ArtifactKind) (shared.RuntimeArtifactBundle, error) {
	if err := validateTarget(target); err != nil {
		return shared.RuntimeArtifactBundle{}, err
	}
	return runtimefiles.ReadArtifacts(ctx, d.saveRoot, target.Cluster, target.Shard, kind)
}

func (d *Native) remember(result shared.RuntimeOperationResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evidence[result.OperationID] = shared.RuntimeOperationEvidence{
		OperationID: result.OperationID, Action: string(result.Action), Completed: true,
		Outcome: result.Outcome, Message: result.Message, ObservedAt: result.ObservedAt,
	}
}

func validateTarget(target Target) error {
	if strings.TrimSpace(target.TargetID) == "" || strings.TrimSpace(target.InstallationID) == "" ||
		strings.TrimSpace(target.Cluster) == "" || strings.TrimSpace(target.Shard) == "" {
		return ErrInvalidTarget
	}
	return nil
}

func nativeStatus(status shards.RuntimeStatus) shared.ShardRuntimeStatus {
	return shared.ShardRuntimeStatus{State: string(status.State), Code: status.Code, Message: status.Message, SessionExists: status.SessionExists}
}
