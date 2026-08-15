package runtimedriver

import (
	"testing"

	"dont/internal/rooms"
	"dont/internal/topology"
)

func TestTargetFromLocalPlacementUsesDefaultInstallation(t *testing.T) {
	target := targetFromPlacement(topology.ExecutionPlacement{
		AppliedTargetID: "local",
		Revision:        "revision-1",
		Room:            rooms.Room{DirectoryName: "Cluster_1"},
		World:           rooms.World{DirectoryName: "Master"},
	}, "room-1", "world-1")

	if target.InstallationID != "default" || target.TargetID != "local" || target.Cluster != "Cluster_1" || target.Shard != "Master" {
		t.Fatalf("unexpected local target: %#v", target)
	}
}
