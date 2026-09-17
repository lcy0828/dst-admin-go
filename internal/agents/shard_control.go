package agents

import (
	"context"
	"errors"
	"strings"

	"dont/internal/requesttiming"
	"dont/shared"
)

func (s *Service) ExecuteShard(ctx context.Context, targetID string, request shared.ShardOperationRequest, timeoutSeconds int) (ShardExecutionResult, error) {
	targetID = strings.TrimSpace(targetID)
	if !strings.HasPrefix(targetID, "agent:") {
		return ShardExecutionResult{}, ErrInvalidInput
	}
	agentID := strings.TrimPrefix(targetID, "agent:")
	finishAgent := requesttiming.Start(ctx, "controller.agent_metadata")
	agent, err := s.Agent(agentID)
	finishAgent()
	if err != nil {
		return ShardExecutionResult{}, err
	}
	if agent.Status != StatusOnline {
		return ShardExecutionResult{}, ErrAgentOffline
	}
	if !containsString(agent.Capabilities, "shard.control.v1") {
		return ShardExecutionResult{}, ErrUnsupportedAction
	}
	if shared.ShardActionMutates(request.Action) && !containsString(agent.Capabilities, "shard.control.v2") {
		return ShardExecutionResult{}, ErrUnsupportedAction
	}
	finishConfig := requesttiming.Start(ctx, "controller.agent_config")
	config, err := s.runtimeConfigForAgent(agent)
	finishConfig()
	if err != nil {
		return ShardExecutionResult{}, err
	}
	if request.InstallationID != "" && request.InstallationID != config.InstallationID {
		return ShardExecutionResult{}, ErrInvalidInput
	}
	request.InstallationID = config.InstallationID
	if timeoutSeconds < 5 || timeoutSeconds > 300 || !shared.IsShardAction(request.Action) {
		return ShardExecutionResult{}, ErrInvalidInput
	}
	result, err := s.transport.ExecuteShard(ctx, agentID, request, timeoutSeconds)
	if err != nil {
		return result, err
	}
	if result.Result.ProtocolVersion != shared.ShardOperationProtocolVersion ||
		result.Result.OperationID != request.OperationID || result.Result.InstallationID != request.InstallationID ||
		result.Result.Action != request.Action || result.Result.Cluster != request.Cluster || result.Result.Shard != request.Shard {
		return ShardExecutionResult{}, errors.New("Agent 返回的分片操作结果与请求不一致")
	}
	return result, nil
}
