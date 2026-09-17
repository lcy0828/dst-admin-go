package agents

import (
	"context"
	"errors"
	"strings"
	"time"

	"dont/internal/networkprobe"
	"dont/shared"

	"github.com/google/uuid"
)

const maximumEndpointProbeTimeout = 15 * time.Second

func (s *Service) DetectEgress(ctx context.Context, targetID string, region shared.RuntimeNetworkRegion) (shared.RuntimeNetworkResult, error) {
	targetID = strings.TrimSpace(targetID)
	if !shared.IsRuntimeNetworkRegion(region) {
		return shared.RuntimeNetworkResult{}, ErrInvalidInput
	}
	if targetID == "local" {
		probeContext, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		address, err := networkprobe.Detect(probeContext, region)
		if err != nil {
			return shared.RuntimeNetworkResult{}, err
		}
		return shared.RuntimeNetworkResult{Address: address.String(), Region: region, ObservedAt: time.Now().UTC()}, nil
	}
	if !strings.HasPrefix(targetID, "agent:") {
		return shared.RuntimeNetworkResult{}, ErrRuntimeTargetNotFound
	}
	request := shared.RuntimeOperationRequest{
		ProtocolVersion:  shared.RuntimeOperationProtocolVersion,
		OperationID:      uuid.NewString(),
		Action:           shared.RuntimeActionNetworkEgressObserve,
		Cluster:          "Network",
		Shard:            "Egress",
		TopologyRevision: "network-egress-v1",
		Network:          &shared.RuntimeNetworkRequest{Region: region},
	}
	result, err := s.ExecuteRuntime(ctx, targetID, request, 15)
	if err != nil {
		return shared.RuntimeNetworkResult{}, err
	}
	if result.Result.Network == nil {
		return shared.RuntimeNetworkResult{}, errors.New("Agent 未返回公网出口探测结果")
	}
	address, err := networkprobe.ParsePublicAddress(result.Result.Network.Address)
	if err != nil {
		return shared.RuntimeNetworkResult{}, errors.New("Agent 返回的公网出口地址无效")
	}
	value := *result.Result.Network
	if value.Region != region {
		return shared.RuntimeNetworkResult{}, errors.New("Agent 版本过旧或未按指定区域执行公网出口探测")
	}
	value.Address = address.String()
	return value, nil
}

func (s *Service) ListenNetworkEndpoint(ctx context.Context, targetID, bindAddress string, port int, tokens []string, timeout time.Duration) ([]string, error) {
	if timeout < 500*time.Millisecond || timeout > maximumEndpointProbeTimeout || port < 1 || port > 65535 || len(tokens) == 0 || len(tokens) > 64 {
		return nil, ErrInvalidInput
	}
	if targetID == "local" {
		return networkprobe.ListenEndpoint(ctx, bindAddress, port, tokens, timeout)
	}
	request := networkRuntimeRequest(shared.RuntimeActionNetworkEndpointListen)
	request.Network = &shared.RuntimeNetworkRequest{
		BindAddress: bindAddress, Port: port, Tokens: append([]string(nil), tokens...), TimeoutMillis: int(timeout.Milliseconds()),
	}
	result, err := s.ExecuteRuntime(ctx, targetID, request, 20)
	if err != nil {
		return nil, err
	}
	if result.Result.Network == nil {
		return nil, errors.New("Agent 未返回端点监听探测结果")
	}
	return append([]string(nil), result.Result.Network.ReceivedTokens...), nil
}

func (s *Service) ProbeNetworkEndpoints(ctx context.Context, targetID string, endpoints []shared.RuntimeNetworkEndpointRequest, timeout time.Duration) ([]shared.RuntimeNetworkEndpointResult, error) {
	if timeout < 500*time.Millisecond || timeout > maximumEndpointProbeTimeout || len(endpoints) == 0 || len(endpoints) > 64 {
		return nil, ErrInvalidInput
	}
	if targetID == "local" {
		return networkprobe.ProbeEndpoints(ctx, endpoints, timeout), nil
	}
	request := networkRuntimeRequest(shared.RuntimeActionNetworkEndpointProbe)
	request.Network = &shared.RuntimeNetworkRequest{Endpoints: append([]shared.RuntimeNetworkEndpointRequest(nil), endpoints...), TimeoutMillis: int(timeout.Milliseconds())}
	result, err := s.ExecuteRuntime(ctx, targetID, request, 20)
	if err != nil {
		return nil, err
	}
	if result.Result.Network == nil {
		return nil, errors.New("Agent 未返回端点连通探测结果")
	}
	return append([]shared.RuntimeNetworkEndpointResult(nil), result.Result.Network.EndpointProbes...), nil
}

func networkRuntimeRequest(action shared.RuntimeAction) shared.RuntimeOperationRequest {
	return shared.RuntimeOperationRequest{
		ProtocolVersion:  shared.RuntimeOperationProtocolVersion,
		OperationID:      uuid.NewString(),
		Action:           action,
		Cluster:          "Network",
		Shard:            "Endpoint",
		TopologyRevision: "network-endpoint-v1",
	}
}
