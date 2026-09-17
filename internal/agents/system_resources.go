package agents

import (
	"context"
	"errors"
	"time"
)

type systemReportRequest struct {
	done chan struct{}
	err  error
}

// RefreshSystemInfo reuses the read-only report protocol without creating a job.
func (s *Service) RefreshSystemInfo(ctx context.Context, agentID string) (Agent, error) {
	if err := ctx.Err(); err != nil {
		return Agent{}, err
	}
	agent, err := s.Agent(agentID)
	if err != nil {
		return Agent{}, err
	}
	if agent.Status != StatusOnline {
		return Agent{}, ErrAgentOffline
	}

	s.systemReportMu.Lock()
	if pending := s.systemReports[agentID]; pending != nil {
		s.systemReportMu.Unlock()
		select {
		case <-ctx.Done():
			return Agent{}, ctx.Err()
		case <-pending.done:
			if pending.err != nil {
				return Agent{}, pending.err
			}
			return s.Agent(agentID)
		}
	}
	pending := &systemReportRequest{done: make(chan struct{})}
	if s.systemReports == nil {
		s.systemReports = make(map[string]*systemReportRequest)
	}
	s.systemReports[agentID] = pending
	s.systemReportMu.Unlock()

	reportContext, cancel := context.WithTimeout(ctx, 8*time.Second)
	result, err := s.transport.Execute(reportContext, agentID, ActionSystemRefresh, 8)
	cancel()
	if err == nil && result.ExitCode != 0 {
		err = errors.New("Agent system report failed")
	}
	s.systemReportMu.Lock()
	pending.err = err
	delete(s.systemReports, agentID)
	close(pending.done)
	s.systemReportMu.Unlock()
	if err != nil {
		return Agent{}, err
	}
	return s.Agent(agentID)
}
