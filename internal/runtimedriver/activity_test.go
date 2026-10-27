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

type activityDriverStub struct {
	cpuLifecycleDriver
	calls   int
	target  Target
	failure error
}

func (d *activityDriverStub) ReadActivityArtifacts(_ context.Context, target Target, _ []string) (shared.RuntimeArtifactBundle, error) {
	d.calls++
	d.target = target
	return shared.RuntimeArtifactBundle{}, d.failure
}
func TestRemoteActivityNeverFallsBackToLocal(t *testing.T) {
	placement := &cpuLifecyclePlacement{placement: topology.ExecutionPlacement{Room: rooms.Room{DirectoryName: "room"}, World: rooms.World{DirectoryName: "Caves"}, Revision: "revision", AppliedTargetID: "agent:node", Target: agents.RuntimeTarget{ID: "agent:node", AgentID: "node", Config: agents.RuntimeConfig{InstallationID: "primary"}}}}
	local := &activityDriverStub{}
	remote := &activityDriverStub{failure: errors.New("offline")}
	router, e := NewRouter(placement, cpuLifecycleLease{}, local, remote)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = router.ReadActivityArtifacts(context.Background(), "room", "world", nil); e == nil {
		t.Fatal("offline result accepted")
	}
	if remote.calls != 1 || local.calls != 0 || remote.target.Shard != "Caves" || remote.target.InstallationID != "primary" {
		t.Fatalf("remote=%+v local=%d", remote, local.calls)
	}
	if _, e = router.ReadActivityArtifacts(context.Background(), "room", "world", []string{"../save"}); e == nil || remote.calls != 1 {
		t.Fatal("unsafe name reached node")
	}
	old := &cpuLifecycleDriver{}
	router, e = NewRouter(placement, cpuLifecycleLease{}, local, old)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = router.ReadActivityArtifacts(context.Background(), "room", "world", nil); !errors.Is(e, ErrCapabilityMissing) || local.calls != 0 {
		t.Fatalf("old agent unsupported %v", e)
	}
}
