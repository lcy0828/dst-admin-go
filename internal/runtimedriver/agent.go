package runtimedriver

import (
	"context"
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
