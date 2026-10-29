package server

import (
	"dont/shared"
	"fmt"
)

func (s *Server) SendServiceLogRequest(agentID string, request shared.ServiceLogRequest) (string, error) {
	switch request.Action {
	case "query", "context", "status", "settings.read", "settings.update":
	default:
		return "", fmt.Errorf("invalid service log action")
	}
	return s.sendCommandPayload(agentID, shared.CommandPayload{Type: shared.ServiceLogsCommand, ServiceLogs: &request, Timeout: 10}, request.Action)
}
