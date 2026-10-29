package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"dont/shared"
)

func (t *LegacyTransport) ExecuteServiceLogs(ctx context.Context, agentID string, request shared.ServiceLogRequest) (shared.ServiceLogResult, error) {
	server := t.current()
	if server == nil {
		return shared.ServiceLogResult{}, ErrUnavailable
	}
	id, err := server.SendServiceLogRequest(agentID, request)
	if err != nil {
		return shared.ServiceLogResult{}, err
	}
	command, err := server.WaitCommandResult(ctx, id, nil)
	if err != nil {
		return shared.ServiceLogResult{}, err
	}
	if !command.Success {
		return shared.ServiceLogResult{}, errors.New(nonEmpty(command.ErrorMsg, "service log request failed"))
	}
	var result shared.ServiceLogResult
	if len(command.Output) > 2*1024*1024 || json.Unmarshal([]byte(command.Output), &result) != nil {
		return result, fmt.Errorf("invalid service log response")
	}
	return result, nil
}
func (s *Service) ServiceLogSources() ([]shared.ServiceLogSource, error) {
	items, err := s.CachedAgents()
	if err != nil {
		return nil, err
	}
	out := []shared.ServiceLogSource{}
	for _, node := range items {
		name := node.DisplayName
		if name == "" {
			name = node.Hostname
		}
		out = append(out, shared.ServiceLogSource{ID: "agent:" + node.ID, Name: name, NodeID: "agent:" + node.ID, Online: node.Status == StatusOnline, Supported: slices.Contains(node.Capabilities, shared.ServiceLogsCommand)})
	}
	return out, nil
}
func (s *Service) ReadServiceLogs(ctx context.Context, sourceID string, request shared.ServiceLogRequest) (shared.ServiceLogResult, error) {
	if !strings.HasPrefix(sourceID, "agent:") {
		return shared.ServiceLogResult{}, ErrInvalidInput
	}
	sources, err := s.ServiceLogSources()
	if err != nil {
		return shared.ServiceLogResult{}, err
	}
	var source *shared.ServiceLogSource
	for i := range sources {
		if sources[i].ID == sourceID {
			source = &sources[i]
			break
		}
	}
	if source == nil {
		return shared.ServiceLogResult{}, ErrAgentNotFound
	}
	if !source.Online {
		return shared.ServiceLogResult{}, ErrAgentOffline
	}
	if !source.Supported {
		return shared.ServiceLogResult{}, ErrUnsupportedAction
	}
	transport, ok := s.transport.(interface {
		ExecuteServiceLogs(context.Context, string, shared.ServiceLogRequest) (shared.ServiceLogResult, error)
	})
	if !ok {
		return shared.ServiceLogResult{}, ErrUnsupportedAction
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return transport.ExecuteServiceLogs(ctx, strings.TrimPrefix(sourceID, "agent:"), request)
}
