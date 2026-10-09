package agents

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"dont/internal/jobs"
	"dont/internal/softwareupdate"
	"dont/shared"
	"github.com/google/uuid"
)

type AgentSoftwareInput struct {
	Version      string `json:"version"`
	Confirmation string `json:"confirmation"`
	Source       string `json:"source"`
	OperationID  string `json:"operationId,omitempty"`
}

var errAgentSoftwareRejected = errors.New("Agent rejected the software update")

type agentSoftwareRelay struct {
	token  string
	until  time.Time
	asset  softwareupdate.Asset
	source string
	used   bool
}

func (s *Service) agentSoftwareClient() softwareupdate.ReleaseClient {
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()
	if s.softwareClient == nil {
		s.softwareClient = softwareupdate.NewAgentGitHubClient(nil)
	}
	return s.softwareClient
}

func (s *Service) checkAgentRelease(ctx context.Context, platform string, force bool) softwareupdate.Check {
	s.softwareCheckMu.Lock()
	defer s.softwareCheckMu.Unlock()
	s.softwareCacheMu.RLock()
	check := s.softwareReleaseChecks[platform]
	s.softwareCacheMu.RUnlock()
	if !check.CheckedAt.IsZero() && (time.Since(check.CheckedAt) < 5*time.Second || !force && time.Since(check.CheckedAt) < 20*time.Minute) {
		check.Cached = true
		return check
	}
	check.CheckedAt = time.Now().UTC()
	check.Cached = false
	checkCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	release, err := s.agentSoftwareClient().Latest(checkCtx, platform, "auto")
	if err != nil {
		check.Warning = err.Error()
		check.Cached = check.Latest != nil
	} else {
		check.Latest = release
		check.Warning = ""
	}
	s.softwareCacheMu.Lock()
	if s.softwareReleaseChecks == nil {
		s.softwareReleaseChecks = map[string]softwareupdate.Check{}
	}
	s.softwareReleaseChecks[platform] = check
	s.softwareCacheMu.Unlock()
	return check
}

func (s *Service) withControllerCheck(snapshot softwareupdate.Snapshot) softwareupdate.Snapshot {
	s.softwareCacheMu.RLock()
	check := s.softwareReleaseChecks[snapshot.Platform]
	s.softwareCacheMu.RUnlock()
	if check.Latest != nil && check.CheckedAt.After(snapshot.Check.CheckedAt) {
		snapshot.Check = check
		snapshot.Check.HasUpdate = softwareupdate.CompareVersions(check.Latest.Version, snapshot.Current.Version) > 0
	}
	return snapshot
}

func (s *Service) softwareRequest(ctx context.Context, agentID string, request shared.AgentUpgradeRequest) (softwareupdate.Snapshot, error) {
	agent, err := s.Agent(agentID)
	if err != nil {
		return softwareupdate.Snapshot{}, err
	}
	if agent.Status != StatusOnline {
		return softwareupdate.Snapshot{}, ErrAgentOffline
	}
	if !containsString(agent.Capabilities, shared.AgentSoftwareUpdateCapability) {
		return softwareupdate.Snapshot{}, ErrUpgradeUnsupported
	}
	request.ProtocolVersion = shared.AgentUpgradeProtocolVersion
	if request.ReleaseID == "" {
		request.ReleaseID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	request.OS, request.Arch = agent.OS, agent.Arch
	result, err := s.transport.ExecuteUpgrade(ctx, agentID, request, 30)
	if err != nil {
		if result.Result.ProtocolVersion == request.ProtocolVersion && result.Result.ReleaseID == request.ReleaseID {
			return softwareupdate.Snapshot{}, fmt.Errorf("%w: %w", errAgentSoftwareRejected, err)
		}
		return softwareupdate.Snapshot{}, err
	}
	var snapshot softwareupdate.Snapshot
	if len(result.Result.Software) == 0 || json.Unmarshal(result.Result.Software, &snapshot) != nil || snapshot.Platform != agent.OS+"-"+agent.Arch {
		return snapshot, ErrInvalidInput
	}
	return snapshot, nil
}

func (s *Service) AgentSoftware(ctx context.Context, id string, check, force bool, source string) (softwareupdate.Snapshot, error) {
	if source == "controller" && check {
		snapshot, err := s.softwareRequest(ctx, id, shared.AgentUpgradeRequest{Action: "status"})
		if err != nil {
			return snapshot, err
		}
		snapshot.Check = s.checkAgentRelease(ctx, snapshot.Platform, force)
		snapshot.Check.HasUpdate = snapshot.Check.Latest != nil && softwareupdate.CompareVersions(snapshot.Check.Latest.Version, snapshot.Current.Version) > 0
		return snapshot, nil
	}
	if source == "" || source == "controller" {
		source = "auto"
	}
	if !softwareupdate.ValidSource(source) {
		return softwareupdate.Snapshot{}, ErrInvalidInput
	}
	action := "status"
	if check {
		action = "check"
	}
	snapshot, err := s.softwareRequest(ctx, id, shared.AgentUpgradeRequest{Action: action, Force: force, Source: source})
	return s.withControllerCheck(snapshot), err
}

func (s *Service) UpdateAgentSoftware(id string, input AgentSoftwareInput) (jobs.Job, error) {
	version, valid := softwareupdate.NormalizeVersion(input.Version)
	if !valid || input.Confirmation != input.Version {
		return jobs.Job{}, ErrInvalidInput
	}
	if input.Source == "" {
		input.Source = "auto"
	}
	if input.Source != "controller" && !softwareupdate.ValidSource(input.Source) {
		return jobs.Job{}, ErrInvalidInput
	}
	agent, err := s.Agent(id)
	if err != nil {
		return jobs.Job{}, err
	}
	if agent.Status != StatusOnline {
		return jobs.Job{}, ErrAgentOffline
	}
	if !containsString(agent.Capabilities, shared.AgentSoftwareUpdateCapability) {
		return jobs.Job{}, ErrUpgradeUnsupported
	}
	if !s.beginAgentUpgrade(id) {
		return jobs.Job{}, ErrUpgradeInProgress
	}
	job, err := s.jobs.SubmitFactory("agent.software.update", "", "", []jobs.TargetSpec{{ID: id, Name: agent.DisplayName}}, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			defer s.finishAgentUpgrade(id)
			ctx, cancel := context.WithTimeout(ctx, 18*time.Minute)
			defer cancel()
			request := shared.AgentUpgradeRequest{Action: "update", Version: version, Source: input.Source, OperationID: input.OperationID}
			if input.OperationID != "" {
				request.Action = "apply"
			}
			baseline, err := s.softwareRequest(ctx, id, shared.AgentUpgradeRequest{Action: "status"})
			if err != nil {
				return reportAgentUpgradeFailure(report, id, "AGENT_SOFTWARE_UPDATE_FAILED", err)
			}
			if softwareupdate.CompareVersions(version, baseline.Current.Version) < 0 || request.Action == "apply" && (baseline.Operation == nil || baseline.Operation.ID != input.OperationID || baseline.Operation.Version != version) {
				return reportAgentUpgradeFailure(report, id, "AGENT_SOFTWARE_UPDATE_FAILED", ErrInvalidInput)
			}
			var revoke func()
			if input.Source == "controller" && request.Action == "update" {
				var relayErr error
				request, revoke, relayErr = s.relaySoftwareRequest(ctx, agent, request)
				if relayErr != nil {
					return reportAgentUpgradeFailure(report, id, "AGENT_UPDATE_RELAY_FAILED", relayErr)
				}
				defer revoke()
			}
			snapshot, executeErr := s.softwareRequest(ctx, id, request)
			if errors.Is(executeErr, errAgentSoftwareRejected) {
				return reportAgentUpgradeFailure(report, id, "AGENT_SOFTWARE_UPDATE_FAILED", executeErr)
			}
			operationID := input.OperationID
			if executeErr == nil && snapshot.Operation != nil {
				operationID = snapshot.Operation.ID
			}
			if executeErr == nil && (operationID == "" || snapshot.Operation == nil || snapshot.Operation.Version != version) {
				return reportAgentUpgradeFailure(report, id, "AGENT_SOFTWARE_UPDATE_FAILED", ErrInvalidInput)
			}
			// Lost acknowledgements are ambiguous. Observe the exact operation or
			// newly started version before deciding; never repeat an installation.
			uncertainUntil := time.Now().Add(3 * time.Minute)
			applySent := request.Action == "apply"
			if executeErr != nil {
				snapshot = softwareupdate.Snapshot{}
			}
			// Poll only this explicit update job. It stops on success, failure or its
			// deadline. Normal machine inventory never queries GitHub or the Agent.
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			lastProgress := ""
			for {
				if operationID == "" && snapshot.Operation != nil && snapshot.Operation.Version == version &&
					(baseline.Operation == nil || snapshot.Operation.ID != baseline.Operation.ID || baseline.Operation.Phase == "prepared" && baseline.Operation.Version == version) {
					operationID = snapshot.Operation.ID
				}
				if snapshot.Operation != nil && snapshot.Operation.ID == operationID {
					op := snapshot.Operation
					if op.Version != version {
						return reportAgentUpgradeFailure(report, id, "AGENT_SOFTWARE_UPDATE_FAILED", ErrInvalidInput)
					}
					if !applySent || op.Phase != "prepared" {
						executeErr = nil
					}
					progressKey := fmt.Sprintf("%s/%d/%d/%d", op.Phase, op.Progress, op.DownloadedBytes, op.BytesPerSecond)
					if progressKey != lastProgress {
						_, _ = s.jobs.UpdateProgressDetail(job.ID, jobs.ProgressUpdate{Progress: op.Progress, Message: op.Phase, CurrentBytes: op.DownloadedBytes, TotalBytes: op.TotalBytes, BytesPerSecond: op.BytesPerSecond, Detail: &jobs.ProgressDetail{Stage: op.Phase, TargetID: id}})
						lastProgress = progressKey
					}
					switch op.Phase {
					case "failed", "rolled_back":
						return reportAgentUpgradeFailure(report, id, "AGENT_SOFTWARE_UPDATE_FAILED", fmt.Errorf("%s: %s", op.Phase, op.Error))
					case "succeeded":
						if snapshot.Ready && snapshot.Current.Version == version {
							report(jobs.TargetResult{TargetID: id, Status: jobs.StatusSucceeded, Message: "Agent 已更新并重新连接"})
							_, _ = s.Sync()
							return nil
						}
					case "prepared":
						if snapshot.Ready && !applySent {
							applySent = true
							uncertainUntil = time.Now().Add(3 * time.Minute)
							var applied softwareupdate.Snapshot
							applied, executeErr = s.softwareRequest(ctx, id, shared.AgentUpgradeRequest{Action: "apply", OperationID: operationID, Version: version})
							if errors.Is(executeErr, errAgentSoftwareRejected) {
								return reportAgentUpgradeFailure(report, id, "AGENT_UPDATE_APPLY_FAILED", executeErr)
							}
							if executeErr == nil {
								snapshot = applied
							}
						}
					}
				}
				if executeErr != nil && time.Now().After(uncertainUntil) {
					return reportAgentUpgradeFailure(report, id, "AGENT_UPDATE_ACK_TIMEOUT", executeErr)
				}
				select {
				case <-ctx.Done():
					return reportAgentUpgradeFailure(report, id, "AGENT_UPDATE_TIMEOUT", ctx.Err())
				case <-ticker.C:
				}
				probe, probeCancel := context.WithTimeout(ctx, 5*time.Second)
				value, statusErr := s.softwareRequest(probe, id, shared.AgentUpgradeRequest{Action: "status"})
				probeCancel()
				if statusErr == nil {
					snapshot = value
				}
			}
		}
	})
	if err != nil {
		s.finishAgentUpgrade(id)
	}
	return job, err
}

func (s *Service) relaySoftwareRequest(ctx context.Context, agent Agent, request shared.AgentUpgradeRequest) (shared.AgentUpgradeRequest, func(), error) {
	client := s.agentSoftwareClient()
	release, err := client.Latest(ctx, agent.OS+"-"+agent.Arch, "auto")
	if err != nil {
		return request, nil, err
	}
	if release == nil || release.Version != request.Version || !release.OnlineUpdate {
		return request, nil, softwareupdate.ErrReleaseChanged
	}
	checksum, err := client.Checksum(ctx, release.Checksum, "auto")
	if err != nil {
		return request, nil, err
	}
	fields := strings.Fields(string(checksum))
	if len(fields) != 2 || !runtimePerformanceSHA256Pattern.MatchString(fields[0]) || strings.TrimPrefix(fields[1], "*") != release.Archive.Name || release.Archive.Digest != "" && release.Archive.Digest != "sha256:"+fields[0] {
		return request, nil, ErrInvalidInput
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	token := uuid.NewString() + uuid.NewString()
	s.upgradeMu.Lock()
	if s.softwareRelays == nil {
		s.softwareRelays = map[string]*agentSoftwareRelay{}
	}
	s.softwareRelays[id] = &agentSoftwareRelay{token: token, until: time.Now().Add(16 * time.Minute), asset: release.Archive, source: "auto"}
	s.upgradeMu.Unlock()
	request.ReleaseID, request.DownloadPath, request.DownloadToken, request.SHA256, request.Size = id, "/agent-software-updates/"+id, token, fields[0], release.Archive.Size
	return request, func() { s.upgradeMu.Lock(); delete(s.softwareRelays, id); s.upgradeMu.Unlock() }, nil
}

// The optional relay streams a single official archive with a short-lived
// token. It never buffers the whole binary, creates a cache, or accepts a URL.
func (s *Service) AgentSoftwareTransfer(ctx context.Context, id, token string, writer func(int64) io.Writer) (int64, error) {
	s.upgradeMu.Lock()
	grant := s.softwareRelays[id]
	if grant == nil || grant.used || time.Now().After(grant.until) || subtle.ConstantTimeCompare([]byte(token), []byte(grant.token)) != 1 {
		s.upgradeMu.Unlock()
		return 0, ErrDownloadToken
	}
	grant.used = true
	copy := *grant
	s.upgradeMu.Unlock()
	return copy.asset.Size, s.agentSoftwareClient().Download(ctx, copy.asset, copy.source, writer(copy.asset.Size))
}
