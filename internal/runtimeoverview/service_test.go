package runtimeoverview

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"
)

type overviewTopology struct{ snapshot topology.Snapshot }

func (s overviewTopology) Topology(context.Context, string) (topology.Snapshot, error) {
	return s.snapshot, nil
}

type overviewRuntime struct {
	statuses map[string]shared.ShardRuntimeStatus
	failures map[string]error
}

func (s overviewRuntime) Status(_ context.Context, _, worldID string) (shared.ShardRuntimeStatus, error) {
	return s.statuses[worldID], s.failures[worldID]
}

type overviewArtifacts struct{ now time.Time }

func (s overviewArtifacts) Health(_ context.Context, _, worldID string) (dstruntime.Health, error) {
	if worldID == "caves" {
		return dstruntime.Health{}, dstruntime.ErrSnapshotUnavailable
	}
	return dstruntime.Health{ProducerVersion: dstruntime.RuntimeVersion, Ready: true, Running: true, ReadAt: s.now}, nil
}

func (s overviewArtifacts) LatestDiagnostic(_ context.Context, _, worldID string) (dstruntime.DiagnosticReport, error) {
	if worldID == "caves" {
		return dstruntime.DiagnosticReport{}, dstruntime.ErrRuntimeResultAbsent
	}
	return dstruntime.DiagnosticReport{OK: true, CompletedAt: s.now}, nil
}

func TestSnapshotKeepsPartialShardFailuresVisible(t *testing.T) {
	now := time.Now().UTC()
	local := topology.TargetSummary{ID: "local", Name: "本机", Kind: agents.RuntimeKindLocal, Online: true, Configured: true}
	remote := topology.TargetSummary{ID: "agent:node", Name: "节点", Kind: agents.RuntimeKindAgent, Online: false, Configured: true}
	topologySnapshot := topology.Snapshot{
		RoomID: "room", Revision: "revision", Targets: []topology.TargetSummary{local, remote},
		Placements: []topology.Placement{
			{WorldID: "master", WorldName: "Master", WorldRole: rooms.WorldRoleMaster, AppliedTargetID: "local", DesiredTargetID: "local", State: topology.PlacementAligned},
			{WorldID: "caves", WorldName: "Caves", WorldRole: rooms.WorldRoleCaves, AppliedTargetID: "agent:node", DesiredTargetID: "agent:node", State: topology.PlacementTargetOffline},
		},
	}
	service, err := New(overviewTopology{snapshot: topologySnapshot}, overviewRuntime{
		statuses: map[string]shared.ShardRuntimeStatus{"master": {State: "running", SessionExists: true}},
		failures: map[string]error{"caves": errors.New("agent offline")},
	}, overviewArtifacts{now: now})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	result, err := service.Snapshot(context.Background(), "room")
	if err != nil || result.Summary.Total != 2 || result.Summary.Healthy != 1 || result.Summary.Unavailable != 1 {
		t.Fatalf("snapshot=%#v err=%v", result, err)
	}
	if result.Shards[0].State != "healthy" || result.Shards[1].RuntimeProblem == nil || result.Shards[1].Health.Problem == nil {
		t.Fatalf("shards=%#v", result.Shards)
	}
}

func TestSnapshotTreatsAlignedStoppedShardAsNeutral(t *testing.T) {
	now := time.Now().UTC()
	local := topology.TargetSummary{ID: "local", Name: "本机", Kind: agents.RuntimeKindLocal, Online: true, Configured: true}
	topologySnapshot := topology.Snapshot{
		RoomID: "room", Revision: "revision", Targets: []topology.TargetSummary{local},
		Placements: []topology.Placement{
			{WorldID: "master", WorldName: "Master", WorldRole: rooms.WorldRoleMaster, AppliedTargetID: "local", DesiredTargetID: "local", State: topology.PlacementAligned},
		},
	}
	service, err := New(overviewTopology{snapshot: topologySnapshot}, overviewRuntime{
		statuses: map[string]shared.ShardRuntimeStatus{"master": {State: "stopped"}},
		failures: map[string]error{},
	}, overviewArtifacts{now: now})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	result, err := service.Snapshot(context.Background(), "room")
	if err != nil {
		t.Fatal(err)
	}
	if result.Shards[0].State != "stopped" || result.Summary.Stopped != 1 || result.Summary.Degraded != 0 {
		t.Fatalf("snapshot=%#v", result)
	}
}

func TestSnapshotTreatsRunningSaveFailureAsDegraded(t *testing.T) {
	now := time.Now().UTC()
	local := topology.TargetSummary{ID: "local", Name: "本机", Kind: agents.RuntimeKindLocal, Online: true, Configured: true}
	service, err := New(overviewTopology{snapshot: topology.Snapshot{
		RoomID: "room", Revision: "revision", Targets: []topology.TargetSummary{local},
		Placements: []topology.Placement{{
			WorldID: "master", WorldName: "Master", WorldRole: rooms.WorldRoleMaster,
			AppliedTargetID: "local", DesiredTargetID: "local", State: topology.PlacementAligned,
		}},
	}}, overviewRuntime{
		statuses: map[string]shared.ShardRuntimeStatus{"master": {
			State: "running", Code: "SAVE_WRITE_FAILED", Message: "save failed", SessionExists: true,
		}},
		failures: map[string]error{},
	}, overviewArtifacts{now: now})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	result, err := service.Snapshot(context.Background(), "room")
	if err != nil {
		t.Fatal(err)
	}
	if result.Shards[0].State != "degraded" || result.Shards[0].Runtime.State != "running" || result.Summary.Degraded != 1 || result.Summary.Running != 1 {
		t.Fatalf("snapshot=%#v", result)
	}
}
