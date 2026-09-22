package runtimedriver

import (
	"context"
	"dont/internal/agents"
	"dont/internal/rooms"
	"dont/internal/topology"
	"dont/shared"
	"errors"
	"testing"
)

type artworkDriver struct {
	Driver
	targets []Target
	err     error
}

func (*artworkDriver) Kind() Kind                 { return KindNative }
func (*artworkDriver) Capabilities() []Capability { return []Capability{CapabilityEntityArtwork} }
func (d *artworkDriver) ReadEntityArtwork(_ context.Context, t Target, _ shared.RuntimeEntityArtworkRequest) ([]byte, error) {
	d.targets = append(d.targets, t)
	return nil, d.err
}
func TestArtworkFollowsSplitPlacementAndNeverFallsBackLocally(t *testing.T) {
	p := &worldStatePlacement{values: map[string]topology.ExecutionPlacement{}}
	for world, node := range map[string]string{"Master": "local", "Caves": "agent:two"} {
		kind := agents.RuntimeKindAgent
		if node == "local" {
			kind = agents.RuntimeKindLocal
		}
		p.values[world] = topology.ExecutionPlacement{AppliedTargetID: node, AppliedInstallationID: "default", Revision: "revision", Room: rooms.Room{DirectoryName: "room"}, World: rooms.World{DirectoryName: world}, Target: agents.RuntimeTarget{ID: node, Kind: kind, Capabilities: []string{"runtime.entity-artwork.v1"}}}
	}
	local, remote := &artworkDriver{}, &artworkDriver{}
	acquires := 0
	r, err := NewRouter(p, cpuLifecycleLease{acquires: &acquires}, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	for _, world := range []string{"Master", "Caves"} {
		if _, err = r.ReadEntityArtwork(context.Background(), "room", world, shared.RuntimeEntityArtworkRequest{Prefab: "log"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(local.targets) != 1 || len(remote.targets) != 1 || remote.targets[0].TargetID != "agent:two" || acquires != 0 {
		t.Fatalf("local=%v remote=%v leases=%v", local.targets, remote.targets, acquires)
	}
	remote.err = errors.New("offline")
	if _, err = r.ReadEntityArtwork(context.Background(), "room", "Caves", shared.RuntimeEntityArtworkRequest{Prefab: "log"}); !errors.Is(err, remote.err) || len(local.targets) != 1 {
		t.Fatal("remote error hidden", err)
	}
	old := p.values["Caves"]
	old.Target.Capabilities = []string{"runtime.worldstate.read.v1"}
	p.values["Caves"] = old
	if _, err = r.ReadEntityArtwork(context.Background(), "room", "Caves", shared.RuntimeEntityArtworkRequest{Prefab: "log"}); !errors.Is(err, ErrCapabilityMissing) {
		t.Fatal("old Agent was called", err)
	}
}

type artworkExecutor struct {
	emptyMigrationExecutor
	request shared.RuntimeOperationRequest
	result  shared.RuntimeEntityArtworkResult
}

func (e *artworkExecutor) ExecuteRuntime(_ context.Context, _ string, request shared.RuntimeOperationRequest, _ int) (agents.RuntimeExecutionResult, error) {
	e.request = request
	return agents.RuntimeExecutionResult{Result: shared.RuntimeOperationResult{EntityArtwork: &e.result}}, nil
}
func TestRemoteArtworkValidatesIdentityAndPNGWithoutMutationLease(t *testing.T) {
	e := &artworkExecutor{result: shared.RuntimeEntityArtworkResult{Prefab: "log", ModID: "workshop-123"}}
	driver, _ := NewAgent(e)
	target := Target{TargetID: "agent:one", InstallationID: "default", Cluster: "room", Shard: "Master"}
	input := shared.RuntimeEntityArtworkRequest{Prefab: "log", ModID: "workshop-123"}
	if data, err := driver.ReadEntityArtwork(context.Background(), target, input); err != nil || len(data) != 0 {
		t.Fatal(err)
	}
	if e.request.EntityArtwork == nil || e.request.EntityArtwork.ModID != input.ModID || e.request.Action != shared.RuntimeActionEntityArtwork || shared.RuntimeOperationRequiresLease(e.request) {
		t.Fatal("incorrect remote read", e.request)
	}
	e.result.ModID = "workshop-456"
	if _, err := driver.ReadEntityArtwork(context.Background(), target, input); err == nil {
		t.Fatal("cross-mod response accepted")
	}
	e.result.ModID = input.ModID
	e.result.Data = []byte("not a PNG")
	if _, err := driver.ReadEntityArtwork(context.Background(), target, input); err == nil {
		t.Fatal("invalid binary response accepted")
	}
}
