package agents

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"dont/internal/jobs"
	"dont/shared"
)

const (
	agentUpgradeCommandTimeout = 300
	agentUpgradeJobTimeout     = 7 * time.Minute
	agentReconnectTimeout      = 90 * time.Second
	agentDownloadTokenTTL      = 10 * time.Minute
)

func (s *Service) Releases() ([]AgentRelease, error) {
	if s == nil || s.releases == nil {
		return nil, ErrUnavailable
	}
	return s.releases.List()
}

func (s *Service) SaveRelease(version, fileName string, source io.Reader) (AgentRelease, error) {
	if s == nil || s.releases == nil {
		return AgentRelease{}, ErrUnavailable
	}
	return s.releases.Save(version, fileName, source)
}

func (s *Service) DeleteRelease(id string) error {
	if s == nil || s.releases == nil {
		return ErrUnavailable
	}
	return s.releases.Delete(strings.TrimSpace(id))
}

func (s *Service) OpenReleaseDownload(releaseID, token string) (AgentRelease, io.ReadCloser, error) {
	if s == nil || s.releases == nil {
		return AgentRelease{}, nil, ErrUnavailable
	}
	return s.releases.OpenDownload(strings.TrimSpace(releaseID), strings.TrimSpace(token))
}

func (s *Service) UpgradeAgent(agentID string, input AgentUpgradeInput) (jobs.Job, error) {
	if s == nil || s.releases == nil {
		return jobs.Job{}, ErrUnavailable
	}
	agent, err := s.Agent(strings.TrimSpace(agentID))
	if err != nil {
		return jobs.Job{}, err
	}
	if agent.Status != StatusOnline {
		return jobs.Job{}, ErrAgentOffline
	}
	if agent.Update.Mode != AgentUpdateModeSelf || !agent.Update.Supported {
		return jobs.Job{}, ErrUpgradeUnsupported
	}
	release, err := s.resolveUpgradeRelease(agent, input.ReleaseID)
	if err != nil {
		return jobs.Job{}, err
	}
	if !s.beginAgentUpgrade(agent.ID) {
		return jobs.Job{}, ErrUpgradeInProgress
	}
	token, err := s.releases.IssueDownloadToken(agent.ID, release.ID, agentDownloadTokenTTL)
	if err != nil {
		s.finishAgentUpgrade(agent.ID)
		return jobs.Job{}, err
	}
	request := shared.AgentUpgradeRequest{
		ProtocolVersion: shared.AgentUpgradeProtocolVersion,
		ReleaseID:       release.ID,
		Version:         release.Version,
		OS:              release.OS,
		Arch:            release.Arch,
		DownloadPath:    "/agent-updates/" + release.ID,
		DownloadToken:   token,
		SHA256:          release.SHA256,
		Size:            release.Size,
	}
	job, err := s.jobs.SubmitFactory("agent.upgrade", "", "", []jobs.TargetSpec{{ID: agent.ID, Name: agent.DisplayName}}, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			defer s.finishAgentUpgrade(agent.ID)
			defer s.releases.RevokeDownloadToken(token)
			upgradeContext, cancel := context.WithTimeout(ctx, agentUpgradeJobTimeout)
			defer cancel()
			result, executeErr := s.transport.ExecuteUpgrade(upgradeContext, agent.ID, request, agentUpgradeCommandTimeout)
			if executeErr != nil {
				return reportAgentUpgradeFailure(report, agent.ID, "AGENT_UPGRADE_FAILED", executeErr)
			}
			if result.Result.ProtocolVersion != shared.AgentUpgradeProtocolVersion || result.Result.ReleaseID != release.ID ||
				result.Result.Version != release.Version || !result.Result.RestartRequired {
				return reportAgentUpgradeFailure(report, agent.ID, "AGENT_UPGRADE_RESPONSE_INVALID", errors.New("Agent 返回的升级结果无效"))
			}
			if reconnectErr := s.waitForAgentVersion(upgradeContext, agent.ID, release.Version); reconnectErr != nil {
				code := "AGENT_RECONNECT_FAILED"
				switch {
				case errors.Is(reconnectErr, ErrUpgradeVersionMismatch):
					code = "AGENT_VERSION_MISMATCH"
				case errors.Is(reconnectErr, ErrUpgradeReconnectTimeout), errors.Is(reconnectErr, context.DeadlineExceeded):
					code = "AGENT_RECONNECT_TIMEOUT"
				}
				return reportAgentUpgradeFailure(report, agent.ID, code, reconnectErr)
			}
			report(jobs.TargetResult{
				TargetID: agent.ID, Status: jobs.StatusSucceeded,
				Message: fmt.Sprintf("Agent 已升级到 %s 并重新连接", release.Version),
			})
			_, _ = s.Sync()
			return nil
		}
	})
	if err != nil {
		s.releases.RevokeDownloadToken(token)
		s.finishAgentUpgrade(agent.ID)
		return jobs.Job{}, err
	}
	return job, nil
}

func (s *Service) beginAgentUpgrade(agentID string) bool {
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()
	if _, exists := s.activeUpgrades[agentID]; exists {
		return false
	}
	s.activeUpgrades[agentID] = struct{}{}
	return true
}

func (s *Service) finishAgentUpgrade(agentID string) {
	s.upgradeMu.Lock()
	delete(s.activeUpgrades, agentID)
	s.upgradeMu.Unlock()
}

func (s *Service) resolveUpgradeRelease(agent Agent, releaseID string) (AgentRelease, error) {
	releaseID = strings.TrimSpace(releaseID)
	var release AgentRelease
	var err error
	if releaseID == "" {
		latest, latestErr := s.releases.Latest(agent.OS, agent.Arch)
		if latestErr != nil {
			return AgentRelease{}, latestErr
		}
		if latest == nil {
			return AgentRelease{}, ErrUpgradeNotAvailable
		}
		release = *latest
	} else {
		release, err = s.releases.Get(releaseID)
		if err != nil {
			return AgentRelease{}, err
		}
	}
	if release.OS != strings.ToLower(agent.OS) || release.Arch != strings.ToLower(agent.Arch) {
		return AgentRelease{}, ErrInvalidInput
	}
	if compareAgentVersions(release.Version, agent.Version) <= 0 {
		return AgentRelease{}, ErrUpgradeNotAvailable
	}
	return release, nil
}

func (s *Service) waitForAgentVersion(ctx context.Context, agentID, expectedVersion string) error {
	return s.waitForAgentVersionWithin(ctx, agentID, expectedVersion, agentReconnectTimeout, 500*time.Millisecond)
}

func (s *Service) waitForAgentVersionWithin(ctx context.Context, agentID, expectedVersion string, timeout, pollInterval time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	lastVersion := ""
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if lastVersion != "" {
				return fmt.Errorf("%w: Agent 重新连接后报告版本 %s，期望版本 %s", ErrUpgradeVersionMismatch, lastVersion, expectedVersion)
			}
			return fmt.Errorf("%w: Agent 未在 %s 内重新连接", ErrUpgradeReconnectTimeout, timeout)
		case <-ticker.C:
			snapshots, err := s.transport.Snapshots()
			if err != nil {
				continue
			}
			for _, snapshot := range snapshots {
				if snapshot.ID != agentID || snapshot.Status != StatusOnline {
					continue
				}
				if snapshot.Version == expectedVersion {
					return nil
				}
				if strings.TrimSpace(snapshot.Version) != "" {
					lastVersion = snapshot.Version
				}
			}
		}
	}
}

func reportAgentUpgradeFailure(report func(jobs.TargetResult), agentID, code string, err error) error {
	report(jobs.TargetResult{
		TargetID: agentID, Status: jobs.StatusFailed,
		Error: &jobs.Error{Code: code, Message: err.Error()},
	})
	return err
}
