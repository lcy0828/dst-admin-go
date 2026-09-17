package runtimeoverview

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/savehealth"
	"dont/internal/topology"
	"dont/shared"
)

type Topology interface {
	Topology(context.Context, string) (topology.Snapshot, error)
}

type Runtime interface {
	Status(context.Context, string, string) (shared.ShardRuntimeStatus, error)
}

type Artifacts interface {
	Health(context.Context, string, string) (dstruntime.Health, error)
	LatestDiagnostic(context.Context, string, string) (dstruntime.DiagnosticReport, error)
}

type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RuntimeStatus struct {
	State         string `json:"state"`
	Code          string `json:"code,omitempty"`
	Message       string `json:"message,omitempty"`
	SessionExists bool   `json:"sessionExists"`
	Paused        *bool  `json:"paused,omitempty"`
}

type ArtifactStatus struct {
	Available  bool                         `json:"available"`
	Freshness  string                       `json:"freshness"`
	AgeSeconds int64                        `json:"ageSeconds,omitempty"`
	ObservedAt *time.Time                   `json:"observedAt,omitempty"`
	Health     *dstruntime.Health           `json:"health,omitempty"`
	Diagnostic *dstruntime.DiagnosticReport `json:"diagnostic,omitempty"`
	Problem    *Problem                     `json:"problem,omitempty"`
}

type Shard struct {
	WorldID          string                  `json:"worldId"`
	WorldName        string                  `json:"worldName"`
	WorldRole        string                  `json:"worldRole"`
	State            string                  `json:"state"`
	Placement        topology.Placement      `json:"placement"`
	Target           *topology.TargetSummary `json:"target,omitempty"`
	Runtime          RuntimeStatus           `json:"runtime"`
	RuntimeProblem   *Problem                `json:"runtimeProblem,omitempty"`
	Health           ArtifactStatus          `json:"health"`
	LatestDiagnostic ArtifactStatus          `json:"latestDiagnostic"`
}

type Summary struct {
	Total       int `json:"total"`
	Healthy     int `json:"healthy"`
	Degraded    int `json:"degraded"`
	Unavailable int `json:"unavailable"`
	Running     int `json:"running"`
	Stopped     int `json:"stopped"`
	Planned     int `json:"planned"`
}

type Snapshot struct {
	RoomID               string                   `json:"roomId"`
	TopologyRevision     string                   `json:"topologyRevision"`
	RemoteExecutionReady bool                     `json:"remoteExecutionReady"`
	Summary              Summary                  `json:"summary"`
	Shards               []Shard                  `json:"shards"`
	Targets              []topology.TargetSummary `json:"targets"`
	Issues               []topology.Issue         `json:"issues"`
	ObservedAt           time.Time                `json:"observedAt"`
}

type Service struct {
	topology  Topology
	runtime   Runtime
	artifacts Artifacts
	now       func() time.Time
	timeout   time.Duration
}

func New(topologyService Topology, runtime Runtime, artifacts Artifacts) (*Service, error) {
	if topologyService == nil || runtime == nil || artifacts == nil {
		return nil, errors.New("runtime overview dependencies are required")
	}
	return &Service{topology: topologyService, runtime: runtime, artifacts: artifacts, now: time.Now, timeout: 15 * time.Second}, nil
}

func (s *Service) Snapshot(ctx context.Context, roomID string) (Snapshot, error) {
	topologySnapshot, err := s.topology.Topology(ctx, roomID)
	if err != nil {
		return Snapshot{}, err
	}
	targets := make(map[string]topology.TargetSummary, len(topologySnapshot.Targets))
	for _, target := range topologySnapshot.Targets {
		targets[target.ID] = target
	}
	result := Snapshot{
		RoomID: topologySnapshot.RoomID, TopologyRevision: topologySnapshot.Revision,
		RemoteExecutionReady: topologySnapshot.RemoteExecutionReady,
		Targets:              append([]topology.TargetSummary(nil), topologySnapshot.Targets...),
		Issues:               append([]topology.Issue(nil), topologySnapshot.Issues...),
		ObservedAt:           s.now().UTC(), Shards: make([]Shard, len(topologySnapshot.Placements)),
	}
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for index, placement := range topologySnapshot.Placements {
		index, placement := index, placement
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			shardContext, cancel := context.WithTimeout(ctx, s.timeout)
			defer cancel()
			result.Shards[index] = s.inspectShard(shardContext, topologySnapshot.RoomID, placement, targets, result.ObservedAt)
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	result.Summary.Total = len(result.Shards)
	for _, shard := range result.Shards {
		switch shard.State {
		case "healthy":
			result.Summary.Healthy++
		case "stopped":
			// A deliberately stopped, correctly placed Shard is neutral rather than degraded.
		case "unavailable":
			result.Summary.Unavailable++
		default:
			result.Summary.Degraded++
		}
		if shard.Runtime.State == "running" {
			result.Summary.Running++
		}
		if shard.Runtime.State == "stopped" {
			result.Summary.Stopped++
		}
		if shard.Placement.DesiredTargetID != shard.Placement.AppliedTargetID ||
			shard.Placement.DesiredInstallationID != shard.Placement.AppliedInstallationID {
			result.Summary.Planned++
		}
	}
	return result, nil
}

func (s *Service) inspectShard(ctx context.Context, roomID string, placement topology.Placement, targets map[string]topology.TargetSummary, observedAt time.Time) Shard {
	shard := Shard{
		WorldID: placement.WorldID, WorldName: placement.WorldName, WorldRole: string(placement.WorldRole),
		State: "unavailable", Placement: placement,
		Health: ArtifactStatus{Freshness: "unavailable"}, LatestDiagnostic: ArtifactStatus{Freshness: "unavailable"},
	}
	if target, exists := targets[placement.AppliedTargetID]; exists {
		value := target
		shard.Target = &value
	}
	status, statusErr := s.runtime.Status(ctx, roomID, placement.WorldID)
	if statusErr != nil {
		shard.Runtime.State = "unknown"
		shard.RuntimeProblem = problem(statusErr, "RUNTIME_STATUS_UNAVAILABLE")
	} else {
		shard.Runtime = RuntimeStatus{State: status.State, Code: status.Code, Message: status.Message, SessionExists: status.SessionExists, Paused: status.Paused}
	}
	health, healthErr := s.artifacts.Health(ctx, roomID, placement.WorldID)
	if healthErr != nil {
		shard.Health.Problem = problem(healthErr, "RUNTIME_HEALTH_UNAVAILABLE")
	} else {
		instant := health.ReadAt.UTC()
		shard.Health = ArtifactStatus{
			Available: true, Freshness: freshness(observedAt, instant, 20*time.Second), AgeSeconds: ageSeconds(observedAt, instant),
			ObservedAt: &instant, Health: &health,
		}
	}
	diagnostic, diagnosticErr := s.artifacts.LatestDiagnostic(ctx, roomID, placement.WorldID)
	if diagnosticErr != nil {
		shard.LatestDiagnostic.Problem = problem(diagnosticErr, "RUNTIME_DIAGNOSTIC_UNAVAILABLE")
	} else {
		instant := diagnostic.CompletedAt.UTC()
		shard.LatestDiagnostic = ArtifactStatus{
			Available: true, Freshness: freshness(observedAt, instant, 24*time.Hour), AgeSeconds: ageSeconds(observedAt, instant),
			ObservedAt: &instant, Diagnostic: &diagnostic,
		}
	}
	shard.State = shardState(shard)
	return shard
}

func shardState(shard Shard) string {
	if shard.RuntimeProblem != nil || shard.Target == nil || !shard.Target.Online || !shard.Target.Configured {
		return "unavailable"
	}
	if shard.Placement.State != topology.PlacementAligned {
		return "degraded"
	}
	if shard.Runtime.State == "stopped" {
		return "stopped"
	}
	if shard.Runtime.Code == savehealth.SaveWriteFailedCode {
		return "degraded"
	}
	if shard.Runtime.State != "running" {
		return "degraded"
	}
	if !shard.Health.Available || shard.Health.Freshness != "live" || shard.Health.Health == nil || !shard.Health.Health.Ready || shard.Health.Health.LastError != nil {
		return "degraded"
	}
	return "healthy"
}

func problem(err error, fallback string) *Problem {
	if err == nil {
		return nil
	}
	code := fallback
	var execution *topology.ExecutionError
	switch {
	case errors.As(err, &execution) && strings.TrimSpace(execution.Code) != "":
		code = execution.Code
	case errors.Is(err, dstruntime.ErrSnapshotUnavailable):
		code = "RUNTIME_ARTIFACT_MISSING"
	case errors.Is(err, dstruntime.ErrSnapshotStale), errors.Is(err, dstruntime.ErrRuntimeResultStale):
		code = "RUNTIME_ARTIFACT_STALE"
	case errors.Is(err, dstruntime.ErrSnapshotInvalid), errors.Is(err, dstruntime.ErrRuntimeResultInvalid):
		code = "RUNTIME_ARTIFACT_INVALID"
	case errors.Is(err, dstruntime.ErrRuntimeResultAbsent):
		code = "RUNTIME_RESULT_NOT_FOUND"
	case errors.Is(err, context.DeadlineExceeded):
		code = "RUNTIME_OBSERVATION_TIMEOUT"
	}
	return &Problem{Code: code, Message: err.Error()}
}

func freshness(now, observedAt time.Time, liveFor time.Duration) string {
	if observedAt.IsZero() {
		return "unavailable"
	}
	if ageSeconds(now, observedAt) <= int64(liveFor/time.Second) {
		return "live"
	}
	return "stale"
}

func ageSeconds(now, observedAt time.Time) int64 {
	age := now.UTC().Sub(observedAt.UTC())
	if age < 0 {
		return 0
	}
	return int64(age / time.Second)
}
