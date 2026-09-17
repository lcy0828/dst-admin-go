package runtimedriver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"dont/internal/agents"
	"dont/internal/configpublication"
	"dont/internal/runtimefiles"
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
	_ Driver              = (*Agent)(nil)
	_ ModDriver           = (*Agent)(nil)
	_ ModPeerDriver       = (*Agent)(nil)
	_ GameVersionDriver   = (*Agent)(nil)
	_ CPUDriver           = (*Agent)(nil)
	_ ConfigurationReader = (*Agent)(nil)
	_ ClusterTokenReader  = (*Agent)(nil)
	_ ConfigurationDriver = (*Agent)(nil)
	_ MapDriver           = (*Agent)(nil)
	_ ChatLogDriver       = (*Agent)(nil)
	_ MigrationPeerSource = (*Agent)(nil)
	_ MigrationPeerTarget = (*Agent)(nil)
	_ RoomRecoveryDriver  = (*Agent)(nil)
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
		CapabilityOperationProof, CapabilityLogContinuation, CapabilityChatHistory, CapabilityArtifacts, CapabilityWorldStateRead,
		CapabilitySnapshotBarrier, CapabilityBackupStage, CapabilityBackupRestore,
		CapabilityModPrepare, CapabilityModPublish, CapabilityGameUpdate,
		CapabilityExclusiveCPU,
		CapabilityConfigRead, CapabilityConfigSecrets, CapabilityConfigPublish, CapabilityConfigApply,
		CapabilityMapRender, CapabilityShardRouting, CapabilityMigrationPeer, CapabilityRoomRecovery,
	}
}

func (d *Agent) MoveRoomToRecovery(ctx context.Context, target Target, operation Operation) (string, error) {
	request := runtimeRequest(target, operation, shared.RuntimeActionRoomRecoveryMove)
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	if err != nil {
		return "", err
	}
	if result.Result.RoomRecovery == nil || strings.TrimSpace(result.Result.RoomRecovery.RecoveryRef) == "" {
		return "", errors.New("Agent 未返回房间回收位置")
	}
	return result.Result.RoomRecovery.RecoveryRef, nil
}

func (d *Agent) ReadConfiguration(ctx context.Context, target Target, scope string) (shared.RuntimeConfigurationResult, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionConfigurationRead)
	request.Configuration = &shared.RuntimeConfigurationRequest{Scope: scope}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	if err != nil {
		return shared.RuntimeConfigurationResult{}, err
	}
	if result.Result.Configuration == nil {
		return shared.RuntimeConfigurationResult{}, errors.New("Agent 未返回 Runtime 配置")
	}
	return *result.Result.Configuration, nil
}

func (d *Agent) RevealClusterToken(ctx context.Context, target Target) (shared.RuntimeClusterTokenReveal, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionClusterTokenReveal)
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	if err != nil {
		return shared.RuntimeClusterTokenReveal{}, err
	}
	if result.Result.ClusterToken == nil {
		return shared.RuntimeClusterTokenReveal{}, errors.New("Agent 未返回 Cluster Token")
	}
	return *result.Result.ClusterToken, nil
}

func (d *Agent) ListMapSessions(ctx context.Context, target Target) ([]shared.RuntimeMapSession, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionMapSessions)
	request.Map = &shared.RuntimeMapRequest{}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	value, err := checkedMapResult(result.Result, "", err)
	if err != nil {
		return nil, err
	}
	return append([]shared.RuntimeMapSession(nil), value.Sessions...), nil
}

func (d *Agent) MapRendererStatus(ctx context.Context, target Target) (shared.RuntimeMapRenderer, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionMapStatus)
	request.Map = &shared.RuntimeMapRequest{}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	value, err := checkedMapResult(result.Result, "", err)
	if err != nil {
		return shared.RuntimeMapRenderer{}, err
	}
	if value.Renderer == nil {
		return shared.RuntimeMapRenderer{}, errors.New("Agent 未返回地图渲染器状态")
	}
	return *value.Renderer, nil
}

func (d *Agent) PrepareMapSnapshot(ctx context.Context, target Target, operation Operation, transferID, sessionID, fileName string) (MapDescriptor, error) {
	return d.prepareMap(ctx, target, operation, shared.RuntimeActionMapSnapshotPrepare, transferID, sessionID, fileName, nil)
}

func (d *Agent) RenderMap(ctx context.Context, target Target, operation Operation, transferID, sessionID, fileName string, layers []string) (MapDescriptor, error) {
	return d.prepareMap(ctx, target, operation, shared.RuntimeActionMapRender, transferID, sessionID, fileName, layers)
}

func (d *Agent) prepareMap(ctx context.Context, target Target, operation Operation, action shared.RuntimeAction, transferID, sessionID, fileName string, layers []string) (MapDescriptor, error) {
	request := runtimeRequest(target, operation, action)
	request.Map = &shared.RuntimeMapRequest{TransferID: transferID, SessionID: sessionID, FileName: fileName, Layers: layers}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 300)
	value, err := checkedMapResult(result.Result, transferID, err)
	if err != nil {
		return MapDescriptor{}, err
	}
	if !value.Complete || value.Size <= 0 || value.SHA256 == "" || value.SourceSHA256 == "" {
		return MapDescriptor{}, errors.New("Agent 未返回有效的地图传输描述")
	}
	return mapDescriptor(*value), nil
}

func (d *Agent) ReadMapTransfer(ctx context.Context, target Target, transferID string, offset int64) (MapChunk, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionMapRead)
	request.Map = &shared.RuntimeMapRequest{TransferID: transferID, Offset: offset}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	value, err := checkedMapResult(result.Result, transferID, err)
	if err != nil {
		return MapChunk{}, err
	}
	return MapChunk{
		MapDescriptor: mapDescriptor(*value), Offset: value.Offset, NextOffset: value.NextOffset,
		Data: value.Data, Complete: value.Complete,
	}, nil
}

func (d *Agent) ReleaseMapTransfer(ctx context.Context, target Target, operation Operation, transferID string) error {
	request := runtimeRequest(target, operation, shared.RuntimeActionMapRelease)
	request.Map = &shared.RuntimeMapRequest{TransferID: transferID}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	value, err := checkedMapResult(result.Result, transferID, err)
	if err != nil {
		return err
	}
	if !value.Complete {
		return errors.New("Agent 未确认地图临时文件已释放")
	}
	return nil
}

func checkedMapResult(result shared.RuntimeOperationResult, transferID string, err error) (*shared.RuntimeMapResult, error) {
	if err != nil {
		return nil, err
	}
	if result.Map == nil || transferID != "" && result.Map.TransferID != transferID {
		return nil, errors.New("Agent 未返回有效的地图结果")
	}
	return result.Map, nil
}

func mapDescriptor(value shared.RuntimeMapResult) MapDescriptor {
	return MapDescriptor{
		TransferID: value.TransferID, Size: value.Size, SHA256: value.SHA256,
		SourceSHA256: value.SourceSHA256, Log: value.Log,
	}
}

func (d *Agent) BeginConfiguration(ctx context.Context, target Target, operation Operation, descriptor ConfigurationDescriptor) (int64, error) {
	result, err := d.executeConfiguration(ctx, target, operation, shared.RuntimeActionConfigurationBegin, descriptor, 0, nil)
	return result.NextOffset, err
}

func (d *Agent) ApplyConfiguration(ctx context.Context, target Target, operation Operation, descriptor ConfigurationDescriptor, data []byte, expected map[string]string) ([]string, error) {
	request := runtimeRequest(target, operation, shared.RuntimeActionConfigurationApply)
	request.Configuration = &shared.RuntimeConfigurationRequest{
		PublicationID: descriptor.PublicationID, Scope: descriptor.Scope, Size: descriptor.Size, SHA256: descriptor.SHA256,
		Data: data, ExpectedFiles: expected,
	}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	value := result.Result.Configuration
	if value != nil && value.RevisionConflict {
		return nil, configpublication.ErrRevisionConflict
	}
	if err != nil {
		return nil, err
	}
	if value == nil || !value.Complete || value.PublicationID != descriptor.PublicationID || value.SHA256 != descriptor.SHA256 || value.Size != descriptor.Size {
		return nil, errors.New("Agent 未确认配置保存结果，请刷新配置后确认")
	}
	return value.Warnings, nil
}

func (d *Agent) WriteModOverrides(ctx context.Context, target Target, operation Operation, expectedSHA256 string, content []byte) error {
	request := runtimeRequest(target, operation, shared.RuntimeActionModConfigurationWrite)
	request.Configuration = &shared.RuntimeConfigurationRequest{
		Scope: runtimefiles.ConfigurationScopeMod, ExpectedSHA256: expectedSHA256, Data: content,
	}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	value := result.Result.Configuration
	if value != nil && value.RevisionConflict {
		return &runtimefiles.ConfigurationConflictError{CurrentSHA256: value.SHA256}
	}
	if err != nil {
		return err
	}
	sum := sha256.Sum256(content)
	if value == nil || !value.Complete || value.SHA256 != hex.EncodeToString(sum[:]) {
		return errors.New("Agent did not confirm the Mod configuration write")
	}
	return nil
}

func (d *Agent) WriteConfiguration(ctx context.Context, target Target, operation Operation, descriptor ConfigurationDescriptor, offset int64, data []byte) (int64, error) {
	result, err := d.executeConfiguration(ctx, target, operation, shared.RuntimeActionConfigurationWrite, descriptor, offset, data)
	return result.NextOffset, err
}

func (d *Agent) PrepareConfiguration(ctx context.Context, target Target, operation Operation, publicationID, scope string) error {
	_, err := d.executeConfiguration(ctx, target, operation, shared.RuntimeActionConfigurationPrepare, ConfigurationDescriptor{PublicationID: publicationID, Scope: scope}, 0, nil)
	return err
}

func (d *Agent) PublishConfiguration(ctx context.Context, target Target, operation Operation, publicationID, scope string) error {
	_, err := d.executeConfiguration(ctx, target, operation, shared.RuntimeActionConfigurationPublish, ConfigurationDescriptor{PublicationID: publicationID, Scope: scope}, 0, nil)
	return err
}

func (d *Agent) RollbackConfiguration(ctx context.Context, target Target, operation Operation, publicationID, scope string) error {
	_, err := d.executeConfiguration(ctx, target, operation, shared.RuntimeActionConfigurationRollback, ConfigurationDescriptor{PublicationID: publicationID, Scope: scope}, 0, nil)
	return err
}

func (d *Agent) CompleteConfiguration(ctx context.Context, target Target, operation Operation, publicationID, scope string) error {
	_, err := d.executeConfiguration(ctx, target, operation, shared.RuntimeActionConfigurationComplete, ConfigurationDescriptor{PublicationID: publicationID, Scope: scope}, 0, nil)
	return err
}

func (d *Agent) executeConfiguration(ctx context.Context, target Target, operation Operation, action shared.RuntimeAction, descriptor ConfigurationDescriptor, offset int64, data []byte) (shared.RuntimeConfigurationResult, error) {
	request := runtimeRequest(target, operation, action)
	request.Configuration = &shared.RuntimeConfigurationRequest{
		PublicationID: descriptor.PublicationID, Scope: descriptor.Scope, Offset: offset,
		Size: descriptor.Size, SHA256: descriptor.SHA256, Data: data,
	}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 300)
	if result.Result.Configuration == nil {
		if err != nil {
			return shared.RuntimeConfigurationResult{}, err
		}
		return shared.RuntimeConfigurationResult{}, errors.New("Agent 未返回有效的配置发布结果")
	}
	return *result.Result.Configuration, err
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
		RuntimeMode: operation.RuntimeMode, LaunchOptions: operation.LaunchOptions,
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

func (d *Agent) ListChatLogGenerations(ctx context.Context, target Target) ([]shared.RuntimeChatLogGeneration, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionChatLogsList)
	request.ChatLogs = &shared.RuntimeChatLogRequest{}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	if err != nil {
		return nil, err
	}
	if result.Result.ChatLogs == nil {
		return nil, errors.New("Agent 未返回聊天日志代次")
	}
	return append([]shared.RuntimeChatLogGeneration(nil), result.Result.ChatLogs.Generations...), nil
}

func (d *Agent) ReadChatLogGeneration(ctx context.Context, target Target, chatLogs shared.RuntimeChatLogRequest) (shared.RuntimeChatLogResult, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionChatLogsRead)
	request.ChatLogs = &chatLogs
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	if err != nil {
		return shared.RuntimeChatLogResult{}, err
	}
	if result.Result.ChatLogs == nil {
		return shared.RuntimeChatLogResult{}, errors.New("Agent 未返回聊天日志块")
	}
	return *result.Result.ChatLogs, nil
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

func (d *Agent) ReadWorldState(ctx context.Context, target Target) (shared.RuntimeWorldStateRead, error) {
	request := runtimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionWorldStateRead)
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 10)
	if err != nil {
		return shared.RuntimeWorldStateRead{}, err
	}
	if result.Result.WorldState == nil {
		return shared.RuntimeWorldStateRead{}, errors.New("Agent did not return world state files")
	}
	return *result.Result.WorldState, nil
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

func (d *Agent) ReadModSchema(ctx context.Context, target Target, workshopID string) (shared.RuntimeModSchema, error) {
	request := modRuntimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionModSchemaRead)
	request.Mod = &shared.RuntimeModRequest{WorkshopID: workshopID}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	value, err := checkedModResult(result.Result, err)
	if err != nil {
		return shared.RuntimeModSchema{}, err
	}
	if !value.Complete || value.Schema == nil || value.Schema.WorkshopID != workshopID || value.Schema.Parser == "" {
		return shared.RuntimeModSchema{}, errors.New("Agent 未返回匹配的 Mod 配置声明")
	}
	return *value.Schema, nil
}

func (d *Agent) GrantModArtifact(ctx context.Context, target Target, subjectTargetID, workshopID, treeSHA string) (shared.RuntimeModFetchLocation, error) {
	request := modRuntimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionModPeerGrant)
	request.Mod = &shared.RuntimeModRequest{
		WorkshopID: workshopID, ExpectedTreeSHA256: treeSHA, PeerSubject: subjectTargetID,
	}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	value, err := checkedModResult(result.Result, err)
	if err != nil {
		return shared.RuntimeModFetchLocation{}, err
	}
	if value.FetchLocation == nil || value.FetchLocation.Source != shared.RuntimeModFetchSourcePeer {
		return shared.RuntimeModFetchLocation{}, errors.New("Agent 未返回有效的 Mod Peer 下载授权")
	}
	return *value.FetchLocation, nil
}

func (d *Agent) FetchModCache(ctx context.Context, target Target, operation Operation, workshopID, treeSHA string, metadata shared.RuntimeModMetadata, locations []shared.RuntimeModFetchLocation) (ModFetchResult, error) {
	sources := []shared.RuntimeModFetchSource{shared.RuntimeModFetchSourceSteam}
	if len(locations) > 0 {
		sources = make([]shared.RuntimeModFetchSource, 0, len(locations))
		for _, location := range locations {
			sources = append(sources, location.Source)
		}
	}
	request := shared.RuntimeModRequest{
		WorkshopID: workshopID, ExpectedTreeSHA256: treeSHA, Metadata: metadata,
		FetchSources: sources, FetchLocations: append([]shared.RuntimeModFetchLocation(nil), locations...),
	}
	started := time.Now()
	result, err := d.executeMod(ctx, target, operation, shared.RuntimeActionModFetch, request, 5*time.Minute)
	fetchResult := ModFetchResult{}
	if result.Mod != nil {
		fetchResult.Attempts = append([]shared.RuntimeModFetchAttempt(nil), result.Mod.FetchAttempts...)
	}
	value, err := checkedModResult(result, err)
	if err != nil {
		return fetchResult, err
	}
	if value.CacheManifest == nil || !value.Complete || !containsModFetchSource(sources, value.FetchSource) {
		return fetchResult, errors.New("Agent 未确认 Mod 节点本地获取")
	}
	fetchResult.Manifest = *value.CacheManifest
	if fetchResult.Manifest.WorkshopID != workshopID || fetchResult.Manifest.TreeSHA256 == "" ||
		(treeSHA != "" && !strings.EqualFold(fetchResult.Manifest.TreeSHA256, treeSHA)) {
		return fetchResult, errors.New("Agent 返回了不匹配的 Mod 缓存 manifest")
	}
	if len(fetchResult.Attempts) == 0 {
		fetchResult.Attempts = []shared.RuntimeModFetchAttempt{{
			Source: value.FetchSource, Feasibility: shared.RuntimeModFetchFeasibilityAvailable,
			Status: shared.RuntimeModFetchStatusSucceeded, Selected: true,
			DurationMillis: maxDurationMillis(time.Since(started)), ObservedAt: started.UTC(),
		}}
	}
	return fetchResult, nil
}

func (d *Agent) DownloadMods(ctx context.Context, target Target, operation Operation, workshopIDs []string) error {
	result, err := d.executeMod(ctx, target, operation, shared.RuntimeActionModDownload, shared.RuntimeModRequest{
		WorkshopIDs: append([]string(nil), workshopIDs...),
	}, 30*time.Minute)
	value, err := checkedModResult(result, err)
	if err != nil {
		return err
	}
	if !value.Complete {
		return errors.New("Agent 未确认 Workshop 模组下载完成")
	}
	return nil
}

func (d *Agent) LinkMods(ctx context.Context, target Target, operation Operation, workshopIDs []string) error {
	result, err := d.executeMod(ctx, target, operation, shared.RuntimeActionModLink, shared.RuntimeModRequest{
		WorkshopIDs: append([]string(nil), workshopIDs...),
	}, 30*time.Second)
	value, err := checkedModResult(result, err)
	if err != nil {
		return err
	}
	if !value.Complete {
		return errors.New("Agent did not confirm local Workshop Mod links")
	}
	return nil
}

func maxDurationMillis(duration time.Duration) int64 {
	millis := duration.Milliseconds()
	if millis == 0 && duration > 0 {
		return 1
	}
	return millis
}

func containsModFetchSource(values []shared.RuntimeModFetchSource, expected shared.RuntimeModFetchSource) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
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

func (d *Agent) ModInstallationState(ctx context.Context, target Target) (*shared.RuntimeModInstallationState, error) {
	request := modRuntimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionModInstallationState)
	request.Mod = &shared.RuntimeModRequest{}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 60)
	value, err := checkedModResult(result.Result, err)
	if err != nil {
		return nil, err
	}
	return value.Installation, nil
}

func (d *Agent) ObserveModFiles(ctx context.Context, target Target, workshopIDs []string, worlds []shared.RuntimeModObserveWorld) (*shared.RuntimeModFilesObservation, error) {
	request := modRuntimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionModFilesObserve)
	request.Mod = &shared.RuntimeModRequest{
		WorkshopIDs: append([]string(nil), workshopIDs...),
		Worlds:      append([]shared.RuntimeModObserveWorld(nil), worlds...),
	}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	value, err := checkedModResult(result.Result, err)
	if err != nil {
		return nil, err
	}
	if value.Files == nil || value.Files.InstallationID != target.InstallationID || len(value.Files.Mods) != len(workshopIDs) {
		return nil, errors.New("Agent 返回的 Mod 文件观察结果不完整")
	}
	requestedIDs := make(map[string]bool, len(workshopIDs))
	for _, workshopID := range workshopIDs {
		if requestedIDs[workshopID] {
			return nil, errors.New("Mod 文件观察请求包含重复项目")
		}
		requestedIDs[workshopID] = true
		state, exists := value.Files.Mods[workshopID]
		if !exists || state.Status != shared.RuntimeModFileReady && state.Status != shared.RuntimeModFileMissing && state.Status != shared.RuntimeModFileInvalid {
			return nil, errors.New("Agent 返回的 Mod 文件状态无效")
		}
	}
	if len(value.Files.Worlds) != len(worlds) {
		return nil, errors.New("Agent 返回的 Mod 世界日志观察结果不完整")
	}
	for _, world := range worlds {
		observed, exists := value.Files.Worlds[world.RoomID+"/"+world.WorldID]
		if !exists || observed.RoomID != world.RoomID || observed.WorldID != world.WorldID {
			return nil, errors.New("Agent 返回的 Mod 世界日志身份无效")
		}
		seenLoaded := make(map[string]bool, len(observed.LoadedModIDs))
		for _, workshopID := range observed.LoadedModIDs {
			if !requestedIDs[workshopID] || seenLoaded[workshopID] {
				return nil, errors.New("Agent 返回的已加载 Mod 列表无效")
			}
			seenLoaded[workshopID] = true
		}
	}
	return value.Files, nil
}

func (d *Agent) InventoryModFiles(ctx context.Context, target Target) (*shared.RuntimeModFilesObservation, error) {
	request := modRuntimeRequest(target, Operation{ID: newOperationID()}, shared.RuntimeActionModFilesInventory)
	request.Mod = &shared.RuntimeModRequest{}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 30)
	value, err := checkedModResult(result.Result, err)
	if err != nil {
		return nil, err
	}
	if value.Files == nil || value.Files.InstallationID != target.InstallationID {
		return nil, errors.New("Agent 返回的 Mod 安装目录清单无效")
	}
	for workshopID, state := range value.Files.Mods {
		if !validRuntimeWorkshopID(workshopID) || state.Status != shared.RuntimeModFileReady && state.Status != shared.RuntimeModFileInvalid {
			return nil, errors.New("Agent 返回的 Mod 安装目录项目无效")
		}
	}
	return value.Files, nil
}

func validRuntimeWorkshopID(value string) bool {
	if len(value) == 0 || len(value) > 20 || value[0] == '0' {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
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

func (d *Agent) GrantMigrationExport(ctx context.Context, target Target, descriptor MigrationDescriptor, subjectTargetID string) (shared.RuntimeMigrationFetchLocation, error) {
	result, err := d.executeMigration(ctx, target, Operation{ID: newOperationID()}, shared.RuntimeActionMigrationPeerGrant, shared.RuntimeMigrationRequest{
		MigrationID: descriptor.MigrationID, PeerSubject: subjectTargetID, Size: descriptor.Size, SHA256: descriptor.SHA256,
	}, time.Minute)
	value, err := checkedMigrationResult(result, descriptor.MigrationID, err)
	if err != nil {
		return shared.RuntimeMigrationFetchLocation{}, err
	}
	if !value.Complete || value.FetchLocation == nil || value.FetchLocation.Size != descriptor.Size ||
		value.FetchLocation.SHA256 != descriptor.SHA256 {
		return shared.RuntimeMigrationFetchLocation{}, errors.New("Agent 未返回有效的迁移 Peer 下载授权")
	}
	return *value.FetchLocation, nil
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
	request := runtimeRequest(target, operation, shared.RuntimeActionMigrationImportBegin)
	request.Migration = &shared.RuntimeMigrationRequest{
		MigrationID: descriptor.MigrationID, Size: descriptor.Size, SHA256: descriptor.SHA256,
		ShardBindAll: descriptor.ShardBindAll, ShardMasterAddress: descriptor.ShardMasterAddress, ShardMasterPort: descriptor.ShardMasterPort,
	}
	result, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, timeoutSeconds(time.Minute))
	err = markOperationNotDispatched(result, err)
	value, err := checkedMigrationResult(result.Result, descriptor.MigrationID, err)
	if err != nil {
		return err
	}
	if value.Size != descriptor.Size || value.SHA256 != descriptor.SHA256 {
		return errors.New("Agent 未确认迁移导入描述")
	}
	return nil
}

func (d *Agent) FetchMigrationImport(ctx context.Context, target Target, operation Operation, descriptor MigrationDescriptor, locations []shared.RuntimeMigrationFetchLocation) (int64, error) {
	request := runtimeRequest(target, operation, shared.RuntimeActionMigrationFetch)
	request.Migration = &shared.RuntimeMigrationRequest{
		MigrationID: descriptor.MigrationID, Size: descriptor.Size, SHA256: descriptor.SHA256,
		FetchLocations: append([]shared.RuntimeMigrationFetchLocation(nil), locations...),
	}
	execution, err := d.executor.ExecuteRuntime(ctx, target.TargetID, request, 1800)
	if err != nil {
		if execution.Result.Migration != nil && execution.Result.Migration.MigrationID == descriptor.MigrationID {
			return execution.Result.Migration.NextOffset, errors.Join(ErrMigrationPeerFallback, err)
		}
		dispatchErr := markOperationNotDispatched(execution, err)
		if errors.Is(dispatchErr, ErrOperationNotDispatched) {
			return 0, errors.Join(ErrMigrationPeerFallback, dispatchErr)
		}
		return 0, dispatchErr
	}
	value, err := checkedMigrationResult(execution.Result, descriptor.MigrationID, nil)
	if err != nil {
		return 0, err
	}
	if !value.Complete || value.NextOffset != descriptor.Size || value.Size != descriptor.Size || value.SHA256 != descriptor.SHA256 {
		return value.NextOffset, errors.Join(ErrMigrationPeerFallback, errors.New("Agent 未确认迁移 Peer 制品获取完成"))
	}
	return value.NextOffset, nil
}

func markOperationNotDispatched(result agents.RuntimeExecutionResult, err error) error {
	if err == nil || result.RemoteID != "" {
		return err
	}
	if errors.Is(err, agents.ErrRuntimeInstallationNotRegistered) ||
		errors.Is(err, agents.ErrRuntimeNotConfigured) ||
		errors.Is(err, agents.ErrAgentNotFound) ||
		errors.Is(err, agents.ErrAgentOffline) ||
		errors.Is(err, agents.ErrUnsupportedAction) ||
		errors.Is(err, agents.ErrInvalidInput) {
		return errors.Join(ErrOperationNotDispatched, err)
	}
	return err
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
